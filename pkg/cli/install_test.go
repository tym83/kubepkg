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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kuberoot-dev/kubepkg/api/v1"
	"github.com/kuberoot-dev/kubepkg/pkg/repo"
	"github.com/kuberoot-dev/kubepkg/pkg/resolve"
)

type servedAPIs map[string]bool

func (a servedAPIs) Served(context.Context) (map[string]bool, error) { return a, nil }

func spec(version string, requires ...v1.Requirement) v1.PackageSourceSpec {
	return v1.PackageSourceSpec{
		Version: version,
		CRDs:    []string{"things.example.org"},
		Variants: []v1.Variant{{Name: "default", Requires: requires, Components: []v1.Component{
			{Name: "main", Chart: &v1.ChartRef{Repository: "oci://example.org/c", Name: "main", Version: version}, Install: &v1.ComponentInstall{Namespace: "ns"}},
		}}},
	}
}

func store(t *testing.T, pkgs map[string][]v1.PackageSourceSpec) *repo.Store {
	t.Helper()
	idx := &repo.Index{Kind: repo.IndexKind, Packages: map[string]repo.Package{}}
	for name, specs := range pkgs {
		p := repo.Package{}
		for _, s := range specs {
			d, err := repo.SpecDigest(s)
			if err != nil {
				t.Fatal(err)
			}
			p.Versions = append(p.Versions, repo.Version{Version: s.Version, Digest: d, Spec: s})
		}
		idx.Packages[name] = p
	}
	s := repo.NewStore()
	s.Set("main", 0, idx)
	return s
}

func fakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	sch := runtime.NewScheme()
	if err := v1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).WithStatusSubresource(&v1.Package{}).Build()
}

func TestInstallWithRequirements(t *testing.T) {
	s := store(t, map[string][]v1.PackageSourceSpec{
		"kubevirt": {spec("1.9.0", v1.Requirement{Package: "cdi", Version: ">=1.60"}), spec("1.8.2", v1.Requirement{Package: "cdi"})},
		"cdi":      {spec("1.59.0"), spec("1.60.1"), spec("1.61.0")},
	})
	c := fakeClient(t)
	ctx := context.Background()
	steps, err := Plan(ctx, c, s, repo.AllowAll{}, servedAPIs{}, "", parseRequests([]string{"kubevirt"}))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WritePlan(&buf, steps); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"install  cdi", "1.61.0", "required by kubevirt 1.9.0", "install  kubevirt", "requested", "owns CRDs: things.example.org", "not rollback-safe"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "install  cdi") > strings.Index(out, "install  kubevirt") {
		t.Error("dependencies should be listed first")
	}

	if err := Apply(ctx, c, steps); err != nil {
		t.Fatal(err)
	}
	get := func(name string) *v1.Package {
		p := &v1.Package{}
		if err := c.Get(ctx, types.NamespacedName{Name: name}, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if p := get("kubevirt"); p.Spec.Version != "~1.9" || p.Annotations[AnnotationDependency] != "" {
		t.Errorf("kubevirt: %+v %v", p.Spec, p.Annotations)
	}
	if p := get("cdi"); p.Spec.Version != ">=1.60" || p.Annotations[AnnotationDependency] == "" {
		t.Errorf("cdi: %+v %v", p.Spec, p.Annotations)
	}
}

func TestInstallKeepsAndUpgrades(t *testing.T) {
	s := store(t, map[string][]v1.PackageSourceSpec{
		"kubevirt": {spec("1.9.0", v1.Requirement{Package: "cdi"}), spec("1.10.0", v1.Requirement{Package: "cdi"})},
		"cdi":      {spec("1.60.0"), spec("1.61.0")},
	})
	installed := func(name, version string) *v1.Package {
		return &v1.Package{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1.PackageSpec{Version: "~" + version[:strings.LastIndex(version, ".")]}, Status: v1.PackageStatus{Version: version}}
	}
	c := fakeClient(t, installed("kubevirt", "1.9.0"), installed("cdi", "1.60.0"))
	ctx := context.Background()
	steps, err := Plan(ctx, c, s, repo.AllowAll{}, servedAPIs{}, "", parseRequests([]string{"kubevirt@~1.10"}))
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]resolve.Action{}
	for _, st := range steps {
		actions[st.Name] = st.Action
	}
	if actions["kubevirt"] != resolve.ActionUpgrade || actions["cdi"] != resolve.ActionKeep {
		t.Fatalf("an installed requirement must be kept, the requested package upgraded: %v", actions)
	}
	if err := Apply(ctx, c, steps); err != nil {
		t.Fatal(err)
	}
	p := &v1.Package{}
	if err := c.Get(ctx, types.NamespacedName{Name: "kubevirt"}, p); err != nil {
		t.Fatal(err)
	}
	if p.Spec.Version != "~1.10" {
		t.Errorf("kubevirt constraint %q", p.Spec.Version)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: "cdi"}, p); err != nil {
		t.Fatal(err)
	}
	if p.Spec.Version != "~1.60" {
		t.Errorf("kept cdi was changed to %q", p.Spec.Version)
	}
}

func TestPlanFailsOnUnsatisfiable(t *testing.T) {
	s := store(t, map[string][]v1.PackageSourceSpec{"kubevirt": {spec("1.9.0", v1.Requirement{Package: "cdi", Version: ">=2"})}, "cdi": {spec("1.60.0")}})
	if _, err := Plan(context.Background(), fakeClient(t), s, repo.AllowAll{}, servedAPIs{}, "", parseRequests([]string{"kubevirt"})); err == nil || !strings.Contains(err.Error(), "cdi") {
		t.Fatalf("got %v", err)
	}
}

func TestMetaPackageMovesItsMembers(t *testing.T) {
	meta := func(version, kubevirt string) v1.PackageSourceSpec {
		return v1.PackageSourceSpec{Version: version, Variants: []v1.Variant{{Name: "default", Requires: []v1.Requirement{{Package: "kubevirt", Version: kubevirt}, {Package: "cdi"}}}}}
	}
	s := store(t, map[string][]v1.PackageSourceSpec{
		"distro":   {meta("1.0.0", "~1.9"), meta("2.0.0", "~1.10")},
		"kubevirt": {spec("1.9.0"), spec("1.9.3"), spec("1.10.1")},
		"cdi":      {spec("1.60.0")},
	})
	c := fakeClient(t)
	ctx := context.Background()
	install := func(req string) {
		t.Helper()
		steps, err := Plan(ctx, c, s, repo.AllowAll{}, servedAPIs{}, "", parseRequests([]string{req}))
		if err != nil {
			t.Fatal(err)
		}
		if err := Apply(ctx, c, steps); err != nil {
			t.Fatal(err)
		}
	}
	version := func(name string) string {
		t.Helper()
		p := &v1.Package{}
		if err := c.Get(ctx, types.NamespacedName{Name: name}, p); err != nil {
			t.Fatal(err)
		}
		return p.Spec.Version
	}
	settle := func(name, applied string) {
		t.Helper()
		p := &v1.Package{}
		if err := c.Get(ctx, types.NamespacedName{Name: name}, p); err != nil {
			t.Fatal(err)
		}
		p.Status.Version = applied
		if err := c.Status().Update(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	steps, err := Plan(ctx, c, s, repo.AllowAll{}, servedAPIs{}, "", parseRequests([]string{"distro@~1.0"}))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WritePlan(&buf, steps); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "distro: not rollback-safe") {
		t.Errorf("a meta package has nothing to roll back:\n%s", buf.String())
	}
	install("distro@~1.0")
	if v := version("kubevirt"); v != "~1.9" {
		t.Fatalf("member constraint after installing the distro: %q", v)
	}
	if v := version("cdi"); v != "~1.60" {
		t.Fatalf("a member without a constraint follows patch releases: %q", v)
	}
	settle("distro", "1.0.0")
	settle("kubevirt", "1.9.3")
	settle("cdi", "1.60.0")

	install("distro@~2.0")
	if v := version("kubevirt"); v != "~1.10" {
		t.Fatalf("the new distro version did not move kubevirt: %q", v)
	}
}
