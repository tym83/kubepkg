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
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tym83/kubepkg/pkg/build"
	"github.com/tym83/kubepkg/pkg/source"
)

func userCacheFetcher(plainHTTP bool) (*source.Fetcher, error) {
	d, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	return &source.Fetcher{CacheDir: filepath.Join(d, "kubepkg"), PlainHTTP: plainHTTP}, nil
}

func initCmd() *cobra.Command {
	var (
		in        build.InitInput
		chart     string
		plainHTTP bool
	)
	cmd := &cobra.Command{
		Use:   "init <dir>",
		Short: "Start a recipe from an upstream chart or release manifests",
		Long: `Init writes <dir>/recipe.yaml with every source pinned: it downloads the
upstream to compute digests, takes the description from the chart, drops
Namespaces from manifests, and lists the CRDs they ship. Review it, then
run "kubepkg validate".

  kubepkg init recipes/cert-manager --chart https://charts.jetstack.io/cert-manager@v1.21.2
  kubepkg init recipes/kubevirt --version 1.9.0 \\
    --manifest https://github.com/kubevirt/kubevirt/releases/download/v1.9.0/kubevirt-operator.yaml`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if in.Name == "" {
				in.Name = filepath.Base(filepath.Clean(args[0]))
			}
			if chart != "" {
				ref, version, ok := strings.Cut(chart, "@")
				i := strings.LastIndex(ref, "/")
				if !ok || i <= 0 {
					return fmt.Errorf("--chart wants <repository>/<name>@<version>, got %q", chart)
				}
				in.Chart = &source.Chart{Repository: ref[:i], Name: ref[i+1:], Version: version}
			}
			f, err := userCacheFetcher(plainHTTP)
			if err != nil {
				return err
			}
			if err := build.Init(cmd.Context(), args[0], in, f); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote %s; review it, then run: kubepkg validate %s\n", filepath.Join(args[0], build.RecipeFile), args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&chart, "chart", "", "upstream chart, <repository>/<name>@<version>, e.g. oci://ghcr.io/org/charts/app@1.2.3")
	cmd.Flags().StringArrayVar(&in.Manifests, "manifest", nil, "URL of upstream release manifests (repeatable)")
	cmd.Flags().StringVar(&in.Name, "name", "", "package name (default: the directory name)")
	cmd.Flags().StringVar(&in.Version, "version", "", "upstream version (default: the chart's appVersion)")
	cmd.Flags().StringVar(&in.Namespace, "namespace", "", "install namespace (default: the package name)")
	cmd.Flags().StringVar(&in.Description, "description", "", "one line about the package (default: the chart's)")
	cmd.Flags().BoolVar(&plainHTTP, "plain-http", false, "talk to registries without TLS (local registries only)")
	return cmd
}

func validateCmd() *cobra.Command {
	var plainHTTP bool
	cmd := &cobra.Command{
		Use:   "validate <recipe-dir>...",
		Short: "Build recipes without publishing and check them",
		Long: `Validate builds each recipe without publishing it, so every source is
fetched and checked against its pin, renders the charts with their default
values, and checks the package against them: a CRD the charts ship but
the package does not declare is an error. A directory without a recipe
is searched for recipes below it.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dirs, err := recipeDirs(args)
			if err != nil {
				return err
			}
			f, err := userCacheFetcher(plainHTTP)
			if err != nil {
				return err
			}
			failed := 0
			for _, d := range dirs {
				work, err := os.MkdirTemp("", "kubepkg-validate-")
				if err != nil {
					return err
				}
				rep := build.Validate(cmd.Context(), d, build.Options{Fetcher: f, WorkDir: work})
				os.RemoveAll(work)
				status := "ok"
				if !rep.OK() {
					status, failed = "FAILED", failed+1
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", d, status)
				for _, e := range rep.Errors {
					fmt.Fprintf(cmd.OutOrStdout(), "  error: %s\n", e)
				}
				for _, w := range rep.Warnings {
					fmt.Fprintf(cmd.OutOrStdout(), "  warning: %s\n", w)
				}
			}
			if failed > 0 {
				return fmt.Errorf("%d of %d recipes failed", failed, len(dirs))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&plainHTTP, "plain-http", false, "talk to registries without TLS (local registries only)")
	return cmd
}

// recipeDirs expands the arguments to recipe directories.
func recipeDirs(args []string) ([]string, error) {
	var out []string
	for _, a := range args {
		if _, err := os.Stat(filepath.Join(a, build.RecipeFile)); err == nil {
			out = append(out, a)
			continue
		}
		err := filepath.WalkDir(a, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && d.Name() == build.RecipeFile {
				out = append(out, filepath.Dir(p))
			}
			return err
		})
		if err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no %s under %s", build.RecipeFile, strings.Join(args, ", "))
	}
	sort.Strings(out)
	return out, nil
}
