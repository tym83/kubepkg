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
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/tym83/kubepkg/api/v1alpha1"
)

// revisions lists a package's revisions, oldest first.
func (r *PackageReconciler) revisions(ctx context.Context, pkg string) ([]v1alpha1.PackageRevision, error) {
	var list v1alpha1.PackageRevisionList
	if err := r.List(ctx, &list, client.MatchingLabels{v1alpha1.LabelPackage: pkg}); err != nil {
		return nil, err
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Spec.Revision < list.Items[j].Spec.Revision })
	return list.Items, nil
}

func lastOf(revs []v1alpha1.PackageRevision) *v1alpha1.PackageRevision {
	if len(revs) == 0 {
		return nil
	}
	return &revs[len(revs)-1]
}

func historyLimit(pkg *v1alpha1.Package) int {
	if pkg.Spec.RevisionHistoryLimit != nil && *pkg.Spec.RevisionHistoryLimit > 0 {
		return int(*pkg.Spec.RevisionHistoryLimit)
	}
	return defaultHistoryLimit
}

// prune deletes the oldest revisions beyond the limit, but never the newest
// successfully applied one: it is what a failed upgrade rolls back to.
func (r *PackageReconciler) prune(ctx context.Context, pkg *v1alpha1.Package, revs *[]v1alpha1.PackageRevision) error {
	limit := historyLimit(pkg)
	if len(*revs) <= limit {
		return nil
	}
	keep := lastGood(*revs, 1<<62)
	excess := len(*revs) - limit
	var kept []v1alpha1.PackageRevision
	for i := range *revs {
		rev := &(*revs)[i]
		if excess > 0 && (keep == nil || rev.Spec.Revision != keep.Spec.Revision) {
			if err := r.Delete(ctx, rev); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			excess--
			continue
		}
		kept = append(kept, *rev)
	}
	*revs = kept
	return nil
}

// recordHistory summarises recent revisions in the Package status.
func (r *PackageReconciler) recordHistory(pkg *v1alpha1.Package, revs *[]v1alpha1.PackageRevision) {
	var h []v1alpha1.RevisionSummary
	for i := len(*revs) - 1; i >= 0 && len(h) < defaultHistoryLimit; i-- {
		rev := (*revs)[i]
		h = append(h, v1alpha1.RevisionSummary{Revision: rev.Spec.Revision, Version: rev.Spec.Version, Phase: rev.Status.Phase})
	}
	pkg.Status.History = h
}
