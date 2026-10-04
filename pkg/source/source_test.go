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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestComposeLibrariesAndValues(t *testing.T) {
	tree := t.TempDir()
	write(t, tree, map[string]string{
		"packages/system/app/Chart.yaml":     "apiVersion: v2\nname: app\nversion: 1.0.0\n",
		"packages/system/app/values.yaml":    "replicas: 1\n",
		"packages/system/app/base.yaml":      "image: {repo: a, tag: \"1\"}\nreplicas: 2\n",
		"packages/system/app/prod.yaml":      "image: {tag: \"2\"}\n",
		"packages/library/common/Chart.yaml": "apiVersion: v2\nname: common\nversion: 0.1.0\ntype: library\n",
	})
	dir, digest, err := Compose(tree, "packages", ChartSpec{
		Path:        "system/app",
		Libraries:   map[string]string{"common-lib": "library/common"},
		ValuesFiles: []string{"base.yaml", "prod.yaml"},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "charts", "common-lib", "Chart.yaml")); err != nil {
		t.Fatalf("library not placed under charts/: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "values.yaml"))
	var v map[string]any
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	img := v["image"].(map[string]any)
	// The first values file replaces values.yaml; later ones merge over it.
	if v["replicas"] != float64(2) || img["repo"] != "a" || img["tag"] != "2" {
		t.Fatalf("values = %v", v)
	}
	if !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("digest = %q", digest)
	}

	// Same input, same digest; changed input, changed digest.
	_, again, _ := Compose(tree, "packages", ChartSpec{Path: "system/app", Libraries: map[string]string{"common-lib": "library/common"}, ValuesFiles: []string{"base.yaml", "prod.yaml"}}, t.TempDir())
	if again != digest {
		t.Fatal("digest is not stable")
	}
	write(t, tree, map[string]string{"packages/system/app/prod.yaml": "image: {tag: \"3\"}\n"})
	_, changed, _ := Compose(tree, "packages", ChartSpec{Path: "system/app", Libraries: map[string]string{"common-lib": "library/common"}, ValuesFiles: []string{"base.yaml", "prod.yaml"}}, t.TempDir())
	if changed == digest {
		t.Fatal("digest did not change with the values")
	}
}

func TestComposeRefusesEscapes(t *testing.T) {
	tree := t.TempDir()
	write(t, tree, map[string]string{"packages/app/Chart.yaml": "apiVersion: v2\nname: app\nversion: 1.0.0\n"})
	for _, spec := range []ChartSpec{
		{Path: "../../etc"},
		{Path: "app", Libraries: map[string]string{"x": "../../../etc"}},
		{Path: "app", ValuesFiles: []string{"../../../../etc/passwd"}},
	} {
		if _, _, err := Compose(tree, "packages", spec, t.TempDir()); err == nil {
			t.Errorf("spec %+v escaped the tree", spec)
		}
	}
}

func tarball(t *testing.T, entries []tar.Header, bodies []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for i, h := range entries {
		h := h
		h.Size = int64(len(bodies[i]))
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(bodies[i])); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestUntar(t *testing.T) {
	good := tarball(t, []tar.Header{{Name: "app/Chart.yaml", Typeflag: tar.TypeReg, Mode: 0o644}}, []string{"name: app"})
	dst := t.TempDir()
	if err := untar(bytes.NewReader(good), dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "app", "Chart.yaml")); err != nil {
		t.Fatal(err)
	}

	for name, hdr := range map[string]tar.Header{
		"traversal": {Name: "../../evil", Typeflag: tar.TypeReg, Mode: 0o644},
		"absolute":  {Name: "/etc/evil", Typeflag: tar.TypeReg, Mode: 0o644},
		"symlink":   {Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
	} {
		bad := tarball(t, []tar.Header{hdr}, []string{""})
		if err := untar(bytes.NewReader(bad), t.TempDir()); err == nil {
			t.Errorf("%s: untar accepted it", name)
		}
	}
}

func TestMergeValuesDoesNotMutate(t *testing.T) {
	base := map[string]any{"a": map[string]any{"b": 1}}
	over := map[string]any{"a": map[string]any{"c": 2}}
	out := MergeValues(base, over)
	if len(base["a"].(map[string]any)) != 1 {
		t.Fatal("base was modified")
	}
	if m := out["a"].(map[string]any); m["b"] != 1 || m["c"] != 2 {
		t.Fatalf("merge = %v", out)
	}
}
