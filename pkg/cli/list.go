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
	"text/tabwriter"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/meta"

	"github.com/tym83/kubepkg/api/v1alpha1"
)

func listCmd(cl *cluster) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List installed packages with their versions, revisions and readiness",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := cl.client()
			if err != nil {
				return err
			}
			var pkgs v1alpha1.PackageList
			if err := c.List(cmd.Context(), &pkgs); err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tVERSION\tREVISION\tREADY\tREASON")
			for _, p := range pkgs.Items {
				ready, reason := "Unknown", ""
				if cond := meta.FindStatusCondition(p.Status.Conditions, "Ready"); cond != nil {
					ready, reason = string(cond.Status), cond.Reason
				}
				fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n", p.Name, p.Status.Version, p.Status.CurrentRevision, ready, reason)
			}
			return w.Flush()
		},
	}
}
