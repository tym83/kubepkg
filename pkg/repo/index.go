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

// Package repo builds and reads package repository indexes.
//
// A repository is one index file listing every version of every package it
// offers. Each version is a complete PackageSource spec, so whatever reads
// the index can install an entry without translating it. The index is
// served over HTTP like a Helm repository index, or pushed to an OCI
// registry.
package repo

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/tym83/kubepkg/api/v1beta1"
	"github.com/tym83/kubepkg/pkg/source"
)

// IndexKind is the kind of an index document.
const IndexKind = "RepositoryIndex"

// IndexAPIVersion is the version of the index file format. It is not the
// version of the cluster API and stays as indexes were first published.
const IndexAPIVersion = "kubepkg.dev/v1alpha1"

// Annotations on a PackageSource that describe the package in the index.
const (
	AnnotationDescription = "kubepkg.dev/description"
	AnnotationHome        = "kubepkg.dev/home"
)

// Index is a repository index.
type Index struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	// Generated is when the index was built. Clusters refuse an index
	// older than one they accepted, so an old signed index cannot be
	// replayed to roll them back to versions since withdrawn.
	Generated *metav1.Time `json:"generated,omitempty"`
	// Expires bounds how long clients accept the index: a mirror that
	// keeps serving an old one is refused once it expires. Repositories
	// with a root must set it.
	Expires  *metav1.Time       `json:"expires,omitempty"`
	Packages map[string]Package `json:"packages"`
}

// Package is every published version of one package.
type Package struct {
	Description string `json:"description,omitempty"`
	Home        string `json:"home,omitempty"`
	// Versions are sorted newest first, by version and then build.
	Versions []Version `json:"versions"`
}

// Version is one published build of a version.
type Version struct {
	Version string `json:"version"`
	Build   int32  `json:"build,omitempty"`
	// Digest is the sha256 of the spec in canonical JSON. It identifies
	// exactly what this version installs, charts included, because every
	// chart in the spec is pinned by digest.
	Digest string                    `json:"digest"`
	Spec   v1beta1.PackageSourceSpec `json:"spec"`
}

// ChartFetcher resolves chart digests; source.Fetcher implements it.
type ChartFetcher interface {
	FetchChart(ctx context.Context, c source.Chart) (dir, digest string, err error)
}

// BuildOptions control Build.
type BuildOptions struct {
	// Verify downloads charts that are already pinned and checks their
	// digests too. Charts without a digest are always downloaded and
	// pinned.
	Verify bool
	// Base is the index already published. Its versions are kept, and a
	// version rebuilt with the same number must not change: published
	// versions are immutable, a changed recipe needs a new build.
	Base *Index
}

// Build reads every PackageSource in the YAML files under dir and returns
// the index. Charts without a digest are downloaded and pinned in the
// index, so a published version always installs the same bytes; the
// source files are left as they are.
func Build(ctx context.Context, dir string, charts ChartFetcher, opts BuildOptions) (*Index, error) {
	srcs, err := readSources(dir)
	if err != nil {
		return nil, err
	}
	now := metav1.NewTime(time.Now().UTC().Truncate(time.Second))
	idx := &Index{APIVersion: IndexAPIVersion, Kind: IndexKind, Generated: &now, Packages: map[string]Package{}}
	published := map[string]string{}
	if opts.Base != nil {
		for name, p := range opts.Base.Packages {
			p.Versions = append([]Version(nil), p.Versions...)
			idx.Packages[name] = p
			for _, v := range p.Versions {
				published[fmt.Sprintf("%s@%s build %d", name, v.Version, v.Build)] = v.Digest
			}
		}
	}
	seen := map[string]string{}
	for _, s := range srcs {
		name, ver := s.src.Name, s.src.Spec.Version
		if name == "" {
			return nil, fmt.Errorf("%s: PackageSource has no name", s.file)
		}
		if _, err := semver.StrictNewVersion(strings.TrimPrefix(ver, "v")); err != nil {
			return nil, fmt.Errorf("%s: %s needs an exact semver version, got %q", s.file, name, ver)
		}
		key := fmt.Sprintf("%s@%s build %d", name, ver, s.src.Spec.Build)
		if prev, dup := seen[key]; dup {
			return nil, fmt.Errorf("%s: %s is also defined in %s", s.file, key, prev)
		}
		seen[key] = s.file
		if err := checkSource(&s.src.Spec); err != nil {
			return nil, fmt.Errorf("%s: %s: %w", s.file, key, err)
		}
		if err := pinCharts(ctx, &s.src.Spec, charts, opts.Verify); err != nil {
			return nil, fmt.Errorf("%s: %s: %w", s.file, key, err)
		}
		d, err := SpecDigest(s.src.Spec)
		if err != nil {
			return nil, err
		}
		p := idx.Packages[name]
		if old, ok := published[key]; ok {
			if old != d {
				return nil, fmt.Errorf("%s: %s is already published with different content; published versions do not change, give the recipe a new build number", s.file, key)
			}
		} else {
			p.Versions = append(p.Versions, Version{Version: ver, Build: s.src.Spec.Build, Digest: d, Spec: s.src.Spec})
		}
		if v := s.src.Annotations[AnnotationDescription]; v != "" {
			p.Description = v
		}
		if v := s.src.Annotations[AnnotationHome]; v != "" {
			p.Home = v
		}
		idx.Packages[name] = p
	}
	for name, p := range idx.Packages {
		sort.Slice(p.Versions, func(i, j int) bool { return Newer(p.Versions[i], p.Versions[j]) })
		idx.Packages[name] = p
	}
	return idx, nil
}

// Newer orders versions, then builds, highest first.
func Newer(a, b Version) bool {
	va, vb := semver.MustParse(a.Version), semver.MustParse(b.Version)
	if !va.Equal(vb) {
		return va.GreaterThan(vb)
	}
	return a.Build > b.Build
}

// checkSource refuses what cannot be installed from a repository: a
// package tree must be pinned by digest, or the same version could
// install different charts tomorrow.
func checkSource(spec *v1beta1.PackageSourceSpec) error {
	usesTree := false
	for _, v := range spec.Variants {
		for _, c := range v.Components {
			if (c.Path == "") == (c.Chart == nil) {
				return fmt.Errorf("component %s: set exactly one of path and chart", c.Name)
			}
			if c.Path != "" {
				usesTree = true
			}
		}
	}
	if !usesTree {
		return nil
	}
	ref := spec.SourceRef
	if ref == nil || ref.Kind != v1beta1.SourceKindOCIArtifact {
		return errors.New("components with path need an OCIArtifact sourceRef")
	}
	if !strings.Contains(ref.URL, "@sha256:") {
		return fmt.Errorf("sourceRef %s must be pinned by digest (oci://...@sha256:...)", ref.URL)
	}
	return nil
}

func pinCharts(ctx context.Context, spec *v1beta1.PackageSourceSpec, charts ChartFetcher, verify bool) error {
	for vi := range spec.Variants {
		for ci := range spec.Variants[vi].Components {
			ch := spec.Variants[vi].Components[ci].Chart
			if ch == nil || (ch.Digest != "" && !verify) {
				continue
			}
			if charts == nil {
				return fmt.Errorf("chart %s %s has no digest", ch.Name, ch.Version)
			}
			_, d, err := charts.FetchChart(ctx, source.Chart{Repository: ch.Repository, Name: ch.Name, Version: ch.Version, Digest: ch.Digest})
			if err != nil {
				return err
			}
			ch.Digest = d
		}
	}
	return nil
}

// SpecDigest is the sha256 of a spec in canonical JSON.
func SpecDigest(spec v1beta1.PackageSourceSpec) (string, error) {
	raw, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

type sourceFile struct {
	file string
	src  v1beta1.PackageSource
}

// readSources collects PackageSource documents from *.yaml and *.yml
// files under dir, in file order. Other kinds are skipped, so recipes can
// sit next to examples and tests.
func readSources(dir string) ([]sourceFile, error) {
	var out []sourceFile
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if ext := filepath.Ext(p); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		dec := utilyaml.NewYAMLReader(bufioReader(raw))
		for {
			doc, err := dec.Read()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			var src v1beta1.PackageSource
			if err := yaml.UnmarshalStrict(doc, &src); err != nil {
				// Not every document is a PackageSource; only complain
				// about the ones that claim to be.
				var head struct{ Kind string }
				if yaml.Unmarshal(doc, &head) == nil && head.Kind == "PackageSource" {
					return fmt.Errorf("%s: %w", p, err)
				}
				continue
			}
			if src.Kind != "PackageSource" {
				continue
			}
			out = append(out, sourceFile{file: p, src: src})
		}
	})
	return out, err
}

// Write encodes the index as YAML with packages sorted by name.
func (idx *Index) Write(w io.Writer) error {
	raw, err := yaml.Marshal(idx)
	if err != nil {
		return err
	}
	_, err = w.Write(raw)
	return err
}

// Parse decodes an index and checks its kind.
func Parse(raw []byte) (*Index, error) {
	var idx Index
	if err := yaml.Unmarshal(raw, &idx); err != nil {
		return nil, fmt.Errorf("decode index: %w", err)
	}
	if idx.Kind != IndexKind {
		return nil, fmt.Errorf("not a repository index (kind %q)", idx.Kind)
	}
	// An index comes from the network: check what the rest of kubepkg
	// relies on instead of trusting it.
	for name, p := range idx.Packages {
		for _, v := range p.Versions {
			if _, err := semver.StrictNewVersion(strings.TrimPrefix(v.Version, "v")); err != nil {
				return nil, fmt.Errorf("package %s: version %q is not exact semver", name, v.Version)
			}
			if v.Spec.Version != v.Version || v.Spec.Build != v.Build {
				return nil, fmt.Errorf("package %s %s: entry and spec disagree on version or build", name, v.Version)
			}
			d, err := SpecDigest(v.Spec)
			if err != nil {
				return nil, err
			}
			if d != v.Digest {
				return nil, fmt.Errorf("package %s %s build %d: spec does not match its digest", name, v.Version, v.Build)
			}
		}
	}
	return &idx, nil
}

func bufioReader(b []byte) *bufio.Reader { return bufio.NewReader(bytes.NewReader(b)) }

// LoadIndex fetches the index at url, checks its signature when public
// keys are given, and parses it. Errors are ErrFetchFailed,
// ErrBadSignature or ErrInvalidIndex.
func LoadIndex(ctx context.Context, fetchers Fetchers, url string, publicKeys []string) (*Index, []byte, error) {
	raw, err := fetchers.Fetch(ctx, url)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrFetchFailed, err)
	}
	if len(publicKeys) > 0 {
		keys, err := ParsePublicKeys(publicKeys)
		if err != nil {
			return nil, nil, err
		}
		sig, err := fetchers.Fetch(ctx, url+SignatureSuffix)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: no signature at %s%s: %v", ErrBadSignature, url, SignatureSuffix, err)
		}
		if err := Verify(raw, sig, keys); err != nil {
			return nil, nil, err
		}
	}
	idx, err := Parse(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalidIndex, err)
	}
	return idx, raw, nil
}

// Errors of LoadIndex besides ErrBadSignature.
var (
	ErrFetchFailed  = errors.New("index cannot be fetched")
	ErrInvalidIndex = errors.New("index is not valid")
)
