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
	"github.com/kuberoot-dev/kubepkg/pkg/admission"
	"github.com/kuberoot-dev/kubepkg/pkg/controller"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"os"
	"os/exec"
	"path/filepath"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/kuberoot-dev/kubepkg/pkg/build"
	"github.com/kuberoot-dev/kubepkg/pkg/images"
	"github.com/kuberoot-dev/kubepkg/pkg/source"
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
		in             build.InitInput
		chart          string
		plainHTTP      bool
		noImages       bool
		registryConfig string
	)
	cmd := &cobra.Command{
		Use:   "init <dir>",
		Short: "Start a recipe from an upstream chart or release manifests",
		Long: `Init writes <dir>/recipe.yaml with every source pinned: it downloads the
upstream to compute digests, takes the description from the chart, drops
Namespaces from manifests, lists the CRDs they ship, and pins the images
they run by their current digests. Review it, add images an operator in
the package deploys on its own, then run "kubepkg validate".

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
			if !noImages {
				in.Images = &images.Resolver{CredentialsFile: registryConfig, PlainHTTP: plainHTTP}
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
	cmd.Flags().BoolVar(&noImages, "no-images", false, "do not pin images (no registry access); fill package.images later with kubepkg images")
	cmd.Flags().StringVar(&registryConfig, "registry-config", "", "Docker config file with registry credentials, for pinning private images")
	return cmd
}

func imagesCmd(cl *cluster) *cobra.Command {
	var (
		plainHTTP      bool
		registryConfig string
		inCluster      bool
	)
	cmd := &cobra.Command{
		Use:   "images <recipe-dir> | --cluster",
		Short: "Print the images a recipe's package runs, pinned by digest, or check a cluster's",
		Long: `Images builds the recipe without publishing it, finds the images its
charts run with their default values, adds those package.images already
lists (images an operator deploys on its own appear in no chart), pins
every one by the digest its tag points at now, and prints the
package.images block to paste into the recipe. Images already pinned keep
their digests; a published version whose images change needs a new build
number.

With --cluster, it checks what runs instead: every container in the
namespaces packages install into, against the images the installed
packages pin, as the image policy would. It lists what no package pins,
by namespace and owner, and fails when there is any.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if inCluster {
				return cobra.NoArgs(cmd, args)
			}
			return cobra.ExactArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if inCluster {
				return checkClusterImages(cmd, cl)
			}
			f, err := userCacheFetcher(plainHTTP)
			if err != nil {
				return err
			}
			work, err := os.MkdirTemp("", "kubepkg-images-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(work)
			res, err := build.Build(cmd.Context(), args[0], build.Options{Fetcher: f, WorkDir: work})
			if err != nil {
				return err
			}
			running, err := build.RenderedImages(res)
			if err != nil {
				return err
			}
			r := images.Resolver{CredentialsFile: registryConfig, PlainHTTP: plainHTTP}
			listed := res.Recipe.Spec.Package.Images
			var out []string
			seen := map[string]bool{}
			add := func(ref string) error {
				p, err := r.Pin(cmd.Context(), ref)
				if err != nil {
					return err
				}
				if !seen[p] {
					seen[p] = true
					out = append(out, p)
				}
				return nil
			}
			for _, ref := range listed {
				if err := add(ref); err != nil {
					return err
				}
			}
			for _, ref := range running {
				if !images.Covered(ref, out) {
					if err := add(ref); err != nil {
						return err
					}
				}
			}
			sort.Strings(out)
			w := cmd.OutOrStdout()
			fmt.Fprintln(w, "    images:")
			for _, ref := range out {
				fmt.Fprintf(w, "      - %s\n", ref)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&plainHTTP, "plain-http", false, "talk to registries without TLS (local registries only)")
	cmd.Flags().StringVar(&registryConfig, "registry-config", "", "Docker config file with registry credentials")
	cmd.Flags().BoolVar(&inCluster, "cluster", false, "check the images running in package namespaces against what the packages pin")
	return cmd
}

// checkClusterImages lists running containers in package namespaces that
// no installed package pins.
func checkClusterImages(cmd *cobra.Command, cl *cluster) error {
	ctx := cmd.Context()
	c, err := cl.client()
	if err != nil {
		return err
	}
	pinned, err := admission.Collect(ctx, c)
	if err != nil {
		return err
	}
	namespaces, err := controller.PackageNamespaces(ctx, c)
	if err != nil {
		return err
	}
	type finding struct{ namespace, owner, image, packages string }
	var found []finding
	seen := map[string]bool{}
	names := make([]string, 0, len(namespaces))
	for ns := range namespaces {
		names = append(names, ns)
	}
	sort.Strings(names)
	for _, ns := range names {
		var pods corev1.PodList
		if err := c.List(ctx, &pods, client.InNamespace(ns)); err != nil {
			return err
		}
		for _, pod := range pods.Items {
			owner := "Pod/" + pod.Name
			if ref := metav1.GetControllerOf(&pod); ref != nil {
				owner = ref.Kind + "/" + ref.Name
			}
			for _, ctr := range append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...) {
				if _, ok := pinned.Resolve(ctr.Image); ok {
					continue
				}
				key := ns + " " + owner + " " + ctr.Image
				if !seen[key] {
					seen[key] = true
					found = append(found, finding{ns, owner, ctr.Image, strings.Join(namespaces[ns], ",")})
				}
			}
		}
	}
	w := cmd.OutOrStdout()
	if len(found) == 0 {
		fmt.Fprintf(w, "every container in %d package namespaces runs an image its packages pin\n", len(names))
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAMESPACE\tPACKAGES\tOWNER\tIMAGE NO PACKAGE PINS")
	for _, f := range found {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", f.namespace, f.packages, f.owner, f.image)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	return fmt.Errorf("%d images run unpinned; add them to package.images (kubepkg images <recipe>), or imagePolicy=enforce will refuse them", len(found))
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
				rep := build.Validate(cmd.Context(), d, build.Options{Fetcher: f, WorkDir: work, VerifyImages: cosign()})
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

// cosign verifies image signatures when the cosign executable is there;
// recipes that ask for verification are refused without it.
func cosign() build.ImageVerifier {
	if _, err := exec.LookPath("cosign"); err != nil {
		return nil
	}
	return build.CosignVerifier("cosign")
}
