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
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/tym83/kubepkg/api/v1"
)

func historyCmd(cl *cluster) *cobra.Command {
	return &cobra.Command{
		Use:   "history <package>",
		Short: "Show the revisions of a package",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := cl.client()
			if err != nil {
				return err
			}
			var revs v1.PackageRevisionList
			if err := c.List(cmd.Context(), &revs, client.MatchingLabels{v1.LabelPackage: args[0]}); err != nil {
				return err
			}
			if len(revs.Items) == 0 {
				return fmt.Errorf("package %s has no revisions", args[0])
			}
			sort.Slice(revs.Items, func(i, j int) bool { return revs.Items[i].Spec.Revision < revs.Items[j].Spec.Revision })
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "REVISION\tVERSION\tPHASE\tCREATED\tNOTE")
			for _, r := range revs.Items {
				note := strings.Join(strings.Fields(r.Status.Message), " ")
				if r.Spec.RestoredFrom != 0 {
					note = fmt.Sprintf("restored from %d", r.Spec.RestoredFrom)
				}
				fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", r.Spec.Revision, r.Spec.Version, r.Status.Phase,
					r.CreationTimestamp.UTC().Format("2006-01-02 15:04:05"), note)
			}
			return w.Flush()
		},
	}
}
