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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/tym83/kubepkg/api/v1alpha1"
	"github.com/tym83/kubepkg/pkg/repo"
)

const (
	defaultRepositoryInterval = 10 * time.Minute
	// repositoryRetry bounds the wait after a failed fetch.
	repositoryRetry = time.Minute
	// AnnotationSpecDigest on a PackageSource made from a repository is
	// the digest of the index entry it was made from.
	AnnotationSpecDigest = "kubepkg.dev/spec-digest"
	// ReasonVersionNotAvailable: no repository offers an acceptable version.
	ReasonVersionNotAvailable = "VersionNotAvailable"
)

// Repositories lets the operator install packages from repository
// indexes. Fetchers and Policy are where a distribution plugs in its own
// transports and trust rules.
type Repositories struct {
	Store    *repo.Store
	Fetchers repo.Fetchers
	Policy   repo.Policy
}

// RepositoryReconciler keeps the index of every Repository loaded.
type RepositoryReconciler struct {
	client.Client
	Repositories *Repositories
}

// Reconcile fetches one repository index.
func (r *RepositoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	rp := &v1alpha1.Repository{}
	if err := r.Get(ctx, req.NamespacedName, rp); err != nil {
		if apierrors.IsNotFound(err) {
			r.Repositories.Store.Delete(req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	interval := defaultRepositoryInterval
	if rp.Spec.Interval != nil && rp.Spec.Interval.Duration > 0 {
		interval = rp.Spec.Interval.Duration
	}

	idx, digest, reason, err := r.load(ctx, rp)
	if err != nil {
		r.Repositories.Store.Failed(rp.Name)
		meta.SetStatusCondition(&rp.Status.Conditions, metav1.Condition{
			Type: "Ready", Status: metav1.ConditionFalse, Reason: reason, Message: err.Error(), ObservedGeneration: rp.Generation,
		})
		if uerr := r.Status().Update(ctx, rp); uerr != nil && !apierrors.IsConflict(uerr) {
			return ctrl.Result{}, uerr
		}
		return ctrl.Result{RequeueAfter: min(interval, repositoryRetry)}, nil
	}
	r.Repositories.Store.Set(rp.Name, rp.Spec.Priority, idx)
	now := metav1.Now()
	rp.Status.Packages = int32(len(idx.Packages))
	rp.Status.IndexDigest = digest
	rp.Status.LastFetched = &now
	meta.SetStatusCondition(&rp.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "IndexLoaded",
		Message: fmt.Sprintf("%d packages", len(idx.Packages)), ObservedGeneration: rp.Generation,
	})
	if err := r.Status().Update(ctx, rp); err != nil && !apierrors.IsConflict(err) {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: interval}, nil
}

func (r *RepositoryReconciler) load(ctx context.Context, rp *v1alpha1.Repository) (*repo.Index, string, string, error) {
	raw, err := r.Repositories.Fetchers.Fetch(ctx, rp.Spec.URL)
	if err != nil {
		return nil, "", "FetchFailed", err
	}
	idx, err := repo.Parse(raw)
	if err != nil {
		return nil, "", "InvalidIndex", err
	}
	if err := r.Repositories.Policy.AdmitIndex(ctx, rp.Name, raw, idx); err != nil {
		return nil, "", "IndexRefused", err
	}
	sum := sha256.Sum256(raw)
	return idx, "sha256:" + hex.EncodeToString(sum[:]), "", nil
}

// SetupWithManager registers the controller.
func (r *RepositoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("kubepkg-repository").
		For(&v1alpha1.Repository{}).
		Complete(r)
}

// selectSource makes the PackageSource of a package from the repositories
// unless one was written by hand. It returns done when the reconcile
// should stop here: the source was just written, or no version can be
// chosen yet.
func (r *PackageReconciler) selectSource(ctx context.Context, pkg *v1alpha1.Package) (ctrl.Result, bool, error) {
	src := &v1alpha1.PackageSource{}
	err := r.Get(ctx, types.NamespacedName{Name: pkg.Name}, src)
	exists := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, true, err
	}
	if exists && src.Labels[v1alpha1.LabelRepository] == "" {
		return ctrl.Result{}, false, nil // written by hand: it wins
	}

	pending, err := r.repositoriesPending(ctx, pkg.Spec.Repository)
	if err != nil {
		return ctrl.Result{}, true, err
	}
	if pending {
		// Choosing now could pick a lower priority repository whose index
		// happened to load first, and switch back moments later.
		if exists {
			return ctrl.Result{}, false, nil
		}
		setReady(pkg, metav1.ConditionFalse, ReasonVersionNotAvailable, "waiting for repository indexes to load")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, true, nil
	}

	sel, err := r.Repositories.Store.Select(ctx, pkg.Name, pkg.Spec.Version, pkg.Spec.Repository, r.Repositories.Policy)
	if err != nil {
		if !errors.Is(err, repo.ErrNoVersion) {
			return ctrl.Result{}, true, err
		}
		if exists {
			// Keep what is installed; the version check further on
			// reports a constraint the current source no longer meets.
			return ctrl.Result{}, false, nil
		}
		setReady(pkg, metav1.ConditionFalse, ReasonVersionNotAvailable, err.Error())
		return ctrl.Result{}, true, nil
	}
	if exists && src.Labels[v1alpha1.LabelRepository] == sel.Repository && src.Annotations[AnnotationSpecDigest] == sel.Version.Digest {
		return ctrl.Result{}, false, nil
	}

	want := &v1alpha1.PackageSource{ObjectMeta: metav1.ObjectMeta{Name: pkg.Name}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, want, func() error {
		if want.Labels == nil {
			want.Labels = map[string]string{}
		}
		if want.Annotations == nil {
			want.Annotations = map[string]string{}
		}
		want.Labels[v1alpha1.LabelRepository] = sel.Repository
		want.Annotations[AnnotationSpecDigest] = sel.Version.Digest
		want.Spec = *sel.Version.Spec.DeepCopy()
		// The source goes away with its package.
		return controllerutil.SetOwnerReference(pkg, want, r.Scheme())
	})
	if err != nil {
		return ctrl.Result{}, true, fmt.Errorf("write PackageSource from repository %s: %w", sel.Repository, err)
	}
	setReady(pkg, metav1.ConditionFalse, v1alpha1.ReasonProgressing,
		fmt.Sprintf("selected %s %s build %d from repository %s", pkg.Name, sel.Version.Version, sel.Version.Build, sel.Repository))
	return ctrl.Result{}, true, nil
}

// repositoriesPending reports whether a Repository the package may use
// has not been fetched yet.
func (r *PackageReconciler) repositoriesPending(ctx context.Context, only string) (bool, error) {
	var list v1alpha1.RepositoryList
	if err := r.List(ctx, &list); err != nil {
		return false, err
	}
	for _, rp := range list.Items {
		if (only == "" || rp.Name == only) && !r.Repositories.Store.Attempted(rp.Name) {
			return true, nil
		}
	}
	return false, nil
}

// allPackages enqueues every Package: a repository change may change the
// version any of them selects.
func allPackages(c client.Client) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		var pkgs v1alpha1.PackageList
		if err := c.List(ctx, &pkgs); err != nil {
			return nil
		}
		out := make([]reconcile.Request, 0, len(pkgs.Items))
		for _, p := range pkgs.Items {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: p.Name}})
		}
		return out
	})
}
