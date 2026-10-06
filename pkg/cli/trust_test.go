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

package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tym83/kubepkg/pkg/repo"
)

type fileFetcher struct{}

func (fileFetcher) FetchIndex(_ context.Context, u string) ([]byte, error) {
	return os.ReadFile(strings.TrimPrefix(u, "file://"))
}

func run(t *testing.T, args ...string) string {
	t.Helper()
	root := NewRootCommand(DefaultOptions())
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("kubepkg %s: %v\n%s", strings.Join(args, " "), err, out.String())
	}
	return out.String()
}

func TestRepositoryWithARootOfTrust(t *testing.T) {
	dir := t.TempDir()
	k := func(name string) string { return filepath.Join(dir, name) }
	for _, name := range []string{"a", "b", "c", "ci"} {
		run(t, "repo", "keygen", k(name))
	}
	run(t, "trust", "root", "new",
		"--root-key", k("a.pub"), "--root-key", k("b.pub"), "--root-key", k("c.pub"), "--root-threshold", "2",
		"--index-key", k("ci.pub"), "--index-threshold", "1", "-o", k("root.yaml"))

	site := k("site")
	if err := os.MkdirAll(filepath.Join(site, "root"), 0o755); err != nil {
		t.Fatal(err)
	}
	recipes := k("recipes")
	if err := os.MkdirAll(recipes, 0o755); err != nil {
		t.Fatal(err)
	}
	src := "apiVersion: kubepkg.dev/v1alpha1\nkind: PackageSource\nmetadata: {name: app}\nspec:\n  version: 1.0.0\n  variants:\n    - name: default\n      components:\n        - {name: app, chart: {repository: \"oci://example.org/c\", name: app, version: 1.0.0, digest: \"sha256:" + strings.Repeat("a", 64) + "\"}, install: {namespace: app}}\n"
	if err := os.WriteFile(filepath.Join(recipes, "app.yaml"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "repo", "index", recipes, "-o", filepath.Join(site, "index.yaml"), "--expires", "720h", "--sign-key", k("ci.key"))

	publish := func() {
		raw, err := os.ReadFile(k("root.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{filepath.Join(site, "root.yaml"), filepath.Join(site, "root", "1.yaml")} {
			if err := os.WriteFile(p, raw, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	pinned, err := readKeys([]string{k("a.pub"), k("b.pub"), k("c.pub")})
	if err != nil {
		t.Fatal(err)
	}
	load := func() error {
		_, _, _, err := repo.LoadIndexWithRoot(context.Background(), repo.Fetchers{"file": fileFetcher{}}, "file://"+filepath.Join(site, "index.yaml"),
			repo.Trust{RootKeys: pinned, RootThreshold: 2}, time.Now())
		return err
	}

	run(t, "trust", "sign", k("root.yaml"), "--key", k("a.key"))
	publish()
	if err := load(); !errors.Is(err, repo.ErrBadSignature) {
		t.Fatalf("a root with 1 of 2 signatures: %v", err)
	}
	if out := run(t, "trust", "sign", k("root.yaml"), "--key", k("c.key")); !strings.Contains(out, "2 signatures") {
		t.Fatalf("second signature: %s", out)
	}
	publish()
	if err := load(); err != nil {
		t.Fatalf("signed by 2 of 3 root keys, index by its key: %v", err)
	}
}
