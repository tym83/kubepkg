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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/Masterminds/semver/v3"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"sigs.k8s.io/yaml"
)

// Media types of a Helm chart in a registry.
const (
	ChartMediaType       = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
	ChartConfigMediaType = "application/vnd.cncf.helm.config.v1+json"
)

// maxChartBytes bounds a downloaded chart archive, and maxIndexBytes a
// repository index. Large public repositories have indexes of tens of
// megabytes; charts are far smaller.
const (
	maxChartBytes = 64 << 20
	maxIndexBytes = 256 << 20
)

// Chart identifies one version of a published Helm chart.
type Chart struct {
	// Repository is an http(s):// Helm repository or an oci:// path.
	Repository string
	Name       string
	// Version is exact.
	Version string
	// Digest, sha256:<hex> of the archive, is verified when set.
	Digest string
}

// FetchChart downloads a chart and returns the directory it is unpacked
// into and the archive digest. Charts are cached by digest, so a pinned
// chart that is already cached needs no network at all.
func (f *Fetcher) FetchChart(ctx context.Context, c Chart) (dir, digest string, err error) {
	if _, err := semver.StrictNewVersion(strings.TrimPrefix(c.Version, "v")); err != nil {
		return "", "", fmt.Errorf("chart %s: version %q is not an exact version", c.Name, c.Version)
	}
	if c.Digest != "" {
		if dir := f.chartDir(c.Digest, c.Name); complete(dir) {
			return dir, c.Digest, nil
		}
	}

	archive, digest, err := f.ChartArchive(ctx, c)
	if err != nil {
		return "", "", err
	}

	dir = f.chartDir(digest, c.Name)
	f.mu.Lock()
	defer f.mu.Unlock()
	if complete(dir) {
		return dir, digest, nil
	}
	root := filepath.Dir(dir)
	if err := os.MkdirAll(filepath.Dir(root), 0o755); err != nil {
		return "", "", err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(root), ".unpack-")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(tmp)
	if err := untar(bytes.NewReader(archive), tmp); err != nil {
		return "", "", fmt.Errorf("unpack chart %s: %w", c.Name, err)
	}
	if _, err := os.Stat(filepath.Join(tmp, c.Name, "Chart.yaml")); err != nil {
		return "", "", fmt.Errorf("archive of chart %s has no %s/Chart.yaml", c.Name, c.Name)
	}
	if err := os.WriteFile(filepath.Join(tmp, c.Name, ".complete"), nil, 0o644); err != nil {
		return "", "", err
	}
	if err := os.Rename(tmp, root); err != nil && !errors.Is(err, os.ErrExist) {
		return "", "", err
	}
	return dir, digest, nil
}

// ChartArchive downloads a chart archive, from the mirror when the
// fetcher has one, and checks it against the pinned digest.
func (f *Fetcher) ChartArchive(ctx context.Context, c Chart) (archive []byte, digest string, err error) {
	c = MirrorChart(f.Mirror, c)
	switch {
	case strings.HasPrefix(c.Repository, "oci://"):
		archive, err = f.chartFromRegistry(ctx, c)
	case strings.HasPrefix(c.Repository, "http://"), strings.HasPrefix(c.Repository, "https://"):
		archive, err = chartFromIndex(ctx, c)
	default:
		err = fmt.Errorf("repository %q is neither http(s):// nor oci://", c.Repository)
	}
	if err != nil {
		return nil, "", fmt.Errorf("chart %s %s: %w", c.Name, c.Version, err)
	}
	sum := sha256.Sum256(archive)
	digest = "sha256:" + hex.EncodeToString(sum[:])
	if c.Digest != "" && c.Digest != digest {
		return nil, "", fmt.Errorf("chart %s %s from %s has digest %s, the package pins %s", c.Name, c.Version, c.Repository, digest, c.Digest)
	}
	return archive, digest, nil
}

func (f *Fetcher) chartDir(digest, name string) string {
	return filepath.Join(f.CacheDir, "charts", strings.ReplaceAll(digest, ":", "-"), name)
}

func complete(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".complete"))
	return err == nil
}

// chartFromRegistry pulls the chart layer of <repository>/<name>:<version>.
func (f *Fetcher) chartFromRegistry(ctx context.Context, c Chart) ([]byte, error) {
	target := strings.TrimSuffix(strings.TrimPrefix(c.Repository, "oci://"), "/") + "/" + c.Name + ":" + strings.ReplaceAll(c.Version, "+", "_")
	repo, err := f.repository(target)
	if err != nil {
		return nil, err
	}
	desc, err := repo.Resolve(ctx, repo.Reference.Reference)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", target, err)
	}
	raw, err := content.FetchAll(ctx, repo, desc)
	if err != nil {
		return nil, fmt.Errorf("fetch manifest of %s: %w", target, err)
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("decode manifest of %s: %w", target, err)
	}
	for _, l := range manifest.Layers {
		if l.MediaType != ChartMediaType {
			continue
		}
		if l.Size > maxChartBytes {
			return nil, fmt.Errorf("chart layer of %s is larger than %d bytes", target, maxChartBytes)
		}
		return content.FetchAll(ctx, repo, l)
	}
	return nil, fmt.Errorf("%s is not a Helm chart", target)
}

// repoIndex is the part of a Helm repository index.yaml kubepkg reads.
type repoIndex struct {
	Entries map[string][]struct {
		Version string   `json:"version"`
		URLs    []string `json:"urls"`
		Digest  string   `json:"digest"`
	} `json:"entries"`
}

// chartFromIndex finds the chart in the repository index and downloads it.
func chartFromIndex(ctx context.Context, c Chart) ([]byte, error) {
	base, err := url.Parse(strings.TrimSuffix(c.Repository, "/") + "/")
	if err != nil {
		return nil, err
	}
	raw, err := Download(ctx, base.ResolveReference(&url.URL{Path: "index.yaml"}).String(), maxIndexBytes)
	if err != nil {
		return nil, err
	}
	var idx repoIndex
	if err := yaml.Unmarshal(raw, &idx); err != nil {
		return nil, fmt.Errorf("decode index of %s: %w", c.Repository, err)
	}
	for _, e := range idx.Entries[c.Name] {
		if e.Version != c.Version {
			continue
		}
		if len(e.URLs) == 0 {
			return nil, fmt.Errorf("index of %s lists no download URL", c.Repository)
		}
		u, err := base.Parse(e.URLs[0])
		if err != nil {
			return nil, err
		}
		archive, err := Download(ctx, u.String(), maxChartBytes)
		if err != nil {
			return nil, err
		}
		if e.Digest != "" {
			sum := sha256.Sum256(archive)
			if hex.EncodeToString(sum[:]) != strings.TrimPrefix(e.Digest, "sha256:") {
				return nil, fmt.Errorf("archive at %s does not match the digest in the repository index", u)
			}
		}
		return archive, nil
	}
	return nil, fmt.Errorf("repository %s has no version %s", c.Repository, c.Version)
}

// ErrNotFound is returned by Download for a 404.
var ErrNotFound = errors.New("not found")

// Download GETs u and fails on a non-200 status or beyond limit bytes.
func Download(ctx context.Context, u string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("GET %s: %w", u, ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("GET %s: more than %d bytes", u, limit)
	}
	return raw, nil
}

// maxFileBytes bounds a downloaded source file or archive.
const maxFileBytes = 256 << 20

// FetchFile downloads url and checks it against sha256 (hex, with or
// without the sha256: prefix). Build sources are always pinned.
func FetchFile(ctx context.Context, u, sum string) ([]byte, error) {
	want := strings.TrimPrefix(sum, "sha256:")
	if len(want) != 64 {
		return nil, fmt.Errorf("%s: sha256 %q is not a sha256 hex digest", u, sum)
	}
	raw, err := Download(ctx, u, maxFileBytes)
	if err != nil {
		return nil, err
	}
	got := sha256.Sum256(raw)
	if hex.EncodeToString(got[:]) != want {
		return nil, fmt.Errorf("%s has sha256 %s, the recipe pins %s", u, hex.EncodeToString(got[:]), want)
	}
	return raw, nil
}

// Untar unpacks a gzipped tarball into dst with the same limits as package
// trees: no links, no escapes, bounded size.
func Untar(r io.Reader, dst string) error { return untar(r, dst) }

// CopyTree copies the regular files under src to dst.
func CopyTree(src, dst string) error { return copyTree(src, dst) }
