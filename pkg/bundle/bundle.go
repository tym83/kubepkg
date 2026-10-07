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

// Package bundle carries packages into clusters that cannot reach the
// places they were published: a bundle holds the signed repository files,
// the charts, package trees and images of the chosen packages, and is
// checked against the repository's signatures again before anything from
// it is copied into a mirror registry.
package bundle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"
	"sigs.k8s.io/yaml"

	"github.com/tym83/kubepkg/api/v1beta1"
	"github.com/tym83/kubepkg/pkg/build"
	"github.com/tym83/kubepkg/pkg/images"
	"github.com/tym83/kubepkg/pkg/repo"
	"github.com/tym83/kubepkg/pkg/sbom"
	"github.com/tym83/kubepkg/pkg/source"
	"github.com/tym83/kubepkg/pkg/version"
)

// Layout of a bundle directory.
const (
	ManifestFile = "bundle.yaml"
	// SBOMFile is a CycloneDX description of the bundle\'s packages.
	SBOMFile        = "sbom.cdx.json"
	repositoriesDir = "repositories"
	chartsDir       = "charts"
	layoutDir       = "oci"
)

// Manifest describes a bundle. It is a table of contents, not a source of
// trust: everything in it is checked against the signed index on import.
type Manifest struct {
	APIVersion   string       `json:"apiVersion"`
	Kind         string       `json:"kind"`
	Created      metav1.Time  `json:"created"`
	Repositories []Repository `json:"repositories"`
	Packages     []Package    `json:"packages"`
	Charts       []Chart      `json:"charts,omitempty"`
	Trees        []Tree       `json:"trees,omitempty"`
	Images       []Image      `json:"images,omitempty"`
}

// Repository is a repository's published files, as fetched.
type Repository struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Priority int32  `json:"priority,omitempty"`
	// Files are paths next to the index, e.g. index.yaml, index.yaml.sig,
	// root.yaml, root/1.yaml.
	Files []string `json:"files"`
}

// Package is one version of a package in the bundle.
type Package struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	Build      int32  `json:"build,omitempty"`
	Repository string `json:"repository"`
	// Digest is the version's spec digest in the index.
	Digest string `json:"digest"`
}

// Chart is a chart archive, stored as charts/<hex>.tgz.
type Chart struct {
	Repository string `json:"repository"`
	Name       string `json:"name"`
	Version    string `json:"version"`
	Digest     string `json:"digest"`
}

// Tree is a package tree, stored in the OCI layout.
type Tree struct {
	// Ref is the oci:// reference the package points at, with its digest.
	Ref string `json:"ref"`
}

// Image is a container image, stored in the OCI layout under Ref.
type Image struct {
	// Ref is registry/repository:tag@sha256:...
	Ref string `json:"ref"`
	// Pinned is true when the repository pins the image in a package's
	// images; otherwise the bundle found it in the charts and pinned it at
	// the time it was made, and only the bundle vouches for it.
	Pinned   bool     `json:"pinned"`
	Packages []string `json:"packages"`
}

// Source is a repository to take packages from, as a cluster would.
type Source struct {
	Name string
	Spec v1beta1.RepositorySpec
}

// Recorded holds the repository files fetched while loading sources.
type Recorded struct {
	sources []Source
	files   map[string]map[string][]byte // repository -> URL -> content
}

// Load loads the sources with the same checks a cluster applies, signatures
// and roots included, and records every file it fetched. Versions left out
// of an index are reported to warn.
func Load(ctx context.Context, sources []Source, fetchers repo.Fetchers, now time.Time, warn io.Writer) (*repo.Store, *Recorded, error) {
	store := repo.NewStore()
	rec := &Recorded{sources: sources, files: map[string]map[string][]byte{}}
	for _, s := range sources {
		files := map[string][]byte{}
		recording := repo.Fetchers{}
		for scheme, f := range fetchers {
			recording[scheme] = recorder{inner: f, files: files}
		}
		idx, _, _, err := repo.LoadRepository(ctx, recording, s.Spec, v1beta1.RepositoryStatus{}, now)
		if err != nil {
			return nil, nil, fmt.Errorf("repository %s: %w", s.Name, err)
		}
		for _, note := range idx.LeftOut() {
			fmt.Fprintf(warn, "warning: repository %s has %s\n", s.Name, note)
		}
		store.Set(s.Name, s.Spec.Priority, idx)
		rec.files[s.Name] = files
	}
	return store, rec, nil
}

type recorder struct {
	inner repo.IndexFetcher
	files map[string][]byte
}

func (r recorder) FetchIndex(ctx context.Context, u string) ([]byte, error) {
	raw, err := r.inner.FetchIndex(ctx, u)
	if err == nil {
		r.files[u] = raw
	}
	return raw, err
}

// Selection is a package version to put in a bundle.
type Selection struct {
	Repository string
	Name       string
	Version    repo.Version
}

// CreateOptions say how to fetch what goes into a bundle.
type CreateOptions struct {
	// Charts downloads chart archives.
	Charts *source.Fetcher
	// Registry opens image and tree repositories.
	Registry images.Resolver
	// Warn receives notes about images no repository pins.
	Warn io.Writer
	Now  time.Time
}

// Create writes a bundle of the selected package versions into dir.
func Create(ctx context.Context, dir string, rec *Recorded, sel []Selection, o CreateOptions) (*Manifest, error) {
	if o.Warn == nil {
		o.Warn = io.Discard
	}
	m := &Manifest{APIVersion: v1beta1.GroupVersion.String(), Kind: "Bundle", Created: metav1.NewTime(o.Now.UTC().Truncate(time.Second))}
	used := map[string]bool{}
	for _, s := range sel {
		used[s.Repository] = true
	}
	for _, s := range rec.sources {
		if !used[s.Name] {
			continue
		}
		r, err := writeRepository(dir, s, rec.files[s.Name])
		if err != nil {
			return nil, err
		}
		m.Repositories = append(m.Repositories, r)
	}
	layout, err := oci.NewWithContext(ctx, filepath.Join(dir, layoutDir))
	if err != nil {
		return nil, err
	}
	charts := map[string]Chart{}
	trees := map[string]bool{}
	imgs := map[string]*Image{}
	for _, s := range sel {
		spec := s.Version.Spec
		m.Packages = append(m.Packages, Package{Name: s.Name, Version: s.Version.Version, Build: s.Version.Build, Repository: s.Repository, Digest: s.Version.Digest})
		var discovered []string
		for _, v := range spec.Variants {
			for _, c := range v.Components {
				switch {
				case c.Chart != nil:
					ch := Chart{Repository: c.Chart.Repository, Name: c.Chart.Name, Version: c.Chart.Version, Digest: c.Chart.Digest}
					if _, done := charts[ch.Digest]; !done {
						if err := writeChart(ctx, dir, ch, o.Charts); err != nil {
							return nil, err
						}
						charts[ch.Digest] = ch
					}
					if len(spec.Images) == 0 {
						found, err := chartImages(ctx, o.Charts, ch)
						if err != nil {
							return nil, fmt.Errorf("%s %s: %w", s.Name, s.Version.Version, err)
						}
						discovered = append(discovered, found...)
					}
				case spec.SourceRef != nil && !trees[spec.SourceRef.URL]:
					if err := copyTree(ctx, layout, spec.SourceRef.URL, o.Registry); err != nil {
						return nil, fmt.Errorf("%s %s: %w", s.Name, s.Version.Version, err)
					}
					trees[spec.SourceRef.URL] = true
				}
			}
		}
		refs, pinned := spec.Images, true
		if len(spec.Images) == 0 && len(discovered) > 0 {
			pinned = false
			fmt.Fprintf(o.Warn, "warning: %s %s does not pin its images; pinning the %d its charts run as they are now\n", s.Name, s.Version.Version, len(dedupe(discovered)))
			for _, ref := range dedupe(discovered) {
				p, err := o.Registry.Pin(ctx, ref)
				if err != nil {
					return nil, err
				}
				refs = append(refs, p)
			}
		}
		for _, ref := range refs {
			if img, ok := imgs[ref]; ok {
				img.Packages = append(img.Packages, s.Name)
				img.Pinned = img.Pinned || pinned
				continue
			}
			if err := copyImage(ctx, layout, ref, o.Registry); err != nil {
				return nil, err
			}
			imgs[ref] = &Image{Ref: ref, Pinned: pinned, Packages: []string{s.Name}}
		}
	}
	for _, d := range sortedKeys(charts) {
		m.Charts = append(m.Charts, charts[d])
	}
	for _, t := range sortedKeys(trees) {
		m.Trees = append(m.Trees, Tree{Ref: t})
	}
	for _, r := range sortedKeys(imgs) {
		m.Images = append(m.Images, *imgs[r])
	}
	var described []sbom.Package
	for _, sl := range sel {
		described = append(described, sbom.Package{Name: sl.Name, Version: sl.Version.Version, Build: sl.Version.Build, Repository: sl.Repository, Digest: sl.Version.Digest, Spec: sl.Version.Spec})
	}
	doc, err := sbom.CycloneDX(described, sbom.Options{ToolVersion: version.Version, Timestamp: m.Created.Time, Name: "bundle"})
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, SBOMFile), doc, 0o644); err != nil {
		return nil, err
	}
	raw, err := yaml.Marshal(m)
	if err != nil {
		return nil, err
	}
	return m, os.WriteFile(filepath.Join(dir, ManifestFile), raw, 0o644)
}

func writeRepository(dir string, s Source, files map[string][]byte) (Repository, error) {
	base := s.Spec.URL[:strings.LastIndex(s.Spec.URL, "/")+1]
	r := Repository{Name: s.Name, URL: s.Spec.URL, Priority: s.Spec.Priority}
	for _, u := range sortedKeys(files) {
		rel, ok := strings.CutPrefix(u, base)
		if !ok {
			return r, fmt.Errorf("repository %s fetched %s, outside %s", s.Name, u, base)
		}
		p, err := safePath(filepath.Join(dir, repositoriesDir, s.Name), rel)
		if err != nil {
			return r, err
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return r, err
		}
		if err := os.WriteFile(p, files[u], 0o644); err != nil {
			return r, err
		}
		r.Files = append(r.Files, rel)
	}
	return r, nil
}

func chartFile(dir, digest string) string {
	return filepath.Join(dir, chartsDir, strings.TrimPrefix(digest, "sha256:")+".tgz")
}

func writeChart(ctx context.Context, dir string, c Chart, f *source.Fetcher) error {
	if c.Digest == "" {
		return fmt.Errorf("chart %s %s is not pinned by digest", c.Name, c.Version)
	}
	archive, _, err := f.ChartArchive(ctx, source.Chart{Repository: c.Repository, Name: c.Name, Version: c.Version, Digest: c.Digest})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, chartsDir), 0o755); err != nil {
		return err
	}
	return os.WriteFile(chartFile(dir, c.Digest), archive, 0o644)
}

// chartImages renders a chart with its default values, which a built
// package's defaults are part of, and lists the images it runs.
func chartImages(ctx context.Context, f *source.Fetcher, ch Chart) ([]string, error) {
	dir, _, err := f.FetchChart(ctx, source.Chart{Repository: ch.Repository, Name: ch.Name, Version: ch.Version, Digest: ch.Digest})
	if err != nil {
		return nil, err
	}
	docs, err := build.RenderChart(dir, nil)
	if err != nil {
		return nil, fmt.Errorf("render %s: %w", ch.Name, err)
	}
	return images.FromManifests(docs...)
}

func copyImage(ctx context.Context, layout *oci.Store, ref string, r images.Resolver) error {
	repoName, _, digest := images.Split(ref)
	if digest == "" {
		return fmt.Errorf("image %s is not pinned by digest", ref)
	}
	src, err := r.Repository(repoName)
	if err != nil {
		return err
	}
	desc, err := src.Resolve(ctx, digest)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", ref, err)
	}
	if err := oras.CopyGraph(ctx, src, layout, desc, oras.DefaultCopyGraphOptions); err != nil {
		return fmt.Errorf("copy %s: %w", ref, err)
	}
	return layout.Tag(ctx, desc, ref)
}

func copyTree(ctx context.Context, layout *oci.Store, ref string, r images.Resolver) error {
	target := strings.TrimPrefix(ref, "oci://")
	repoName, _, digest := images.Split(target)
	if digest == "" {
		return fmt.Errorf("package tree %s is not pinned by digest", ref)
	}
	src, err := r.Repository(repoName)
	if err != nil {
		return err
	}
	desc, err := src.Resolve(ctx, digest)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", ref, err)
	}
	if err := oras.CopyGraph(ctx, src, layout, desc, oras.DefaultCopyGraphOptions); err != nil {
		return fmt.Errorf("copy %s: %w", ref, err)
	}
	return layout.Tag(ctx, desc, ref)
}

// safePath joins rel under root, refusing paths that leave it.
func safePath(root, rel string) (string, error) {
	clean := path.Clean("/" + filepath.ToSlash(rel))
	if rel == "" || strings.Contains(rel, "\\") || clean == "/" || path.Clean(rel) != strings.TrimPrefix(clean, "/") {
		return "", fmt.Errorf("unsafe path %q in bundle", rel)
	}
	return filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(clean, "/"))), nil
}

// Read loads a bundle's manifest from dir.
func Read(dir string) (*Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := yaml.UnmarshalStrict(raw, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", ManifestFile, err)
	}
	if m.Kind != "Bundle" {
		return nil, fmt.Errorf("%s is not a kubepkg bundle", dir)
	}
	return &m, nil
}

// fileFetchers answer a repository's URLs from the files in a bundle.
type fileFetchers struct {
	dir  string
	repo Repository
}

func (f fileFetchers) FetchIndex(_ context.Context, u string) ([]byte, error) {
	base := f.repo.URL[:strings.LastIndex(f.repo.URL, "/")+1]
	rel, ok := strings.CutPrefix(u, base)
	if !ok {
		return nil, fmt.Errorf("%s is not in the bundle", u)
	}
	p, err := safePath(filepath.Join(f.dir, repositoriesDir, f.repo.Name), rel)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s is not in the bundle", u)
	}
	return raw, err
}

// Verified is a bundle whose contents match its signed repositories.
type Verified struct {
	Dir      string
	Manifest *Manifest
	// Specs are the signed specs of the bundle's packages, by name.
	Specs map[string]v1beta1.PackageSourceSpec
}

// Verify checks a bundle against its repositories, trusted as trust says
// (public keys or a root of trust, never what the bundle itself carries),
// the way a cluster would: signatures, roots and expiry. Every chart, tree
// and image must be what the signed packages pin. Images found in charts
// rather than pinned by a repository are refused unless allowUnpinned.
func Verify(ctx context.Context, dir string, trust v1beta1.RepositorySpec, now time.Time, allowUnpinned bool) (*Verified, error) {
	m, err := Read(dir)
	if err != nil {
		return nil, err
	}
	if len(trust.PublicKeys) == 0 && trust.Trust == nil {
		return nil, errors.New("give the keys to trust the bundle's repositories with: public keys or root keys")
	}
	indexes := map[string]*repo.Index{}
	for _, r := range m.Repositories {
		spec := v1beta1.RepositorySpec{URL: r.URL, Priority: r.Priority, PublicKeys: trust.PublicKeys, Trust: trust.Trust}
		u, err := url.Parse(r.URL)
		if err != nil {
			return nil, err
		}
		idx, _, _, err := repo.LoadRepository(ctx, repo.Fetchers{u.Scheme: fileFetchers{dir: dir, repo: r}}, spec, v1beta1.RepositoryStatus{}, now)
		if err != nil {
			return nil, fmt.Errorf("repository %s: %w", r.Name, err)
		}
		indexes[r.Name] = idx
	}
	v := &Verified{Dir: dir, Manifest: m, Specs: map[string]v1beta1.PackageSourceSpec{}}
	wantCharts := map[string]bool{}
	wantTrees := map[string]bool{}
	pinnedBy := map[string][]string{} // image -> packages that pin it
	for _, p := range m.Packages {
		idx := indexes[p.Repository]
		if idx == nil {
			return nil, fmt.Errorf("package %s comes from repository %s, which the bundle does not carry", p.Name, p.Repository)
		}
		var spec *v1beta1.PackageSourceSpec
		for _, ver := range idx.Packages[p.Name].Versions {
			if ver.Digest == p.Digest {
				spec = ver.Spec.DeepCopy()
			}
		}
		if spec == nil {
			return nil, fmt.Errorf("%s %s is not in the signed index of %s", p.Name, p.Version, p.Repository)
		}
		v.Specs[p.Name] = *spec
		for _, variant := range spec.Variants {
			for _, c := range variant.Components {
				if c.Chart != nil {
					wantCharts[c.Chart.Digest] = true
				} else if spec.SourceRef != nil {
					wantTrees[spec.SourceRef.URL] = true
				}
			}
		}
		for _, img := range spec.Images {
			pinnedBy[img] = append(pinnedBy[img], p.Name)
		}
	}
	for _, c := range m.Charts {
		if !wantCharts[c.Digest] {
			return nil, fmt.Errorf("chart %s %s is in the bundle but no signed package uses it", c.Name, c.Version)
		}
		delete(wantCharts, c.Digest)
		archive, err := os.ReadFile(chartFile(dir, c.Digest))
		if err != nil {
			return nil, err
		}
		if got := digestOf(archive); got != c.Digest {
			return nil, fmt.Errorf("chart %s %s in the bundle has digest %s, the package pins %s", c.Name, c.Version, got, c.Digest)
		}
	}
	for d := range wantCharts {
		return nil, fmt.Errorf("the bundle lacks the chart with digest %s", d)
	}
	layout, err := oci.NewWithContext(ctx, filepath.Join(dir, layoutDir))
	if err != nil {
		return nil, err
	}
	for _, t := range m.Trees {
		if !wantTrees[t.Ref] {
			return nil, fmt.Errorf("package tree %s is in the bundle but no signed package uses it", t.Ref)
		}
		delete(wantTrees, t.Ref)
		if err := checkInLayout(ctx, layout, t.Ref); err != nil {
			return nil, err
		}
	}
	for t := range wantTrees {
		return nil, fmt.Errorf("the bundle lacks the package tree %s", t)
	}
	for _, img := range m.Images {
		if _, ok := pinnedBy[img.Ref]; ok {
			delete(pinnedBy, img.Ref)
		} else if !allowUnpinned {
			return nil, fmt.Errorf("image %s of %s is pinned only by the bundle, not by a signed package; pin it in the recipe (kubepkg images), or accept the bundle's word with --allow-unpinned-images", img.Ref, strings.Join(img.Packages, ", "))
		}
		if err := checkInLayout(ctx, layout, img.Ref); err != nil {
			return nil, err
		}
	}
	for img := range pinnedBy {
		return nil, fmt.Errorf("the bundle lacks image %s", img)
	}
	return v, nil
}

func checkInLayout(ctx context.Context, layout *oci.Store, ref string) error {
	_, _, want := images.Split(strings.TrimPrefix(ref, "oci://"))
	desc, err := layout.Resolve(ctx, ref)
	if err != nil {
		return fmt.Errorf("the bundle lacks %s", ref)
	}
	if desc.Digest.String() != want {
		return fmt.Errorf("%s in the bundle has digest %s", ref, desc.Digest)
	}
	return nil
}

func digestOf(b []byte) string {
	return "sha256:" + sha256Hex(b)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
