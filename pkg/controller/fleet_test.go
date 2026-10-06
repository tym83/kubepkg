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

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/tym83/kubepkg/api/v1alpha1"
)

type fakeMembers struct {
	clients map[string]client.Client
	down    map[string]bool
}

func (f *fakeMembers) For(_ context.Context, c *v1alpha1.Cluster) (*Member, error) {
	if f.down[c.Name] {
		return nil, errors.New("connection refused")
	}
	return &Member{Client: f.clients[c.Name]}, nil
}

func kubepkgClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	sch := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Package{}, &v1alpha1.Cluster{}, &v1alpha1.PackageSet{}).Build()
}

type fleet struct {
	t       *testing.T
	hub     client.Client
	members *fakeMembers
	r       *PackageSetReconciler
}

func newFleet(t *testing.T) *fleet {
	cluster := func(name, env string) *v1alpha1.Cluster {
		return &v1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"env": env}}}
	}
	hub := kubepkgClient(t, cluster("a", "prod"), cluster("b", "prod"), cluster("c", "dev"))
	m := &fakeMembers{clients: map[string]client.Client{"a": kubepkgClient(t), "b": kubepkgClient(t), "c": kubepkgClient(t)}, down: map[string]bool{}}
	return &fleet{t: t, hub: hub, members: m, r: &PackageSetReconciler{Client: hub, Members: m}}
}

func (f *fleet) reconcile() {
	f.t.Helper()
	if _, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "base"}}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fleet) set() *v1alpha1.PackageSet {
	f.t.Helper()
	s := &v1alpha1.PackageSet{}
	if err := f.hub.Get(context.Background(), types.NamespacedName{Name: "base"}, s); err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *fleet) packages(cluster string) map[string]v1alpha1.Package {
	f.t.Helper()
	var list v1alpha1.PackageList
	if err := f.members.clients[cluster].List(context.Background(), &list); err != nil {
		f.t.Fatal(err)
	}
	out := map[string]v1alpha1.Package{}
	for _, p := range list.Items {
		out[p.Name] = p
	}
	return out
}

func (f *fleet) markReady(cluster, name string) {
	f.t.Helper()
	c := f.members.clients[cluster]
	p := &v1alpha1.Package{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, p); err != nil {
		f.t.Fatal(err)
	}
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "ReconciliationSucceeded"})
	if err := c.Status().Update(context.Background(), p); err != nil {
		f.t.Fatal(err)
	}
}

func baseSet() *v1alpha1.PackageSet {
	return &v1alpha1.PackageSet{
		ObjectMeta: metav1.ObjectMeta{Name: "base"},
		Spec: v1alpha1.PackageSetSpec{
			ClusterSelector: metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}},
			Repositories:    []v1alpha1.PackageSetRepository{{Name: "main", Spec: v1alpha1.RepositorySpec{URL: "https://packages.example.org/index.yaml"}}},
			Packages: []v1alpha1.PackageSetPackage{
				{Name: "cert-manager", Spec: v1alpha1.PackageSpec{Version: "~1.21"}},
				{Name: "app"},
			},
		},
	}
}

func TestPackageSetSpreadsAndReports(t *testing.T) {
	f := newFleet(t)
	// b already has an app of its own.
	if err := f.members.clients["b"].Create(context.Background(), &v1alpha1.Package{ObjectMeta: metav1.ObjectMeta{Name: "app"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Create(context.Background(), baseSet()); err != nil {
		t.Fatal(err)
	}
	f.reconcile()

	a := f.packages("a")
	if a["cert-manager"].Spec.Version != "~1.21" || a["cert-manager"].Labels[v1alpha1.LabelPackageSet] != "base" || len(a) != 2 {
		t.Fatalf("cluster a: %+v", a)
	}
	if len(f.packages("c")) != 0 {
		t.Fatal("a cluster outside the selector got packages")
	}
	if got := f.packages("b")["app"].Labels[v1alpha1.LabelPackageSet]; got != "" {
		t.Fatal("the set took over a Package it did not create")
	}
	repo := &v1alpha1.Repository{}
	if err := f.members.clients["a"].Get(context.Background(), types.NamespacedName{Name: "main"}, repo); err != nil || repo.Spec.URL == "" {
		t.Fatalf("repository on a: %v", err)
	}

	f.markReady("a", "cert-manager")
	f.markReady("a", "app")
	f.reconcile()
	s := f.set()
	if s.Status.ReadyClusters != 1 || len(s.Status.Clusters) != 2 {
		t.Fatalf("status: %+v", s.Status)
	}
	for _, c := range s.Status.Clusters {
		if c.Name == "b" && !strings.Contains(c.Message, "not managed by this set") {
			t.Fatalf("b must report the conflict: %+v", c)
		}
	}
	if c := meta.FindStatusCondition(s.Status.Conditions, "Ready"); c == nil || c.Status != metav1.ConditionFalse || c.Message != "1 of 2 clusters ready" {
		t.Fatalf("condition: %+v", c)
	}

	// Dropping a package removes it where the set put it, only there.
	s.Spec.Packages = s.Spec.Packages[:1]
	if err := f.hub.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	f.reconcile()
	if _, ok := f.packages("a")["app"]; ok {
		t.Fatal("a package dropped from the set stays")
	}
	if _, ok := f.packages("b")["app"]; !ok {
		t.Fatal("the hand-made app on b was deleted")
	}

	// A cluster that leaves the selector loses what the set wrote.
	ca := &v1alpha1.Cluster{}
	if err := f.hub.Get(context.Background(), types.NamespacedName{Name: "a"}, ca); err != nil {
		t.Fatal(err)
	}
	ca.Labels["env"] = "dev"
	if err := f.hub.Update(context.Background(), ca); err != nil {
		t.Fatal(err)
	}
	f.reconcile()
	if n := len(f.packages("a")); n != 0 {
		t.Fatalf("a deselected cluster keeps %d packages of the set", n)
	}
}

func TestDeletingASetCleansUpButWaitsForUnreachableClusters(t *testing.T) {
	f := newFleet(t)
	if err := f.hub.Create(context.Background(), baseSet()); err != nil {
		t.Fatal(err)
	}
	f.reconcile()
	f.members.down["b"] = true
	if err := f.hub.Delete(context.Background(), f.set()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "base"}}); err == nil {
		t.Fatal("deleting a set with an unreachable member must wait")
	}
	if n := len(f.packages("a")); n != 0 {
		t.Fatalf("the reachable member keeps %d packages", n)
	}
	f.members.down["b"] = false
	f.reconcile()
	if n := len(f.packages("b")); n != 0 {
		t.Fatalf("b keeps %d packages", n)
	}
	if err := f.hub.Get(context.Background(), types.NamespacedName{Name: "base"}, &v1alpha1.PackageSet{}); err == nil {
		t.Fatal("the set was not released")
	}
}
