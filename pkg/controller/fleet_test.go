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

	"github.com/tym83/kubepkg/api/v1beta1"
)

type fakeMembers struct {
	clients map[string]client.Client
	down    map[string]bool
	deletes []string // cluster/name of every deleted object
}

// countingClient records deletes on a member.
type countingClient struct {
	client.Client
	cluster string
	m       *fakeMembers
}

func (c countingClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	c.m.deletes = append(c.m.deletes, c.cluster+"/"+obj.GetName())
	return c.Client.Delete(ctx, obj, opts...)
}

func (f *fakeMembers) For(_ context.Context, c *v1beta1.Cluster) (*Member, error) {
	if f.down[c.Name] {
		return nil, errors.New("connection refused")
	}
	return &Member{Client: countingClient{Client: f.clients[c.Name], cluster: c.Name, m: f}}, nil
}

func kubepkgClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	sch := runtime.NewScheme()
	if err := v1beta1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).
		WithStatusSubresource(&v1beta1.Package{}, &v1beta1.Cluster{}, &v1beta1.PackageSet{}).Build()
}

type fleet struct {
	t       *testing.T
	hub     client.Client
	members *fakeMembers
	r       *PackageSetReconciler
}

func newFleet(t *testing.T) *fleet {
	cluster := func(name, env string) *v1beta1.Cluster {
		return &v1beta1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"env": env}}}
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

func (f *fleet) set() *v1beta1.PackageSet {
	f.t.Helper()
	s := &v1beta1.PackageSet{}
	if err := f.hub.Get(context.Background(), types.NamespacedName{Name: "base"}, s); err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *fleet) packages(cluster string) map[string]v1beta1.Package {
	f.t.Helper()
	var list v1beta1.PackageList
	if err := f.members.clients[cluster].List(context.Background(), &list); err != nil {
		f.t.Fatal(err)
	}
	out := map[string]v1beta1.Package{}
	for _, p := range list.Items {
		out[p.Name] = p
	}
	return out
}

func (f *fleet) markReady(cluster, name string) {
	f.t.Helper()
	c := f.members.clients[cluster]
	p := &v1beta1.Package{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, p); err != nil {
		f.t.Fatal(err)
	}
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "ReconciliationSucceeded", ObservedGeneration: p.Generation})
	if err := c.Status().Update(context.Background(), p); err != nil {
		f.t.Fatal(err)
	}
}

func baseSet() *v1beta1.PackageSet {
	return &v1beta1.PackageSet{
		ObjectMeta: metav1.ObjectMeta{Name: "base"},
		Spec: v1beta1.PackageSetSpec{
			ClusterSelector: metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}},
			Repositories:    []v1beta1.PackageSetRepository{{Name: "main", Spec: v1beta1.RepositorySpec{URL: "https://packages.example.org/index.yaml"}}},
			Packages: []v1beta1.PackageSetPackage{
				{Name: "cert-manager", Spec: v1beta1.PackageSpec{Version: "~1.21"}},
				{Name: "app"},
			},
		},
	}
}

func TestPackageSetSpreadsAndReports(t *testing.T) {
	f := newFleet(t)
	// b already has an app of its own.
	if err := f.members.clients["b"].Create(context.Background(), &v1beta1.Package{ObjectMeta: metav1.ObjectMeta{Name: "app"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Create(context.Background(), baseSet()); err != nil {
		t.Fatal(err)
	}
	f.reconcile()

	a := f.packages("a")
	if a["cert-manager"].Spec.Version != "~1.21" || a["cert-manager"].Labels[v1beta1.LabelPackageSet] != "base" || len(a) != 2 {
		t.Fatalf("cluster a: %+v", a)
	}
	if len(f.packages("c")) != 0 {
		t.Fatal("a cluster outside the selector got packages")
	}
	if got := f.packages("b")["app"].Labels[v1beta1.LabelPackageSet]; got != "" {
		t.Fatal("the set took over a Package it did not create")
	}
	repo := &v1beta1.Repository{}
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
	if c := meta.FindStatusCondition(s.Status.Conditions, "Ready"); c == nil || c.Status != metav1.ConditionFalse || c.Message != "1 of 2 clusters ready, 1 updated" {
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
	ca := &v1beta1.Cluster{}
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
	if err := f.hub.Get(context.Background(), types.NamespacedName{Name: "base"}, &v1beta1.PackageSet{}); err == nil {
		t.Fatal("the set was not released")
	}
}

func (f *fleet) markFailed(cluster, name string) {
	f.t.Helper()
	c := f.members.clients[cluster]
	p := &v1beta1.Package{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, p); err != nil {
		f.t.Fatal(err)
	}
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: v1beta1.ReasonUpgradeRolledBack, Message: "pods crashlooping", ObservedGeneration: p.Generation})
	if err := c.Status().Update(context.Background(), p); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fleet) status(cluster string) v1beta1.PackageSetClusterStatus {
	f.t.Helper()
	for _, c := range f.set().Status.Clusters {
		if c.Name == cluster {
			return c
		}
	}
	f.t.Fatalf("no status for %s", cluster)
	return v1beta1.PackageSetClusterStatus{}
}

func oneBy(s *v1beta1.PackageSet) *v1beta1.PackageSet {
	s.Spec.Packages = s.Spec.Packages[:1]
	s.Spec.Rollout = &v1beta1.PackageSetRollout{MaxInProgress: 1}
	return s
}

func TestRolloutOneClusterAtATimeAndPauseOnFailure(t *testing.T) {
	f := newFleet(t)
	if err := f.hub.Create(context.Background(), oneBy(baseSet())); err != nil {
		t.Fatal(err)
	}
	f.reconcile()
	if len(f.packages("a")) != 1 || len(f.packages("b")) != 0 {
		t.Fatalf("first step: a=%d b=%d", len(f.packages("a")), len(f.packages("b")))
	}
	if m := f.status("b").Message; m != "waiting for its turn" {
		t.Fatalf("b: %q", m)
	}
	f.markReady("a", "cert-manager")
	f.reconcile()
	if len(f.packages("b")) != 1 {
		t.Fatal("b did not get the set once a was done")
	}
	f.markReady("b", "cert-manager")
	f.reconcile()
	if s := f.set(); s.Status.ReadyClusters != 2 || s.Status.UpdatedClusters != 2 {
		t.Fatalf("status: %+v", s.Status)
	}

	// A change goes to a first; a fails with it, so b keeps the old one.
	s := f.set()
	s.Spec.Packages[0].Spec.Version = "~1.22"
	if err := f.hub.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	f.reconcile()
	if f.packages("a")["cert-manager"].Spec.Version != "~1.22" || f.packages("b")["cert-manager"].Spec.Version != "~1.21" {
		t.Fatal("the change did not start with a alone")
	}
	f.markFailed("a", "cert-manager")
	f.reconcile()
	f.reconcile()
	if f.packages("b")["cert-manager"].Spec.Version != "~1.21" {
		t.Fatal("the change reached b after a failed with it")
	}
	if c := meta.FindStatusCondition(f.set().Status.Conditions, "RolloutPaused"); c == nil || c.Status != metav1.ConditionTrue || !strings.Contains(c.Message, "a: cert-manager: pods crashlooping") {
		t.Fatalf("paused: %+v", c)
	}
	if m := f.status("b").Message; !strings.Contains(m, "paused after a failure on a") {
		t.Fatalf("b: %q", m)
	}

	// A new change resumes, starting again with a.
	s = f.set()
	s.Spec.Packages[0].Spec.Version = "~1.23"
	if err := f.hub.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	f.reconcile()
	if f.packages("a")["cert-manager"].Spec.Version != "~1.23" {
		t.Fatal("a new change did not resume the rollout")
	}
	if c := meta.FindStatusCondition(f.set().Status.Conditions, "RolloutPaused"); c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("still paused: %+v", c)
	}
}

func TestCanariesGoFirst(t *testing.T) {
	f := newFleet(t)
	cb := &v1beta1.Cluster{}
	if err := f.hub.Get(context.Background(), types.NamespacedName{Name: "b"}, cb); err != nil {
		t.Fatal(err)
	}
	cb.Labels["ring"] = "canary"
	if err := f.hub.Update(context.Background(), cb); err != nil {
		t.Fatal(err)
	}
	set := baseSet()
	set.Spec.Packages = set.Spec.Packages[:1]
	set.Spec.Rollout = &v1beta1.PackageSetRollout{Canary: &metav1.LabelSelector{MatchLabels: map[string]string{"ring": "canary"}}}
	if err := f.hub.Create(context.Background(), set); err != nil {
		t.Fatal(err)
	}
	f.reconcile()
	if len(f.packages("b")) != 1 || len(f.packages("a")) != 0 {
		t.Fatal("the canary did not go alone first")
	}
	if m := f.status("a").Message; m != "waiting for the canary clusters" {
		t.Fatalf("a: %q", m)
	}
	f.markReady("b", "cert-manager")
	f.reconcile()
	if len(f.packages("a")) != 1 {
		t.Fatal("a did not follow the canary")
	}
}

func TestAClusterMovesBetweenSetsWithoutReinstalling(t *testing.T) {
	for _, order := range []string{"old set first", "new set first"} {
		t.Run(order, func(t *testing.T) {
			f := newFleet(t)
			ctx := context.Background()
			base := baseSet()
			base.Spec.Packages = base.Spec.Packages[:1]
			edge := baseSet()
			edge.Name = "edge"
			edge.Spec.ClusterSelector = metav1.LabelSelector{MatchLabels: map[string]string{"env": "edge"}}
			edge.Spec.Packages = base.Spec.Packages[:1]
			edge.Spec.Packages[0].Spec.Version = "~1.22"
			for _, s := range []*v1beta1.PackageSet{base, edge} {
				if err := f.hub.Create(ctx, s); err != nil {
					t.Fatal(err)
				}
			}
			f.reconcile()
			before := f.packages("a")["cert-manager"]
			if before.Labels[v1beta1.LabelPackageSet] != "base" {
				t.Fatalf("setup: %+v", before.Labels)
			}

			ca := &v1beta1.Cluster{}
			if err := f.hub.Get(ctx, types.NamespacedName{Name: "a"}, ca); err != nil {
				t.Fatal(err)
			}
			ca.Labels["env"] = "edge"
			if err := f.hub.Update(ctx, ca); err != nil {
				t.Fatal(err)
			}
			edgeReq := ctrl.Request{NamespacedName: types.NamespacedName{Name: "edge"}}
			if order == "new set first" {
				if _, err := f.r.Reconcile(ctx, edgeReq); err != nil {
					t.Fatal(err)
				}
				// The new set takes the package at once: base no longer
				// wants it there, so there is no conflict to report.
				if got := f.packages("a")["cert-manager"].Labels[v1beta1.LabelPackageSet]; got != "edge" {
					t.Fatalf("the new set did not take the package over: owner %q", got)
				}
				f.reconcile()
			} else {
				f.reconcile()
				if _, err := f.r.Reconcile(ctx, edgeReq); err != nil {
					t.Fatal(err)
				}
			}
			for _, d := range f.members.deletes {
				if d == "a/cert-manager" {
					t.Fatal("the package was deleted while moving between sets, which uninstalls it")
				}
			}
			after, ok := f.packages("a")["cert-manager"]
			if !ok {
				t.Fatal("the package was removed while moving between sets")
			}
			if after.UID != before.UID || after.Labels[v1beta1.LabelPackageSet] != "edge" || after.Spec.Version != "~1.22" {
				t.Fatalf("after the move: uid %s->%s, labels %v, version %s", before.UID, after.UID, after.Labels, after.Spec.Version)
			}
		})
	}
}
