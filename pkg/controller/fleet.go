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
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/kuberoot-dev/kubepkg/api/v1"
)

const (
	// FinalizerPackageSet removes what a set wrote before it goes away.
	FinalizerPackageSet = "kubepkg.dev/package-set-cleanup"
	fleetRequeue        = 30 * time.Second
)

// Member is an open connection to a member cluster.
type Member struct {
	Client client.Client
	Config *rest.Config
}

// MemberClients opens connections to member clusters.
type MemberClients interface {
	For(ctx context.Context, c *v1.Cluster) (*Member, error)
}

// KubeconfigClients reads members' kubeconfigs from Secrets in the hub and
// keeps one client per Secret version.
type KubeconfigClients struct {
	Hub   client.Reader
	Group string

	mu    sync.Mutex
	cache map[string]cachedMember
}

type cachedMember struct {
	version string
	member  *Member
}

// For implements MemberClients.
func (k *KubeconfigClients) For(ctx context.Context, c *v1.Cluster) (*Member, error) {
	ref := c.Spec.KubeconfigSecretRef
	key := ref.Key
	if key == "" {
		key = "kubeconfig"
	}
	sec := &corev1.Secret{}
	if err := k.Hub.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, sec); err != nil {
		return nil, fmt.Errorf("kubeconfig secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if m, ok := k.cache[c.Name]; ok && m.version == sec.ResourceVersion {
		return m.member, nil
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(sec.Data[key])
	if err != nil {
		return nil, fmt.Errorf("kubeconfig in %s/%s key %s: %w", ref.Namespace, ref.Name, key, err)
	}
	cfg.Timeout = 30 * time.Second
	scheme := runtime.NewScheme()
	if err := v1.AddToSchemeForGroup(k.Group)(scheme); err != nil {
		return nil, err
	}
	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, err
	}
	if k.cache == nil {
		k.cache = map[string]cachedMember{}
	}
	m := &Member{Client: cl, Config: cfg}
	k.cache[c.Name] = cachedMember{version: sec.ResourceVersion, member: m}
	return m, nil
}

// ClusterReconciler checks that member clusters can be reached and run
// kubepkg.
type ClusterReconciler struct {
	client.Client
	Members MemberClients
}

// Reconcile checks one member.
func (r *ClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	c := &v1.Cluster{}
	if err := r.Get(ctx, req.NamespacedName, c); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	status, reason, msg := metav1.ConditionTrue, "Reachable", "kubepkg is installed"
	m, err := r.Members.For(ctx, c)
	if err == nil {
		err = probe(ctx, m)
	}
	if err != nil {
		status, reason, msg = metav1.ConditionFalse, "Unreachable", err.Error()
	} else if m.Config != nil {
		if dc, derr := discovery.NewDiscoveryClientForConfig(m.Config); derr == nil {
			if v, verr := dc.ServerVersion(); verr == nil {
				c.Status.KubernetesVersion = v.GitVersion
			}
		}
	}
	meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{Type: "Ready", Status: status, Reason: reason, Message: msg, ObservedGeneration: c.Generation})
	if err := r.Status().Update(ctx, c); err != nil && !apierrors.IsConflict(err) {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: time.Minute}, nil
}

// probe checks that the member serves the kubepkg API.
func probe(ctx context.Context, m *Member) error {
	var pkgs v1.PackageList
	if err := m.Client.List(ctx, &pkgs, client.Limit(1)); err != nil {
		if meta.IsNoMatchError(err) {
			return fmt.Errorf("kubepkg is not installed in the cluster")
		}
		return err
	}
	return nil
}

// SetupWithManager registers the controller.
func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("kubepkg-cluster").For(&v1.Cluster{}).Complete(r)
}

// PackageSetReconciler writes a set's repositories and packages to the
// clusters it selects, paced by its rollout, and reports how far each got.
type PackageSetReconciler struct {
	client.Client
	Members MemberClients
}

// memberState is what the set finds on one cluster.
type memberState struct {
	cluster *v1.Cluster
	member  *Member
	status  v1.PackageSetClusterStatus
	// updated: every object the set writes there carries the current
	// change; done: and every package is ready with it; failed: a package
	// failed or rolled back with it.
	updated, done, failed bool
	failure               string
}

// Reconcile brings one set's clusters in line.
func (r *PackageSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	set := &v1.PackageSet{}
	if err := r.Get(ctx, req.NamespacedName, set); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	selected, all, err := r.clusters(ctx, set)
	if err != nil {
		return ctrl.Result{}, err
	}
	var sets v1.PackageSetList
	if err := r.List(ctx, &sets); err != nil {
		return ctrl.Result{}, err
	}

	if !set.DeletionTimestamp.IsZero() {
		var failed []string
		for _, name := range previous(set) {
			if c, ok := all[name]; ok {
				if err := r.cleanup(ctx, set, c, nil, nil, sets.Items); err != nil {
					failed = append(failed, name+": "+err.Error())
				}
			}
		}
		if len(failed) > 0 {
			// Keep the finalizer: an unreachable member would keep the
			// set's packages otherwise, with nobody left to remove them.
			return ctrl.Result{RequeueAfter: fleetRequeue}, fmt.Errorf("remove the set from %s", strings.Join(failed, "; "))
		}
		controllerutil.RemoveFinalizer(set, FinalizerPackageSet)
		return ctrl.Result{}, r.Update(ctx, set)
	}
	if controllerutil.AddFinalizer(set, FinalizerPackageSet) {
		if err := r.Update(ctx, set); err != nil {
			return ctrl.Result{}, err
		}
	}
	change, err := digestOf(struct {
		Repositories []v1.PackageSetRepository
		Packages     []v1.PackageSetPackage
	}{set.Spec.Repositories, set.Spec.Packages})
	if err != nil {
		return ctrl.Result{}, err
	}
	change = strings.TrimPrefix(change, "sha256:")[:16]

	// Clusters the set no longer selects lose what it wrote there, unless
	// another set takes it over.
	for _, name := range previous(set) {
		if _, still := selected[name]; !still {
			if c, ok := all[name]; ok {
				_ = r.cleanup(ctx, set, c, nil, nil, sets.Items)
			}
		}
	}

	rollout := set.Spec.Rollout
	if rollout == nil {
		rollout = &v1.PackageSetRollout{}
	}
	var canary labels.Selector
	if rollout.Canary != nil {
		if canary, err = metav1.LabelSelectorAsSelector(rollout.Canary); err != nil {
			return ctrl.Result{}, err
		}
	}
	isCanary := func(c *v1.Cluster) bool { return canary != nil && canary.Matches(labels.Set(c.Labels)) }
	order := sortedClusterNames(selected)
	sort.SliceStable(order, func(i, j int) bool { return isCanary(selected[order[i]]) && !isCanary(selected[order[j]]) })

	states := make([]*memberState, 0, len(order))
	for _, name := range order {
		states = append(states, r.observe(ctx, set, selected[name], change))
	}
	pause := rollout.PauseOnFailure == nil || *rollout.PauseOnFailure
	inProgress, canariesDone, failedOn := 0, true, ""
	for _, st := range states {
		if st.updated && !st.done {
			inProgress++
		}
		if st.updated && st.failed && failedOn == "" {
			failedOn = st.cluster.Name + ": " + st.failure
		}
		if isCanary(st.cluster) && !(st.updated && st.done) {
			canariesDone = false
		}
	}
	paused := pause && failedOn != ""
	for _, st := range states {
		if st.member == nil {
			continue
		}
		if !st.updated {
			switch {
			case paused:
				st.status.Message = "waiting: the rollout is paused after a failure on " + failedOn
				continue
			case canary != nil && !isCanary(st.cluster) && !canariesDone:
				st.status.Message = "waiting for the canary clusters"
				continue
			case rollout.MaxInProgress > 0 && inProgress >= int(rollout.MaxInProgress):
				st.status.Message = "waiting for its turn"
				continue
			}
			inProgress++
		}
		// Clusters that have the change are written again too, which puts
		// back anything edited by hand there.
		if problems := r.write(ctx, set, st, change, sets.Items); len(problems) > 0 {
			st.status.Message = strings.Join(problems, "; ")
			continue
		}
		st.status.Updated = true
		st.updated = true
	}

	var statuses []v1.PackageSetClusterStatus
	readyClusters, updatedClusters := int32(0), int32(0)
	for _, st := range states {
		if st.updated && st.done {
			readyClusters++
		}
		if st.updated {
			updatedClusters++
		}
		statuses = append(statuses, st.status)
	}
	set.Status.Clusters = statuses
	set.Status.ReadyClusters = readyClusters
	set.Status.UpdatedClusters = updatedClusters
	set.Status.Change = change
	cond := metav1.Condition{Type: "Ready", ObservedGeneration: set.Generation}
	switch {
	case len(selected) == 0:
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, "NoClusters", "the selector matches no Cluster"
	case int(readyClusters) == len(selected):
		cond.Status, cond.Reason, cond.Message = metav1.ConditionTrue, "AllReady", fmt.Sprintf("%d of %d clusters ready", readyClusters, len(selected))
	default:
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, "Progressing", fmt.Sprintf("%d of %d clusters ready, %d updated", readyClusters, len(selected), updatedClusters)
	}
	meta.SetStatusCondition(&set.Status.Conditions, cond)
	pc := metav1.Condition{Type: "RolloutPaused", Status: metav1.ConditionFalse, Reason: "Rolling", ObservedGeneration: set.Generation}
	if paused {
		pc.Status, pc.Reason, pc.Message = metav1.ConditionTrue, "ClusterFailed", "stopped after a failure on "+failedOn+"; change the set to go on"
	}
	meta.SetStatusCondition(&set.Status.Conditions, pc)
	if err := r.Status().Update(ctx, set); err != nil && !apierrors.IsConflict(err) {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: fleetRequeue}, nil
}

// firstLine keeps a status message short: its first line, cut at max.
func firstLine(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// failedReasons are package conditions that end a change badly.
var failedReasons = map[string]bool{v1.ReasonUpgradeFailed: true, v1.ReasonUpgradeRolledBack: true}

// observe reads what the set has on one cluster.
func (r *PackageSetReconciler) observe(ctx context.Context, set *v1.PackageSet, c *v1.Cluster, change string) *memberState {
	st := &memberState{cluster: c, status: v1.PackageSetClusterStatus{Name: c.Name, Total: int32(len(set.Spec.Packages))}}
	m, err := r.Members.For(ctx, c)
	if err != nil {
		st.status.Message = err.Error()
		return st
	}
	st.member = m
	sel := client.MatchingLabels{v1.LabelPackageSet: set.Name}
	var pkgs v1.PackageList
	var repos v1.RepositoryList
	if err := m.Client.List(ctx, &pkgs, sel); err != nil {
		st.status.Message = err.Error()
		return st
	}
	if err := m.Client.List(ctx, &repos, sel); err != nil {
		st.status.Message = err.Error()
		return st
	}
	carries := map[string]bool{}
	for _, rp := range repos.Items {
		carries["repository/"+rp.Name] = rp.Annotations[v1.AnnotationPackageSetChange] == change
	}
	byName := map[string]v1.Package{}
	for _, p := range pkgs.Items {
		carries["package/"+p.Name] = p.Annotations[v1.AnnotationPackageSetChange] == change
		byName[p.Name] = p
	}
	st.updated = true
	for _, rp := range set.Spec.Repositories {
		st.updated = st.updated && carries["repository/"+rp.Name]
	}
	var notReady []string
	for _, sp := range set.Spec.Packages {
		st.updated = st.updated && carries["package/"+sp.Name]
		p, ok := byName[sp.Name]
		if !ok {
			notReady = append(notReady, sp.Name)
			continue
		}
		if p.Status.Version != "" {
			if st.status.Versions == nil {
				st.status.Versions = map[string]string{}
			}
			st.status.Versions[p.Name] = p.Status.Version
		}
		cond := meta.FindStatusCondition(p.Status.Conditions, "Ready")
		current := cond != nil && cond.ObservedGeneration == p.Generation
		switch {
		case current && cond.Status == metav1.ConditionTrue:
			st.status.Ready++
		case current && failedReasons[cond.Reason]:
			st.failed = true
			if st.failure == "" {
				st.failure = p.Name + ": " + firstLine(cond.Message, 200)
			}
			notReady = append(notReady, p.Name)
		default:
			notReady = append(notReady, p.Name)
		}
	}
	st.status.Updated = st.updated
	st.done = st.updated && len(notReady) == 0
	if len(notReady) > 0 {
		sort.Strings(notReady)
		st.status.Message = "not ready: " + strings.Join(notReady, ", ")
	}
	return st
}

// write puts the set's current change on one cluster and drops what the
// set no longer wants there.
func (r *PackageSetReconciler) write(ctx context.Context, set *v1.PackageSet, st *memberState, change string, sets []v1.PackageSet) []string {
	c := st.member.Client
	var problems []string
	wanted, wantedRepos := map[string]bool{}, map[string]bool{}
	for _, rp := range set.Spec.Repositories {
		wantedRepos[rp.Name] = true
		obj := &v1.Repository{ObjectMeta: metav1.ObjectMeta{Name: rp.Name}}
		if err := writeManaged(ctx, c, set.Name, change, obj, r.mayTakeOver(sets, st.cluster, "repository", rp.Name), func() { obj.Spec = *rp.Spec.DeepCopy() }); err != nil {
			problems = append(problems, "repository "+rp.Name+": "+err.Error())
		}
	}
	for _, p := range set.Spec.Packages {
		wanted[p.Name] = true
		obj := &v1.Package{ObjectMeta: metav1.ObjectMeta{Name: p.Name}}
		if err := writeManaged(ctx, c, set.Name, change, obj, r.mayTakeOver(sets, st.cluster, "package", p.Name), func() { obj.Spec = *p.Spec.DeepCopy() }); err != nil {
			problems = append(problems, "package "+p.Name+": "+err.Error())
		}
	}
	if err := r.cleanup(ctx, set, st.cluster, wanted, wantedRepos, sets); err != nil {
		problems = append(problems, err.Error())
	}
	return problems
}

// ErrNotManaged is returned for an object the set did not create.
var ErrNotManaged = fmt.Errorf("exists and is not managed by this set; left alone")

// writeManaged creates or updates an object the set manages. It refuses
// an object made by hand, and one another set manages unless takeOver
// says that set no longer wants it there.
func writeManaged(ctx context.Context, c client.Client, set, change string, obj client.Object, takeOver func(owner string) bool, mutate func()) error {
	err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj)
	switch {
	case apierrors.IsNotFound(err):
		mutate()
		obj.SetLabels(map[string]string{v1.LabelPackageSet: set})
		obj.SetAnnotations(map[string]string{v1.AnnotationPackageSetChange: change})
		return c.Create(ctx, obj)
	case err != nil:
		return err
	}
	owner := obj.GetLabels()[v1.LabelPackageSet]
	if owner == "" || (owner != set && !takeOver(owner)) {
		return ErrNotManaged
	}
	mutate()
	l := obj.GetLabels()
	l[v1.LabelPackageSet] = set
	obj.SetLabels(l)
	a := obj.GetAnnotations()
	if a == nil {
		a = map[string]string{}
	}
	a[v1.AnnotationPackageSetChange] = change
	obj.SetAnnotations(a)
	return c.Update(ctx, obj)
}

// claimant names another set that selects the cluster and wants an object
// of kind (package or repository) and name there, or "".
func claimant(sets []v1.PackageSet, cluster *v1.Cluster, kind, name, except string) string {
	for _, s := range sets {
		if s.Name == except || !s.DeletionTimestamp.IsZero() {
			continue
		}
		sel, err := metav1.LabelSelectorAsSelector(&s.Spec.ClusterSelector)
		if err != nil || !sel.Matches(labels.Set(cluster.Labels)) {
			continue
		}
		if kind == "package" {
			for _, p := range s.Spec.Packages {
				if p.Name == name {
					return s.Name
				}
			}
		} else {
			for _, rp := range s.Spec.Repositories {
				if rp.Name == name {
					return s.Name
				}
			}
		}
	}
	return ""
}

// mayTakeOver lets a set take an object another set no longer wants on
// the cluster, so a cluster moving between sets keeps what both carry.
func (r *PackageSetReconciler) mayTakeOver(sets []v1.PackageSet, cluster *v1.Cluster, kind, name string) func(string) bool {
	return func(owner string) bool {
		for _, s := range sets {
			if s.Name == owner {
				return claimant([]v1.PackageSet{s}, cluster, kind, name, "") == ""
			}
		}
		return true // the owner set is gone
	}
}

// cleanup removes what the set wrote to a cluster and no longer wants,
// everything when wanted is nil. An object another set wants there is
// handed to that set instead: it keeps running and moves on with that
// set's own rollout.
func (r *PackageSetReconciler) cleanup(ctx context.Context, set *v1.PackageSet, c *v1.Cluster, wanted, wantedRepos map[string]bool, sets []v1.PackageSet) error {
	m, err := r.Members.For(ctx, c)
	if err != nil {
		return err
	}
	release := func(obj client.Object, kind string) error {
		if other := claimant(sets, c, kind, obj.GetName(), set.Name); other != "" {
			l := obj.GetLabels()
			l[v1.LabelPackageSet] = other
			obj.SetLabels(l)
			a := obj.GetAnnotations()
			delete(a, v1.AnnotationPackageSetChange)
			obj.SetAnnotations(a)
			return m.Client.Update(ctx, obj)
		}
		if err := m.Client.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	}
	sel := client.MatchingLabels{v1.LabelPackageSet: set.Name}
	var pkgs v1.PackageList
	if err := m.Client.List(ctx, &pkgs, sel); err != nil && !meta.IsNoMatchError(err) {
		return err
	}
	for i := range pkgs.Items {
		if !wanted[pkgs.Items[i].Name] {
			if err := release(&pkgs.Items[i], "package"); err != nil {
				return err
			}
		}
	}
	var repos v1.RepositoryList
	if err := m.Client.List(ctx, &repos, sel); err != nil && !meta.IsNoMatchError(err) {
		return err
	}
	for i := range repos.Items {
		if !wantedRepos[repos.Items[i].Name] {
			if err := release(&repos.Items[i], "repository"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *PackageSetReconciler) clusters(ctx context.Context, set *v1.PackageSet) (map[string]*v1.Cluster, map[string]*v1.Cluster, error) {
	sel, err := metav1.LabelSelectorAsSelector(&set.Spec.ClusterSelector)
	if err != nil {
		return nil, nil, err
	}
	var list v1.ClusterList
	if err := r.List(ctx, &list); err != nil {
		return nil, nil, err
	}
	selected, all := map[string]*v1.Cluster{}, map[string]*v1.Cluster{}
	for i := range list.Items {
		c := &list.Items[i]
		all[c.Name] = c
		if c.DeletionTimestamp.IsZero() && sel.Matches(labels.Set(c.Labels)) {
			selected[c.Name] = c
		}
	}
	return selected, all, nil
}

// previous lists the clusters the set reported on last time.
func previous(set *v1.PackageSet) []string {
	var out []string
	for _, c := range set.Status.Clusters {
		out = append(out, c.Name)
	}
	return out
}

func sortedClusterNames(m map[string]*v1.Cluster) []string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// SetupWithManager registers the controller; any Cluster change may
// change what a set selects.
func (r *PackageSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	allSets := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		var sets v1.PackageSetList
		if err := mgr.GetClient().List(ctx, &sets); err != nil {
			return nil
		}
		out := make([]reconcile.Request, 0, len(sets.Items))
		for _, s := range sets.Items {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: s.Name}})
		}
		return out
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("kubepkg-package-set").
		For(&v1.PackageSet{}).
		Watches(&v1.Cluster{}, allSets).
		// A set that drops a cluster or package may hand objects to another.
		Watches(&v1.PackageSet{}, allSets).
		Complete(r)
}
