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

	"github.com/tym83/kubepkg/api/v1alpha1"
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
	For(ctx context.Context, c *v1alpha1.Cluster) (*Member, error)
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
func (k *KubeconfigClients) For(ctx context.Context, c *v1alpha1.Cluster) (*Member, error) {
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
	if err := v1alpha1.AddToSchemeForGroup(k.Group)(scheme); err != nil {
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
	c := &v1alpha1.Cluster{}
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
	var pkgs v1alpha1.PackageList
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
	return ctrl.NewControllerManagedBy(mgr).Named("kubepkg-cluster").For(&v1alpha1.Cluster{}).Complete(r)
}

// PackageSetReconciler writes a set's repositories and packages to the
// clusters it selects and reports how far each got.
type PackageSetReconciler struct {
	client.Client
	Members MemberClients
}

// Reconcile brings one set's clusters in line.
func (r *PackageSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	set := &v1alpha1.PackageSet{}
	if err := r.Get(ctx, req.NamespacedName, set); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	selected, all, err := r.clusters(ctx, set)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !set.DeletionTimestamp.IsZero() {
		var failed []string
		for _, name := range previous(set) {
			if c, ok := all[name]; ok {
				if err := r.cleanup(ctx, set, c, nil, nil); err != nil {
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

	wanted := map[string]bool{}
	for _, p := range set.Spec.Packages {
		wanted[p.Name] = true
	}
	wantedRepos := map[string]bool{}
	for _, rp := range set.Spec.Repositories {
		wantedRepos[rp.Name] = true
	}
	// Clusters the set no longer selects lose what it wrote there.
	for _, name := range previous(set) {
		if _, still := selected[name]; !still {
			if c, ok := all[name]; ok {
				_ = r.cleanup(ctx, set, c, nil, nil)
			}
		}
	}

	var statuses []v1alpha1.PackageSetClusterStatus
	readyClusters := int32(0)
	for _, name := range sortedClusterNames(selected) {
		st := r.apply(ctx, set, selected[name], wanted, wantedRepos)
		if st.Total > 0 && st.Ready == st.Total && st.Message == "" {
			readyClusters++
		}
		statuses = append(statuses, st)
	}
	set.Status.Clusters = statuses
	set.Status.ReadyClusters = readyClusters
	cond := metav1.Condition{Type: "Ready", ObservedGeneration: set.Generation}
	switch {
	case len(selected) == 0:
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, "NoClusters", "the selector matches no Cluster"
	case int(readyClusters) == len(selected):
		cond.Status, cond.Reason, cond.Message = metav1.ConditionTrue, "AllReady", fmt.Sprintf("%d of %d clusters ready", readyClusters, len(selected))
	default:
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, "Progressing", fmt.Sprintf("%d of %d clusters ready", readyClusters, len(selected))
	}
	meta.SetStatusCondition(&set.Status.Conditions, cond)
	if err := r.Status().Update(ctx, set); err != nil && !apierrors.IsConflict(err) {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: fleetRequeue}, nil
}

// apply writes the set to one cluster and counts its ready packages.
func (r *PackageSetReconciler) apply(ctx context.Context, set *v1alpha1.PackageSet, c *v1alpha1.Cluster, wanted, wantedRepos map[string]bool) v1alpha1.PackageSetClusterStatus {
	st := v1alpha1.PackageSetClusterStatus{Name: c.Name, Total: int32(len(set.Spec.Packages))}
	m, err := r.Members.For(ctx, c)
	if err != nil {
		st.Message = err.Error()
		return st
	}
	var problems []string
	for _, rp := range set.Spec.Repositories {
		obj := &v1alpha1.Repository{ObjectMeta: metav1.ObjectMeta{Name: rp.Name}}
		if err := writeManaged(ctx, m.Client, set.Name, obj, func() { obj.Spec = *rp.Spec.DeepCopy() }); err != nil {
			problems = append(problems, "repository "+rp.Name+": "+err.Error())
		}
	}
	for _, p := range set.Spec.Packages {
		obj := &v1alpha1.Package{ObjectMeta: metav1.ObjectMeta{Name: p.Name}}
		if err := writeManaged(ctx, m.Client, set.Name, obj, func() { obj.Spec = *p.Spec.DeepCopy() }); err != nil {
			problems = append(problems, "package "+p.Name+": "+err.Error())
		}
	}
	if err := r.cleanup(ctx, set, c, wanted, wantedRepos); err != nil {
		problems = append(problems, err.Error())
	}
	var pkgs v1alpha1.PackageList
	if err := m.Client.List(ctx, &pkgs, client.MatchingLabels{v1alpha1.LabelPackageSet: set.Name}); err != nil {
		problems = append(problems, err.Error())
	}
	var notReady []string
	for _, p := range pkgs.Items {
		if !wanted[p.Name] {
			continue
		}
		if isReady(p.Status.Conditions) {
			st.Ready++
		} else {
			notReady = append(notReady, p.Name)
		}
	}
	if len(problems) == 0 && len(notReady) > 0 {
		sort.Strings(notReady)
		st.Message = "not ready: " + strings.Join(notReady, ", ")
	}
	if len(problems) > 0 {
		st.Message = strings.Join(problems, "; ")
	}
	return st
}

// ErrNotManaged is returned for an object the set did not create.
var ErrNotManaged = fmt.Errorf("exists and is not managed by this set; left alone")

// writeManaged creates or updates an object the set manages, refusing
// one created by somebody else.
func writeManaged(ctx context.Context, c client.Client, set string, obj client.Object, mutate func()) error {
	err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj)
	switch {
	case apierrors.IsNotFound(err):
		mutate()
		obj.SetLabels(map[string]string{v1alpha1.LabelPackageSet: set})
		return c.Create(ctx, obj)
	case err != nil:
		return err
	case obj.GetLabels()[v1alpha1.LabelPackageSet] != set:
		return ErrNotManaged
	}
	mutate()
	return c.Update(ctx, obj)
}

// cleanup deletes what the set wrote to a cluster and no longer wants:
// everything when wanted is nil.
func (r *PackageSetReconciler) cleanup(ctx context.Context, set *v1alpha1.PackageSet, c *v1alpha1.Cluster, wanted, wantedRepos map[string]bool) error {
	m, err := r.Members.For(ctx, c)
	if err != nil {
		return err
	}
	sel := client.MatchingLabels{v1alpha1.LabelPackageSet: set.Name}
	var pkgs v1alpha1.PackageList
	if err := m.Client.List(ctx, &pkgs, sel); err != nil && !meta.IsNoMatchError(err) {
		return err
	}
	for i := range pkgs.Items {
		if !wanted[pkgs.Items[i].Name] {
			if err := m.Client.Delete(ctx, &pkgs.Items[i]); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
	var repos v1alpha1.RepositoryList
	if err := m.Client.List(ctx, &repos, sel); err != nil && !meta.IsNoMatchError(err) {
		return err
	}
	for i := range repos.Items {
		if !wantedRepos[repos.Items[i].Name] {
			if err := m.Client.Delete(ctx, &repos.Items[i]); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}

func (r *PackageSetReconciler) clusters(ctx context.Context, set *v1alpha1.PackageSet) (map[string]*v1alpha1.Cluster, map[string]*v1alpha1.Cluster, error) {
	sel, err := metav1.LabelSelectorAsSelector(&set.Spec.ClusterSelector)
	if err != nil {
		return nil, nil, err
	}
	var list v1alpha1.ClusterList
	if err := r.List(ctx, &list); err != nil {
		return nil, nil, err
	}
	selected, all := map[string]*v1alpha1.Cluster{}, map[string]*v1alpha1.Cluster{}
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
func previous(set *v1alpha1.PackageSet) []string {
	var out []string
	for _, c := range set.Status.Clusters {
		out = append(out, c.Name)
	}
	return out
}

func sortedClusterNames(m map[string]*v1alpha1.Cluster) []string {
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
		var sets v1alpha1.PackageSetList
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
		For(&v1alpha1.PackageSet{}).
		Watches(&v1alpha1.Cluster{}, allSets).
		Complete(r)
}
