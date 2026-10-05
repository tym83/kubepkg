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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tym83/kubepkg/pkg/source"
)

const crdYAML = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: widgets.example.org}
spec:
  group: example.org
  names: {kind: Widget, plural: widgets}
  scope: Namespaced
  versions: [{name: v1, served: true, storage: true, schema: {openAPIV3Schema: {type: object}}}]
`

const bundleYAML = `apiVersion: v1
kind: Namespace
metadata: {name: widgets}
---
` + crdYAML + `---
apiVersion: v1
kind: ServiceAccount
metadata: {name: widget-operator, namespace: widgets}
`

func chartTgz(t *testing.T) []byte {
	t.Helper()
	files := map[string]string{
		"widgets/Chart.yaml":         "apiVersion: v2\nname: widgets\nversion: 0.4.0\nappVersion: v2.1.0\ndescription: Widgets for everyone\nhome: https://widgets.example.org\n",
		"widgets/values.yaml":        "replicas: 1\n",
		"widgets/templates/crd.yaml": crdYAML,
		"widgets/templates/sa.yaml":  "apiVersion: v1\nkind: ServiceAccount\nmetadata: {name: widgets}\n",
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, n := range []string{"widgets/Chart.yaml", "widgets/values.yaml", "widgets/templates/crd.yaml", "widgets/templates/sa.yaml"} {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(files[n])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(files[n])); err != nil {
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

func upstream(t *testing.T) string {
	t.Helper()
	archive := chartTgz(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/charts/index.yaml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("apiVersion: v1\nentries:\n  widgets:\n  - version: 0.4.0\n    urls: [widgets-0.4.0.tgz]\n"))
	})
	mux.HandleFunc("/charts/widgets-0.4.0.tgz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) })
	mux.HandleFunc("/releases/v2.1.0/widgets.yaml", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(bundleYAML)) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func opts(t *testing.T) Options {
	return Options{Fetcher: &source.Fetcher{CacheDir: t.TempDir()}, WorkDir: t.TempDir()}
}

func TestInitFromChartThenValidate(t *testing.T) {
	base := upstream(t)
	dir := filepath.Join(t.TempDir(), "widgets")
	ctx := context.Background()
	err := Init(ctx, dir, InitInput{Name: "widgets", Chart: &source.Chart{Repository: base + "/charts", Name: "widgets", Version: "0.4.0"}}, opts(t).Fetcher)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, RecipeFile))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{"version: 2.1.0", "Widgets for everyone", "home: https://widgets.example.org", "digest: sha256:", "- widgets.example.org", "namespace: widgets"} {
		if !strings.Contains(s, want) {
			t.Errorf("recipe lacks %q:\n%s", want, s)
		}
	}
	if rep := Validate(ctx, dir, opts(t)); !rep.OK() {
		t.Fatalf("a fresh recipe does not validate: %v", rep.Errors)
	}
	if err := Init(ctx, dir, InitInput{Name: "widgets", Manifests: []string{base + "/releases/v2.1.0/widgets.yaml"}, Version: "2.1.0"}, opts(t).Fetcher); err == nil {
		t.Error("init overwrote an existing recipe")
	}
}

func TestInitFromManifestsThenValidate(t *testing.T) {
	base := upstream(t)
	dir := filepath.Join(t.TempDir(), "widgets")
	ctx := context.Background()
	err := Init(ctx, dir, InitInput{Name: "widgets", Version: "2.1.0", Manifests: []string{base + "/releases/v2.1.0/widgets.yaml"}}, opts(t).Fetcher)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, RecipeFile))
	s := string(raw)
	for _, want := range []string{"widgets:\n      url: ", "sha256: ", "exclude: [{kind: Namespace}]", "- widgets.example.org", "TODO: one line"} {
		if !strings.Contains(s, want) {
			t.Errorf("recipe lacks %q:\n%s", want, s)
		}
	}
	rep := Validate(ctx, dir, opts(t))
	if !rep.OK() {
		t.Fatalf("a fresh recipe does not validate: %v", rep.Errors)
	}

	// A CRD the charts ship but the package does not declare is an error.
	if err := os.WriteFile(filepath.Join(dir, RecipeFile), []byte(strings.Replace(s, "      - widgets.example.org\n", "", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	rep = Validate(ctx, dir, opts(t))
	if rep.OK() || !strings.Contains(strings.Join(rep.Errors, " "), "does not declare") {
		t.Fatalf("an undeclared CRD passed: %+v", rep)
	}
}
