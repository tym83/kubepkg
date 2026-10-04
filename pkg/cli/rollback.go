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

package cli

import (
	"fmt"
	"sort"
	"strconv"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/tym83/kubepkg/api/v1alpha1"
	"github.com/tym83/kubepkg/pkg/controller"
)

func rollbackCmd(cl *cluster) *cobra.Command {
	var to int64
	cmd := &cobra.Command{
		Use:   "rollback <package>",
		Short: "Re-apply an earlier revision of a package",
		Long: `Rollback asks the operator to re-apply an earlier successfully applied
revision. Without --to it picks the newest applied revision before the
current one. The operator records the result as a new revision.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := cl.client()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			var pkg v1alpha1.Package
			if err := c.Get(ctx, client.ObjectKey{Name: args[0]}, &pkg); err != nil {
				return err
			}
			if to == 0 {
				var revs v1alpha1.PackageRevisionList
				if err := c.List(ctx, &revs, client.MatchingLabels{v1alpha1.LabelPackage: pkg.Name}); err != nil {
					return err
				}
				if to = previousApplied(revs.Items, pkg.Status.CurrentRevision); to == 0 {
					return fmt.Errorf("package %s has no earlier applied revision", pkg.Name)
				}
			}
			patch := client.MergeFrom(pkg.DeepCopy())
			if pkg.Annotations == nil {
				pkg.Annotations = map[string]string{}
			}
			pkg.Annotations[controller.AnnotationRollbackTo] = strconv.FormatInt(to, 10)
			if err := c.Patch(ctx, &pkg, patch); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "package %s: rollback to revision %d requested\n", pkg.Name, to)
			return nil
		},
	}
	cmd.Flags().Int64Var(&to, "to", 0, "revision to re-apply")
	return cmd
}

// previousApplied returns the newest revision older than current that was
// applied successfully, or 0.
func previousApplied(revs []v1alpha1.PackageRevision, current int64) int64 {
	sort.Slice(revs, func(i, j int) bool { return revs[i].Spec.Revision > revs[j].Spec.Revision })
	for _, r := range revs {
		if r.Spec.Revision < current && (r.Status.Phase == v1alpha1.PhaseApplied || r.Status.Phase == v1alpha1.PhaseSuperseded) {
			return r.Spec.Revision
		}
	}
	return 0
}
