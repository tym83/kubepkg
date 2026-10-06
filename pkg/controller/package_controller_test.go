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
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/meta/testrestmapper"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/tym83/kubepkg/api/v1beta1"
	"github.com/tym83/kubepkg/pkg/backend"
)

// fakeBackend keeps Helm-like release histories in memory. A release fails
// when its component name is listed in failOn for the current chart digest.
type fakeBackend struct {
	releases map[string][]fakeRelease // key -> history, newest last
	failOn   map[string]bool          // component name -> fail on next apply
	calls    []string
	values   map[string]map[string]any // key -> values of the last apply
	adopted  []string                  // keys applied with Adopt
}

type fakeRelease struct {
	chart  string
	failed bool
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{releases: map[string][]fakeRelease{}, failOn: map[string]bool{}, values: map[string]map[string]any{}}
}

func (f *fakeBackend) Name() string { return "fake" }

func (f *fakeBackend) state(key string) backend.State {
	h := f.releases[key]
	if len(h) == 0 {
		return backend.State{}
	}
	last := h[len(h)-1]
	return backend.State{Exists: true, Revision: len(h), Ready: !last.failed, Failed: last.failed}
}

func (f *fakeBackend) Apply(_ context.Context, c backend.Component) (backend.State, error) {
	f.calls = append(f.calls, "apply "+c.Key()+" "+c.ChartDir)
	f.values[c.Key()] = c.Values
	if c.Adopt {
		f.adopted = append(f.adopted, c.Key())
	}
	failed := f.failOn[c.Name]
	f.releases[c.Key()] = append(f.releases[c.Key()], fakeRelease{chart: c.ChartDir, failed: failed})
	st := f.state(c.Key())
	if failed {
		st.Message = "pods crashlooping"
		return st, fmt.Errorf("upgrade %s: timed out", c.Key())
	}
	return st, nil
}

func (f *fakeBackend) Status(_ context.Context, c backend.Component) (backend.State, error) {
	return f.state(c.Key()), nil
}

func (f *fakeBackend) Rollback(_ context.Context, c backend.Component, to int) (backend.State, error) {
	h := f.releases[c.Key()]
	if to < 1 || to > len(h) {
		return backend.State{}, fmt.Errorf("no revision %d of %s", to, c.Key())
	}
	f.calls = append(f.calls, fmt.Sprintf("rollback %s to %d", c.Key(), to))
	f.releases[c.Key()] = append(h, fakeRelease{chart: h[to-1].chart})
	return f.state(c.Key()), nil
}

func (f *fakeBackend) Uninstall(_ context.Context, c backend.Component) error {
	f.calls = append(f.calls, "uninstall "+c.Key())
	delete(f.releases, c.Key())
	return nil
}

// chartOf reports which chart a release currently runs.
func (f *fakeBackend) chartOf(key string) string {
	h := f.releases[key]
	if len(h) == 0 {
		return ""
	}
	return h[len(h)-1].chart
}

// fakePreparer names the chart after the package version, so a version
// bump is a chart change.
type fakePreparer struct{}

func (fakePreparer) Prepare(_ context.Context, src *v1beta1.PackageSource, _ *v1beta1.Variant, comp *v1beta1.Component, c *backend.Component) (string, error) {
	c.ChartDir = comp.Name + "@" + sourceVersion(src)
	return "sha256:" + c.ChartDir, nil
}

type fakeAPIs map[string]bool

func (a fakeAPIs) Served(context.Context) (map[string]bool, error) { return a, nil }

type env struct {
	t  *testing.T
	c  client.Client
	be *fakeBackend
	r  *PackageReconciler
}

func newEnv(t *testing.T) *env {
	sch := runtime.NewScheme()
	if err := v1beta1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	// Kinds the tests treat as an operator's custom resources, next to the
	// scheme's own with their real scopes.
	custom := meta.NewDefaultRESTMapper(nil)
	custom.Add(schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "Widget"}, meta.RESTScopeNamespace)
	custom.Add(schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "Fleet"}, meta.RESTScopeRoot)
	ours := meta.NewDefaultRESTMapper(nil)
	for gvk := range sch.AllKnownTypes() {
		if gvk.Group == v1beta1.GroupName {
			ours.Add(gvk, meta.RESTScopeRoot)
		}
	}
	mapper := meta.MultiRESTMapper{testrestmapper.TestOnlyStaticRESTMapper(sch), ours, custom}
	c := fake.NewClientBuilder().WithScheme(sch).WithRESTMapper(mapper).
		WithStatusSubresource(&v1beta1.Package{}, &v1beta1.PackageRevision{}, &v1beta1.PackageSource{}, &v1beta1.Repository{}).
		Build()
	be := newFakeBackend()
	return &env{t: t, c: c, be: be, r: &PackageReconciler{
		Client: c, Profile: DefaultProfile(), Backend: be, Preparer: fakePreparer{}, APIs: fakeAPIs{},
	}}
}

func mkSource(name, version string, safe bool, comps ...string) *v1beta1.PackageSource {
	s := &v1beta1.PackageSource{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1beta1.PackageSourceSpec{Version: version}}
	if safe {
		s.Spec.Rollback = &v1beta1.RollbackPolicy{Safe: true}
	}
	v := v1beta1.Variant{Name: "default"}
	for _, c := range comps {
		v.Components = append(v.Components, v1beta1.Component{Name: c, Path: "x/" + c, Install: &v1beta1.ComponentInstall{Namespace: "ns-" + name}})
	}
	s.Spec.Variants = []v1beta1.Variant{v}
	return s
}

func (e *env) create(objs ...client.Object) {
	e.t.Helper()
	for _, o := range objs {
		if err := e.c.Create(context.Background(), o); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *env) reconcile(name string) ctrl.Result {
	e.t.Helper()
	res, err := e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	if err != nil {
		e.t.Fatalf("reconcile %s: %v", name, err)
	}
	return res
}

func (e *env) pkg(name string) *v1beta1.Package {
	e.t.Helper()
	p := &v1beta1.Package{}
	if err := e.c.Get(context.Background(), types.NamespacedName{Name: name}, p); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) setVersion(name, version string, safe bool) {
	e.t.Helper()
	s := &v1beta1.PackageSource{}
	if err := e.c.Get(context.Background(), types.NamespacedName{Name: name}, s); err != nil {
		e.t.Fatal(err)
	}
	s.Spec.Version = version
	s.Spec.Rollback = nil
	if safe {
		s.Spec.Rollback = &v1beta1.RollbackPolicy{Safe: true}
	}
	if err := e.c.Update(context.Background(), s); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) revs(name string) string {
	e.t.Helper()
	var list v1beta1.PackageRevisionList
	if err := e.c.List(context.Background(), &list, client.MatchingLabels{v1beta1.LabelPackage: name}); err != nil {
		e.t.Fatal(err)
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Spec.Revision < list.Items[j].Spec.Revision })
	var parts []string
	for _, r := range list.Items {
		s := fmt.Sprintf("%d:%s:%s", r.Spec.Revision, r.Spec.Version, r.Status.Phase)
		if r.Spec.RestoredFrom != 0 {
			s += fmt.Sprintf("<-%d", r.Spec.RestoredFrom)
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}

func ready(p *v1beta1.Package) (bool, string, string) {
	c := meta.FindStatusCondition(p.Status.Conditions, "Ready")
	if c == nil {
		return false, "", ""
	}
	return c.Status == metav1.ConditionTrue, c.Reason, c.Message
}

func TestInstallAndUpgrade(t *testing.T) {
	e := newEnv(t)
	e.create(mkSource("app", "1.0.0", true, "api", "web"), &v1beta1.Package{ObjectMeta: metav1.ObjectMeta{Name: "app"}})
	e.reconcile("app")
	if ok, reason, msg := ready(e.pkg("app")); !ok {
		t.Fatalf("not ready: %s %s", reason, msg)
	}
	if got := e.revs("app"); got != "1:1.0.0:Applied" {
		t.Fatalf("revisions = %s", got)
	}
	ns := &corev1.Namespace{}
	if err := e.c.Get(context.Background(), types.NamespacedName{Name: "ns-app"}, ns); err != nil {
		t.Fatalf("namespace not created: %v", err)
	}

	// Nothing changed: no new revision.
	e.reconcile("app")
	if got := e.revs("app"); got != "1:1.0.0:Applied" {
		t.Fatalf("a no-op reconcile made a revision: %s", got)
	}

	e.setVersion("app", "1.1.0", true)
	e.reconcile("app")
	if got := e.revs("app"); got != "1:1.0.0:Superseded 2:1.1.0:Applied" {
		t.Fatalf("revisions = %s", got)
	}
	if p := e.pkg("app"); p.Status.Version != "1.1.0" || p.Status.CurrentRevision != 2 {
		t.Fatalf("status = %+v", p.Status)
	}
}

func TestFailedUpgradeRollsBackWholePackage(t *testing.T) {
	e := newEnv(t)
	e.create(mkSource("app", "1.0.0", true, "api", "web"), &v1beta1.Package{ObjectMeta: metav1.ObjectMeta{Name: "app"}})
	e.reconcile("app")

	e.setVersion("app", "1.1.0", true)
	e.be.failOn["web"] = true // api upgrades fine, web fails
	e.reconcile("app")

	if got := e.revs("app"); got != "1:1.0.0:Superseded 2:1.1.0:Failed 3:1.0.0:Applied<-1" {
		t.Fatalf("revisions = %s", got)
	}
	// Both components are back on 1.0.0, not just the one that failed.
	if a, w := e.be.chartOf("ns-app/api"), e.be.chartOf("ns-app/web"); a != "api@1.0.0" || w != "web@1.0.0" {
		t.Fatalf("after rollback api=%s web=%s", a, w)
	}
	_, reason, _ := ready(e.pkg("app"))
	if reason != v1beta1.ReasonUpgradeRolledBack {
		t.Fatalf("reason = %s", reason)
	}

	// Held: reconciling again must not retry the same failing upgrade.
	e.be.failOn["web"] = false
	calls := len(e.be.calls)
	e.reconcile("app")
	if len(e.be.calls) != calls {
		t.Fatalf("a held package was applied again: %v", e.be.calls[calls:])
	}

	// The retry annotation releases the hold.
	p := e.pkg("app")
	p.Annotations = map[string]string{AnnotationRetry: "1"}
	if err := e.c.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	e.reconcile("app")
	if ok, reason, msg := ready(e.pkg("app")); !ok {
		t.Fatalf("retry did not succeed: %s %s", reason, msg)
	}
	if !strings.HasSuffix(e.revs("app"), "4:1.1.0:Applied") {
		t.Fatalf("revisions = %s", e.revs("app"))
	}
}

func TestUnsafeVersionIsNotRolledBack(t *testing.T) {
	e := newEnv(t)
	e.create(mkSource("db", "1.0.0", true, "db"), &v1beta1.Package{ObjectMeta: metav1.ObjectMeta{Name: "db"}})
	e.reconcile("db")

	e.setVersion("db", "2.0.0", false) // runs a schema migration
	e.be.failOn["db"] = true
	e.reconcile("db")

	if got := e.revs("db"); got != "1:1.0.0:Superseded 2:2.0.0:Failed" {
		t.Fatalf("revisions = %s", got)
	}
	for _, c := range e.be.calls {
		if strings.HasPrefix(c, "rollback") {
			t.Fatalf("an unsafe version was rolled back: %v", e.be.calls)
		}
	}
	_, reason, msg := ready(e.pkg("db"))
	if reason != v1beta1.ReasonUpgradeFailed || !strings.Contains(msg, "fix forward") {
		t.Fatalf("condition = %s %s", reason, msg)
	}
}

func TestRequirementsGateInstall(t *testing.T) {
	e := newEnv(t)
	app := mkSource("app", "1.0.0", false, "app")
	app.Spec.Variants[0].Requires = []v1beta1.Requirement{{Package: "cert-manager", Version: ">=1.16"}, {Capability: "api:monitoring.coreos.com/v1", Optional: true}}
	e.create(app, &v1beta1.Package{ObjectMeta: metav1.ObjectMeta{Name: "app"}})

	res := e.reconcile("app")
	_, reason, msg := ready(e.pkg("app"))
	if reason != v1beta1.ReasonRequirementsNotMet || !strings.Contains(msg, "cert-manager") || res.RequeueAfter == 0 {
		t.Fatalf("condition = %s %s, requeue %v", reason, msg, res.RequeueAfter)
	}
	if len(e.be.calls) != 0 {
		t.Fatal("installed before its requirement")
	}

	e.create(mkSource("cert-manager", "1.15.0", false, "cm"), &v1beta1.Package{ObjectMeta: metav1.ObjectMeta{Name: "cert-manager"}})
	e.reconcile("cert-manager")
	e.reconcile("app")
	if _, _, msg := ready(e.pkg("app")); !strings.Contains(msg, "does not satisfy >=1.16") {
		t.Fatalf("too old a dependency must block: %s", msg)
	}

	e.setVersion("cert-manager", "1.16.2", false)
	e.reconcile("cert-manager")
	e.reconcile("app")
	if ok, reason, msg := ready(e.pkg("app")); !ok {
		t.Fatalf("not ready: %s %s", reason, msg)
	}
	if !e.pkg("app").Status.Dependencies["cert-manager"].Ready {
		t.Fatal("status.dependencies must report cert-manager as ready")
	}
}

func TestConflictAndVersionMismatch(t *testing.T) {
	e := newEnv(t)
	e.create(mkSource("traefik", "3.0.0", false, "t"), &v1beta1.Package{ObjectMeta: metav1.ObjectMeta{Name: "traefik"}})
	e.reconcile("traefik")
	nginx := mkSource("nginx", "4.0.0", false, "n")
	nginx.Spec.Conflicts = []string{"traefik"}
	e.create(nginx, &v1beta1.Package{ObjectMeta: metav1.ObjectMeta{Name: "nginx"}})
	e.reconcile("nginx")
	if _, reason, _ := ready(e.pkg("nginx")); reason != v1beta1.ReasonConflict {
		t.Fatalf("reason = %s", reason)
	}

	e.create(mkSource("pinned", "2.0.0", false, "p"), &v1beta1.Package{ObjectMeta: metav1.ObjectMeta{Name: "pinned"}, Spec: v1beta1.PackageSpec{Version: "~1.4"}})
	e.reconcile("pinned")
	if _, reason, _ := ready(e.pkg("pinned")); reason != v1beta1.ReasonVersionMismatch {
		t.Fatalf("reason = %s", reason)
	}
}

func TestRollbackAnnotationAndHold(t *testing.T) {
	e := newEnv(t)
	e.create(mkSource("app", "1.0.0", true, "app"), &v1beta1.Package{ObjectMeta: metav1.ObjectMeta{Name: "app"}})
	e.reconcile("app")
	e.setVersion("app", "1.1.0", true)
	e.reconcile("app")

	p := e.pkg("app")
	p.Annotations = map[string]string{AnnotationRollbackTo: "1"}
	if err := e.c.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	e.reconcile("app")
	if got := e.revs("app"); got != "1:1.0.0:Superseded 2:1.1.0:Superseded 3:1.0.0:Applied<-1" {
		t.Fatalf("revisions = %s", got)
	}
	if e.be.chartOf("ns-app/app") != "app@1.0.0" {
		t.Fatalf("chart = %s", e.be.chartOf("ns-app/app"))
	}
	if _, ok := e.pkg("app").Annotations[AnnotationRollbackTo]; ok {
		t.Fatal("the rollback annotation must be cleared")
	}
	// Desired state still says 1.1.0, but nothing changed since the
	// rollback, so the operator must not upgrade straight back.
	e.reconcile("app")
	if e.be.chartOf("ns-app/app") != "app@1.0.0" {
		t.Fatal("the operator undid a manual rollback")
	}
	if c := meta.FindStatusCondition(e.pkg("app").Status.Conditions, "Ready"); c == nil || c.Status != metav1.ConditionTrue || c.Reason != v1beta1.ReasonRolledBack {
		t.Fatalf("a requested rollback that runs is ready: %+v", c)
	}
	// A real change to the desired state releases the hold.
	e.setVersion("app", "1.2.0", true)
	e.reconcile("app")
	if e.be.chartOf("ns-app/app") != "app@1.2.0" {
		t.Fatalf("chart = %s", e.be.chartOf("ns-app/app"))
	}
}

func TestRemovedComponentIsUninstalledAndFinalize(t *testing.T) {
	e := newEnv(t)
	e.create(mkSource("app", "1.0.0", true, "api", "worker"), &v1beta1.Package{ObjectMeta: metav1.ObjectMeta{Name: "app"}})
	e.reconcile("app")

	off := false
	p := e.pkg("app")
	p.Spec.Components = map[string]v1beta1.PackageComponent{"worker": {Enabled: &off}}
	if err := e.c.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	e.reconcile("app")
	if _, ok := e.be.releases["ns-app/worker"]; ok {
		t.Fatal("a disabled component must be uninstalled")
	}

	if err := e.c.Delete(context.Background(), e.pkg("app")); err != nil {
		t.Fatal(err)
	}
	e.reconcile("app")
	if len(e.be.releases) != 0 {
		t.Fatalf("releases left after delete: %v", e.be.releases)
	}
}

func TestTopoOrderAndCycle(t *testing.T) {
	mk := func(name string, deps ...string) v1beta1.Component {
		return v1beta1.Component{Name: name, Install: &v1beta1.ComponentInstall{Namespace: "x", DependsOn: deps}}
	}
	order, err := topoOrder([]v1beta1.Component{mk("web", "api"), mk("api", "db"), mk("db")})
	if err != nil || strings.Join(order, ",") != "db,api,web" {
		t.Fatalf("order = %v, %v", order, err)
	}
	if _, err := topoOrder([]v1beta1.Component{mk("a", "b"), mk("b", "a")}); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle not reported: %v", err)
	}
}

func TestMetaPackageIsReadyWhenItsMembersAre(t *testing.T) {
	e := newEnv(t)
	meta := mkSource("distro", "1.0.0", false)
	meta.Spec.Variants[0].Requires = []v1beta1.Requirement{{Package: "cert-manager", Version: "~1.16"}}
	e.create(meta, &v1beta1.Package{ObjectMeta: metav1.ObjectMeta{Name: "distro"}})
	e.reconcile("distro")
	if ok, r, _ := ready(e.pkg("distro")); ok || r != v1beta1.ReasonRequirementsNotMet {
		t.Fatalf("a meta package must wait for its members, got ready=%v %s", ok, r)
	}
	e.create(mkSource("cert-manager", "1.16.2", true, "controller"), &v1beta1.Package{ObjectMeta: metav1.ObjectMeta{Name: "cert-manager"}})
	e.reconcile("cert-manager")
	e.reconcile("distro")
	if ok, r, msg := ready(e.pkg("distro")); !ok {
		t.Fatalf("meta package not ready with its members installed: %s %s", r, msg)
	}
	for _, c := range e.be.calls {
		if strings.Contains(c, "ns-distro") {
			t.Fatalf("a meta package installed something itself: %v", e.be.calls)
		}
	}
}

// slowBackend is asynchronous: a release reports ready only once its
// component is listed in ready.
type slowBackend struct {
	*fakeBackend
	ready map[string]bool
}

func (s *slowBackend) Apply(ctx context.Context, c backend.Component) (backend.State, error) {
	st, err := s.fakeBackend.Apply(ctx, c)
	st.Ready, st.Progressing = s.ready[c.Name], !s.ready[c.Name]
	return st, err
}

func (s *slowBackend) Status(ctx context.Context, c backend.Component) (backend.State, error) {
	st, err := s.fakeBackend.Status(ctx, c)
	st.Ready, st.Progressing = s.ready[c.Name], !s.ready[c.Name]
	return st, err
}

func TestAsyncComponentsGoInDependencyOrder(t *testing.T) {
	e := newEnv(t)
	slow := &slowBackend{fakeBackend: e.be, ready: map[string]bool{}}
	e.r.Backend = slow
	src := mkSource("virt", "1.0.0", false, "operator", "cr")
	src.Spec.Variants[0].Components[1].Install.DependsOn = []string{"operator"}
	e.create(src, &v1beta1.Package{ObjectMeta: metav1.ObjectMeta{Name: "virt"}})

	e.reconcile("virt")
	e.reconcile("virt")
	for _, c := range e.be.calls {
		if strings.Contains(c, "/cr ") {
			t.Fatalf("cr applied before operator was ready: %v", e.be.calls)
		}
	}
	if ok, r, _ := ready(e.pkg("virt")); ok || r != v1beta1.ReasonProgressing {
		t.Fatalf("want Progressing, got ready=%v %s", ok, r)
	}

	slow.ready["operator"] = true
	e.reconcile("virt")
	slow.ready["cr"] = true
	e.reconcile("virt")
	if ok, r, msg := ready(e.pkg("virt")); !ok {
		t.Fatalf("not ready once both components are: %s %s", r, msg)
	}
}

func widget(ns, name, available string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("example.org/v1")
	u.SetKind("Widget")
	u.SetNamespace(ns)
	u.SetName(name)
	if available != "" {
		_ = unstructured.SetNestedSlice(u.Object, []any{map[string]any{"type": "Available", "status": available}}, "status", "conditions")
	}
	return u
}

func TestReadyWhenWaitsForTheResourcesCondition(t *testing.T) {
	e := newEnv(t)
	src := mkSource("virt", "1.0.0", true, "operator")
	src.Spec.Variants[0].Components[0].Install.ReadyWhen = []v1beta1.ReadyCondition{{APIVersion: "example.org/v1", Kind: "Widget", Name: "main", Condition: "Available"}}
	e.create(src, &v1beta1.Package{ObjectMeta: metav1.ObjectMeta{Name: "virt"}})

	e.reconcile("virt")
	e.reconcile("virt")
	if ok, r, msg := ready(e.pkg("virt")); ok || r != v1beta1.ReasonProgressing || !strings.Contains(msg, "not created yet") {
		t.Fatalf("before the resource exists: %v %s %q", ok, r, msg)
	}

	w := widget("ns-virt", "main", "False")
	if err := e.c.Create(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	e.reconcile("virt")
	if ok, _, msg := ready(e.pkg("virt")); ok || !strings.Contains(msg, "Widget main condition Available=True") {
		t.Fatalf("while the condition is False: %v %q", ok, msg)
	}

	_ = unstructured.SetNestedSlice(w.Object, []any{map[string]any{"type": "Available", "status": "True"}}, "status", "conditions")
	if err := e.c.Update(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	e.reconcile("virt")
	if ok, r, msg := ready(e.pkg("virt")); !ok {
		t.Fatalf("once Available: %s %q", r, msg)
	}
}

func TestReadyWhenClusterScopedAndUnservedKinds(t *testing.T) {
	e := newEnv(t)
	src := mkSource("fleet", "1.0.0", true, "operator")
	src.Spec.Variants[0].Components[0].Install.ReadyWhen = []v1beta1.ReadyCondition{
		{APIVersion: "example.org/v1", Kind: "Fleet", Name: "main", Condition: "Ready"},
	}
	e.create(src, &v1beta1.Package{ObjectMeta: metav1.ObjectMeta{Name: "fleet"}})
	f := &unstructured.Unstructured{}
	f.SetAPIVersion("example.org/v1")
	f.SetKind("Fleet")
	f.SetName("main")
	_ = unstructured.SetNestedSlice(f.Object, []any{map[string]any{"type": "Ready", "status": "True"}}, "status", "conditions")
	if err := e.c.Create(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	e.reconcile("fleet")
	e.reconcile("fleet")
	if ok, r, msg := ready(e.pkg("fleet")); !ok {
		t.Fatalf("a cluster-scoped resource must be found without a namespace: %s %q", r, msg)
	}

	src2 := mkSource("later", "1.0.0", true, "operator")
	src2.Spec.Variants[0].Components[0].Install.ReadyWhen = []v1beta1.ReadyCondition{{APIVersion: "nothing.example.org/v1", Kind: "Ghost", Name: "x", Condition: "Ready"}}
	e.create(src2, &v1beta1.Package{ObjectMeta: metav1.ObjectMeta{Name: "later"}})
	e.reconcile("later")
	e.reconcile("later")
	if ok, _, msg := ready(e.pkg("later")); ok || !strings.Contains(msg, "not served yet") {
		t.Fatalf("a kind whose CRD is not installed yet: %v %q", ok, msg)
	}
}

func TestMetricsFollowRevisions(t *testing.T) {
	e := newEnv(t)
	e.create(mkSource("metered", "1.0.0", true, "api", "web"), &v1beta1.Package{ObjectMeta: metav1.ObjectMeta{Name: "metered"}})
	e.reconcile("metered")
	if v := testutil.ToFloat64(packageReady.WithLabelValues("metered")); v != 1 {
		t.Fatalf("ready after install = %v", v)
	}
	if v := testutil.ToFloat64(packageInfo.WithLabelValues("metered", "1.0.0", "1")); v != 1 {
		t.Fatalf("info after install = %v", v)
	}

	e.setVersion("metered", "1.1.0", true)
	e.be.failOn["web"] = true
	e.reconcile("metered")
	for outcome, want := range map[string]float64{outcomeApplied: 1, outcomeFailed: 1, outcomeRolledBack: 1} {
		if v := testutil.ToFloat64(revisionsTotal.WithLabelValues("metered", outcome)); v != want {
			t.Errorf("revisions %s = %v, want %v", outcome, v, want)
		}
	}
	if v := testutil.ToFloat64(packageReady.WithLabelValues("metered")); v != 0 {
		t.Errorf("ready after a rolled back upgrade = %v", v)
	}
	if n := testutil.CollectAndCount(packageInfo); n == 0 {
		t.Error("info series missing")
	}

	forgetPackage("metered")
	// Nothing left to delete means forgetting took every series of it.
	if n := revisionsTotal.DeletePartialMatch(prometheus.Labels{"package": "metered"}); n != 0 {
		t.Errorf("revision series of a removed package stay: %d", n)
	}
	if n := packageInfo.DeletePartialMatch(prometheus.Labels{"package": "metered"}); n != 0 {
		t.Errorf("info series of a removed package stay: %d", n)
	}
}

func hookedSource(version string) *v1beta1.PackageSource {
	src := mkSource("db", version, false, "migrate", "server")
	src.Spec.Variants[0].Components[0].Install.Phase = v1beta1.PhasePreUpgrade
	return src
}

func callsMatching(calls []string, sub string) []int {
	var out []int
	for i, c := range calls {
		if strings.Contains(c, sub) {
			out = append(out, i)
		}
	}
	return out
}

func TestPreUpgradeHookRunsFirstOnlyOnUpgrades(t *testing.T) {
	e := newEnv(t)
	e.create(hookedSource("1.0.0"), &v1beta1.Package{ObjectMeta: metav1.ObjectMeta{Name: "db"}})
	e.reconcile("db")
	if ok, r, msg := ready(e.pkg("db")); !ok {
		t.Fatalf("install: %s %q", r, msg)
	}
	if n := callsMatching(e.be.calls, "/migrate"); len(n) != 0 {
		t.Fatalf("the hook ran on install: %v", e.be.calls)
	}

	e.be.calls = nil
	e.setVersion("db", "2.0.0", false)
	e.reconcile("db")
	if ok, r, msg := ready(e.pkg("db")); !ok {
		t.Fatalf("upgrade: %s %q", r, msg)
	}
	hook, server, gone := callsMatching(e.be.calls, "apply ns-db/migrate"), callsMatching(e.be.calls, "apply ns-db/server"), callsMatching(e.be.calls, "uninstall ns-db/migrate")
	if len(hook) != 1 || len(server) != 1 || len(gone) != 1 || !(hook[0] < server[0] && server[0] < gone[0]) {
		t.Fatalf("want hook, then server, then the hook removed: %v", e.be.calls)
	}
	kp, _ := e.be.values["ns-db/migrate"]["kubepkg"].(map[string]any)
	if kp["fromVersion"] != "1.0.0" || kp["toVersion"] != "2.0.0" {
		t.Fatalf("hook values: %v", e.be.values["ns-db/migrate"])
	}
	if e.be.chartOf("ns-db/migrate") != "" {
		t.Fatal("the hook release stays after the upgrade")
	}
	for _, rev := range e.revisionsOf("db") {
		for _, c := range rev.Spec.Components {
			if c.Name == "migrate" {
				t.Fatal("a hook must not be part of a revision snapshot")
			}
		}
	}

	// A change within the version does not run it.
	e.be.calls = nil
	pkg := e.pkg("db")
	pkg.Spec.Components = map[string]v1beta1.PackageComponent{"server": {Values: &apiextensionsv1.JSON{Raw: []byte(`{"replicas":3}`)}}}
	if err := e.c.Update(context.Background(), pkg); err != nil {
		t.Fatal(err)
	}
	e.reconcile("db")
	if n := callsMatching(e.be.calls, "/migrate"); len(n) != 0 {
		t.Fatalf("the hook ran without a version change: %v", e.be.calls)
	}
}

func TestFailedHookStopsTheUpgrade(t *testing.T) {
	e := newEnv(t)
	e.create(hookedSource("1.0.0"), &v1beta1.Package{ObjectMeta: metav1.ObjectMeta{Name: "db"}})
	e.reconcile("db")
	e.setVersion("db", "2.0.0", false)
	e.be.failOn["migrate"] = true
	e.reconcile("db")
	if got := e.be.chartOf("ns-db/server"); got != "server@1.0.0" {
		t.Fatalf("server changed although the migration failed: %s", got)
	}
	if e.be.chartOf("ns-db/migrate") != "" {
		t.Fatal("a failed hook release stays")
	}
	if ok, _, msg := ready(e.pkg("db")); ok || !strings.Contains(msg, "pre-upgrade hook migrate failed") {
		t.Fatalf("status: %v %q", ok, msg)
	}
}

func TestDependingOnAHookIsRefused(t *testing.T) {
	e := newEnv(t)
	src := hookedSource("1.0.0")
	src.Spec.Variants[0].Components[1].Install.DependsOn = []string{"migrate"}
	e.create(src, &v1beta1.Package{ObjectMeta: metav1.ObjectMeta{Name: "db"}})
	res, err := e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "db"}})
	_ = res
	msg := ""
	if err != nil {
		msg = err.Error()
	} else {
		_, _, msg = ready(e.pkg("db"))
	}
	if !strings.Contains(msg, "pre-upgrade hook") {
		t.Fatalf("got %q", msg)
	}
}

func (e *env) revisionsOf(name string) []v1beta1.PackageRevision {
	e.t.Helper()
	revs, err := e.r.revisions(context.Background(), name)
	if err != nil {
		e.t.Fatal(err)
	}
	return revs
}

func TestAdoptTakesOverOnceInTheReleaseItNames(t *testing.T) {
	e := newEnv(t)
	pkg := &v1beta1.Package{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Annotations: map[string]string{AnnotationAdopt: "true"}},
		Spec: v1beta1.PackageSpec{Components: map[string]v1beta1.PackageComponent{
			"web": {ReleaseName: "legacy-web", Namespace: "legacy"},
		}},
	}
	e.create(mkSource("app", "1.0.0", true, "db", "web"), pkg)
	e.reconcile("app")
	e.reconcile("app")
	if e.be.chartOf("legacy/legacy-web") != "web@1.0.0" || e.be.chartOf("ns-app/db") != "db@1.0.0" {
		t.Fatalf("releases: %v", e.be.calls)
	}
	if len(e.be.adopted) != 2 {
		t.Fatalf("adopted: %v", e.be.adopted)
	}
	if _, ok := e.pkg("app").Annotations[AnnotationAdopt]; ok {
		t.Fatal("the adopt annotation stays after the revision was applied")
	}
	// Later changes never take over anything.
	e.setVersion("app", "1.1.0", true)
	e.reconcile("app")
	e.reconcile("app")
	if len(e.be.adopted) != 2 || e.be.chartOf("legacy/legacy-web") != "web@1.1.0" {
		t.Fatalf("after an upgrade: adopted %v, calls %v", e.be.adopted, e.be.calls)
	}
}

func TestAComponentThatMovesLeavesItsOldPlaceFirst(t *testing.T) {
	e := newEnv(t)
	e.create(mkSource("logs", "1.0.0", true, "agent"), &v1beta1.Package{ObjectMeta: metav1.ObjectMeta{Name: "logs"}})
	e.reconcile("logs")
	e.reconcile("logs")

	p := e.pkg("logs")
	p.Spec.Components = map[string]v1beta1.PackageComponent{"agent": {Namespace: "logging-agent"}}
	if err := e.c.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	e.be.calls = nil
	e.reconcile("logs")
	e.reconcile("logs")
	if len(e.be.calls) < 2 || e.be.calls[0] != "uninstall ns-logs/agent" || !strings.HasPrefix(e.be.calls[1], "apply logging-agent/agent ") {
		t.Fatalf("calls: %v", e.be.calls)
	}
	rev := &v1beta1.PackageRevision{}
	if err := e.c.Get(context.Background(), types.NamespacedName{Name: "logs-2"}, rev); err != nil {
		t.Fatal(err)
	}
	if rev.Spec.RollbackSafe {
		t.Fatal("a revision that moved a component claims it can be rolled back")
	}

	// Deleting the package takes away every release it ever made.
	e.be.releases["ns-logs/agent"] = []fakeRelease{{chart: "left behind"}}
	if err := e.c.Delete(context.Background(), e.pkg("logs")); err != nil {
		t.Fatal(err)
	}
	e.reconcile("logs")
	if len(e.be.releases) != 0 {
		t.Fatalf("left behind: %v", e.be.releases)
	}
}
