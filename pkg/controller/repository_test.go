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
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/kuberoot-dev/kubepkg/api/v1"
	"github.com/kuberoot-dev/kubepkg/pkg/repo"
)

// fakeIndexes serves index bytes by URL under the test:// scheme.
type fakeIndexes map[string][]byte

func (f fakeIndexes) FetchIndex(_ context.Context, u string) ([]byte, error) {
	raw, ok := f[u]
	if !ok {
		return nil, errors.New("connection refused")
	}
	return raw, nil
}

func indexOf(t *testing.T, versions ...string) []byte {
	t.Helper()
	idx := &repo.Index{APIVersion: "kubepkg.dev/v1alpha1", Kind: repo.IndexKind, Packages: map[string]repo.Package{}}
	p := repo.Package{}
	for _, v := range versions {
		spec := mkSource("app", v, true, "web").Spec
		d, err := repo.SpecDigest(spec)
		if err != nil {
			t.Fatal(err)
		}
		p.Versions = append(p.Versions, repo.Version{Version: v, Digest: d, Spec: spec})
	}
	idx.Packages["app"] = p
	var buf bytes.Buffer
	if err := idx.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type repoEnv struct {
	*env
	indexes fakeIndexes
	rr      *RepositoryReconciler
}

func newRepoEnv(t *testing.T) *repoEnv {
	e := newEnv(t)
	idx := fakeIndexes{}
	repos := &Repositories{Store: repo.NewStore(), Fetchers: repo.Fetchers{"test": idx}, Policy: repo.AllowAll{}}
	e.r.Repositories = repos
	return &repoEnv{env: e, indexes: idx, rr: &RepositoryReconciler{Client: e.c, Repositories: repos}}
}

func (e *repoEnv) fetch(name string) ctrl.Result {
	e.t.Helper()
	res, err := e.rr.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

func (e *repoEnv) source(name string) *v1.PackageSource {
	e.t.Helper()
	s := &v1.PackageSource{}
	if err := e.c.Get(context.Background(), types.NamespacedName{Name: name}, s); err != nil {
		e.t.Fatal(err)
	}
	return s
}

func repository(name, url string, priority int32) *v1.Repository {
	return &v1.Repository{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1.RepositorySpec{URL: url, Priority: priority, AllowUnsigned: true}}
}

func reason(p *v1.Package) (string, string) {
	c := meta.FindStatusCondition(p.Status.Conditions, "Ready")
	if c == nil {
		return "", ""
	}
	return c.Reason, c.Message
}

func TestInstallFromRepositoryAndFollowConstraint(t *testing.T) {
	e := newRepoEnv(t)
	e.indexes["test://main"] = indexOf(t, "1.0.0", "1.1.0")
	e.create(repository("main", "test://main", 0), &v1.Package{ObjectMeta: metav1.ObjectMeta{Name: "app"}, Spec: v1.PackageSpec{Version: "~1.0"}})

	e.fetch("main")
	e.reconcile("app")
	src := e.source("app")
	if src.Spec.Version != "1.0.0" || src.Labels[v1.LabelRepository] != "main" || len(src.OwnerReferences) != 1 {
		t.Fatalf("source from repository: version %s labels %v owners %v", src.Spec.Version, src.Labels, src.OwnerReferences)
	}
	e.reconcile("app")
	if ok, r, msg := ready(e.pkg("app")); !ok {
		t.Fatalf("not installed: %s %s", r, msg)
	}
	if got := e.be.chartOf("ns-app/web"); got != "web@1.0.0" {
		t.Fatalf("installed %q", got)
	}

	// A new patch release in the index is picked up; 1.1.0 stays out.
	e.indexes["test://main"] = indexOf(t, "1.0.0", "1.0.1", "1.1.0")
	e.fetch("main")
	e.reconcile("app")
	e.reconcile("app")
	if got := e.be.chartOf("ns-app/web"); got != "web@1.0.1" {
		t.Fatalf("after the index gained 1.0.1, installed %q", got)
	}
}

func TestHandWrittenSourceWins(t *testing.T) {
	e := newRepoEnv(t)
	e.indexes["test://main"] = indexOf(t, "2.0.0")
	e.create(repository("main", "test://main", 0), mkSource("app", "1.0.0", true, "web"), &v1.Package{ObjectMeta: metav1.ObjectMeta{Name: "app"}})
	e.fetch("main")
	e.reconcile("app")
	if v := e.source("app").Spec.Version; v != "1.0.0" {
		t.Fatalf("hand-written source replaced by %s", v)
	}
}

func TestWaitsForRepositoriesAndReportsMissingVersions(t *testing.T) {
	e := newRepoEnv(t)
	e.indexes["test://main"] = indexOf(t, "1.0.0")
	e.create(repository("main", "test://main", 0), repository("down", "test://down", 10),
		&v1.Package{ObjectMeta: metav1.ObjectMeta{Name: "app"}, Spec: v1.PackageSpec{Version: ">=2"}})

	e.fetch("main")
	if res := e.reconcile("app"); res.RequeueAfter == 0 {
		t.Fatal("selection did not wait for the repository not fetched yet")
	}
	if r, msg := reason(e.pkg("app")); r != ReasonVersionNotAvailable || !strings.Contains(msg, "waiting") {
		t.Fatalf("got %s %q", r, msg)
	}

	e.fetch("down")
	rp := &v1.Repository{}
	if err := e.c.Get(context.Background(), types.NamespacedName{Name: "down"}, rp); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(rp.Status.Conditions, "Ready"); c == nil || c.Reason != "FetchFailed" {
		t.Fatalf("repository status: %+v", rp.Status.Conditions)
	}
	e.reconcile("app")
	if r, msg := reason(e.pkg("app")); r != ReasonVersionNotAvailable || !strings.Contains(msg, "matching >=2") {
		t.Fatalf("got %s %q", r, msg)
	}
}

func TestFailedRefreshKeepsTheLoadedIndex(t *testing.T) {
	e := newRepoEnv(t)
	e.indexes["test://main"] = indexOf(t, "1.0.0")
	e.create(repository("main", "test://main", 0), &v1.Package{ObjectMeta: metav1.ObjectMeta{Name: "app"}})
	e.fetch("main")
	e.indexes["test://main"] = []byte("not an index")
	if res := e.fetch("main"); res.RequeueAfter > repositoryRetry {
		t.Fatalf("retry after %s", res.RequeueAfter)
	}
	e.reconcile("app")
	if v := e.source("app").Spec.Version; v != "1.0.0" {
		t.Fatalf("got %s", v)
	}
}

func signedIndex(t *testing.T, priv []byte, generated time.Time, versions ...string) ([]byte, []byte) {
	t.Helper()
	idx, err := repo.Parse(indexOf(t, versions...))
	if err != nil {
		t.Fatal(err)
	}
	g := metav1.NewTime(generated)
	idx.Generated = &g
	var buf bytes.Buffer
	if err := idx.Write(&buf); err != nil {
		t.Fatal(err)
	}
	sig, err := repo.Sign(buf.Bytes(), priv)
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), sig
}

func TestSignedRepositories(t *testing.T) {
	e := newRepoEnv(t)
	priv, pub, err := repo.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	rp := repository("main", "test://main", 0)
	rp.Spec.PublicKeys = []string{string(pub)}
	e.create(rp, &v1.Package{ObjectMeta: metav1.ObjectMeta{Name: "app"}})
	status := func() (string, string) {
		t.Helper()
		got := &v1.Repository{}
		if err := e.c.Get(context.Background(), types.NamespacedName{Name: "main"}, got); err != nil {
			t.Fatal(err)
		}
		c := meta.FindStatusCondition(got.Status.Conditions, "Ready")
		return c.Reason, c.Message
	}

	// Unsigned: refused.
	e.indexes["test://main"] = indexOf(t, "1.0.0")
	e.fetch("main")
	if r, _ := status(); r != "IndexRefused" {
		t.Fatalf("unsigned index: %s", r)
	}

	now := time.Now().UTC().Truncate(time.Second)
	raw, sig := signedIndex(t, priv, now, "1.0.0", "1.1.0")
	e.indexes["test://main"], e.indexes["test://main.sig"] = raw, sig
	e.fetch("main")
	if r, msg := status(); r != "IndexLoaded" {
		t.Fatalf("signed index: %s %s", r, msg)
	}

	// An older index, validly signed, is a rollback.
	raw, sig = signedIndex(t, priv, now.Add(-time.Hour), "1.0.0")
	e.indexes["test://main"], e.indexes["test://main.sig"] = raw, sig
	e.fetch("main")
	if r, msg := status(); r != "IndexRefused" || !strings.Contains(msg, "rollback") {
		t.Fatalf("older index: %s %s", r, msg)
	}
	e.reconcile("app")
	if v := e.source("app").Spec.Version; v != "1.1.0" {
		t.Fatalf("the newer accepted index must stay in use, got %s", v)
	}
}
