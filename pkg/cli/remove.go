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
	"sort"
	"strings"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kuberoot-dev/kubepkg/api/v1"
	"github.com/kuberoot-dev/kubepkg/pkg/resolve"
)

// Removal is one package a remove deletes, and why.
type Removal struct {
	Name   string
	Reason string
}

// installedPackage is what remove needs to know about a package.
type installedPackage struct {
	pkg      v1.Package
	provides []string
	requires []resolve.Requirement
}

// PlanRemove works out what removing names takes. It refuses while a
// package that stays requires one of them, directly or as the only
// provider of a capability. With autoremove it also removes packages that
// were installed as requirements and that nothing staying needs any more.
func PlanRemove(ctx context.Context, c client.Client, names []string, autoremove bool) ([]Removal, error) {
	installed, err := installedPackages(ctx, c)
	if err != nil {
		return nil, err
	}
	removing := map[string]string{}
	for _, n := range names {
		if _, ok := installed[n]; !ok {
			return nil, fmt.Errorf("package %s is not installed", n)
		}
		removing[n] = "requested"
	}
	if blockers := requiredBy(installed, removing); len(blockers) > 0 {
		return nil, fmt.Errorf("cannot remove: %s", strings.Join(blockers, "; "))
	}
	for autoremove {
		added := false
		for _, n := range sortedNames(installed) {
			p := installed[n]
			if _, gone := removing[n]; gone || p.pkg.Annotations[AnnotationDependency] == "" {
				continue
			}
			trial := map[string]string{n: ""}
			for k, v := range removing {
				trial[k] = v
			}
			if len(requiredBy(installed, trial)) == 0 {
				removing[n] = "no longer required (installed as a requirement: " + p.pkg.Annotations[AnnotationDependency] + ")"
				added = true
			}
		}
		if !added {
			break
		}
	}
	out := make([]Removal, 0, len(removing))
	for _, n := range sortedNames(installed) {
		if why, ok := removing[n]; ok {
			out = append(out, Removal{Name: n, Reason: why})
		}
	}
	return out, nil
}

func installedPackages(ctx context.Context, c client.Client) (map[string]installedPackage, error) {
	var list v1.PackageList
	if err := c.List(ctx, &list); err != nil {
		return nil, err
	}
	out := map[string]installedPackage{}
	for _, p := range list.Items {
		ip := installedPackage{pkg: p, provides: []string{p.Name}}
		src := &v1.PackageSource{}
		err := c.Get(ctx, types.NamespacedName{Name: p.Name}, src)
		switch {
		case err == nil:
			ip.provides = append(ip.provides, src.Spec.Provides...)
			variant := p.Spec.Variant
			if variant == "" {
				variant = "default"
			}
			for i := range src.Spec.Variants {
				if src.Spec.Variants[i].Name == variant {
					ip.requires = resolve.RequirementsOf(&src.Spec.Variants[i], p.Spec.IgnoreDependencies)
				}
			}
		case !apierrors.IsNotFound(err):
			return nil, err
		}
		out[p.Name] = ip
	}
	return out, nil
}

// requiredBy lists why the packages in removing cannot go: requirements
// of packages that stay which only removed packages satisfy.
func requiredBy(installed map[string]installedPackage, removing map[string]string) []string {
	var out []string
	for _, n := range sortedNames(installed) {
		if _, gone := removing[n]; gone {
			continue
		}
		for _, q := range installed[n].requires {
			if q.Optional {
				continue
			}
			if q.Package != "" {
				if _, gone := removing[q.Package]; gone {
					out = append(out, fmt.Sprintf("%s is required by %s", q.Package, n))
				}
				continue
			}
			var stay, leave []string
			for _, m := range sortedNames(installed) {
				for _, p := range installed[m].provides {
					if p != q.Capability {
						continue
					}
					if _, gone := removing[m]; gone {
						leave = append(leave, m)
					} else {
						stay = append(stay, m)
					}
				}
			}
			// A capability nobody declares is the cluster's own, e.g. an
			// API it serves; only declared providers can be taken away.
			if len(stay) == 0 && len(leave) > 0 {
				out = append(out, fmt.Sprintf("%s provides %s, which %s requires", strings.Join(leave, ", "), q.Capability, n))
			}
		}
	}
	return out
}

func sortedNames(m map[string]installedPackage) []string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func removeCmd(cl *cluster) *cobra.Command {
	var autoremove, yes bool
	cmd := &cobra.Command{
		Use:   "remove <package>...",
		Short: "Remove packages that nothing else requires",
		Long: `Remove deletes the Packages; the operator uninstalls their releases. It
refuses while a package that stays requires one of them, directly or as
the only provider of a capability. With --autoremove it also removes
packages that were installed as requirements and that nothing needs any
more. CRDs a package owns stay unless its crdPolicy is Delete.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := cl.client()
			if err != nil {
				return err
			}
			plan, err := PlanRemove(cmd.Context(), c, args, autoremove)
			if err != nil {
				return err
			}
			for _, r := range plan {
				fmt.Fprintf(cmd.OutOrStdout(), "remove  %s  (%s)\n", r.Name, r.Reason)
			}
			if !yes {
				fmt.Fprint(cmd.OutOrStdout(), "Remove? [y/N] ")
				line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
					return fmt.Errorf("aborted")
				}
			}
			for _, r := range plan {
				if err := c.Delete(cmd.Context(), &v1.Package{ObjectMeta: objectMeta(r.Name)}); err != nil && !apierrors.IsNotFound(err) {
					return err
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%d packages removed; the operator uninstalls them\n", len(plan))
			return nil
		},
	}
	cmd.Flags().BoolVar(&autoremove, "autoremove", false, "also remove requirements nothing needs any more")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "remove without asking")
	return cmd
}

func objectMeta(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name} }
