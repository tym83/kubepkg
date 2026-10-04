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
	"bufio"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/Masterminds/semver/v3"
	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/tym83/kubepkg/api/v1alpha1"
	"github.com/tym83/kubepkg/pkg/controller"
	"github.com/tym83/kubepkg/pkg/repo"
	"github.com/tym83/kubepkg/pkg/resolve"
)

// AnnotationDependency marks a Package the CLI installed because another
// package required it, not because it was asked for.
const AnnotationDependency = "kubepkg.dev/dependency"

// Step is one line of an install plan.
type Step struct {
	resolve.Change
	// Repository and Entry are where the target version comes from.
	Repository string
	Entry      repo.Version
	// Constraint is what Package.spec.version is set to.
	Constraint string
	Dependency bool
	// Local means a hand-written PackageSource decides, not repositories.
	Local bool
}

// Plan works out what installing reqs takes: the requested packages and
// everything they require, from the cluster's repositories, as the
// operator would select them.
func Plan(ctx context.Context, c client.Client, store *repo.Store, policy repo.Policy, apis controller.APIs, variant string, reqs []resolve.Request) ([]Step, error) {
	st, err := controller.ClusterState(ctx, c, apis)
	if err != nil {
		return nil, err
	}
	p, err := resolve.Resolve(store.Catalog(ctx, variant, policy), st, reqs)
	if err != nil {
		return nil, err
	}
	asked := map[string]string{}
	for _, r := range reqs {
		asked[r.Name] = r.Version
	}
	var out []Step
	for _, ch := range p.Changes {
		s := Step{Change: ch}
		constraint, requested := asked[ch.Name]
		s.Dependency = !requested
		if requested && constraint != "" {
			s.Constraint = constraint
		} else {
			s.Constraint = defaultConstraint(ch.To)
		}
		if repoName, versions, ok := store.Offered(ch.Name, ""); ok {
			s.Repository = repoName
			for _, v := range versions {
				if v.Version == ch.To && policy.AdmitVersion(ctx, repoName, ch.Name, v) == nil {
					s.Entry = v
					break
				}
			}
		}
		src := &v1alpha1.PackageSource{}
		if err := c.Get(ctx, types.NamespacedName{Name: ch.Name}, src); err == nil {
			s.Local = src.Labels[v1alpha1.LabelRepository] == ""
		} else if !apierrors.IsNotFound(err) {
			return nil, err
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Dependency && !out[j].Dependency })
	return out, nil
}

// defaultConstraint follows patch releases of a version: ~X.Y.
func defaultConstraint(version string) string {
	v, err := semver.NewVersion(version)
	if err != nil || version == resolve.Unversioned {
		return ""
	}
	return fmt.Sprintf("~%d.%d", v.Major(), v.Minor())
}

// Apply creates or updates the Packages of a plan. Kept packages are left
// alone; the operator does the installing.
func Apply(ctx context.Context, c client.Client, steps []Step) error {
	for _, s := range steps {
		if s.Action == resolve.ActionKeep {
			continue
		}
		pkg := &v1alpha1.Package{}
		err := c.Get(ctx, types.NamespacedName{Name: s.Name}, pkg)
		switch {
		case apierrors.IsNotFound(err):
			pkg = &v1alpha1.Package{ObjectMeta: metav1.ObjectMeta{Name: s.Name}, Spec: v1alpha1.PackageSpec{Version: s.Constraint}}
			if s.Dependency {
				pkg.Annotations = map[string]string{AnnotationDependency: s.Reason}
			}
			if err := c.Create(ctx, pkg); err != nil {
				return fmt.Errorf("create package %s: %w", s.Name, err)
			}
		case err != nil:
			return err
		case pkg.Spec.Version != s.Constraint:
			patch := client.MergeFrom(pkg.DeepCopy())
			pkg.Spec.Version = s.Constraint
			if err := c.Patch(ctx, pkg, patch); err != nil {
				return fmt.Errorf("update package %s: %w", s.Name, err)
			}
		}
	}
	return nil
}

// WritePlan prints a plan with what each change brings into the cluster.
func WritePlan(w io.Writer, steps []Step) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ACTION\tPACKAGE\tFROM\tTO\tREPOSITORY\tWHY")
	for _, s := range steps {
		to := s.To
		if s.Entry.Build > 0 {
			to = fmt.Sprintf("%s build %d", s.To, s.Entry.Build)
		}
		repoName := s.Repository
		if s.Local {
			repoName = "(local PackageSource)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", s.Action, s.Name, dash(s.From), to, dash(repoName), s.Reason)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, s := range steps {
		if s.Action == resolve.ActionKeep {
			continue
		}
		var notes []string
		spec := s.Entry.Spec
		if len(spec.CRDs) > 0 {
			notes = append(notes, "owns CRDs: "+strings.Join(spec.CRDs, ", "))
		}
		if spec.Permissions != nil && spec.Permissions.ClusterWide {
			notes = append(notes, "needs cluster-wide permissions")
		}
		if spec.Rollback == nil || !spec.Rollback.Safe {
			notes = append(notes, "not rollback-safe: a failed upgrade is fixed forward, not rolled back")
		}
		if s.Action == resolve.ActionDowngrade {
			notes = append(notes, "DOWNGRADE from "+s.From)
		}
		if s.Local {
			notes = append(notes, "a hand-written PackageSource decides the version, not the repositories")
		}
		for _, n := range notes {
			fmt.Fprintf(w, "  %s: %s\n", s.Name, n)
		}
	}
	return nil
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func parseRequests(args []string) []resolve.Request {
	out := make([]resolve.Request, 0, len(args))
	for _, a := range args {
		name, version, _ := strings.Cut(a, "@")
		out = append(out, resolve.Request{Name: name, Version: version})
	}
	return out
}

type planFlags struct {
	variant string
}

func (cl *cluster) plan(ctx context.Context, f planFlags, args []string, stderr io.Writer) (client.Client, []Step, error) {
	c, err := cl.client()
	if err != nil {
		return nil, nil, err
	}
	cfg, err := cl.config()
	if err != nil {
		return nil, nil, err
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	store, err := loadStore(ctx, c, cl.fetchers, stderr)
	if err != nil {
		return nil, nil, err
	}
	steps, err := Plan(ctx, c, store, cl.policy, &controller.DiscoveryAPIs{Client: dc}, f.variant, parseRequests(args))
	return c, steps, err
}

func planCmd(cl *cluster) *cobra.Command {
	var f planFlags
	cmd := &cobra.Command{
		Use:   "plan <package>[@constraint]...",
		Short: "Show what installing or upgrading packages would change, without changing anything",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, steps, err := cl.plan(cmd.Context(), f, args, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			return WritePlan(cmd.OutOrStdout(), steps)
		},
	}
	cmd.Flags().StringVar(&f.variant, "variant", "", "variant whose requirements are resolved (default: default)")
	return cmd
}

func installCmd(cl *cluster) *cobra.Command {
	var (
		f   planFlags
		yes bool
	)
	cmd := &cobra.Command{
		Use:   "install <package>[@constraint]...",
		Short: "Install or upgrade packages together with what they require",
		Long: `Install resolves the packages and everything they require against the
cluster's repositories, shows the plan, and on confirmation creates or
updates one Package per package; the operator does the rest. Without a
constraint a package follows patch releases of the version chosen (~X.Y).
Packages installed because another one required them are marked with the
kubepkg.dev/dependency annotation.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, steps, err := cl.plan(cmd.Context(), f, args, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if err := WritePlan(cmd.OutOrStdout(), steps); err != nil {
				return err
			}
			changes := 0
			for _, s := range steps {
				if s.Action != resolve.ActionKeep {
					changes++
				}
			}
			if changes == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "nothing to do")
				return nil
			}
			if !yes {
				fmt.Fprint(cmd.OutOrStdout(), "Apply? [y/N] ")
				line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
					return fmt.Errorf("aborted")
				}
			}
			if err := Apply(cmd.Context(), c, steps); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%d packages requested; follow them with \"list\"\n", changes)
			return nil
		},
	}
	cmd.Flags().StringVar(&f.variant, "variant", "", "variant whose requirements are resolved (default: default)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "apply without asking")
	return cmd
}

func searchCmd(cl *cluster) *cobra.Command {
	return &cobra.Command{
		Use:   "search [term]",
		Short: "Search the packages in the cluster's repositories",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := cl.client()
			if err != nil {
				return err
			}
			store, err := loadStore(cmd.Context(), c, cl.fetchers, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			term := ""
			if len(args) == 1 {
				term = strings.ToLower(args[0])
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tLATEST\tREPOSITORY\tDESCRIPTION")
			for _, name := range store.Packages() {
				desc, _ := store.Describe(name)
				if term != "" && !strings.Contains(strings.ToLower(name+" "+desc), term) {
					continue
				}
				repoName, versions, _ := store.Offered(name, "")
				latest := "-"
				if len(versions) > 0 {
					latest = versions[0].Version
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", name, latest, repoName, desc)
			}
			return tw.Flush()
		},
	}
}
