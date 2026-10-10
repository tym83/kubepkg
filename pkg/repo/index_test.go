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
	"fmt"

	"bytes"
	"context"
	v1 "github.com/kuberoot-dev/kubepkg/api/v1"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuberoot-dev/kubepkg/pkg/source"
)

type fakeCharts struct{ fetched []string }

func (f *fakeCharts) FetchChart(_ context.Context, c source.Chart) (string, string, error) {
	f.fetched = append(f.fetched, c.Name+"@"+c.Version)
	return "", "sha256:" + strings.Repeat("a", 64), nil
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const kubevirt = `apiVersion: kubepkg.dev/v1alpha1
kind: PackageSource
metadata:
  name: kubevirt
  annotations:
    kubepkg.dev/description: Virtual machines on Kubernetes
spec:
  version: %s
  variants:
    - name: default
      requires: [{package: cdi}]
      components:
        - name: operator
          chart: {repository: "https://charts.example.org", name: kubevirt, version: %s%s}
          install: {namespace: kubevirt}
`

func recipe(version, digest string) string {
	d := ""
	if digest != "" {
		d = ", digest: \"" + digest + "\""
	}
	return strings.NewReplacer("%s%s", version+d, "%s", version).Replace(kubevirt)
}

func TestBuildPinsChartsAndSortsVersions(t *testing.T) {
	dir := t.TempDir()
	pinned := "sha256:" + strings.Repeat("b", 64)
	write(t, dir, "kubevirt/1.3.0.yaml", recipe("1.3.0", pinned))
	write(t, dir, "kubevirt/1.10.0.yaml", recipe("1.10.0", ""))
	write(t, dir, "notes/example.yaml", "apiVersion: kubepkg.dev/v1alpha1\nkind: Package\nmetadata: {name: kubevirt}\nspec: {version: \"~1.10\"}\n")

	charts := &fakeCharts{}
	idx, err := Build(context.Background(), dir, charts, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p := idx.Packages["kubevirt"]
	if p.Description != "Virtual machines on Kubernetes" {
		t.Errorf("description %q", p.Description)
	}
	if len(p.Versions) != 2 || p.Versions[0].Version != "1.10.0" || p.Versions[1].Version != "1.3.0" {
		t.Fatalf("versions not newest first by semver: %+v", p.Versions)
	}
	if got := p.Versions[0].Spec.Variants[0].Components[0].Chart.Digest; got != "sha256:"+strings.Repeat("a", 64) {
		t.Errorf("unpinned chart not pinned: %q", got)
	}
	if got := p.Versions[1].Spec.Variants[0].Components[0].Chart.Digest; got != pinned {
		t.Errorf("pinned chart changed: %q", got)
	}
	if len(charts.fetched) != 1 || charts.fetched[0] != "kubevirt@1.10.0" {
		t.Errorf("only the unpinned chart should be fetched, fetched %v", charts.fetched)
	}
	if p.Versions[0].Digest == "" || p.Versions[0].Digest == p.Versions[1].Digest {
		t.Errorf("version digests missing or equal")
	}

	var buf bytes.Buffer
	if err := idx.Write(&buf); err != nil {
		t.Fatal(err)
	}
	back, err := Parse(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := SpecDigest(back.Packages["kubevirt"].Versions[0].Spec); d != p.Versions[0].Digest {
		t.Error("spec digest changes after a write and parse round trip")
	}
}

func TestBuildVerifyRefetchesPinned(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "k.yaml", recipe("1.3.0", "sha256:"+strings.Repeat("b", 64)))
	charts := &fakeCharts{}
	if _, err := Build(context.Background(), dir, charts, BuildOptions{Verify: true}); err != nil {
		t.Fatal(err)
	}
	if len(charts.fetched) != 1 {
		t.Fatal("verify must fetch pinned charts")
	}
}

func TestBuildRejects(t *testing.T) {
	cases := map[string]string{
		"duplicate version": recipe("1.3.0", "") + "---\n" + recipe("1.3.0", ""),
		"range version":     strings.Replace(recipe("1.3.0", ""), "version: 1.3.0\n", "version: \"~1.3\"\n", 1),
		"unpinned tree": `apiVersion: kubepkg.dev/v1alpha1
kind: PackageSource
metadata: {name: tree}
spec:
  version: 1.0.0
  sourceRef: {kind: OCIArtifact, url: "oci://example.org/packages:latest"}
  variants:
    - name: default
      components: [{name: a, path: a, install: {namespace: a}}]
`,
		"typo in a PackageSource": strings.Replace(recipe("1.3.0", ""), "variants:", "variantz:", 1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, "r.yaml", body)
			if _, err := Build(context.Background(), dir, &fakeCharts{}, BuildOptions{}); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestBuildOrdersBuildsOfOneVersion(t *testing.T) {
	dir := t.TempDir()
	pinned := "sha256:" + strings.Repeat("b", 64)
	b1 := recipe("1.3.0", pinned)
	b2 := strings.Replace(b1, "version: 1.3.0\n", "version: 1.3.0\n  build: 2\n", 1)
	write(t, dir, "a.yaml", b1)
	write(t, dir, "b.yaml", b2)
	idx, err := Build(context.Background(), dir, &fakeCharts{}, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	v := idx.Packages["kubevirt"].Versions
	if len(v) != 2 || v[0].Build != 2 || v[1].Build != 0 {
		t.Fatalf("builds not newest first: %+v", v)
	}
	write(t, dir, "c.yaml", b2)
	if _, err := Build(context.Background(), dir, &fakeCharts{}, BuildOptions{}); err == nil {
		t.Fatal("the same build of a version defined twice was accepted")
	}
}

func TestBuildMergesWithPublishedIndex(t *testing.T) {
	pinned := "sha256:" + strings.Repeat("b", 64)
	first := t.TempDir()
	write(t, first, "a.yaml", recipe("1.3.0", pinned))
	base, err := Build(context.Background(), first, &fakeCharts{}, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// The next run only has the new recipe, plus the old one unchanged.
	next := t.TempDir()
	write(t, next, "a.yaml", recipe("1.3.0", pinned))
	write(t, next, "b.yaml", recipe("1.4.0", pinned))
	idx, err := Build(context.Background(), next, &fakeCharts{}, BuildOptions{Base: base})
	if err != nil {
		t.Fatal(err)
	}
	if v := idx.Packages["kubevirt"].Versions; len(v) != 2 || v[0].Version != "1.4.0" {
		t.Fatalf("merged versions: %+v", v)
	}

	// A version dropped from the recipes stays published.
	only := t.TempDir()
	write(t, only, "b.yaml", recipe("1.4.0", pinned))
	idx, err = Build(context.Background(), only, &fakeCharts{}, BuildOptions{Base: base})
	if err != nil {
		t.Fatal(err)
	}
	if v := idx.Packages["kubevirt"].Versions; len(v) != 2 {
		t.Fatalf("a published version disappeared: %+v", v)
	}

	// Changing a published version without a new build is refused.
	changed := t.TempDir()
	write(t, changed, "a.yaml", recipe("1.3.0", "sha256:"+strings.Repeat("c", 64)))
	if _, err := Build(context.Background(), changed, &fakeCharts{}, BuildOptions{Base: base}); err == nil || !strings.Contains(err.Error(), "new build number") {
		t.Fatalf("got %v", err)
	}
}

// chartsAt knows which charts are where, by repository/name and digest.
type chartsAt map[string]string

func (c chartsAt) FetchChart(_ context.Context, ch source.Chart) (string, string, error) {
	d, ok := c[ch.Repository+"/"+ch.Name]
	if !ok {
		return "", "", fmt.Errorf("%s/%s: not found", ch.Repository, ch.Name)
	}
	if ch.Digest != "" && ch.Digest != d {
		return "", "", fmt.Errorf("%s/%s has digest %s", ch.Repository, ch.Name, d)
	}
	return "", d, nil
}

func TestRelocateMovesPublishedVersionsToANewRegistry(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	tree := "oci://ghcr.io/old/packages/tree@sha256:" + strings.Repeat("b", 64)
	published := func(repo string) *Index {
		spec := v1.PackageSourceSpec{Version: "1.0.0", Build: 1,
			SourceRef: &v1.PackageSourceRef{Kind: v1.SourceKindOCIArtifact, URL: tree},
			Variants: []v1.Variant{{Name: "default", Components: []v1.Component{
				{Name: "app", Chart: &v1.ChartRef{Repository: repo, Name: "app", Version: "1.0.0-1", Digest: digest}, Install: &v1.ComponentInstall{Namespace: "app"}},
				{Name: "tree", Path: "tree", Install: &v1.ComponentInstall{Namespace: "app"}},
			}}}}
		d, _ := SpecDigest(spec)
		return &Index{Packages: map[string]Package{"app": {Versions: []Version{{Version: "1.0.0", Build: 1, Digest: d, Spec: spec}}}}}
	}
	base := published("oci://ghcr.io/old/packages/app")
	moves := map[string]string{"oci://ghcr.io/old/packages": "oci://ghcr.io/new/packages"}
	dir := t.TempDir()

	// The chart is at its new place with the same digest: relocated.
	idx, err := Build(context.Background(), dir, chartsAt{"oci://ghcr.io/new/packages/app/app": digest}, BuildOptions{Base: base, Relocate: moves})
	if err != nil {
		t.Fatal(err)
	}
	v := idx.Packages["app"].Versions[0]
	if v.Spec.Variants[0].Components[0].Chart.Repository != "oci://ghcr.io/new/packages/app" || v.Spec.SourceRef.URL != "oci://ghcr.io/new/packages/tree@sha256:"+strings.Repeat("b", 64) {
		t.Fatalf("not relocated: %+v", v.Spec)
	}
	if want, _ := SpecDigest(published("oci://ghcr.io/new/packages/app").Packages["app"].Versions[0].Spec); v.Digest == base.Packages["app"].Versions[0].Digest {
		t.Fatalf("digest kept after relocation (want a new one like %s)", want)
	}
	// A rebuilt recipe publishing to the new place now matches the base.
	// Missing, or different content there: refused.
	for name, at := range map[string]chartsAt{
		"missing":   {},
		"different": {"oci://ghcr.io/new/packages/app/app": "sha256:" + strings.Repeat("c", 64)},
	} {
		if _, err := Build(context.Background(), dir, at, BuildOptions{Base: published("oci://ghcr.io/old/packages/app"), Relocate: moves}); err == nil {
			t.Errorf("%s chart at the new place was accepted", name)
		}
	}
	// A path that only shares a prefix is not moved.
	other := published("oci://ghcr.io/old/packages-extra/app")
	idx, err = Build(context.Background(), dir, chartsAt{}, BuildOptions{Base: other, Relocate: moves})
	if err != nil {
		t.Fatal(err)
	}
	if got := idx.Packages["app"].Versions[0].Spec.Variants[0].Components[0].Chart.Repository; got != "oci://ghcr.io/old/packages-extra/app" {
		t.Fatalf("moved by a bare prefix: %s", got)
	}
}
