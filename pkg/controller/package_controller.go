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

// Package controller reconciles Packages into package revisions.
package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/tym83/kubepkg/api/v1alpha1"
	"github.com/tym83/kubepkg/pkg/backend"
	"github.com/tym83/kubepkg/pkg/resolve"
)

const (
	defaultHistoryLimit = 10
	// progressRequeue is how often an asynchronous backend is polled while a
	// revision is being applied.
	progressRequeue = 15 * time.Second
	// waitRequeue re-checks requirements that no watch covers, such as an API
	// appearing through discovery.
	waitRequeue = time.Minute
)

// PackageReconciler reconciles Package resources.
type PackageReconciler struct {
	client.Client
	Profile  Profile
	Backend  backend.Backend
	Preparer Preparer
	APIs     APIs
	// CRDs manages CRD ownership; nil disables it (tests without CRDs).
	CRDs CRDOwner
	// Repositories, when set, selects versions from repository indexes
	// for packages without a hand-written PackageSource.
	Repositories *Repositories
}

// Reconcile is the main loop.
func (r *PackageReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	pkg := &v1alpha1.Package{}
	if err := r.Get(ctx, req.NamespacedName, pkg); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !pkg.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, pkg)
	}
	if controllerutil.AddFinalizer(pkg, FinalizerCleanup) {
		if err := r.Update(ctx, pkg); err != nil {
			return ctrl.Result{}, err
		}
	}

	res, err := r.reconcile(ctx, pkg)
	recordPackage(pkg)
	if uerr := r.Status().Update(ctx, pkg); uerr != nil {
		if apierrors.IsConflict(uerr) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, errors.Join(err, uerr)
	}
	return res, err
}

func setReady(pkg *v1alpha1.Package, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&pkg.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: status, Reason: reason, Message: msg, ObservedGeneration: pkg.Generation,
	})
}

func (r *PackageReconciler) reconcile(ctx context.Context, pkg *v1alpha1.Package) (ctrl.Result, error) {
	if r.Repositories != nil {
		if res, done, err := r.selectSource(ctx, pkg); done {
			return res, err
		}
	}
	src := &v1alpha1.PackageSource{}
	if err := r.Get(ctx, types.NamespacedName{Name: pkg.Name}, src); err != nil {
		if apierrors.IsNotFound(err) {
			setReady(pkg, metav1.ConditionFalse, v1alpha1.ReasonPackageSourceNotFound, fmt.Sprintf("PackageSource %s not found", pkg.Name))
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	v := findVariant(src, variantName(pkg))
	if v == nil {
		setReady(pkg, metav1.ConditionFalse, v1alpha1.ReasonVariantNotFound, fmt.Sprintf("Variant %s not found in PackageSource %s", variantName(pkg), pkg.Name))
		return ctrl.Result{}, nil
	}

	ok, err := resolve.Satisfies(sourceVersion(src), pkg.Spec.Version)
	if err != nil {
		setReady(pkg, metav1.ConditionFalse, v1alpha1.ReasonVersionMismatch, err.Error())
		return ctrl.Result{}, nil
	}
	if !ok {
		setReady(pkg, metav1.ConditionFalse, v1alpha1.ReasonVersionMismatch,
			fmt.Sprintf("PackageSource version %s does not satisfy %s", sourceVersion(src), pkg.Spec.Version))
		return ctrl.Result{}, nil
	}

	st, err := r.clusterState(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	reqs := requirementsOf(pkg, v)
	unmet, err := resolve.Check(reqs, st)
	if err != nil {
		setReady(pkg, metav1.ConditionFalse, v1alpha1.ReasonRequirementsNotMet, err.Error())
		return ctrl.Result{}, nil
	}
	r.recordDependencies(pkg, reqs, st)
	if len(unmet) > 0 {
		var parts []string
		for _, u := range unmet {
			parts = append(parts, u.String())
		}
		setReady(pkg, metav1.ConditionFalse, v1alpha1.ReasonRequirementsNotMet, strings.Join(parts, "; "))
		return ctrl.Result{RequeueAfter: waitRequeue}, nil
	}
	self := resolve.Release{Name: pkg.Name, Version: sourceVersion(src), Provides: src.Spec.Provides, Conflicts: src.Spec.Conflicts}
	if c := resolve.Conflicts(self, st); len(c) > 0 {
		setReady(pkg, metav1.ConditionFalse, v1alpha1.ReasonConflict, "conflicts with installed "+strings.Join(c, ", "))
		return ctrl.Result{}, nil
	}
	if r.CRDs != nil {
		if owner, crd, err := r.CRDs.Conflict(ctx, pkg.Name, src.Spec.CRDs); err != nil {
			return ctrl.Result{}, err
		} else if owner != "" {
			setReady(pkg, metav1.ConditionFalse, v1alpha1.ReasonCRDOwnershipConflict, fmt.Sprintf("CRD %s is owned by package %s", crd, owner))
			return ctrl.Result{}, nil
		}
	}

	if err := r.reconcileNamespaces(ctx, pkg, v); err != nil {
		return ctrl.Result{}, err
	}
	depReleases, err := r.dependencyReleases(ctx, reqs)
	if err != nil {
		return ctrl.Result{}, err
	}
	d, err := r.buildDesired(ctx, pkg, src, v, depReleases)
	if err != nil {
		setReady(pkg, metav1.ConditionFalse, "InvalidConfiguration", err.Error())
		return ctrl.Result{}, nil
	}

	revs, err := r.revisions(ctx, pkg.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	defer r.recordHistory(pkg, &revs)

	if to, ok := pkg.Annotations[AnnotationRollbackTo]; ok {
		return r.rollbackTo(ctx, pkg, to, d, &revs)
	}

	latest := lastOf(revs)
	switch {
	case latest != nil && latest.Status.Phase == v1alpha1.PhaseApplying && latest.Annotations[AnnotationDesiredDigest] == d.digest:
		return r.progress(ctx, pkg, latest, d, &revs, src)
	case latest != nil && latest.Annotations[AnnotationDesiredDigest] == d.digest && held(latest):
		// A failed or rolled-back state stays until the desired state
		// changes; retrying the same thing in a loop helps nobody.
		r.reportHeld(pkg, latest)
		return ctrl.Result{}, nil
	case latest != nil && latest.Annotations[AnnotationDesiredDigest] == d.digest &&
		latest.Status.Phase == v1alpha1.PhaseApplied && sameComponents(latest.Spec.Components, d.components):
		return r.observe(ctx, pkg, latest, d)
	}
	return r.applyNew(ctx, pkg, d, &revs, src)
}

func held(rev *v1alpha1.PackageRevision) bool {
	return rev.Status.Phase == v1alpha1.PhaseFailed || rev.Spec.RestoredFrom != 0
}

func (r *PackageReconciler) reportHeld(pkg *v1alpha1.Package, rev *v1alpha1.PackageRevision) {
	pkg.Status.CurrentRevision = rev.Spec.Revision
	if rev.Spec.RestoredFrom != 0 {
		reason := v1alpha1.ReasonUpgradeRolledBack
		if rev.Status.Message == "" {
			rev.Status.Message = fmt.Sprintf("restored revision %d", rev.Spec.RestoredFrom)
		}
		pkg.Status.Version = rev.Spec.Version
		if rev.Annotations[AnnotationRollbackRequested] == "true" && rev.Status.Phase == v1alpha1.PhaseApplied {
			// Someone asked for this state and it runs: the package is
			// healthy, it just does not follow its spec until told to.
			setReady(pkg, metav1.ConditionTrue, v1alpha1.ReasonRolledBack, rev.Status.Message+"; change the Package or set "+AnnotationRetry+" to follow it again")
			return
		}
		setReady(pkg, metav1.ConditionFalse, reason, rev.Status.Message+"; change the Package or set "+AnnotationRetry+" to try again")
		return
	}
	setReady(pkg, metav1.ConditionFalse, v1alpha1.ReasonUpgradeFailed, rev.Status.Message+"; change the Package or set "+AnnotationRetry+" to try again")
}

// observe keeps an applied revision honest: the backend may report a release
// that broke after it was applied.
func (r *PackageReconciler) observe(ctx context.Context, pkg *v1alpha1.Package, rev *v1alpha1.PackageRevision, d *desiredState) (ctrl.Result, error) {
	pkg.Status.CurrentRevision = rev.Spec.Revision
	pkg.Status.Version = rev.Spec.Version
	for _, c := range d.components {
		s, err := r.Backend.Status(ctx, c.backend)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !s.Ready {
			msg := fmt.Sprintf("component %s is not ready: %s", c.snapshot.Name, s.Message)
			setReady(pkg, metav1.ConditionFalse, v1alpha1.ReasonProgressing, msg)
			return ctrl.Result{RequeueAfter: progressRequeue}, nil
		}
	}
	setReady(pkg, metav1.ConditionTrue, v1alpha1.ReasonApplied,
		fmt.Sprintf("reconciliation succeeded, revision %d, %d component(s)", rev.Spec.Revision, len(d.components)))
	return ctrl.Result{}, nil
}

// applyNew records a new revision and applies it.
func (r *PackageReconciler) applyNew(ctx context.Context, pkg *v1alpha1.Package, d *desiredState, revs *[]v1alpha1.PackageRevision, src *v1alpha1.PackageSource) (ctrl.Result, error) {
	n := int64(1)
	if l := lastOf(*revs); l != nil {
		n = l.Spec.Revision + 1
	}
	rev := &v1alpha1.PackageRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:        fmt.Sprintf("%s-%d", pkg.Name, n),
			Labels:      map[string]string{v1alpha1.LabelPackage: pkg.Name},
			Annotations: map[string]string{AnnotationDesiredDigest: d.digest},
		},
		Spec: v1alpha1.PackageRevisionSpec{
			Package: pkg.Name, Revision: n, Version: d.version, Variant: d.variant, RollbackSafe: d.rollbackSafe,
		},
	}
	for _, c := range d.components {
		rev.Spec.Components = append(rev.Spec.Components, c.snapshot)
	}
	if err := controllerutil.SetControllerReference(pkg, rev, r.Scheme()); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, rev); err != nil {
		return ctrl.Result{}, fmt.Errorf("record revision %d: %w", n, err)
	}
	rev.Status.Phase = v1alpha1.PhaseApplying
	if err := r.Status().Update(ctx, rev); err != nil {
		return ctrl.Result{}, err
	}
	*revs = append(*revs, *rev)
	setReady(pkg, metav1.ConditionFalse, v1alpha1.ReasonProgressing, fmt.Sprintf("applying revision %d", n))
	return r.progress(ctx, pkg, &(*revs)[len(*revs)-1], d, revs, src)
}

// progress applies the components of a revision that is being applied and
// settles it as Applied or Failed.
func (r *PackageReconciler) progress(ctx context.Context, pkg *v1alpha1.Package, rev *v1alpha1.PackageRevision, d *desiredState, revs *[]v1alpha1.PackageRevision, src *v1alpha1.PackageSource) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	pkg.Status.CurrentRevision = rev.Spec.Revision
	// Components go one at a time in dependency order: the next one is
	// applied only once the one before it is ready. A synchronous backend
	// returns ready from Apply; an asynchronous one (flux, argo) is
	// re-checked on the next round, so ordering does not depend on the
	// delivery tool supporting it.
	if res, done, err := r.runHooks(ctx, pkg, rev, d, revs); done {
		return res, err
	}
	var applied []desiredComponent
	for _, c := range d.components {
		s, err := r.Backend.Apply(ctx, c.backend)
		setComponentStatus(rev, c.snapshot.Name, s.Revision)
		applied = append(applied, c)
		if err != nil || s.Failed {
			msg := s.Message
			if err != nil {
				msg = err.Error()
			}
			logger.Info("component failed", "package", pkg.Name, "component", c.snapshot.Name, "revision", rev.Spec.Revision, "message", msg)
			return r.fail(ctx, pkg, rev, d, applied, revs, fmt.Sprintf("component %s failed: %s", c.snapshot.Name, msg))
		}
		waiting := ""
		if !s.Ready {
			waiting = c.snapshot.Name
		} else if len(c.readyWhen) > 0 {
			why, err := r.readyWhenMet(ctx, c.readyWhen, c.backend.Namespace)
			if err != nil {
				return ctrl.Result{}, err
			}
			if why != "" {
				waiting = c.snapshot.Name + ": " + why
				// The release is in, but what it runs never got ready:
				// past the timeout that is a failed revision.
				if t := c.backend.Timeout; t > 0 && !rev.CreationTimestamp.IsZero() && time.Since(rev.CreationTimestamp.Time) > t {
					return r.fail(ctx, pkg, rev, d, applied, revs, fmt.Sprintf("component %s not ready after %s: %s", c.snapshot.Name, t, why))
				}
			}
		}
		if waiting != "" {
			if err := r.Status().Update(ctx, rev); err != nil {
				return ctrl.Result{}, err
			}
			setReady(pkg, metav1.ConditionFalse, v1alpha1.ReasonProgressing, fmt.Sprintf("applying revision %d: waiting for %s", rev.Spec.Revision, waiting))
			return ctrl.Result{RequeueAfter: progressRequeue}, nil
		}
	}

	if err := r.removeOrphans(ctx, pkg, d, *revs, rev); err != nil {
		return ctrl.Result{}, err
	}
	rev.Status.Phase = v1alpha1.PhaseApplied
	rev.Status.Message = ""
	if err := r.Status().Update(ctx, rev); err != nil {
		return ctrl.Result{}, err
	}
	countRevision(pkg.Name, outcomeApplied)
	if rev.Annotations[AnnotationHooksDone] == "true" {
		r.uninstallHooks(ctx, d)
	}
	for i := range *revs {
		o := &(*revs)[i]
		if o.Spec.Revision < rev.Spec.Revision && o.Status.Phase == v1alpha1.PhaseApplied {
			o.Status.Phase = v1alpha1.PhaseSuperseded
			if err := r.Status().Update(ctx, o); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	if r.CRDs != nil {
		if err := r.CRDs.Claim(ctx, pkg.Name, src.Spec.CRDs); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.prune(ctx, pkg, revs); err != nil {
		return ctrl.Result{}, err
	}
	pkg.Status.Version = rev.Spec.Version
	setReady(pkg, metav1.ConditionTrue, v1alpha1.ReasonApplied,
		fmt.Sprintf("reconciliation succeeded, revision %d, %d component(s)", rev.Spec.Revision, len(d.components)))
	return ctrl.Result{}, nil
}

func setComponentStatus(rev *v1alpha1.PackageRevision, name string, backendRev int) {
	for i := range rev.Status.Components {
		if rev.Status.Components[i].Name == name {
			if backendRev != 0 {
				rev.Status.Components[i].BackendRevision = backendRev
			}
			return
		}
	}
	rev.Status.Components = append(rev.Status.Components, v1alpha1.ComponentRevisionStatus{Name: name, BackendRevision: backendRev})
}

func atomic(pkg *v1alpha1.Package) bool {
	return pkg.Spec.Upgrade == nil || pkg.Spec.Upgrade.Atomic == nil || *pkg.Spec.Upgrade.Atomic
}

// lastGood is the newest revision that was applied successfully.
func lastGood(revs []v1alpha1.PackageRevision, before int64) *v1alpha1.PackageRevision {
	for i := len(revs) - 1; i >= 0; i-- {
		p := revs[i].Status.Phase
		if revs[i].Spec.Revision < before && (p == v1alpha1.PhaseApplied || p == v1alpha1.PhaseSuperseded) {
			return &revs[i]
		}
	}
	return nil
}

// fail settles a failed revision: rolls the package back as a whole when
// that is allowed and safe, otherwise leaves it for a forward fix.
func (r *PackageReconciler) fail(ctx context.Context, pkg *v1alpha1.Package, rev *v1alpha1.PackageRevision, d *desiredState, applied []desiredComponent, revs *[]v1alpha1.PackageRevision, msg string) (ctrl.Result, error) {
	rev.Status.Phase = v1alpha1.PhaseFailed
	rev.Status.Message = msg
	if err := r.Status().Update(ctx, rev); err != nil {
		return ctrl.Result{}, err
	}
	countRevision(pkg.Name, outcomeFailed)
	prev := lastGood(*revs, rev.Spec.Revision)
	// The cluster no longer matches any earlier revision as recorded: part
	// of the package may already run the new version. The last good one
	// stays the rollback target, but it is not what is applied any more.
	for i := range *revs {
		o := &(*revs)[i]
		if o.Spec.Revision < rev.Spec.Revision && o.Status.Phase == v1alpha1.PhaseApplied {
			o.Status.Phase = v1alpha1.PhaseSuperseded
			if err := r.Status().Update(ctx, o); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	switch {
	case !atomic(pkg):
		setReady(pkg, metav1.ConditionFalse, v1alpha1.ReasonUpgradeFailed, msg+" (atomic upgrades are off)")
		return ctrl.Result{}, nil
	case prev == nil:
		setReady(pkg, metav1.ConditionFalse, v1alpha1.ReasonUpgradeFailed, msg+" (no earlier revision to return to)")
		return ctrl.Result{}, nil
	case !d.rollbackSafe:
		setReady(pkg, metav1.ConditionFalse, v1alpha1.ReasonUpgradeFailed,
			fmt.Sprintf("%s (version %s does not declare rollback as safe; fix forward)", msg, d.version))
		return ctrl.Result{}, nil
	}

	if err := r.restoreComponents(ctx, prev, applied); err != nil {
		setReady(pkg, metav1.ConditionFalse, v1alpha1.ReasonUpgradeFailed, fmt.Sprintf("%s; rolling back failed too: %v", msg, err))
		return ctrl.Result{}, nil
	}
	restored, err := r.recordRestored(ctx, pkg, prev, d.digest, *revs, false, fmt.Sprintf("revision %d failed (%s), restored revision %d", rev.Spec.Revision, msg, prev.Spec.Revision))
	if err != nil {
		return ctrl.Result{}, err
	}
	*revs = append(*revs, *restored)
	pkg.Status.CurrentRevision = restored.Spec.Revision
	pkg.Status.Version = restored.Spec.Version
	setReady(pkg, metav1.ConditionFalse, v1alpha1.ReasonUpgradeRolledBack, restored.Status.Message)
	countRevision(pkg.Name, outcomeRolledBack)
	return ctrl.Result{}, nil
}

// restoreComponents returns the given components to their state in prev,
// newest first. A component prev did not have is removed.
func (r *PackageReconciler) restoreComponents(ctx context.Context, prev *v1alpha1.PackageRevision, comps []desiredComponent) error {
	backendRev := map[string]int{}
	for _, s := range prev.Status.Components {
		backendRev[s.Name] = s.BackendRevision
	}
	for i := len(comps) - 1; i >= 0; i-- {
		c := comps[i]
		to, ok := backendRev[c.snapshot.Name]
		if !ok || to == 0 {
			if err := r.Backend.Uninstall(ctx, c.backend); err != nil {
				return err
			}
			continue
		}
		cur, err := r.Backend.Status(ctx, c.backend)
		if err != nil {
			return err
		}
		if cur.Exists && cur.Revision == to && cur.Ready {
			continue // never changed in this attempt
		}
		if _, err := r.Backend.Rollback(ctx, c.backend, to); err != nil {
			return err
		}
	}
	return nil
}

// recordRestored writes a revision that re-applies prev. It carries the
// current desired digest so the operator does not immediately upgrade again.
func (r *PackageReconciler) recordRestored(ctx context.Context, pkg *v1alpha1.Package, prev *v1alpha1.PackageRevision, digest string, revs []v1alpha1.PackageRevision, requested bool, msg string) (*v1alpha1.PackageRevision, error) {
	n := lastOf(revs).Spec.Revision + 1
	restored := &v1alpha1.PackageRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:        fmt.Sprintf("%s-%d", pkg.Name, n),
			Labels:      map[string]string{v1alpha1.LabelPackage: pkg.Name},
			Annotations: map[string]string{AnnotationDesiredDigest: digest},
		},
		Spec: *prev.Spec.DeepCopy(),
	}
	restored.Spec.Revision = n
	restored.Spec.RestoredFrom = prev.Spec.Revision
	if requested {
		restored.Annotations[AnnotationRollbackRequested] = "true"
	}
	if err := controllerutil.SetControllerReference(pkg, restored, r.Scheme()); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, restored); err != nil {
		return nil, err
	}
	restored.Status.Phase = v1alpha1.PhaseApplied
	restored.Status.Message = msg
	for _, c := range prev.Spec.Components {
		bc := backend.Component{Name: c.Name, ReleaseName: c.ReleaseName, Namespace: c.Namespace}
		s, err := r.Backend.Status(ctx, bc)
		if err != nil {
			return nil, err
		}
		setComponentStatus(restored, c.Name, s.Revision)
	}
	if err := r.Status().Update(ctx, restored); err != nil {
		return nil, err
	}
	for i := range revs {
		o := &revs[i]
		if o.Status.Phase == v1alpha1.PhaseApplied {
			o.Status.Phase = v1alpha1.PhaseSuperseded
			if err := r.Status().Update(ctx, o); err != nil {
				return nil, err
			}
		}
	}
	return restored, nil
}

// rollbackTo handles the rollback annotation: re-apply an earlier revision.
func (r *PackageReconciler) rollbackTo(ctx context.Context, pkg *v1alpha1.Package, value string, d *desiredState, revs *[]v1alpha1.PackageRevision) (ctrl.Result, error) {
	clear := func() error {
		patch := client.MergeFrom(pkg.DeepCopy())
		delete(pkg.Annotations, AnnotationRollbackTo)
		return r.Patch(ctx, pkg, patch)
	}
	n, err := strconv.ParseInt(value, 10, 64)
	var target *v1alpha1.PackageRevision
	for i := range *revs {
		if err == nil && (*revs)[i].Spec.Revision == n {
			target = &(*revs)[i]
		}
	}
	if target == nil || (target.Status.Phase != v1alpha1.PhaseApplied && target.Status.Phase != v1alpha1.PhaseSuperseded) {
		setReady(pkg, metav1.ConditionFalse, "RollbackFailed", fmt.Sprintf("revision %q is not a successfully applied revision", value))
		return ctrl.Result{}, clear()
	}
	var comps []desiredComponent
	for _, c := range target.Spec.Components {
		comps = append(comps, desiredComponent{snapshot: c, backend: backend.Component{Package: pkg.Name, Name: c.Name, ReleaseName: c.ReleaseName, Namespace: c.Namespace}})
	}
	// Components the current state has and the target does not are removed.
	inTarget := map[string]bool{}
	for _, c := range target.Spec.Components {
		inTarget[c.Name] = true
	}
	if cur := lastOf(*revs); cur != nil {
		for _, c := range cur.Spec.Components {
			if !inTarget[c.Name] {
				comps = append(comps, desiredComponent{snapshot: c, backend: backend.Component{Package: pkg.Name, Name: c.Name, ReleaseName: c.ReleaseName, Namespace: c.Namespace}})
			}
		}
	}
	if err := r.restoreComponents(ctx, target, comps); err != nil {
		setReady(pkg, metav1.ConditionFalse, "RollbackFailed", err.Error())
		return ctrl.Result{}, clear()
	}
	restored, err := r.recordRestored(ctx, pkg, target, d.digest, *revs, true, fmt.Sprintf("rolled back to revision %d on request", target.Spec.Revision))
	if err != nil {
		return ctrl.Result{}, err
	}
	*revs = append(*revs, *restored)
	pkg.Status.CurrentRevision = restored.Spec.Revision
	pkg.Status.Version = restored.Spec.Version
	setReady(pkg, metav1.ConditionFalse, v1alpha1.ReasonUpgradeRolledBack, restored.Status.Message)
	countRevision(pkg.Name, outcomeRolledBack)
	return ctrl.Result{}, clear()
}

// removeOrphans uninstalls components the previous revision had and this
// one does not.
func (r *PackageReconciler) removeOrphans(ctx context.Context, pkg *v1alpha1.Package, d *desiredState, revs []v1alpha1.PackageRevision, rev *v1alpha1.PackageRevision) error {
	prev := lastGood(revs, rev.Spec.Revision)
	if prev == nil {
		return nil
	}
	want := map[string]bool{}
	for _, c := range d.components {
		want[c.snapshot.Namespace+"/"+c.snapshot.ReleaseName] = true
	}
	for i := len(prev.Spec.Components) - 1; i >= 0; i-- {
		c := prev.Spec.Components[i]
		if want[c.Namespace+"/"+c.ReleaseName] {
			continue
		}
		if err := r.Backend.Uninstall(ctx, backend.Component{Package: pkg.Name, Name: c.Name, ReleaseName: c.ReleaseName, Namespace: c.Namespace}); err != nil {
			return err
		}
	}
	return nil
}

// finalize removes releases, newest components first, and handles CRDs.
func (r *PackageReconciler) finalize(ctx context.Context, pkg *v1alpha1.Package) error {
	forgetPackage(pkg.Name)
	if !controllerutil.ContainsFinalizer(pkg, FinalizerCleanup) {
		return nil
	}
	revs, err := r.revisions(ctx, pkg.Name)
	if err != nil {
		return err
	}
	if cur := lastOf(revs); cur != nil {
		for i := len(cur.Spec.Components) - 1; i >= 0; i-- {
			c := cur.Spec.Components[i]
			if err := r.Backend.Uninstall(ctx, backend.Component{Package: pkg.Name, Name: c.Name, ReleaseName: c.ReleaseName, Namespace: c.Namespace}); err != nil {
				return err
			}
		}
	}
	if r.CRDs != nil {
		src := &v1alpha1.PackageSource{}
		var crds []string
		if err := r.Get(ctx, types.NamespacedName{Name: pkg.Name}, src); err == nil {
			crds = src.Spec.CRDs
		}
		if err := r.CRDs.Release(ctx, pkg.Name, crds, pkg.Spec.CRDPolicy == v1alpha1.CRDPolicyDelete); err != nil {
			return err
		}
	}
	controllerutil.RemoveFinalizer(pkg, FinalizerCleanup)
	return r.Update(ctx, pkg)
}

// recordDependencies reports the readiness of each required package in
// status.dependencies.
func (r *PackageReconciler) recordDependencies(pkg *v1alpha1.Package, reqs []resolve.Requirement, st resolve.State) {
	deps := map[string]v1alpha1.DependencyStatus{}
	for _, q := range reqs {
		if q.Package == "" {
			continue
		}
		p, ok := st.Packages[q.Package]
		deps[q.Package] = v1alpha1.DependencyStatus{Ready: ok && p.Ready}
	}
	if len(deps) == 0 {
		deps = nil
	}
	pkg.Status.Dependencies = deps
}

// dependencyReleases lists the releases of required packages, so the flux
// backend can order HelmReleases across packages.
func (r *PackageReconciler) dependencyReleases(ctx context.Context, reqs []resolve.Requirement) ([]string, error) {
	var out []string
	for _, q := range reqs {
		if q.Package == "" {
			continue
		}
		dep := &v1alpha1.Package{}
		if err := r.Get(ctx, types.NamespacedName{Name: q.Package}, dep); err != nil {
			if apierrors.IsNotFound(err) && q.Optional {
				continue
			}
			return nil, client.IgnoreNotFound(err)
		}
		src := &v1alpha1.PackageSource{}
		if err := r.Get(ctx, types.NamespacedName{Name: q.Package}, src); err != nil {
			return nil, client.IgnoreNotFound(err)
		}
		v := findVariant(src, variantName(dep))
		if v == nil {
			continue
		}
		for _, c := range enabledComponents(dep, v) {
			out = append(out, c.Install.Namespace+"/"+releaseName(c))
		}
	}
	return out, nil
}

// reconcileNamespaces creates the namespaces components go to. A namespace
// becomes privileged when any package has a privileged component in it.
func (r *PackageReconciler) reconcileNamespaces(ctx context.Context, pkg *v1alpha1.Package, v *v1alpha1.Variant) error {
	targets := map[string]bool{}
	for _, c := range enabledComponents(pkg, v) {
		if c.Install.Namespace != "" {
			targets[c.Install.Namespace] = targets[c.Install.Namespace] || c.Install.Privileged
		}
	}
	if len(targets) == 0 {
		return nil
	}
	privileged, err := r.privilegedNamespaces(ctx, targets)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(targets))
	for n := range targets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		labels := map[string]string{}
		for k, v := range r.Profile.NamespaceLabels {
			labels[k] = v
		}
		if privileged[n] {
			labels["pod-security.kubernetes.io/enforce"] = "privileged"
		}
		if err := r.ensureNamespace(ctx, n, labels); err != nil {
			return fmt.Errorf("namespace %s: %w", n, err)
		}
	}
	return nil
}

// ensureNamespace creates a namespace or adds labels to it. It only adds:
// labels set by others, and labels kubepkg set earlier, are left alone.
func (r *PackageReconciler) ensureNamespace(ctx context.Context, name string, labels map[string]string) error {
	ns := &corev1.Namespace{}
	err := r.Get(ctx, types.NamespacedName{Name: name}, ns)
	if apierrors.IsNotFound(err) {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: name, Labels: labels, Annotations: map[string]string{"helm.sh/resource-policy": "keep"},
		}}
		return client.IgnoreAlreadyExists(r.Create(ctx, ns))
	}
	if err != nil {
		return err
	}
	patch := client.MergeFrom(ns.DeepCopy())
	changed := false
	for k, v := range labels {
		if ns.Labels[k] != v {
			if ns.Labels == nil {
				ns.Labels = map[string]string{}
			}
			ns.Labels[k] = v
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return r.Patch(ctx, ns, patch)
}

func (r *PackageReconciler) privilegedNamespaces(ctx context.Context, targets map[string]bool) (map[string]bool, error) {
	out := map[string]bool{}
	for n, p := range targets {
		out[n] = p
	}
	var pkgs v1alpha1.PackageList
	if err := r.List(ctx, &pkgs); err != nil {
		return nil, err
	}
	for i := range pkgs.Items {
		p := &pkgs.Items[i]
		src := &v1alpha1.PackageSource{}
		if err := r.Get(ctx, types.NamespacedName{Name: p.Name}, src); err != nil {
			continue
		}
		v := findVariant(src, variantName(p))
		if v == nil {
			continue
		}
		for _, c := range enabledComponents(p, v) {
			if _, relevant := targets[c.Install.Namespace]; relevant && c.Install.Privileged {
				out[c.Install.Namespace] = true
			}
		}
	}
	return out, nil
}

// SetupWithManager wires the watches. Any Package change re-queues the
// packages that are not ready, because they may be waiting on it; at
// platform scale that is a few dozen objects and simpler than an index.
func (r *PackageReconciler) SetupWithManager(mgr ctrl.Manager) error {
	sameName := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: o.GetName()}}}
	})
	waiting := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		var pkgs v1alpha1.PackageList
		if err := mgr.GetClient().List(ctx, &pkgs); err != nil {
			return nil
		}
		var out []reconcile.Request
		for _, p := range pkgs.Items {
			if p.Name != o.GetName() && !isReady(p.Status.Conditions) {
				out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: p.Name}})
			}
		}
		return out
	})
	b := ctrl.NewControllerManagedBy(mgr).
		Named("kubepkg-package").
		For(&v1alpha1.Package{}).
		Watches(&v1alpha1.PackageSource{}, sameName).
		Watches(&v1alpha1.Package{}, waiting)
	if r.Repositories != nil {
		b = b.Watches(&v1alpha1.Repository{}, allPackages(mgr.GetClient()))
	}
	return b.Complete(r)
}
