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

// Package source fetches package trees and composes charts from them.
//
// A package tree is published as a single-layer OCI artifact in the shape
// Flux uses (`flux push artifact`): one gzipped tarball of the packages
// directory. kubepkg reads these artifacts without needing Flux to be
// installed.
package source

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"
)

// Layer media types accepted as a package tree.
var treeMediaTypes = map[string]bool{
	"application/vnd.cncf.flux.content.v1.tar+gzip": true,
	"application/vnd.oci.image.layer.v1.tar+gzip":   true,
	"application/vnd.kubepkg.tree.v1.tar+gzip":      true,
}

// maxTreeBytes bounds an unpacked tree. A package tree is charts and YAML;
// anything larger is a mistake or an attack, and filling the operator's disk
// would take every package down with it.
const maxTreeBytes = 512 << 20

// Fetcher downloads package trees and charts and caches them by digest,
// under trees/ and charts/ of CacheDir.
type Fetcher struct {
	CacheDir string
	// PlainHTTP talks to registries without TLS (local test registries).
	PlainHTTP bool
	// CredentialsFile is a Docker config with registry credentials; empty
	// means anonymous access.
	CredentialsFile string
	// Mirror, an oci:// registry path, is where every chart and package
	// tree is fetched from instead of where it was published; see
	// MirrorPath. Digests are verified as usual.
	Mirror string

	mu sync.Mutex
}

// Fetch resolves an oci:// reference and returns the directory the tree is
// unpacked into and the manifest digest. Unpacked trees are immutable and
// keyed by digest, so a tag that moves produces a new directory.
func (f *Fetcher) Fetch(ctx context.Context, ref string) (dir, digest string, err error) {
	if !strings.HasPrefix(ref, "oci://") {
		return "", "", fmt.Errorf("source %q is not an oci:// reference", ref)
	}
	ref, err = MirrorRef(f.Mirror, ref)
	if err != nil {
		return "", "", err
	}
	target := strings.TrimPrefix(ref, "oci://")
	repo, err := f.repository(target)
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", ref, err)
	}

	desc, err := repo.Resolve(ctx, repo.Reference.Reference)
	if err != nil {
		return "", "", fmt.Errorf("resolve %s: %w", ref, err)
	}
	digest = desc.Digest.String()
	trees := filepath.Join(f.CacheDir, "trees")
	dir = filepath.Join(trees, strings.ReplaceAll(digest, ":", "-"))

	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := os.Stat(filepath.Join(dir, ".complete")); err == nil {
		return dir, digest, nil
	}

	raw, err := content.FetchAll(ctx, repo, desc)
	if err != nil {
		return "", "", fmt.Errorf("fetch manifest of %s: %w", ref, err)
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return "", "", fmt.Errorf("decode manifest of %s: %w", ref, err)
	}
	var layer *ocispec.Descriptor
	for i := range manifest.Layers {
		if treeMediaTypes[manifest.Layers[i].MediaType] {
			layer = &manifest.Layers[i]
			break
		}
	}
	if layer == nil {
		return "", "", fmt.Errorf("%s has no package tree layer", ref)
	}
	rc, err := repo.Fetch(ctx, *layer)
	if err != nil {
		return "", "", fmt.Errorf("fetch layer of %s: %w", ref, err)
	}
	defer rc.Close()

	if err := os.MkdirAll(trees, 0o755); err != nil {
		return "", "", err
	}
	tmp, err := os.MkdirTemp(trees, ".unpack-")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(tmp)
	if err := untar(content.NewVerifyReader(rc, *layer), tmp); err != nil {
		return "", "", fmt.Errorf("unpack %s: %w", ref, err)
	}
	if err := os.WriteFile(filepath.Join(tmp, ".complete"), nil, 0o644); err != nil {
		return "", "", err
	}
	if err := os.Rename(tmp, dir); err != nil && !errors.Is(err, os.ErrExist) {
		return "", "", err
	}
	return dir, digest, nil
}

// repository opens an OCI repository with the fetcher's transport settings.
func (f *Fetcher) repository(target string) (*remote.Repository, error) {
	repo, err := remote.NewRepository(target)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", target, err)
	}
	repo.PlainHTTP = f.PlainHTTP
	client := &auth.Client{Client: retry.DefaultClient, Cache: auth.NewCache()}
	if f.CredentialsFile != "" {
		store, err := credentials.NewStore(f.CredentialsFile, credentials.StoreOptions{})
		if err != nil {
			return nil, fmt.Errorf("load registry credentials: %w", err)
		}
		client.Credential = credentials.Credential(store)
	}
	repo.Client = client
	return repo, nil
}

// untar unpacks a gzipped tarball into dst, refusing entries that escape
// dst, links, and trees beyond maxTreeBytes.
func untar(r io.Reader, dst string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(h.Name)
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("entry %q escapes the tree", h.Name)
		}
		path := filepath.Join(dst, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			total += h.Size
			if total > maxTreeBytes {
				return fmt.Errorf("tree is larger than %d bytes", maxTreeBytes)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.CopyN(out, tr, h.Size); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		default:
			// Symlinks and devices have no place in a chart tree and are the
			// usual way out of the unpack directory.
			return fmt.Errorf("entry %q has unsupported type %c", h.Name, h.Typeflag)
		}
	}
}
