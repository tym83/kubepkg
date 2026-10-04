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
	"context"
	"strings"
	"testing"

	"github.com/tym83/kubepkg/api/v1alpha1"
	"github.com/tym83/kubepkg/pkg/repo"
)

func renderStore(t *testing.T) *repo.Store {
	t.Helper()
	chart := func(name string) *v1alpha1.ChartRef {
		return &v1alpha1.ChartRef{Repository: "oci://ghcr.io/example/packages/" + name, Name: name, Version: "1.0.0-1", Digest: "sha256:" + strings.Repeat("a", 64)}
	}
	return store(t, map[string][]v1alpha1.PackageSourceSpec{
		"kubevirt": {{Version: "1.9.0", Variants: []v1alpha1.Variant{{
			Name:     "default",
			Requires: []v1alpha1.Requirement{{Capability: "storage-importer"}, {Capability: "api:apps/v1"}},
			Components: []v1alpha1.Component{
				{Name: "cr", Chart: chart("kubevirt"), Install: &v1alpha1.ComponentInstall{Namespace: "kubevirt", DependsOn: []string{"operator"}}},
				{Name: "operator", Chart: chart("kubevirt-operator"), Install: &v1alpha1.ComponentInstall{Namespace: "kubevirt"}},
			},
		}}}},
		"cdi": {{Version: "1.66.1", Provides: []string{"storage-importer"}, Variants: []v1alpha1.Variant{{
			Name:       "default",
			Components: []v1alpha1.Component{{Name: "cdi", Chart: chart("cdi"), Install: &v1alpha1.ComponentInstall{Namespace: "cdi"}}},
		}}}},
	})
}

func TestRenderFlux(t *testing.T) {
	out, err := Render(context.Background(), renderStore(t), repo.AllowAll{}, parseRequests([]string{"kubevirt"}), RenderOptions{Format: "flux"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	// cdi first: kubevirt needs the capability it provides; within
	// kubevirt the operator before the resource it serves.
	cdi, op, cr := strings.Index(s, "name: cdi\n"), strings.Index(s, "name: operator"), strings.Index(s, "name: cr\n")
	if !(cdi >= 0 && op > cdi && cr > op) {
		t.Errorf("order cdi < operator < cr not kept (%d %d %d):\n%s", cdi, op, cr, s)
	}
	for _, want := range []string{"kind: OCIRepository", "kind: HelmRelease", "url: oci://ghcr.io/example/packages/kubevirt/kubevirt", "- name: operator\n    namespace: kubevirt", "- name: cdi\n    namespace: cdi"} {
		if !strings.Contains(s, want) {
			t.Errorf("flux output lacks %q", want)
		}
	}
	if strings.Contains(s, "status:") || strings.Contains(s, "creationTimestamp") {
		t.Error("output carries status or creation times")
	}
}

func TestRenderArgoAndHelmfile(t *testing.T) {
	ctx := context.Background()
	out, err := Render(ctx, renderStore(t), repo.AllowAll{}, parseRequests([]string{"kubevirt"}), RenderOptions{Format: "argo"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Count(s, "kind: Application") != 3 || !strings.Contains(s, "argocd.argoproj.io/sync-wave: \"2\"") {
		t.Errorf("argo output:\n%s", s)
	}
	out, err = Render(ctx, renderStore(t), repo.AllowAll{}, parseRequests([]string{"kubevirt"}), RenderOptions{Format: "helmfile"})
	if err != nil {
		t.Fatal(err)
	}
	s = string(out)
	for _, want := range []string{"chart: oci://ghcr.io/example/packages/cdi/cdi", "- cdi/cdi", "- kubevirt/operator"} {
		if !strings.Contains(s, want) {
			t.Errorf("helmfile lacks %q:\n%s", want, s)
		}
	}
	if _, err := Render(ctx, renderStore(t), repo.AllowAll{}, parseRequests([]string{"kubevirt"}), RenderOptions{Format: "kustomize"}); err == nil {
		t.Error("unknown format accepted")
	}
}
