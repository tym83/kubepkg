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
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tym83/kubepkg/pkg/source"
)

const pluginRecipe = `apiVersion: kubepkg.dev/v1alpha1
kind: Recipe
metadata: {name: app}
spec:
  version: 2.0.0
  sources:
    src: {plugin: gen, with: {image: "upstream.io/app:2.0.0"}}
  charts:
    app:
      from: [src]
      steps:
        - {plugin: relocate, with: {registry: mirror.example.com}}
        - {plugin: stamp}
  package:
    variants: [{name: default, components: [{name: app, path: app, install: {namespace: app}}]}]
`

// script puts an executable plugin on PATH.
func script(t *testing.T, bin, name, body string) {
	t.Helper()
	writeFile(t, filepath.Join(bin, name), "#!/bin/sh\nset -eu\n"+body)
	if err := os.Chmod(filepath.Join(bin, name), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestPlugins(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	// A source plugin in shell: reads With from stdin, fills KUBEPKG_OUT.
	script(t, bin, "kubepkg-source-gen", `image=$(sed 's/.*"image":"\([^"]*\)".*/\1/')
printf 'apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: %s}\nspec: {template: {spec: {containers: [{name: app, image: %s}]}}}\n' "$KUBEPKG_PACKAGE" "$image" > "$KUBEPKG_OUT/deploy.yaml"
`)
	// A step plugin in shell: rewrites images inside KUBEPKG_CHART.
	script(t, bin, "kubepkg-step-relocate", `registry=$(sed 's/.*"registry":"\([^"]*\)".*/\1/')
for f in "$KUBEPKG_CHART"/manifests/*; do sed -i.bak "s#upstream.io/#$registry/#" "$f" && rm "$f.bak"; done
`)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, RecipeFile), pluginRecipe)

	stamped := ""
	opts := Options{
		Fetcher: &source.Fetcher{CacheDir: t.TempDir()},
		WorkDir: t.TempDir(),
		Plugins: Plugins{Steps: map[string]StepPlugin{
			"stamp": StepFunc(func(_ context.Context, in PluginInput) error {
				stamped = in.Package + "@" + in.Version
				return os.WriteFile(filepath.Join(in.Chart, "STAMP"), []byte(stamped), 0o644)
			}),
		}},
	}
	res, err := Build(context.Background(), dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	got := render(t, filepath.Join(res.TreeDir, "app"))
	if !strings.Contains(got, "image: mirror.example.com/app:2.0.0") {
		t.Errorf("source and step plugins did not run in order:\n%s", got)
	}
	if stamped != "app@2.0.0" {
		t.Errorf("Go step plugin got %q", stamped)
	}
	if _, err := os.Stat(filepath.Join(res.TreeDir, "app", "STAMP")); err != nil {
		t.Error("Go step plugin output missing")
	}
}

func TestPluginErrors(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, RecipeFile), pluginRecipe)
	opts := Options{Fetcher: &source.Fetcher{CacheDir: t.TempDir()}, WorkDir: t.TempDir()}

	if _, err := Build(context.Background(), dir, opts); err == nil || !strings.Contains(err.Error(), "kubepkg-source-gen is not on PATH") {
		t.Fatalf("missing plugin: %v", err)
	}
	script(t, bin, "kubepkg-source-gen", "echo upstream is gone >&2\nexit 3\n")
	if _, err := Build(context.Background(), dir, opts); err == nil || !strings.Contains(err.Error(), "upstream is gone") {
		t.Fatalf("failing plugin: %v", err)
	}
}
