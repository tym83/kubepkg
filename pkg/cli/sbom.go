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
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/tym83/kubepkg/api/v1beta1"
	"github.com/tym83/kubepkg/pkg/bundle"
	"github.com/tym83/kubepkg/pkg/controller"
	"github.com/tym83/kubepkg/pkg/sbom"
	"github.com/tym83/kubepkg/pkg/version"
)

func sbomCmd(cl *cluster) *cobra.Command {
	var (
		output, variant, at string
		repos               []string
		trust               trustFlags
		installed           bool
	)
	cmd := &cobra.Command{
		Use:   "sbom (<package>[@constraint]... | --cluster) [-o file]",
		Short: "Write a CycloneDX software bill of materials for packages or a cluster",
		Long: `Sbom describes packages as a CycloneDX 1.6 document: every package,
the Helm charts and container images it installs, each pinned by digest,
and which packages require which. With package names it resolves them and
their requirements from the repositories, as install would; with --cluster
it describes what the cluster runs now. Images are those the packages pin;
a package that pins none lists only its charts.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			when := time.Now()
			if at != "" {
				t, err := time.Parse(time.RFC3339, at)
				if err != nil {
					return fmt.Errorf("--timestamp: %w", err)
				}
				when = t
			}
			var pkgs []sbom.Package
			var err error
			name := ""
			switch {
			case installed && len(args) > 0:
				return errors.New("give packages or --cluster, not both")
			case installed:
				name = "cluster"
				if cl.kubeContext != "" {
					name = cl.kubeContext
				}
				pkgs, err = clusterPackages(ctx, cl)
			case len(args) == 0:
				return errors.New("give packages, or --cluster for what the cluster runs")
			default:
				pkgs, err = resolvedPackages(ctx, cl, repos, trust, args, variant)
			}
			if err != nil {
				return err
			}
			doc, err := sbom.CycloneDX(pkgs, sbom.Options{ToolVersion: version.Version, Timestamp: when, Name: name})
			if err != nil {
				return err
			}
			if output == "" || output == "-" {
				_, err = cmd.OutOrStdout().Write(doc)
				return err
			}
			return os.WriteFile(output, doc, 0o644)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "file to write (default: standard output)")
	cmd.Flags().StringArrayVar(&repos, "repo", nil, "repository index URL, highest priority first (repeatable; default: the cluster's repositories)")
	cmd.Flags().StringVar(&variant, "variant", "", "variant whose requirements are resolved (default: default)")
	cmd.Flags().BoolVar(&installed, "cluster", false, "describe the packages the cluster runs")
	cmd.Flags().StringVar(&at, "timestamp", "", "RFC 3339 time to record instead of now, for reproducible documents")
	trust.bind(cmd)
	return cmd
}

// resolvedPackages resolves packages and their requirements for an empty
// cluster and returns them as signed in their repositories.
func resolvedPackages(ctx context.Context, cl *cluster, repos []string, trust trustFlags, args []string, variant string) ([]sbom.Package, error) {
	sources, err := repoSources(ctx, cl, repos, trust)
	if err != nil {
		return nil, err
	}
	store, _, err := bundle.Load(ctx, sources, cl.fetchers, time.Now())
	if err != nil {
		return nil, err
	}
	chosen, order, _, err := resolveFresh(ctx, store, cl.policy, parseRequests(args), variant)
	if err != nil {
		return nil, err
	}
	var out []sbom.Package
	for _, n := range order {
		repoName, v, err := admittedVersion(ctx, store, cl.policy, n, chosen[n].Version)
		if err != nil {
			return nil, err
		}
		out = append(out, sbom.Package{Name: n, Version: v.Version, Build: v.Build, Repository: repoName, Digest: v.Digest, Spec: v.Spec, Variant: variant})
	}
	return out, nil
}

// clusterPackages are the packages the cluster runs, with the specs it
// installed them from.
func clusterPackages(ctx context.Context, cl *cluster) ([]sbom.Package, error) {
	c, err := cl.client()
	if err != nil {
		return nil, err
	}
	var list v1beta1.PackageList
	if err := c.List(ctx, &list); err != nil {
		return nil, err
	}
	var out []sbom.Package
	for _, p := range list.Items {
		src := &v1beta1.PackageSource{}
		if err := c.Get(ctx, types.NamespacedName{Name: p.Name}, src); err != nil {
			if client.IgnoreNotFound(err) == nil {
				continue // nothing installed for it yet
			}
			return nil, err
		}
		out = append(out, sbom.Package{
			Name: p.Name, Version: src.Spec.Version, Build: src.Spec.Build,
			Repository: src.Labels[v1beta1.LabelRepository], Digest: src.Annotations[controller.AnnotationSpecDigest],
			Spec: src.Spec, Variant: p.Spec.Variant,
		})
	}
	return out, nil
}
