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
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"
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
}

// Push packs dir into a reproducible gzipped tarball and pushes it as a
// single-layer artifact to ref (oci://host/repo:tag). It returns the
// manifest digest.
func Push(ctx context.Context, dir, ref string, opts PushOptions) (string, error) {
	target := strings.TrimPrefix(ref, "oci://")
	if target == ref {
		return "", fmt.Errorf("target %q must be an oci:// reference", ref)
	}
	repo, err := remote.NewRepository(target)
	if err != nil {
		return "", err
	}
	repo.PlainHTTP = opts.PlainHTTP
	client := &auth.Client{Client: retry.DefaultClient, Cache: auth.NewCache()}
	if opts.CredentialsFile != "" {
		store, err := credentials.NewStore(opts.CredentialsFile, credentials.StoreOptions{})
		if err != nil {
			return "", err
		}
		client.Credential = credentials.Credential(store)
	} else if store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{}); err == nil {
		client.Credential = credentials.Credential(store)
	}
	repo.Client = client

	layer, err := Pack(dir)
	if err != nil {
		return "", err
	}
	config := []byte("{}")
	configDesc := ocispec.Descriptor{MediaType: FluxConfigMediaType, Digest: digest.FromBytes(config), Size: int64(len(config))}
	layerDesc := ocispec.Descriptor{MediaType: FluxContentMediaType, Digest: digest.FromBytes(layer), Size: int64(len(layer))}
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
	mraw, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	mdesc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(mraw), Size: int64(len(mraw))}

	for _, b := range []struct {
		d ocispec.Descriptor
		c []byte
	}{{configDesc, config}, {layerDesc, layer}} {
		if ok, err := repo.Exists(ctx, b.d); err == nil && ok {
			continue
		}
		if err := repo.Push(ctx, b.d, bytes.NewReader(b.c)); err != nil {
			return "", fmt.Errorf("push blob: %w", err)
		}
	}
	if err := repo.PushReference(ctx, mdesc, bytes.NewReader(mraw), repo.Reference.Reference); err != nil {
		return "", fmt.Errorf("push manifest: %w", err)
	}
	return mdesc.Digest.String(), nil
}

// Pack makes a gzipped tarball of dir with sorted entries, zero
// timestamps and fixed ownership, so the same tree always produces the
// same bytes and the same digest.
func Pack(dir string) ([]byte, error) {
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
		return nil, err
	}
	sort.Strings(files)
	var buf bytes.Buffer
	gz, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	gz.ModTime = time.Unix(0, 0)
	tw := tar.NewWriter(gz)
	for _, p := range files {
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		hdr := &tar.Header{Name: filepath.ToSlash(rel), Mode: 0o644, Size: info.Size(), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		_, err = io.Copy(tw, f)
		f.Close()
		if err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
