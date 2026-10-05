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
	"bytes"
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/tym83/kubepkg/api/v1alpha1"
	"github.com/tym83/kubepkg/pkg/backend"
	"github.com/tym83/kubepkg/pkg/backend/argo"
	"github.com/tym83/kubepkg/pkg/backend/flux"
	"github.com/tym83/kubepkg/pkg/controller"
	"github.com/tym83/kubepkg/pkg/repo"
	"github.com/tym83/kubepkg/pkg/resolve"
)

// RenderFormats are the formats Render writes.
var RenderFormats = []string{"flux", "argo", "helmfile"}

// RenderOptions tune the output of Render.
type RenderOptions struct {
	Format  string
	Variant string
	// ArgoNamespace and ArgoProject place Applications (argo format).
	ArgoNamespace, ArgoProject string
	// Insecure lets Flux pull from registries over plain HTTP.
	Insecure bool
}

// Render resolves the requested packages and their requirements for an
// empty cluster and writes what another delivery tool needs to install
// them, in the order kubepkg would: Flux sources and HelmReleases with
// dependsOn, Argo CD Applications with sync waves, or a helmfile with
// needs. Requirements on APIs no package provides are assumed served.
func Render(ctx context.Context, store *repo.Store, policy repo.Policy, reqs []resolve.Request, o RenderOptions) ([]byte, error) {
	cat := store.Catalog(ctx, o.Variant, policy)
	st := resolve.State{Packages: map[string]resolve.Installed{}, APIs: assumedAPIs(cat)}
	p, err := resolve.Resolve(cat, st, reqs)
	if err != nil {
		return nil, err
	}
	chosen := map[string]resolve.Release{}
	for _, ch := range p.Changes {
		for _, r := range cat[ch.Name] {
			if r.Version == ch.To {
				chosen[ch.Name] = r
			}
		}
	}
	order, providers, err := packageOrder(chosen)
	if err != nil {
		return nil, err
	}

	releases := map[string][]string{} // package -> its releases, namespace/name
	var comps []backend.Component
	for _, name := range order {
		spec, err := admittedSpec(ctx, store, policy, name, chosen[name].Version)
		if err != nil {
			return nil, err
		}
		pc, err := renderComponents(name, spec, o.Variant, requiredPackages(chosen[name], providers), releases)
		if err != nil {
			return nil, err
		}
		comps = append(comps, pc...)
	}

	var docs [][]byte
	add := func(v any) error {
		raw, err := cleanYAML(v)
		if err != nil {
			return err
		}
		docs = append(docs, raw)
		return nil
	}
	switch o.Format {
	case "flux":
		b := &flux.Backend{Insecure: o.Insecure}
		for _, c := range comps {
			src, err := b.RenderSource(c)
			if err != nil {
				return nil, err
			}
			hr, err := b.Render(c)
			if err != nil {
				return nil, err
			}
			if err := add(src.Object); err != nil {
				return nil, err
			}
			if err := add(hr); err != nil {
				return nil, err
			}
		}
	case "argo":
		b := &argo.Backend{Namespace: o.ArgoNamespace, Project: o.ArgoProject}
		for i, c := range comps {
			app, err := b.Render(c, nil)
			if err != nil {
				return nil, err
			}
			ann := app.GetAnnotations()
			if ann == nil {
				ann = map[string]string{}
			}
			// Waves apply the Applications in kubepkg's order when an
			// app of apps syncs them.
			ann["argocd.argoproj.io/sync-wave"] = strconv.Itoa(i)
			app.SetAnnotations(ann)
			if err := add(app.Object); err != nil {
				return nil, err
			}
		}
	case "helmfile":
		if err := add(helmfile(comps)); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown format %q; one of %s", o.Format, strings.Join(RenderFormats, ", "))
	}
	return bytes.Join(docs, []byte("---\n")), nil
}

// assumedAPIs lists the api: capabilities required in the catalog that no
// package provides: without a cluster to ask, they are taken as served.
func assumedAPIs(cat resolve.Catalog) map[string]bool {
	provided := map[string]bool{}
	for _, rs := range cat {
		for _, r := range rs {
			for _, p := range r.Provides {
				provided[p] = true
			}
		}
	}
	out := map[string]bool{}
	for _, rs := range cat {
		for _, r := range rs {
			for _, q := range r.Requires {
				if strings.HasPrefix(q.Capability, resolve.APIPrefix) && !provided[q.Capability] {
					out[strings.TrimPrefix(q.Capability, resolve.APIPrefix)] = true
				}
			}
		}
	}
	return out
}

// packageOrder puts every package after the packages it requires, by name
// otherwise.
func packageOrder(chosen map[string]resolve.Release) ([]string, map[string]string, error) {
	providers := map[string]string{}
	names := make([]string, 0, len(chosen))
	for n, r := range chosen {
		names = append(names, n)
		for _, p := range r.Provides {
			providers[p] = n
		}
	}
	sort.Strings(names)
	state := map[string]int{}
	var out []string
	var visit func(n string) error
	visit = func(n string) error {
		switch state[n] {
		case 1:
			return fmt.Errorf("packages require each other in a cycle through %s", n)
		case 2:
			return nil
		}
		state[n] = 1
		for _, dep := range requiredPackages(chosen[n], providers) {
			if _, ok := chosen[dep]; ok && dep != n {
				if err := visit(dep); err != nil {
					return err
				}
			}
		}
		state[n] = 2
		out = append(out, n)
		return nil
	}
	for _, n := range names {
		if err := visit(n); err != nil {
			return nil, nil, err
		}
	}
	return out, providers, nil
}

func requiredPackages(r resolve.Release, providers map[string]string) []string {
	var out []string
	for _, q := range r.Requires {
		switch {
		case q.Package != "":
			out = append(out, q.Package)
		case providers[q.Capability] != "":
			out = append(out, providers[q.Capability])
		}
	}
	sort.Strings(out)
	return out
}

func admittedSpec(ctx context.Context, store *repo.Store, policy repo.Policy, name, version string) (*v1alpha1.PackageSourceSpec, error) {
	repoName, versions, _ := store.Offered(name, "")
	for _, v := range versions {
		if v.Version == version && policy.AdmitVersion(ctx, repoName, name, v) == nil {
			return &v.Spec, nil
		}
	}
	return nil, fmt.Errorf("package %s %s vanished from repository %s", name, version, repoName)
}

// renderComponents describes a package's components the way the operator
// would hand them to a backend, in apply order, and records its releases
// for the packages that require it.
func renderComponents(name string, spec *v1alpha1.PackageSourceSpec, variant string, deps []string, releases map[string][]string) ([]backend.Component, error) {
	if variant == "" {
		variant = "default"
	}
	var v *v1alpha1.Variant
	for i := range spec.Variants {
		if spec.Variants[i].Name == variant {
			v = &spec.Variants[i]
		}
	}
	if v == nil {
		return nil, fmt.Errorf("package %s has no variant %s", name, variant)
	}
	var comps []v1alpha1.Component
	byName := map[string]v1alpha1.Component{}
	for _, c := range v.Components {
		if c.Install != nil {
			comps = append(comps, c)
			byName[c.Name] = c
		}
	}
	order, err := controller.ComponentOrder(comps)
	if err != nil {
		return nil, fmt.Errorf("package %s: %w", name, err)
	}
	var cross []string
	for _, dep := range deps {
		cross = append(cross, releases[dep]...)
	}
	var out []backend.Component
	for _, cn := range order {
		c := byName[cn]
		if c.Chart == nil {
			return nil, fmt.Errorf("package %s component %s is in a package tree; only published charts can be rendered", name, c.Name)
		}
		bc := backend.Component{
			Package:          name,
			Name:             c.Name,
			ReleaseName:      controller.ReleaseName(c),
			Namespace:        c.Install.Namespace,
			Chart:            &backend.Chart{Repository: c.Chart.Repository, Name: c.Chart.Name, Version: c.Chart.Version, Digest: c.Chart.Digest},
			Labels:           map[string]string{v1alpha1.LabelPackage: name},
			UpgradeCRDs:      c.Install.UpgradeCRDs,
			WaitStrategy:     c.Install.WaitStrategy,
			HealthCheckExprs: c.Install.HealthCheckExprs,
			Timeout:          10 * time.Minute,
		}
		for _, d := range c.Install.DependsOn {
			bc.DependsOn = append(bc.DependsOn, byName[d].Install.Namespace+"/"+controller.ReleaseName(byName[d]))
		}
		bc.DependsOn = append(bc.DependsOn, cross...)
		releases[name] = append(releases[name], bc.Namespace+"/"+bc.ReleaseName)
		out = append(out, bc)
	}
	return out, nil
}

// helmfile describes the components as helmfile releases.
func helmfile(comps []backend.Component) map[string]any {
	var repos []any
	repoNames := map[string]string{}
	var rels []any
	for _, c := range comps {
		chart := strings.TrimSuffix(c.Chart.Repository, "/") + "/" + c.Chart.Name
		if !c.Chart.OCI() {
			n, ok := repoNames[c.Chart.Repository]
			if !ok {
				n = fmt.Sprintf("repo%d", len(repoNames)+1)
				repoNames[c.Chart.Repository] = n
				repos = append(repos, map[string]any{"name": n, "url": c.Chart.Repository})
			}
			chart = n + "/" + c.Chart.Name
		}
		r := map[string]any{
			"name":            c.ReleaseName,
			"namespace":       c.Namespace,
			"chart":           chart,
			"version":         c.Chart.Version,
			"createNamespace": true,
			"labels":          map[string]any{"package": c.Package},
		}
		if len(c.DependsOn) > 0 {
			needs := make([]any, 0, len(c.DependsOn))
			for _, d := range c.DependsOn {
				needs = append(needs, d)
			}
			r["needs"] = needs
		}
		rels = append(rels, r)
	}
	out := map[string]any{"releases": rels}
	if len(repos) > 0 {
		out["repositories"] = repos
	}
	return out
}

// cleanYAML marshals an object without status and empty creation times.
func cleanYAML(v any) ([]byte, error) {
	raw, err := yaml.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	delete(m, "status")
	if md, ok := m["metadata"].(map[string]any); ok {
		delete(md, "creationTimestamp")
	}
	return yaml.Marshal(m)
}

func renderCmd(cl *cluster) *cobra.Command {
	var (
		o        RenderOptions
		repos    []string
		keyFiles []string
	)
	cmd := &cobra.Command{
		Use:   "render <package>[@constraint]...",
		Short: "Write what Flux, Argo CD or helmfile need to install packages without the operator",
		Long: `Render resolves the packages and everything they require, as install
would for an empty cluster, and writes them for another delivery tool in
kubepkg's order: Flux sources and HelmReleases with dependsOn, Argo CD
Applications with sync waves, or a helmfile with needs. Commit the output
to Git and let the tool apply it. Packages come from the indexes given
with --repo, highest priority first, or else from the cluster's
repositories. Package defaults apply; set values in the output.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var store *repo.Store
			if len(repos) > 0 {
				var keys []string
				for _, f := range keyFiles {
					raw, err := os.ReadFile(f)
					if err != nil {
						return err
					}
					keys = append(keys, string(raw))
				}
				store = repo.NewStore()
				for i, u := range repos {
					idx, _, err := repo.LoadIndex(cmd.Context(), cl.fetchers, u, keys)
					if err != nil {
						return fmt.Errorf("%s: %w", u, err)
					}
					store.Set(fmt.Sprintf("repo%d", i), int32(len(repos)-i), idx)
				}
			} else {
				c, err := cl.client()
				if err != nil {
					return err
				}
				if store, err = loadStore(cmd.Context(), c, cl.fetchers, cmd.ErrOrStderr()); err != nil {
					return err
				}
			}
			out, err := Render(cmd.Context(), store, cl.policy, parseRequests(args), o)
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(out)
			return err
		},
	}
	cmd.Flags().StringVar(&o.Format, "format", "flux", "output format: "+strings.Join(RenderFormats, ", "))
	cmd.Flags().StringArrayVar(&repos, "repo", nil, "repository index URL, highest priority first (repeatable; default: the cluster's repositories)")
	cmd.Flags().StringVar(&o.Variant, "variant", "", "variant to render (default: default)")
	cmd.Flags().StringVar(&o.ArgoNamespace, "argo-namespace", "", "namespace of the Applications (argo; default argocd)")
	cmd.Flags().StringVar(&o.ArgoProject, "argo-project", "", "Argo CD project (argo; default default)")
	cmd.Flags().BoolVar(&o.Insecure, "insecure", false, "let Flux pull from registries over plain HTTP (flux)")
	cmd.Flags().StringArrayVar(&keyFiles, "public-key", nil, "with --repo: trust only indexes signed with this ed25519 public key file (repeatable)")
	return cmd
}
