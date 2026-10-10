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

package repo

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kuberoot-dev/kubepkg/api/v1"
)

func entry(t *testing.T, version string, build int32) Version {
	t.Helper()
	spec := v1.PackageSourceSpec{Version: version, Build: build}
	d, err := SpecDigest(spec)
	if err != nil {
		t.Fatal(err)
	}
	return Version{Version: version, Build: build, Digest: d, Spec: spec}
}

func index(versions map[string][]Version) *Index {
	idx := &Index{APIVersion: "kubepkg.dev/v1alpha1", Kind: IndexKind, Packages: map[string]Package{}}
	for name, v := range versions {
		idx.Packages[name] = Package{Versions: v}
	}
	return idx
}

type refuse struct{ version string }

func (refuse) AdmitIndex(context.Context, string, []byte, *Index) error { return nil }
func (r refuse) AdmitVersion(_ context.Context, _, _ string, v Version) error {
	if v.Version == r.version {
		return errors.New("not signed")
	}
	return nil
}

func TestSelect(t *testing.T) {
	s := NewStore()
	s.Set("community", 0, index(map[string][]Version{"kubevirt": {entry(t, "1.9.0", 1), entry(t, "1.10.0", 1)}}))
	s.Set("vendor", 10, index(map[string][]Version{"kubevirt": {entry(t, "1.9.0", 1), entry(t, "1.9.0", 3), entry(t, "1.9.0", 2)}}))
	ctx := context.Background()

	cases := []struct {
		name, constraint, only string
		policy                 Policy
		repo, version          string
		build                  int32
	}{
		{"priority wins over newer versions elsewhere", "", "", AllowAll{}, "vendor", "1.9.0", 3},
		{"the priority repo shadows others even without a match", ">=1.10", "", AllowAll{}, "", "", 0},
		{"only one repository", "", "community", AllowAll{}, "community", "1.10.0", 1},
		{"refused versions are skipped", "", "", refuse{"1.9.0"}, "", "", 0},
		{"refused versions fall back to older ones", "", "community", refuse{"1.10.0"}, "community", "1.9.0", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sel, err := s.Select(ctx, "kubevirt", c.constraint, c.only, c.policy)
			if c.version == "" {
				if !errors.Is(err, ErrNoVersion) {
					t.Fatalf("want no version, got %+v %v", sel, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if sel.Repository != c.repo || sel.Version.Version != c.version || sel.Version.Build != c.build {
				t.Fatalf("got %s %s build %d", sel.Repository, sel.Version.Version, sel.Version.Build)
			}
		})
	}
	if _, err := s.Select(ctx, "nope", "", "", AllowAll{}); !errors.Is(err, ErrNoVersion) {
		t.Fatalf("unknown package: %v", err)
	}
	if _, err := s.Select(ctx, "kubevirt", "", "missing", AllowAll{}); !errors.Is(err, ErrNoVersion) {
		t.Fatalf("unknown repository: %v", err)
	}
}

func TestParseRejectsTamperedIndex(t *testing.T) {
	idx := index(map[string][]Version{"app": {entry(t, "1.0.0", 1)}})
	var buf bytes.Buffer
	if err := idx.Write(&buf); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(buf.Bytes()); err != nil {
		t.Fatalf("valid index rejected: %v", err)
	}
	evil := entry(t, "1.0.0", 1)
	evil.Spec.Provides = []string{"evil"}
	buf.Reset()
	if err := index(map[string][]Version{"app": {evil}}).Write(&buf); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(buf.Bytes()); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("an index whose spec does not match its digest: %v", err)
	}
	bad := strings.ReplaceAll(buf.String(), "1.0.0", "latest")
	if bad == buf.String() {
		t.Fatal("fixture did not change")
	}
	if _, err := Parse([]byte(bad)); err == nil {
		t.Fatal("a non-semver version was accepted")
	}
}

func TestCatalogFollowsShadowing(t *testing.T) {
	s := NewStore()
	s.Set("community", 0, index(map[string][]Version{"kubevirt": {entry(t, "1.10.0", 1)}, "cdi": {entry(t, "1.60.0", 1)}}))
	s.Set("vendor", 10, index(map[string][]Version{"kubevirt": {entry(t, "1.9.0", 1), entry(t, "1.9.0", 2)}}))
	cat := s.Catalog(context.Background(), "default", AllowAll{})
	if len(cat["kubevirt"]) != 1 || cat["kubevirt"][0].Version != "1.9.0" {
		t.Errorf("kubevirt must come from the vendor repository only, once per version: %+v", cat["kubevirt"])
	}
	if len(cat["cdi"]) != 1 {
		t.Errorf("cdi from community: %+v", cat["cdi"])
	}
	if cat := s.Catalog(context.Background(), "default", refuse{"1.9.0"}); len(cat["kubevirt"]) != 0 {
		t.Errorf("refused versions listed: %+v", cat["kubevirt"])
	}
}

func TestVersionsFromANewerKubepkgAreLeftOutNotFatal(t *testing.T) {
	good := v1.PackageSourceSpec{Version: "1.0.0"}
	d, err := SpecDigest(good)
	if err != nil {
		t.Fatal(err)
	}
	index := func(second string) []byte {
		return []byte(`apiVersion: kubepkg.dev/v1alpha1
kind: RepositoryIndex
packages:
  app:
    versions:
      - version: 1.1.0
        digest: sha256:` + strings.Repeat("1", 64) + `
        spec: {version: 1.1.0, ` + second + `}
      - version: 1.0.0
        digest: ` + d + `
        spec: {version: 1.0.0}
`)
	}
	idx, err := Parse(index("someFieldFromTheFuture: {enabled: true}"))
	if err != nil {
		t.Fatalf("a version built for a newer kubepkg broke the index: %v", err)
	}
	if v := idx.Packages["app"].Versions; len(v) != 1 || v[0].Version != "1.0.0" || len(idx.Unknown) != 1 {
		t.Fatalf("versions %+v, unknown %v", v, idx.Unknown)
	}
	if _, err := Parse(index("provides: [forged]")); err == nil || !strings.Contains(err.Error(), "does not match its digest") {
		t.Fatalf("a forged spec of known fields: %v", err)
	}
	if _, err := Build(context.Background(), t.TempDir(), nil, BuildOptions{Base: idx}); err == nil || !strings.Contains(err.Error(), "newer kubepkg") {
		t.Fatalf("merging would drop versions from the published index: %v", err)
	}
}
