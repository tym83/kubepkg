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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/tym83/kubepkg/api/v1"
)

func installed(name string, dependency bool, provides []string, requires ...v1.Requirement) []client.Object {
	p := &v1.Package{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if dependency {
		p.Annotations = map[string]string{AnnotationDependency: "required by something"}
	}
	src := &v1.PackageSource{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1.PackageSourceSpec{
		Version: "1.0.0", Provides: provides, Variants: []v1.Variant{{Name: "default", Requires: requires}},
	}}
	return []client.Object{p, src}
}

func clusterOf(t *testing.T, sets ...[]client.Object) client.Client {
	t.Helper()
	var objs []client.Object
	for _, s := range sets {
		objs = append(objs, s...)
	}
	return fakeClient(t, objs...)
}

func names(rs []Removal) string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Name)
	}
	return strings.Join(out, ",")
}

func TestRemoveRefusesWhatOthersRequire(t *testing.T) {
	c := clusterOf(t,
		installed("virtualization", false, nil, v1.Requirement{Package: "kubevirt"}, v1.Requirement{Package: "cdi"}),
		installed("kubevirt", true, nil, v1.Requirement{Capability: "storage-importer"}),
		installed("cdi", true, []string{"storage-importer"}),
		installed("ingress-a", false, []string{"ingress"}),
		installed("ingress-b", false, []string{"ingress"}),
		installed("app", false, nil, v1.Requirement{Capability: "ingress"}, v1.Requirement{Capability: "api:apps/v1"}),
	)
	ctx := context.Background()

	if _, err := PlanRemove(ctx, c, []string{"kubevirt"}, false); err == nil || !strings.Contains(err.Error(), "kubevirt is required by virtualization") {
		t.Fatalf("got %v", err)
	}
	if _, err := PlanRemove(ctx, c, []string{"cdi"}, false); err == nil || !strings.Contains(err.Error(), "provides storage-importer, which kubevirt requires") {
		t.Fatalf("only provider of a capability: %v", err)
	}
	if plan, err := PlanRemove(ctx, c, []string{"ingress-a"}, false); err != nil || names(plan) != "ingress-a" {
		t.Fatalf("another provider stays: %v %v", plan, err)
	}
	if _, err := PlanRemove(ctx, c, []string{"ingress-a", "ingress-b"}, false); err == nil {
		t.Fatal("removing every provider of a required capability was allowed")
	}
	if _, err := PlanRemove(ctx, c, []string{"nope"}, false); err == nil {
		t.Fatal("removing a package that is not installed was allowed")
	}
}

func TestAutoremoveFollowsTheChain(t *testing.T) {
	c := clusterOf(t,
		installed("virtualization", false, nil, v1.Requirement{Package: "kubevirt"}, v1.Requirement{Package: "cdi"}),
		installed("kubevirt", true, nil, v1.Requirement{Capability: "storage-importer"}),
		installed("cdi", true, []string{"storage-importer"}),
		installed("cert-manager", false, nil),
	)
	ctx := context.Background()
	plan, err := PlanRemove(ctx, c, []string{"virtualization"}, false)
	if err != nil || names(plan) != "virtualization" {
		t.Fatalf("without autoremove: %v %v", plan, err)
	}
	plan, err = PlanRemove(ctx, c, []string{"virtualization"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if names(plan) != "cdi,kubevirt,virtualization" {
		t.Fatalf("autoremove must take the members, cdi only once kubevirt goes: %s", names(plan))
	}
}
