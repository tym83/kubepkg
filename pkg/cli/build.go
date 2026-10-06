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
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/tym83/kubepkg/api/v1beta1"
	"github.com/tym83/kubepkg/pkg/build"
	"github.com/tym83/kubepkg/pkg/source"
)

func buildCmd() *cobra.Command {
	var (
		registry, out, workDir, cacheDir string
		push                             source.PushOptions
	)
	cmd := &cobra.Command{
		Use:   "build <recipe-dir>",
		Short: "Build a package from a recipe and publish it",
		Long: `Build fetches the upstream sources a recipe pins, makes charts of them
(a chart is used as is, plain manifests are wrapped into one), applies the
recipe's values and overlays, and writes the package tree. With --registry
it pushes the tree and writes the PackageSource that installs it, pinned
to the pushed digest; feed those files to "kubepkg repo index".

The same recipe always builds the same content. Published versions are
immutable: when the tag already holds that content, the published
artifact is reused, whatever compressor built it; different content
under the same version and build is an error.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if workDir == "" {
				d, err := os.MkdirTemp("", "kubepkg-build-")
				if err != nil {
					return err
				}
				defer os.RemoveAll(d)
				workDir = d
			}
			if cacheDir == "" {
				d, err := os.UserCacheDir()
				if err != nil {
					return err
				}
				cacheDir = filepath.Join(d, "kubepkg")
			}
			fetcher := &source.Fetcher{CacheDir: cacheDir, PlainHTTP: push.PlainHTTP, CredentialsFile: push.CredentialsFile}
			res, err := build.Build(cmd.Context(), args[0], build.Options{Fetcher: fetcher, WorkDir: workDir})
			if err != nil {
				return err
			}
			if registry == "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "built %s %s build %d in %s (not published: no --registry)\n",
					res.Recipe.Metadata.Name, res.Recipe.Spec.Version, res.Recipe.Spec.Build, res.TreeDir)
				return nil
			}
			src, reused, err := build.Publish(cmd.Context(), res, registry, push)
			if err != nil {
				return err
			}
			raw, err := yaml.Marshal(src)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(out, 0o755); err != nil {
				return err
			}
			file := filepath.Join(out, fmt.Sprintf("%s-%s-%d.yaml", src.Name, src.Spec.Version, src.Spec.Build))
			if err := os.WriteFile(file, raw, 0o644); err != nil {
				return err
			}
			verb := "published"
			if reused {
				verb = "already published"
			}
			where := "a meta package, nothing to push"
			if repo := publishedRepository(src); repo != "" {
				where = repo
			} else {
				verb = "built"
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "%s %s, wrote %s\n", verb, where, file)
			return nil
		},
	}
	cmd.Flags().StringVar(&registry, "registry", "", "OCI registry path to publish to, e.g. oci://ghcr.io/example/packages")
	cmd.Flags().StringVarP(&out, "output", "o", "dist", "directory for the published PackageSource")
	cmd.Flags().StringVar(&workDir, "work-dir", "", "where to build; kept for inspection when set")
	cmd.Flags().StringVar(&cacheDir, "cache-dir", "", "chart download cache (default: the user cache directory)")
	cmd.Flags().BoolVar(&push.PlainHTTP, "plain-http", false, "talk to registries without TLS (local registries only)")
	cmd.Flags().StringVar(&push.CredentialsFile, "registry-config", "", "Docker config file with registry credentials")
	return cmd
}

// publishedRepository names where a published package's charts are.
func publishedRepository(src *v1beta1.PackageSource) string {
	for _, v := range src.Spec.Variants {
		for _, c := range v.Components {
			if c.Chart != nil {
				return c.Chart.Repository
			}
		}
	}
	return ""
}
