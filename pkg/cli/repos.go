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
	"context"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/tym83/kubepkg/api/v1beta1"
	"github.com/tym83/kubepkg/pkg/repo"
)

func repoAddCmd(cl *cluster) *cobra.Command {
	var (
		priority int32
		interval time.Duration
		keyFiles []string
		rootKeys []string
		rootTh   int32
	)
	cmd := &cobra.Command{
		Use:   "add <name> <index-url>",
		Short: "Subscribe the cluster to a package repository",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := cl.client()
			if err != nil {
				return err
			}
			var keys []string
			for _, f := range keyFiles {
				raw, err := os.ReadFile(f)
				if err != nil {
					return err
				}
				keys = append(keys, string(raw))
			}
			spec := v1beta1.RepositorySpec{URL: args[1], Priority: priority, PublicKeys: keys}
			if len(rootKeys) > 0 {
				rk, err := readKeys(rootKeys)
				if err != nil {
					return err
				}
				spec.Trust = &v1beta1.RepositoryTrust{RootKeys: rk, RootThreshold: rootTh}
			}
			if _, _, _, err := repo.LoadRepository(cmd.Context(), cl.fetchers, spec, v1beta1.RepositoryStatus{}, time.Now()); err != nil {
				return fmt.Errorf("index at %s: %w", args[1], err)
			}
			rp := &v1beta1.Repository{ObjectMeta: metav1.ObjectMeta{Name: args[0]}, Spec: spec}
			if interval > 0 {
				rp.Spec.Interval = &metav1.Duration{Duration: interval}
			}
			if err := c.Create(cmd.Context(), rp); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "repository %s added\n", args[0])
			return nil
		},
	}
	cmd.Flags().Int32Var(&priority, "priority", 0, "the highest priority repository carrying a package supplies it")
	cmd.Flags().DurationVar(&interval, "interval", 0, "how often the operator refreshes the index (default 10m)")
	cmd.Flags().StringArrayVar(&keyFiles, "public-key", nil, "trust only an index signed with this ed25519 public key file (repeatable)")
	cmd.Flags().StringArrayVar(&rootKeys, "root-key", nil, "pin this root key of a repository with a root of trust (repeatable)")
	cmd.Flags().Int32Var(&rootTh, "root-threshold", 1, "how many pinned root keys must have signed version 1 of the root")
	return cmd
}

func repoListCmd(cl *cluster) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the repositories the cluster uses",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := cl.client()
			if err != nil {
				return err
			}
			var list v1beta1.RepositoryList
			if err := c.List(cmd.Context(), &list); err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tPRIORITY\tPACKAGES\tREADY\tURL")
			for _, r := range list.Items {
				ready := "Unknown"
				if cond := meta.FindStatusCondition(r.Status.Conditions, "Ready"); cond != nil {
					ready = string(cond.Status)
					if cond.Status != metav1.ConditionTrue {
						ready += " (" + cond.Reason + ")"
					}
				}
				fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%s\n", r.Name, r.Spec.Priority, r.Status.Packages, ready, r.Spec.URL)
			}
			return w.Flush()
		},
	}
}

func repoRemoveCmd(cl *cluster) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Unsubscribe the cluster from a repository; installed packages stay",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := cl.client()
			if err != nil {
				return err
			}
			if err := c.Delete(cmd.Context(), &v1beta1.Repository{ObjectMeta: metav1.ObjectMeta{Name: args[0]}}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "repository %s removed\n", args[0])
			return nil
		},
	}
}

// loadStore fetches the index of every Repository in the cluster, the way
// the operator does, so plans are made from the same versions. A
// repository that cannot be read is reported and left out.
func loadStore(ctx context.Context, c client.Client, fetchers repo.Fetchers, warn io.Writer) (*repo.Store, error) {
	var list v1beta1.RepositoryList
	if err := c.List(ctx, &list); err != nil {
		return nil, err
	}
	if len(list.Items) == 0 {
		return nil, fmt.Errorf("the cluster uses no repositories; add one with \"repo add\"")
	}
	store := repo.NewStore()
	for _, r := range list.Items {
		idx, _, _, err := repo.LoadRepository(ctx, fetchers, r.Spec, r.Status, time.Now())
		if err != nil {
			fmt.Fprintf(warn, "warning: repository %s left out: %v\n", r.Name, err)
			continue
		}
		for _, note := range idx.LeftOut() {
			fmt.Fprintf(warn, "warning: repository %s has %s\n", r.Name, note)
		}
		store.Set(r.Name, r.Spec.Priority, idx)
	}
	return store, nil
}
