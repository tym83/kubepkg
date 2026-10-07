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
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/tym83/kubepkg/api/v1"
	"github.com/tym83/kubepkg/pkg/backend"
	"github.com/tym83/kubepkg/pkg/source"
)

// runHooks runs the pre-upgrade hooks of a revision that moves the
// package to another version. done means the reconcile stops here: a
// hook is still running, or one failed and the revision with it.
func (r *PackageReconciler) runHooks(ctx context.Context, pkg *v1.Package, rev *v1.PackageRevision, d *desiredState, revs *[]v1.PackageRevision) (ctrl.Result, bool, error) {
	if len(d.hooks) == 0 || rev.Annotations[AnnotationHooksDone] == "true" || rev.Spec.RestoredFrom != 0 {
		return ctrl.Result{}, false, nil
	}
	prev := lastGood(*revs, rev.Spec.Revision)
	if prev == nil || prev.Spec.Version == rev.Spec.Version {
		return ctrl.Result{}, false, nil // an install or a change within a version
	}
	for _, h := range d.hooks {
		bc := h.backend
		bc.Values = source.MergeValues(bc.Values, map[string]any{
			"kubepkg": map[string]any{"fromVersion": prev.Spec.Version, "toVersion": rev.Spec.Version},
		})
		s, err := r.Backend.Apply(ctx, bc)
		if err != nil || s.Failed {
			msg := s.Message
			if err != nil {
				msg = err.Error()
			}
			r.uninstallHooks(ctx, d)
			res, ferr := r.fail(ctx, pkg, rev, d, nil, revs, fmt.Sprintf("pre-upgrade hook %s failed: %s", h.snapshot.Name, msg))
			return res, true, ferr
		}
		if !s.Ready {
			setReady(pkg, metav1.ConditionFalse, v1.ReasonProgressing,
				fmt.Sprintf("applying revision %d: running pre-upgrade hook %s", rev.Spec.Revision, h.snapshot.Name))
			return ctrl.Result{RequeueAfter: progressRequeue}, true, nil
		}
	}
	if rev.Annotations == nil {
		rev.Annotations = map[string]string{}
	}
	rev.Annotations[AnnotationHooksDone] = "true"
	if err := r.Update(ctx, rev); err != nil {
		return ctrl.Result{}, true, err
	}
	return ctrl.Result{}, false, nil
}

// uninstallHooks removes hook releases, so the next upgrade runs them
// afresh; a hook that is already gone is fine.
func (r *PackageReconciler) uninstallHooks(ctx context.Context, d *desiredState) {
	for _, h := range d.hooks {
		// A delivery tool still removing the hook finishes on its own.
		if err := r.Backend.Uninstall(ctx, h.backend); err != nil && !errors.Is(err, backend.ErrUninstalling) {
			log.FromContext(ctx).Error(err, "uninstall pre-upgrade hook", "component", h.snapshot.Name)
		}
	}
}
