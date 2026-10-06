/*
Copyright 2026 The kubepkg Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package source

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	content2 "oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"
	"sigs.k8s.io/yaml"
)

// Media types of a Flux artifact, so Flux and kubepkg both read what
// kubepkg publishes.
const (
	FluxConfigMediaType  = "application/vnd.cncf.flux.config.v1+json"
	FluxContentMediaType = "application/vnd.cncf.flux.content.v1.tar+gzip"
)

// PushOptions describe where a tree came from; they end up in annotations.
type PushOptions struct {
	Source    string
	Revision  string
	PlainHTTP bool
	// CredentialsFile is a Docker config with registry credentials.
	CredentialsFile string
	// Immutable refuses to move a tag: when the tag already holds the same
	// content, the published artifact is reused; different content is an
	// error.
	Immutable bool
}

// AnnotationContentDigest on a published tree is the sha256 of its
// uncompressed tarball. The compressed layer depends on the compressor,
// which changes between Go releases; the content does not.
const AnnotationContentDigest = "dev.kubepkg.content.digest"

// ErrTagChanged is returned when an immutable tag already holds different
// content.
var ErrTagChanged = errors.New("tag already published with different content")

// gzipLevel and annotateContent are variables so tests can stand in for
// another compressor and for artifacts published without the annotation.
var (
	gzipLevel       = gzip.BestCompression
	annotateContent = true
)

// PushResult describes a push.
type PushResult struct {
	// Digest is the manifest digest the tag points at.
	Digest string
	// LayerDigest is the digest of the single layer: for a chart, the
	// digest of the chart archive.
	LayerDigest string
	// Reused is true when the tag already held the same content and
	// nothing was pushed.
	Reused bool
}

// Push packs dir into a reproducible gzipped tarball and pushes it as a
// single-layer artifact to ref (oci://host/repo:tag).
func Push(ctx context.Context, dir, ref string, opts PushOptions) (PushResult, error) {
	layer, content, err := Pack(dir)
	if err != nil {
		return PushResult{}, err
	}
	return pushArtifact(ctx, ref, opts, artifact{
		config: []byte("{}"), configType: FluxConfigMediaType,
		layer: layer, layerType: FluxContentMediaType, content: content,
	})
}

// artifact is one single-layer OCI artifact to push.
type artifact struct {
	config     []byte
	configType string
	layer      []byte
	layerType  string
	content    string
}

func (o PushOptions) repository(ref string) (*remote.Repository, error) {
	target := strings.TrimPrefix(ref, "oci://")
	if target == ref {
		return nil, fmt.Errorf("target %q must be an oci:// reference", ref)
	}
	repo, err := remote.NewRepository(target)
	if err != nil {
		return nil, err
	}
	repo.PlainHTTP = o.PlainHTTP
	client := &auth.Client{Client: retry.DefaultClient, Cache: auth.NewCache()}
	if o.CredentialsFile != "" {
		store, err := credentials.NewStore(o.CredentialsFile, credentials.StoreOptions{})
		if err != nil {
			return nil, err
		}
		client.Credential = credentials.Credential(store)
	} else if store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{}); err == nil {
		client.Credential = credentials.Credential(store)
	}
	repo.Client = client
	return repo, nil
}

// Open opens the OCI repository of an oci:// reference with the push
// options' transport and credentials.
func (o PushOptions) Open(ref string) (*remote.Repository, error) { return o.repository(ref) }

func pushArtifact(ctx context.Context, ref string, opts PushOptions, a artifact) (PushResult, error) {
	repo, err := opts.repository(ref)
	if err != nil {
		return PushResult{}, err
	}
	if opts.Immutable {
		if res, done, err := published(ctx, repo, a.content); done {
			return res, err
		}
	}
	configDesc := ocispec.Descriptor{MediaType: a.configType, Digest: digest.FromBytes(a.config), Size: int64(len(a.config))}
	layerDesc := ocispec.Descriptor{MediaType: a.layerType, Digest: digest.FromBytes(a.layer), Size: int64(len(a.layer))}
	created := time.Unix(0, 0).UTC().Format(time.RFC3339)
	manifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    []ocispec.Descriptor{layerDesc},
		Annotations: map[string]string{
			"org.opencontainers.image.created":  created,
			"org.opencontainers.image.source":   opts.Source,
			"org.opencontainers.image.revision": opts.Revision,
		},
	}
	if annotateContent {
		manifest.Annotations[AnnotationContentDigest] = a.content
	}
	mraw, err := json.Marshal(manifest)
	if err != nil {
		return PushResult{}, err
	}
	mdesc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(mraw), Size: int64(len(mraw))}
	for _, b := range []struct {
		d ocispec.Descriptor
		c []byte
	}{{configDesc, a.config}, {layerDesc, a.layer}} {
		if ok, err := repo.Exists(ctx, b.d); err == nil && ok {
			continue
		}
		if err := repo.Push(ctx, b.d, bytes.NewReader(b.c)); err != nil {
			return PushResult{}, fmt.Errorf("push blob: %w", err)
		}
	}
	if err := repo.PushReference(ctx, mdesc, bytes.NewReader(mraw), repo.Reference.Reference); err != nil {
		return PushResult{}, fmt.Errorf("push manifest: %w", err)
	}
	return PushResult{Digest: mdesc.Digest.String(), LayerDigest: layerDesc.Digest.String()}, nil
}

// published checks an immutable tag before pushing. done is true when the
// tag exists: then either the same content is there and res names it, or
// err says it changed.
func published(ctx context.Context, repo *remote.Repository, content string) (res PushResult, done bool, err error) {
	desc, err := repo.Resolve(ctx, repo.Reference.Reference)
	if errors.Is(err, errdef.ErrNotFound) {
		return PushResult{}, false, nil
	}
	if err != nil {
		return PushResult{}, true, fmt.Errorf("resolve %s: %w", repo.Reference, err)
	}
	raw, err := content2.FetchAll(ctx, repo, desc)
	if err != nil {
		return PushResult{}, true, err
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return PushResult{}, true, err
	}
	have := m.Annotations[AnnotationContentDigest]
	if have == "" {
		// Published before the annotation existed, or by another tool:
		// work the content out from the layer itself.
		if have, err = layerContent(ctx, repo, m); err != nil {
			return PushResult{}, true, fmt.Errorf("%s: %w", repo.Reference, err)
		}
	}
	if have != content {
		return PushResult{}, true, fmt.Errorf("%s: %w", repo.Reference, ErrTagChanged)
	}
	res = PushResult{Digest: desc.Digest.String(), Reused: true}
	if len(m.Layers) == 1 {
		res.LayerDigest = m.Layers[0].Digest.String()
	}
	return res, true, nil
}

// layerContent is the digest of the uncompressed tree layer of m.
func layerContent(ctx context.Context, repo *remote.Repository, m ocispec.Manifest) (string, error) {
	for _, l := range m.Layers {
		if !treeMediaTypes[l.MediaType] && l.MediaType != ChartMediaType {
			continue
		}
		rc, err := repo.Fetch(ctx, l)
		if err != nil {
			return "", err
		}
		defer rc.Close()
		gz, err := gzip.NewReader(content2.NewVerifyReader(rc, l))
		if err != nil {
			return "", err
		}
		d := digest.Canonical.Digester()
		if _, err := io.Copy(d.Hash(), io.LimitReader(gz, maxTreeBytes)); err != nil {
			return "", err
		}
		return d.Digest().String(), nil
	}
	return "", errors.New("no package tree or chart layer")
}

// Pack makes a gzipped tarball of dir with sorted entries, zero
// timestamps and fixed ownership, and returns it with the digest of the
// uncompressed tarball. The content digest is the same for the same tree
// everywhere; the compressed bytes are the same only with the same
// compressor.
func Pack(dir string) ([]byte, string, error) {
	return pack(dir, "", nil)
}

// pack tars the files under dir below prefix; replace substitutes the
// content of files by their path relative to dir.
func pack(dir, prefix string, replace map[string][]byte) ([]byte, string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink; package trees must not contain links", p)
		}
		if !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	sort.Strings(files)
	var tarball bytes.Buffer
	tw := tar.NewWriter(&tarball)
	for _, p := range files {
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return nil, "", err
		}
		body, ok := replace[filepath.ToSlash(rel)]
		if !ok {
			if body, err = os.ReadFile(p); err != nil {
				return nil, "", err
			}
		}
		hdr := &tar.Header{Name: path.Join(prefix, filepath.ToSlash(rel)), Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, "", err
		}
		if _, err := tw.Write(body); err != nil {
			return nil, "", err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, "", err
	}
	var buf bytes.Buffer
	gz, _ := gzip.NewWriterLevel(&buf, gzipLevel)
	gz.ModTime = time.Unix(0, 0)
	if _, err := gz.Write(tarball.Bytes()); err != nil {
		return nil, "", err
	}
	if err := gz.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), digest.FromBytes(tarball.Bytes()).String(), nil
}

// PushChart publishes the chart in dir as a Helm chart in an OCI registry,
// at <repository>/<name>:<version>, the way helm push does, with name and
// version set in its Chart.yaml. Helm, Flux, Argo CD and werf all install
// it as it is. LayerDigest of the result is the digest of the chart
// archive.
func PushChart(ctx context.Context, dir, repository, name, version string, opts PushOptions) (PushResult, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "Chart.yaml"))
	if err != nil {
		return PushResult{}, err
	}
	meta := map[string]any{}
	if err := yaml.Unmarshal(raw, &meta); err != nil {
		return PushResult{}, fmt.Errorf("Chart.yaml: %w", err)
	}
	meta["name"], meta["version"] = name, version
	chartYAML, err := yaml.Marshal(meta)
	if err != nil {
		return PushResult{}, err
	}
	config, err := json.Marshal(meta)
	if err != nil {
		return PushResult{}, err
	}
	layer, content, err := pack(dir, name, map[string][]byte{"Chart.yaml": chartYAML})
	if err != nil {
		return PushResult{}, err
	}
	ref := strings.TrimSuffix(repository, "/") + "/" + name + ":" + strings.ReplaceAll(version, "+", "_")
	return pushArtifact(ctx, ref, opts, artifact{
		config: config, configType: ChartConfigMediaType,
		layer: layer, layerType: ChartMediaType, content: content,
	})
}

// PushChartArchive publishes a chart archive exactly as it is at
// <repository>/<name>:<version>, so the copy keeps the archive digest a
// package pins. It is how kubepkg bundle import fills a mirror.
func PushChartArchive(ctx context.Context, archive []byte, repository, name, version string, opts PushOptions) (PushResult, error) {
	meta, err := chartMetadataOf(archive, name)
	if err != nil {
		return PushResult{}, err
	}
	config, err := json.Marshal(meta)
	if err != nil {
		return PushResult{}, err
	}
	ref := strings.TrimSuffix(repository, "/") + "/" + name + ":" + strings.ReplaceAll(version, "+", "_")
	// A mirror holds copies, not builds: the archive digest is the
	// identity, and a tag that already holds it is left alone.
	return pushArtifact(ctx, ref, opts, artifact{
		config: config, configType: ChartConfigMediaType,
		layer: archive, layerType: ChartMediaType, content: digest.FromBytes(archive).String(),
	})
}

// chartMetadataOf reads <name>/Chart.yaml from a chart archive.
func chartMetadataOf(archive []byte, name string) (map[string]any, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("chart archive has no %s/Chart.yaml", name)
		}
		if err != nil {
			return nil, err
		}
		if h.Name != name+"/Chart.yaml" {
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(tr, 1<<20))
		if err != nil {
			return nil, err
		}
		meta := map[string]any{}
		if err := yaml.Unmarshal(raw, &meta); err != nil {
			return nil, fmt.Errorf("Chart.yaml: %w", err)
		}
		return meta, nil
	}
}
