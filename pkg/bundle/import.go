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

package bundle

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"

	"github.com/tym83/kubepkg/pkg/images"
	"github.com/tym83/kubepkg/pkg/source"
)

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// ImportOptions say where a verified bundle goes.
type ImportOptions struct {
	// Mirror is the oci:// registry path to copy into; the operator's
	// --mirror (chart value mirror) points at the same path.
	Mirror string
	// Push carries the registry transport and credentials.
	Push source.PushOptions
	// SiteDir receives each repository's files, <SiteDir>/<repository>/,
	// to serve over HTTP inside the air gap.
	SiteDir string
}

// ImportResult says what was copied.
type ImportResult struct {
	Charts, Trees, Images int
	// Registries are the image registries the mirror now stands in for.
	Registries []string
}

// Import copies a verified bundle into the mirror registry and writes the
// repositories' files for serving. Copies keep their digests, so the
// signed index serves unchanged.
func Import(ctx context.Context, v *Verified, o ImportOptions) (*ImportResult, error) {
	mirror := strings.TrimSuffix(o.Mirror, "/")
	if !strings.HasPrefix(mirror, "oci://") {
		return nil, fmt.Errorf("mirror %q must be an oci:// registry path", o.Mirror)
	}
	res := &ImportResult{}
	push := o.Push
	push.Immutable = true
	for _, c := range v.Manifest.Charts {
		archive, err := os.ReadFile(chartFile(v.Dir, c.Digest))
		if err != nil {
			return nil, err
		}
		m := source.MirrorChart(mirror, source.Chart{Repository: c.Repository, Name: c.Name, Version: c.Version})
		pushed, err := source.PushChartArchive(ctx, archive, m.Repository, c.Name, c.Version, push)
		if err != nil {
			return nil, fmt.Errorf("chart %s %s: %w", c.Name, c.Version, err)
		}
		if pushed.LayerDigest != c.Digest {
			return nil, fmt.Errorf("chart %s %s landed with digest %s, not %s", c.Name, c.Version, pushed.LayerDigest, c.Digest)
		}
		res.Charts++
	}
	layout, err := oci.NewWithContext(ctx, filepath.Join(v.Dir, layoutDir))
	if err != nil {
		return nil, err
	}
	for _, t := range v.Manifest.Trees {
		dst, err := source.MirrorRef(mirror, t.Ref)
		if err != nil {
			return nil, err
		}
		if err := copyOut(ctx, layout, t.Ref, dst, push); err != nil {
			return nil, err
		}
		res.Trees++
	}
	registries := map[string]bool{}
	for _, img := range v.Manifest.Images {
		repoName, tag, digest := images.Split(img.Ref)
		dst := mirror + "/" + source.MirrorPath(repoName)
		if tag != "" {
			dst += ":" + tag
		}
		dst += "@" + digest
		if err := copyOut(ctx, layout, img.Ref, "oci://"+strings.TrimPrefix(dst, "oci://"), push); err != nil {
			return nil, err
		}
		registries[strings.SplitN(repoName, "/", 2)[0]] = true
		res.Images++
	}
	for r := range registries {
		res.Registries = append(res.Registries, r)
	}
	sort.Strings(res.Registries)
	if o.SiteDir != "" {
		for _, r := range v.Manifest.Repositories {
			for _, f := range r.Files {
				src, err := safePath(filepath.Join(v.Dir, repositoriesDir, r.Name), f)
				if err != nil {
					return nil, err
				}
				dst, err := safePath(filepath.Join(o.SiteDir, r.Name), f)
				if err != nil {
					return nil, err
				}
				raw, err := os.ReadFile(src)
				if err != nil {
					return nil, err
				}
				if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
					return nil, err
				}
				if err := os.WriteFile(dst, raw, 0o644); err != nil {
					return nil, err
				}
			}
		}
	}
	return res, nil
}

// copyOut copies the layout entry ref to dst, oci://host/path[:tag]@digest,
// tagging it when dst has a tag.
func copyOut(ctx context.Context, layout *oci.Store, ref, dst string, push source.PushOptions) error {
	desc, err := layout.Resolve(ctx, ref)
	if err != nil {
		return fmt.Errorf("%s: %w", ref, err)
	}
	target := strings.TrimPrefix(dst, "oci://")
	repoName, tag, _ := images.Split(target)
	repo, err := push.Open("oci://" + repoName)
	if err != nil {
		return err
	}
	if err := oras.CopyGraph(ctx, layout, repo, desc, oras.DefaultCopyGraphOptions); err != nil {
		return fmt.Errorf("copy %s to %s: %w", ref, repoName, err)
	}
	ref2 := desc.Digest.String()
	if tag != "" {
		ref2 = tag
	}
	if err := repo.Tag(ctx, desc, ref2); err != nil {
		return fmt.Errorf("tag %s in %s: %w", ref, repoName, err)
	}
	return nil
}

// NodeMirrors returns registry mirror settings that send image pulls for
// registries to the mirror: a containerd hosts.toml per registry, keyed
// by registry, and a Talos machine config patch.
func NodeMirrors(mirror string, plainHTTP bool, registries []string) (map[string]string, string, error) {
	u, err := url.Parse(strings.Replace(strings.TrimSuffix(mirror, "/"), "oci://", "https://", 1))
	if err != nil {
		return nil, "", err
	}
	scheme := "https"
	if plainHTTP {
		scheme = "http"
	}
	containerd := map[string]string{}
	var talos strings.Builder
	talos.WriteString("machine:\n  registries:\n    mirrors:\n")
	for _, r := range registries {
		endpoint := fmt.Sprintf("%s://%s/v2%s/%s", scheme, u.Host, u.Path, source.MirrorPath(r))
		server := "https://" + r
		if r == "docker.io" {
			server = "https://registry-1.docker.io"
		}
		containerd[r] = fmt.Sprintf("server = %q\n\n[host.%q]\n  capabilities = [\"pull\", \"resolve\"]\n  override_path = true\n", server, endpoint)
		fmt.Fprintf(&talos, "      %s:\n        endpoints: [%q]\n        overridePath: true\n", r, endpoint)
	}
	return containerd, talos.String(), nil
}

// Pack writes the bundle directory dir as a tar stream. Images are
// compressed already, so the stream is not.
func Pack(dir string, w io.Writer) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			return fmt.Errorf("%s: only files and directories go into a bundle", rel)
		}
		h, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		h.Name = filepath.ToSlash(rel)
		h.ModTime, h.Uid, h.Gid, h.Uname, h.Gname = h.ModTime.UTC().Truncate(1e9), 0, 0, "", ""
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// Unpack extracts a bundle tar stream into dir, refusing entries that
// leave it and anything but files and directories.
func Unpack(r io.Reader, dir string) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		p, err := safePath(dir, h.Name)
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s: only files and directories may be in a bundle", h.Name)
		}
	}
}
