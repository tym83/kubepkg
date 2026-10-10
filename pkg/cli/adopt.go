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
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kuberoot-dev/kubepkg/api/v1"
	"github.com/kuberoot-dev/kubepkg/pkg/backend/helm"
	"github.com/kuberoot-dev/kubepkg/pkg/controller"
	"github.com/kuberoot-dev/kubepkg/pkg/repo"
)

// ReleaseReader reads existing Helm releases.
type ReleaseReader interface {
	Release(namespace, name string) (*helm.ReleaseInfo, error)
}

// AdoptComponent is what adopting finds for one component.
type AdoptComponent struct {
	Name, Namespace, Release string
	// Overridden is true when the release is not where the package would
	// put it, so the Package must say where it is.
	Overridden bool
	Existing   *helm.ReleaseInfo
}

// AdoptPlan is what adopting a package would do.
type AdoptPlan struct {
	Package    string
	Repository string
	Version    repo.Version
	Variant    string
	Components []AdoptComponent
	// Missing are required packages the cluster does not have.
	Missing []string
}

// PlanAdopt finds the release each component of the package's chosen
// version would be, existing or not. placements maps component names to
// namespace/release where the release is not where the package puts it.
func PlanAdopt(ctx context.Context, store *repo.Store, policy repo.Policy, releases ReleaseReader, installed map[string]bool, name, constraint, variant string, placements map[string]string) (*AdoptPlan, error) {
	sel, err := store.Select(ctx, name, constraint, "", policy)
	if err != nil {
		return nil, err
	}
	if variant == "" {
		variant = "default"
	}
	var v *v1.Variant
	for i := range sel.Version.Spec.Variants {
		if sel.Version.Spec.Variants[i].Name == variant {
			v = &sel.Version.Spec.Variants[i]
		}
	}
	if v == nil {
		return nil, fmt.Errorf("%s %s has no variant %s", name, sel.Version.Version, variant)
	}
	p := &AdoptPlan{Package: name, Repository: sel.Repository, Version: sel.Version, Variant: variant}
	known := map[string]bool{}
	for _, c := range v.Components {
		known[c.Name] = true
		if c.Install != nil && c.Install.Phase == v1.PhasePreUpgrade {
			continue
		}
		ac := AdoptComponent{Name: c.Name, Release: c.Name}
		if c.Install != nil {
			ac.Namespace = c.Install.Namespace
			if c.Install.ReleaseName != "" {
				ac.Release = c.Install.ReleaseName
			}
		}
		if where, ok := placements[c.Name]; ok {
			ns, rel, ok := strings.Cut(where, "/")
			if !ok || ns == "" || rel == "" {
				return nil, fmt.Errorf("component %s: --component wants name=namespace/release, got %q", c.Name, where)
			}
			ac.Overridden = ns != ac.Namespace || rel != ac.Release
			ac.Namespace, ac.Release = ns, rel
		}
		if ac.Existing, err = releases.Release(ac.Namespace, ac.Release); err != nil {
			return nil, err
		}
		if ac.Existing != nil && ac.Existing.ManagedBy != "" && ac.Existing.ManagedBy != name {
			return nil, fmt.Errorf("release %s/%s belongs to package %s", ac.Namespace, ac.Release, ac.Existing.ManagedBy)
		}
		p.Components = append(p.Components, ac)
	}
	for c := range placements {
		if !known[c] {
			return nil, fmt.Errorf("%s %s has no component %s", name, sel.Version.Version, c)
		}
	}
	for _, r := range v.Requires {
		if r.Package != "" && !installed[r.Package] && !r.Optional {
			p.Missing = append(p.Missing, r.Package)
		}
	}
	for _, d := range v.DependsOn {
		if !installed[d] {
			p.Missing = append(p.Missing, d)
		}
	}
	return p, nil
}

// Package is the Package that adopts, carrying each release's values.
func (p *AdoptPlan) PackageObject(constraint string) (*v1.Package, error) {
	if constraint == "" {
		constraint = defaultConstraint(p.Version.Version)
	}
	pkg := &v1.Package{
		ObjectMeta: metav1.ObjectMeta{Name: p.Package, Annotations: map[string]string{controller.AnnotationAdopt: "true"}},
		Spec:       v1.PackageSpec{Version: constraint, Repository: p.Repository},
	}
	if p.Variant != "default" {
		pkg.Spec.Variant = p.Variant
	}
	for _, c := range p.Components {
		var pc v1.PackageComponent
		set := false
		if c.Overridden {
			pc.Namespace, pc.ReleaseName, set = c.Namespace, c.Release, true
		}
		if c.Existing != nil && len(c.Existing.Values) > 0 {
			raw, err := json.Marshal(c.Existing.Values)
			if err != nil {
				return nil, err
			}
			pc.Values, set = &apiextensionsv1.JSON{Raw: raw}, true
		}
		if set {
			if pkg.Spec.Components == nil {
				pkg.Spec.Components = map[string]v1.PackageComponent{}
			}
			pkg.Spec.Components[c.Name] = pc
		}
	}
	return pkg, nil
}

func (p *AdoptPlan) print(w io.Writer) {
	fmt.Fprintf(w, "adopt %s %s build %d from %s\n\n", p.Package, p.Version.Version, p.Version.Build, p.Repository)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "COMPONENT\tRELEASE\tFOUND\tTHEN")
	for _, c := range p.Components {
		found, then := "nothing", "install; objects that already exist are taken over"
		if e := c.Existing; e != nil {
			found = fmt.Sprintf("%s %s (app %s), revision %d, %s", e.Chart, e.ChartVersion, e.AppVersion, e.Revision, e.Status)
			then = "upgrade in place to the package's chart"
			if n := len(e.Values); n > 0 {
				then += fmt.Sprintf(", keeping its %d top-level values", n)
			}
		}
		fmt.Fprintf(tw, "%s\t%s/%s\t%s\t%s\n", c.Name, c.Namespace, c.Release, found, then)
	}
	_ = tw.Flush()
	if len(p.Missing) > 0 {
		fmt.Fprintf(w, "\nrequires %s, which the cluster does not have: adopt or install it first, or the package waits\n", strings.Join(p.Missing, ", "))
	}
	fmt.Fprintln(w, "\nValues are carried over as the releases have them; check they suit the package's charts.")
}

func adoptCmd(cl *cluster) *cobra.Command {
	var (
		variant    string
		components []string
		yes        bool
	)
	cmd := &cobra.Command{
		Use:   "adopt <package>[@constraint]",
		Short: "Take what is already installed under kubepkg's management, without reinstalling it",
		Long: `Adopt manages software that was installed before kubepkg, with Helm or by
applying manifests. It finds the Helm release each component of the
package would be, by its release name and namespace or where --component
says, shows what it found, and writes a Package that carries each
release's current values. The operator then upgrades the releases in
place to the package's charts and takes over objects that exist without
a Helm release; nothing is reinstalled. Taking over applies to that first
revision only.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name, constraint, _ := strings.Cut(args[0], "@")
			placements := map[string]string{}
			for _, c := range components {
				k, v, ok := strings.Cut(c, "=")
				if !ok {
					return fmt.Errorf("--component wants name=namespace/release, got %q", c)
				}
				placements[k] = v
			}
			c, err := cl.client()
			if err != nil {
				return err
			}
			existing := &v1.Package{}
			if err := c.Get(ctx, types.NamespacedName{Name: name}, existing); err == nil {
				return fmt.Errorf("package %s is managed already", name)
			} else if !apierrors.IsNotFound(err) {
				return err
			}
			store, err := loadStore(ctx, c, cl.fetchers, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			cfg, err := cl.config()
			if err != nil {
				return err
			}
			releases, err := helm.New(cfg)
			if err != nil {
				return err
			}
			installed, err := installedNames(ctx, c)
			if err != nil {
				return err
			}
			plan, err := PlanAdopt(ctx, store, cl.policy, releases, installed, name, constraint, variant, placements)
			if err != nil {
				return err
			}
			plan.print(cmd.OutOrStdout())
			if !yes {
				fmt.Fprintln(cmd.OutOrStdout(), "\nNothing changed; run again with --yes to adopt.")
				return nil
			}
			pkg, err := plan.PackageObject(constraint)
			if err != nil {
				return err
			}
			if err := c.Create(ctx, pkg); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\npackage %s adopted; follow it with \"list\"\n", name)
			return nil
		},
	}
	cmd.Flags().StringVar(&variant, "variant", "", "variant to adopt (default: default)")
	cmd.Flags().StringArrayVar(&components, "component", nil, "where a component's release is, name=namespace/release (repeatable)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "adopt; without it, only show the plan")
	return cmd
}

func installedNames(ctx context.Context, c client.Client) (map[string]bool, error) {
	var list v1.PackageList
	if err := c.List(ctx, &list); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, p := range list.Items {
		out[p.Name] = true
	}
	return out, nil
}
