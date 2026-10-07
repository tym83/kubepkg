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
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/tym83/kubepkg/api/v1"
	"github.com/tym83/kubepkg/pkg/bundle"
	"github.com/tym83/kubepkg/pkg/images"
	"github.com/tym83/kubepkg/pkg/source"
)

// trustFlags are the keys a repository is trusted with.
type trustFlags struct {
	publicKeys, rootKeys []string
	rootThreshold        int32
	allowUnsigned        bool
}

func (t *trustFlags) bind(cmd *cobra.Command) {
	cmd.Flags().StringArrayVar(&t.publicKeys, "public-key", nil, "trust indexes signed with this ed25519 public key file (repeatable)")
	cmd.Flags().StringArrayVar(&t.rootKeys, "root-key", nil, "trust a repository with a root of trust through this pinned root key file (repeatable)")
	cmd.Flags().Int32Var(&t.rootThreshold, "root-threshold", 1, "how many pinned root keys must have signed version 1 of the root")
	cmd.Flags().BoolVar(&t.allowUnsigned, "allow-unsigned", false, "accept an index nobody signed, for local testing")
}

// spec fills the trust part of a repository spec from key files.
func (t *trustFlags) spec() (v1.RepositorySpec, error) {
	s := v1.RepositorySpec{AllowUnsigned: t.allowUnsigned}
	read := func(files []string) ([]string, error) {
		var out []string
		for _, f := range files {
			raw, err := os.ReadFile(f)
			if err != nil {
				return nil, err
			}
			out = append(out, string(raw))
		}
		return out, nil
	}
	var err error
	if s.PublicKeys, err = read(t.publicKeys); err != nil {
		return s, err
	}
	if len(t.rootKeys) > 0 {
		keys, err := read(t.rootKeys)
		if err != nil {
			return s, err
		}
		s.Trust = &v1.RepositoryTrust{RootKeys: keys, RootThreshold: t.rootThreshold}
	}
	return s, nil
}

// repoSources are the repositories to take packages from: the --repo
// indexes, highest priority first, trusted with the given keys, or else
// the cluster's repositories.
func repoSources(ctx context.Context, cl *cluster, repos []string, trust trustFlags) ([]bundle.Source, error) {
	var sources []bundle.Source
	if len(repos) > 0 {
		spec, err := trust.spec()
		if err != nil {
			return nil, err
		}
		for i, u := range repos {
			s := spec
			s.URL, s.Priority = u, int32(len(repos)-i)
			name := fmt.Sprintf("repo%d", i+1)
			if len(repos) == 1 {
				name = "main"
			}
			sources = append(sources, bundle.Source{Name: name, Spec: s})
		}
	} else {
		c, err := cl.client()
		if err != nil {
			return nil, err
		}
		var list v1.RepositoryList
		if err := c.List(ctx, &list); err != nil {
			return nil, err
		}
		for _, r := range list.Items {
			sources = append(sources, bundle.Source{Name: r.Name, Spec: r.Spec})
		}
	}
	if len(sources) == 0 {
		return nil, errors.New("no repositories: give --repo or add one to the cluster")
	}
	return sources, nil
}

func bundleCmd(cl *cluster) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bundle",
		Short: "Carry packages into air-gapped clusters",
	}
	cmd.AddCommand(bundleCreateCmd(cl), bundleInspectCmd(), bundleImportCmd())
	return cmd
}

func bundleCreateCmd(cl *cluster) *cobra.Command {
	var (
		output, variant, registryConfig string
		repos                           []string
		trust                           trustFlags
		plainHTTP                       bool
	)
	cmd := &cobra.Command{
		Use:   "create <package>[@constraint]... -o <file.tar>",
		Short: "Bundle packages, their requirements, charts and images into one file",
		Long: `Create resolves the packages and everything they require, as install
would for an empty cluster, and writes one tar file with the repository
files exactly as published (index, signatures, roots), the chart archives,
package trees and container images. Repositories are loaded with the same
checks a cluster applies. Packages come from the indexes given with
--repo, highest priority first, trusted with --public-key or --root-key,
or else from the cluster's repositories.

Images come from each package's pinned images. A package that pins none
gets the images its charts run, pinned as they are now; import refuses
those unless told to accept them.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output == "" {
				return errors.New("give the bundle file with -o")
			}
			ctx := cmd.Context()
			sources, err := repoSources(ctx, cl, repos, trust)
			if err != nil {
				return err
			}
			store, rec, err := bundle.Load(ctx, sources, cl.fetchers, time.Now(), cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			chosen, order, _, err := resolveFresh(ctx, store, cl.policy, parseRequests(args), variant)
			if err != nil {
				return err
			}
			var sel []bundle.Selection
			for _, name := range order {
				repoName, v, err := admittedVersion(ctx, store, cl.policy, name, chosen[name].Version)
				if err != nil {
					return err
				}
				sel = append(sel, bundle.Selection{Repository: repoName, Name: name, Version: v})
			}
			work, err := os.MkdirTemp(filepath.Dir(output), ".kubepkg-bundle-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(work)
			f, err := userCacheFetcher(plainHTTP)
			if err != nil {
				return err
			}
			f.CredentialsFile = registryConfig
			m, err := bundle.Create(ctx, work, rec, sel, bundle.CreateOptions{
				Charts:   f,
				Registry: images.Resolver{CredentialsFile: registryConfig, PlainHTTP: plainHTTP},
				Warn:     cmd.ErrOrStderr(),
				Now:      time.Now(),
			})
			if err != nil {
				return err
			}
			out, err := os.Create(output)
			if err != nil {
				return err
			}
			if err := bundle.Pack(work, out); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote %s: %d packages, %d charts, %d package trees, %d images\n", output, len(m.Packages), len(m.Charts), len(m.Trees), len(m.Images))
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "bundle file to write")
	cmd.Flags().StringArrayVar(&repos, "repo", nil, "repository index URL, highest priority first (repeatable; default: the cluster's repositories)")
	cmd.Flags().StringVar(&variant, "variant", "", "variant whose requirements are resolved (default: default)")
	cmd.Flags().StringVar(&registryConfig, "registry-config", "", "Docker config file with registry credentials")
	cmd.Flags().BoolVar(&plainHTTP, "plain-http", false, "talk to registries without TLS (local registries only)")
	trust.bind(cmd)
	return cmd
}

// readManifest reads bundle.yaml from a bundle file without unpacking it.
func readManifest(file string) (*bundle.Manifest, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s has no %s: not a kubepkg bundle", file, bundle.ManifestFile)
		}
		if err != nil {
			return nil, err
		}
		if h.Name != bundle.ManifestFile {
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(tr, 64<<20))
		if err != nil {
			return nil, err
		}
		var m bundle.Manifest
		if err := yaml.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		return &m, nil
	}
}

func bundleInspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <file.tar>",
		Short: "List what a bundle carries, without checking it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := readManifest(args[0])
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintf(w, "created %s\n\nPACKAGE\tVERSION\tREPOSITORY\n", m.Created.UTC().Format(time.RFC3339))
			for _, p := range m.Packages {
				fmt.Fprintf(w, "%s\t%s build %d\t%s\n", p.Name, p.Version, p.Build, p.Repository)
			}
			fmt.Fprintln(w, "\nIMAGE\tPINNED BY\tPACKAGES")
			for _, i := range m.Images {
				by := "repository"
				if !i.Pinned {
					by = "bundle only"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\n", i.Ref, by, strings.Join(i.Packages, ","))
			}
			fmt.Fprintf(w, "\n%d charts, %d package trees\n", len(m.Charts), len(m.Trees))
			return w.Flush()
		},
	}
}

func bundleImportCmd() *cobra.Command {
	var (
		mirror, site, nodeConfig, workDir, registryConfig string
		trust                                             trustFlags
		allowUnpinned, plainHTTP                          bool
	)
	cmd := &cobra.Command{
		Use:   "import <file.tar> --mirror oci://<registry>/<path> --public-key <file>",
		Short: "Check a bundle and copy it into a mirror registry",
		Long: `Import checks a bundle against the repository keys you give, never keys
the bundle carries: signatures, the root chain and expiry as a cluster
checks them, then every chart, package tree and image against the signed
packages. Only then does it copy them into the mirror registry, keeping
every digest, and write the repositories' files to --site for serving
inside the air gap. Point the operator at the mirror (chart value mirror)
and the cluster's repositories at the site; --node-config writes the
containerd and Talos settings that send image pulls to the mirror.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if mirror == "" {
				return errors.New("give the mirror registry with --mirror oci://<registry>/<path>")
			}
			spec, err := trust.spec()
			if err != nil {
				return err
			}
			work, err := os.MkdirTemp(workDir, "kubepkg-import-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(work)
			f, err := os.Open(args[0])
			if err != nil {
				return err
			}
			err = bundle.Unpack(f, work)
			f.Close()
			if err != nil {
				return fmt.Errorf("unpack %s: %w", args[0], err)
			}
			ctx := cmd.Context()
			v, err := bundle.Verify(ctx, work, spec, time.Now(), allowUnpinned)
			if err != nil {
				return fmt.Errorf("the bundle does not check out: %w", err)
			}
			res, err := bundle.Import(ctx, v, bundle.ImportOptions{
				Mirror:  mirror,
				Push:    source.PushOptions{PlainHTTP: plainHTTP, CredentialsFile: registryConfig},
				SiteDir: site,
			})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "verified and imported %d packages: %d charts, %d package trees, %d images into %s\n", len(v.Manifest.Packages), res.Charts, res.Trees, res.Images, mirror)
			if site != "" {
				var names []string
				for _, r := range v.Manifest.Repositories {
					names = append(names, filepath.Join(site, r.Name)+"/")
				}
				fmt.Fprintf(out, "repository files: %s; serve them over HTTP and point the cluster's repositories there\n", strings.Join(names, ", "))
			}
			if nodeConfig != "" && len(res.Registries) > 0 {
				containerd, talos, err := bundle.NodeMirrors(mirror, plainHTTP, res.Registries)
				if err != nil {
					return err
				}
				for _, r := range res.Registries {
					p := filepath.Join(nodeConfig, "containerd", "certs.d", r, "hosts.toml")
					if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
						return err
					}
					if err := os.WriteFile(p, []byte(containerd[r]), 0o644); err != nil {
						return err
					}
				}
				if err := os.WriteFile(filepath.Join(nodeConfig, "talos-registries.yaml"), []byte(talos), 0o644); err != nil {
					return err
				}
				fmt.Fprintf(out, "node settings for %s: %s\n", strings.Join(res.Registries, ", "), nodeConfig)
			}
			fmt.Fprintf(out, "operator: helm upgrade kubepkg ... --set mirror=%s\n", mirror)
			return nil
		},
	}
	cmd.Flags().StringVar(&mirror, "mirror", "", "oci:// registry path to copy into, the operator's mirror")
	cmd.Flags().StringVar(&site, "site", "", "directory for the repositories' files, to serve over HTTP")
	cmd.Flags().StringVar(&nodeConfig, "node-config", "", "directory for containerd hosts.toml files and a Talos patch that send image pulls to the mirror")
	cmd.Flags().StringVar(&workDir, "work-dir", "", "where to unpack the bundle (default: the system temporary directory)")
	cmd.Flags().StringVar(&registryConfig, "registry-config", "", "Docker config file with credentials for the mirror registry")
	cmd.Flags().BoolVar(&allowUnpinned, "allow-unpinned-images", false, "accept images that only the bundle pins, not a signed package")
	cmd.Flags().BoolVar(&plainHTTP, "plain-http", false, "talk to the mirror registry without TLS")
	trust.bind(cmd)
	return cmd
}
