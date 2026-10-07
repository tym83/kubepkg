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

package helm

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"helm.sh/helm/v4/pkg/chart/loader"
)

func TestCRDsOfASubchartSwitchedOffAreNotApplied(t *testing.T) {
	dir := t.TempDir()
	crd := func(name string) string {
		return "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata: {name: " + name + "}\n"
	}
	for n, body := range map[string]string{
		"Chart.yaml":                     "apiVersion: v2\nname: operator\nversion: 1.0.0\ndependencies:\n  - {name: crds, version: 1.0.0, condition: crds.plain}\n",
		"values.yaml":                    "crds: {plain: false}\n",
		"crds/own.yaml":                  crd("own.example.org"),
		"charts/crds/Chart.yaml":         "apiVersion: v2\nname: crds\nversion: 1.0.0\n",
		"charts/crds/crds/from-sub.yaml": crd("sub.example.org"),
	} {
		p := filepath.Join(dir, n)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	names := func(values map[string]any) string {
		ch, err := loader.Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		crds, err := crdsToApply(ch, values)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range crds {
			out = append(out, c.GetName())
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}
	if got := names(map[string]any{}); got != "own.example.org" {
		t.Fatalf("with the subchart off by default: %s", got)
	}
	if got := names(map[string]any{"crds": map[string]any{"plain": true}}); got != "own.example.org,sub.example.org" {
		t.Fatalf("with the subchart switched on: %s", got)
	}
}
