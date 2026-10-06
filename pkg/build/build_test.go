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

package build

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"helm.sh/helm/v4/pkg/chart/common"
	"helm.sh/helm/v4/pkg/chart/common/util"
	"helm.sh/helm/v4/pkg/chart/loader"
	"helm.sh/helm/v4/pkg/engine"

	"github.com/tym83/kubepkg/pkg/source"
)

const operatorYAML = `apiVersion: v1
kind: Namespace
metadata: {name: virt}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: alerts, namespace: virt}
data:
  rule: "{{ $labels.instance }} is down"
`

const crYAML = "apiVersion: example.org/v1\nkind: Virt\nmetadata: {name: virt, namespace: virt}\n"

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func targz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, n := range []string{"src/deploy/a.yaml", "src/deploy/b.yaml", "src/README"} {
		if body, ok := files[n]; ok {
			if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixture serves upstream files and writes a recipe using them.
func fixture(t *testing.T, operatorSum string) string {
	t.Helper()
	archive := targz(t, map[string]string{
		"src/deploy/a.yaml": "apiVersion: v1\nkind: ServiceAccount\nmetadata: {name: a, namespace: virt}\n",
		"src/deploy/b.yaml": "apiVersion: v1\nkind: ServiceAccount\nmetadata: {name: b, namespace: virt}\n",
		"src/README":        "not a manifest",
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/operator.yaml", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(operatorYAML)) })
	mux.HandleFunc("/v1/cr.yaml", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(crYAML)) })
	mux.HandleFunc("/v1/src.tar.gz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	if operatorSum == "" {
		operatorSum = sum([]byte(operatorYAML))
	}

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "recipe.yaml"), `apiVersion: kubepkg.dev/v1alpha1
kind: Recipe
metadata:
  name: virt
  annotations: {kubepkg.dev/description: Virtual machines}
spec:
  version: 1.4.0
  build: 2
  sources:
    operator: {url: "`+srv.URL+`/v1/operator.yaml", sha256: "`+operatorSum+`"}
    cr: {url: "`+srv.URL+`/v1/cr.yaml", sha256: "`+sum([]byte(crYAML))+`"}
    accounts: {url: "`+srv.URL+`/v1/src.tar.gz", sha256: "`+sum(archive)+`", path: src/deploy}
    ui: {dir: charts/ui}
  charts:
    virt-operator: {from: [operator, accounts], exclude: [{kind: Namespace}]}
    virt: {from: [cr]}
    ui:
      from: [ui]
      values: {replicas: 2, image: {tag: "1.4.0"}}
      overlay: overlay/ui
  package:
    provides: [virt]
    variants:
      - name: default
        components:
          - {name: operator, path: virt-operator, install: {namespace: virt}}
          - {name: virt, path: virt, install: {namespace: virt, dependsOn: [operator]}}
          - {name: ui, path: ui, install: {namespace: virt}}
`)
	writeFile(t, filepath.Join(dir, "charts/ui/Chart.yaml"), "apiVersion: v2\nname: ui\nversion: 0.3.0\n")
	writeFile(t, filepath.Join(dir, "charts/ui/values.yaml"), "replicas: 1\nimage: {repository: example.org/ui, tag: latest}\n")
	writeFile(t, filepath.Join(dir, "charts/ui/templates/cm.yaml"), "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: ui}\n")
	writeFile(t, filepath.Join(dir, "overlay/ui/templates/extra.yaml"), "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: extra}\n")
	return dir
}

func render(t *testing.T, chartDir string) string {
	t.Helper()
	ch, err := loader.Load(chartDir)
	if err != nil {
		t.Fatal(err)
	}
	vals, err := util.ToRenderValues(ch, map[string]any{}, common.ReleaseOptions{Name: "r", Namespace: "virt"}, common.DefaultCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	out, err := engine.Render(ch, vals)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, k := range []string{filepath.Base(chartDir) + "/templates/manifests.yaml"} {
		b.WriteString(out[k])
	}
	return b.String()
}

func TestBuildWrapsManifestsAndKeepsCharts(t *testing.T) {
	dir := fixture(t, "")
	res, err := Build(context.Background(), dir, Options{Fetcher: &source.Fetcher{CacheDir: t.TempDir()}, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	got := render(t, filepath.Join(res.TreeDir, "virt-operator"))
	for _, want := range []string{"name: alerts", "name: a", "name: b", `"{{ $labels.instance }} is down"`} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered operator chart lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "kind: Namespace") {
		t.Error("excluded Namespace was kept")
	}
	if strings.Index(got, "name: alerts") > strings.Index(got, "name: a") {
		t.Error("manifests are not applied in the order the recipe gives")
	}
	if strings.Contains(got, "not a manifest") {
		t.Error("non-YAML files from the archive were wrapped")
	}

	values, err := os.ReadFile(filepath.Join(res.TreeDir, "ui", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"replicas: 2", "tag: 1.4.0", "repository: example.org/ui"} {
		if !strings.Contains(string(values), want) {
			t.Errorf("ui values lack %q:\n%s", want, values)
		}
	}
	if _, err := os.Stat(filepath.Join(res.TreeDir, "ui", "templates", "extra.yaml")); err != nil {
		t.Error("overlay not applied")
	}

	src := res.Source
	if src.Name != "virt" || src.Spec.Version != "1.4.0" || src.Spec.Build != 2 || src.Annotations["kubepkg.dev/description"] != "Virtual machines" {
		t.Errorf("package source: %+v", src)
	}
}

func TestBuildIsReproducible(t *testing.T) {
	dir := fixture(t, "")
	digest := func() string {
		res, err := Build(context.Background(), dir, Options{Fetcher: &source.Fetcher{CacheDir: t.TempDir()}, WorkDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		_, content, err := source.Pack(res.TreeDir)
		if err != nil {
			t.Fatal(err)
		}
		return content
	}
	if a, b := digest(), digest(); a != b {
		t.Fatalf("two builds of one recipe differ: %s %s", a, b)
	}
}

func TestBuildRefusesChangedUpstream(t *testing.T) {
	dir := fixture(t, strings.Repeat("0", 64))
	_, err := Build(context.Background(), dir, Options{Fetcher: &source.Fetcher{CacheDir: t.TempDir()}, WorkDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "pins") {
		t.Fatalf("want a sha256 mismatch, got %v", err)
	}
}

func TestRecipeChecks(t *testing.T) {
	base := `apiVersion: kubepkg.dev/v1alpha1
kind: Recipe
metadata: {name: x}
spec:
  version: 1.0.0
  sources:
    s: {url: "https://example.org/a.yaml", sha256: "` + strings.Repeat("a", 64) + `"}
  charts:
    c: {from: [s]}
  package:
    variants: [{name: default, components: [{name: c, path: c, install: {namespace: x}}]}]
`
	cases := map[string]string{
		"range version":        strings.Replace(base, "version: 1.0.0", "version: \"~1.0\"", 1),
		"url without sha256":   strings.Replace(base, `, sha256: "`+strings.Repeat("a", 64)+`"`, "", 1),
		"unknown source":       strings.Replace(base, "from: [s]", "from: [nope]", 1),
		"component not built":  strings.Replace(base, "path: c,", "path: other,", 1),
		"version in package":   strings.Replace(base, "  package:\n", "  package:\n    version: 2.0.0\n", 1),
		"upstream chart as is": strings.Replace(base, "path: c,", `chart: {repository: "https://e.org", name: c, version: 1.0.0},`, 1),
		"typo":                 strings.Replace(base, "charts:", "chart:", 1),
		"exclude everything":   strings.Replace(base, "c: {from: [s]}", "c: {from: [s], exclude: [{}]}", 1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, RecipeFile), body)
			if _, err := LoadRecipe(dir); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, RecipeFile), base)
	if _, err := LoadRecipe(dir); err != nil {
		t.Fatalf("valid recipe rejected: %v", err)
	}
}

func TestMetaRecipeHasNothingToPush(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, RecipeFile), `apiVersion: kubepkg.dev/v1alpha1
kind: Recipe
metadata: {name: virtualization}
spec:
  version: 1.0.0
  package:
    variants:
      - name: default
        requires:
          - {package: kubevirt, version: "~1.9"}
          - {package: cdi, version: "~1.66"}
`)
	res, err := Build(context.Background(), dir, Options{Fetcher: &source.Fetcher{CacheDir: t.TempDir()}, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// An unreachable registry: a meta package must not need it.
	src, reused, err := Publish(context.Background(), res, "oci://127.0.0.1:1/nowhere", source.PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if reused || src.Spec.SourceRef != nil || src.Spec.Version != "1.0.0" || len(src.Spec.Variants[0].Requires) != 2 {
		t.Fatalf("meta package source: %+v", src.Spec)
	}
}

func TestPatchesChangeOnlyWhatTheyMatch(t *testing.T) {
	dir := fixture(t, "")
	recipe := filepath.Join(dir, RecipeFile)
	raw, err := os.ReadFile(recipe)
	if err != nil {
		t.Fatal(err)
	}
	patched := strings.Replace(string(raw), "    virt-operator: {from: [operator, accounts], exclude: [{kind: Namespace}]}\n", `    virt-operator:
      from: [operator, accounts]
      exclude: [{kind: Namespace}]
      patches:
        - kind: ConfigMap
          name: alerts
          merge: {data: {rule: null, owner: platform}}
`, 1)
	if patched == string(raw) {
		t.Fatal("fixture did not change")
	}
	writeFile(t, recipe, patched)
	res, err := Build(context.Background(), dir, Options{Fetcher: &source.Fetcher{CacheDir: t.TempDir()}, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	got := render(t, filepath.Join(res.TreeDir, "virt-operator"))
	if strings.Contains(got, "is down") || !strings.Contains(got, "owner: platform") {
		t.Errorf("patch not applied:\n%s", got)
	}
	if !strings.Contains(got, "name: a") || strings.Contains(got, "kind: Namespace") {
		t.Errorf("other objects must be kept and exclusions still apply:\n%s", got)
	}

	writeFile(t, recipe, strings.Replace(patched, "name: alerts", "name: renamed-upstream", 1))
	if _, err := Build(context.Background(), dir, Options{Fetcher: &source.Fetcher{CacheDir: t.TempDir()}, WorkDir: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "matched no object") {
		t.Fatalf("a patch that matches nothing must fail the build, got %v", err)
	}
}

func crdDoc(name string) string {
	return "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata: {name: " + name + "}\nspec: {group: x, names: {kind: X, plural: x}, scope: Namespaced, versions: []}\n"
}

// tgz packs files under prefix into a gzipped tar, as helm package does.
func tgz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for n, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExcludeAndPatchCRDsOfAChartAndItsPackedSubcharts(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "chart/Chart.yaml"), "apiVersion: v2\nname: gateway\nversion: 1.0.0\n")
	writeFile(t, filepath.Join(dir, "chart/crds/envoy.yaml"), crdDoc("backends.gateway.envoyproxy.io"))
	sub := tgz(t, map[string]string{
		"gateway-crds/Chart.yaml":           "apiVersion: v2\nname: gateway-crds\nversion: 1.0.0\n",
		"gateway-crds/crds/gatewayapi.yaml": crdDoc("gateways.gateway.networking.k8s.io") + "---\n" + crdDoc("httproutes.gateway.networking.k8s.io"),
		"gateway-crds/crds/envoy-more.yaml": crdDoc("envoyproxies.gateway.envoyproxy.io"),
	})
	if err := os.MkdirAll(filepath.Join(dir, "chart/charts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "chart/charts/gateway-crds-1.0.0.tgz"), sub, 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "recipe.yaml"), `apiVersion: kubepkg.dev/v1beta1
kind: Recipe
metadata: {name: gateway}
spec:
  version: 1.0.0
  build: 1
  sources: {chart: {dir: chart}}
  charts:
    gateway:
      from: [chart]
      exclude: [{kind: CustomResourceDefinition, name: gateways.gateway.networking.k8s.io}, {kind: CustomResourceDefinition, name: httproutes.gateway.networking.k8s.io}]
      patches:
        - kind: CustomResourceDefinition
          name: backends.gateway.envoyproxy.io
          merge: {metadata: {annotations: {example.org/patched: "yes"}}}
  package:
    variants: [{name: default, components: [{name: gateway, path: gateway, install: {namespace: gw}}]}]
`)
	res, err := Build(context.Background(), dir, opts(t))
	if err != nil {
		t.Fatal(err)
	}
	crds, err := chartCRDs(filepath.Join(res.TreeDir, "gateway"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(crds, " ") != "backends.gateway.envoyproxy.io envoyproxies.gateway.envoyproxy.io" {
		t.Fatalf("CRDs left: %v", crds)
	}
	if _, err := os.Stat(filepath.Join(res.TreeDir, "gateway/charts/gateway-crds/crds/gatewayapi.yaml")); !os.IsNotExist(err) {
		t.Fatal("a crds/ file with nothing left in it stays; Helm refuses empty ones")
	}
	raw, _ := os.ReadFile(filepath.Join(res.TreeDir, "gateway/crds/envoy.yaml"))
	if !strings.Contains(string(raw), "example.org/patched") {
		t.Fatalf("the patch did not apply:\n%s", raw)
	}
}

func TestCRDsOfWrappedManifestsCanGoToCrdsDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "upstream/all.yaml"), crdDoc("big.example.org")+"---\napiVersion: v1\nkind: ServiceAccount\nmetadata: {name: op}\n")
	writeFile(t, filepath.Join(dir, "recipe.yaml"), `apiVersion: kubepkg.dev/v1beta1
kind: Recipe
metadata: {name: op}
spec:
  version: 1.0.0
  build: 1
  sources: {up: {dir: upstream}}
  charts: {op: {from: [up], crdsDir: true}}
  package:
    crds: [big.example.org]
    variants: [{name: default, components: [{name: op, path: op, install: {namespace: op}}]}]
`)
	res, err := Build(context.Background(), dir, opts(t))
	if err != nil {
		t.Fatal(err)
	}
	crds, _ := os.ReadFile(filepath.Join(res.TreeDir, "op/crds/0000-all.yaml"))
	rest, _ := os.ReadFile(filepath.Join(res.TreeDir, "op/manifests/0000-all.yaml"))
	if !strings.Contains(string(crds), "big.example.org") || strings.Contains(string(rest), "CustomResourceDefinition") || !strings.Contains(string(rest), "ServiceAccount") {
		t.Fatalf("crds/:\n%s\nmanifests/:\n%s", crds, rest)
	}
	docs, err := renderChart(filepath.Join(res.TreeDir, "op"))
	if err != nil {
		t.Fatal(err)
	}
	if names, _ := crdNames(docs); strings.Join(names, ",") != "big.example.org" {
		t.Fatalf("the CRD is not installed from crds/: %v", names)
	}
}
