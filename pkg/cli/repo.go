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
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tym83/kubepkg/pkg/repo"
	"github.com/tym83/kubepkg/pkg/source"
)

func repoCmd(cl *cluster) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repo",
		Short: "Build package repositories and subscribe the cluster to them",
	}
	cmd.AddCommand(repoIndexCmd(), repoAddCmd(cl), repoListCmd(cl), repoRemoveCmd(cl))
	return cmd
}

func repoIndexCmd() *cobra.Command {
	var (
		out, merge string
		opts       repo.BuildOptions
		plainHTTP  bool
	)
	cmd := &cobra.Command{
		Use:   "index <dir>",
		Short: "Build a repository index from the PackageSources under a directory",
		Long: `Index collects every PackageSource in the YAML files under dir into one
repository index. Each version needs an exact semver version; charts
without a digest are downloaded and pinned in the index, so a published
version always installs the same charts. Serve the index over HTTP as
index.yaml; packages and charts stay in their registries.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cache, err := os.MkdirTemp("", "kubepkg-index-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(cache)
			if merge != "" {
				base, err := readBaseIndex(cmd.Context(), merge)
				if err != nil {
					return err
				}
				if base == nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "%s does not exist yet: starting a new index\n", merge)
				}
				opts.Base = base
			}
			idx, err := repo.Build(cmd.Context(), args[0], &source.Fetcher{CacheDir: cache, PlainHTTP: plainHTTP}, opts)
			if err != nil {
				return err
			}
			if out == "-" {
				return idx.Write(cmd.OutOrStdout())
			}
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				return err
			}
			f, err := os.Create(out)
			if err != nil {
				return err
			}
			if err := idx.Write(f); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			versions := 0
			for _, p := range idx.Packages {
				versions += len(p.Versions)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s: %d packages, %d versions\n", out, len(idx.Packages), versions)
			return nil
		},
	}
	cmd.Flags().StringVarP(&out, "output", "o", "index.yaml", "where to write the index, - for stdout")
	cmd.Flags().BoolVar(&opts.Verify, "verify", false, "also download pinned charts and check their digests")
	cmd.Flags().StringVar(&merge, "merge", "", "published index (file or URL) whose versions are kept; published versions must not change")
	cmd.Flags().BoolVar(&plainHTTP, "plain-http", false, "talk to OCI registries without TLS (local registries only)")
	return cmd
}

// readBaseIndex reads a published index from a file or URL; nil when it
// does not exist yet.
func readBaseIndex(ctx context.Context, from string) (*repo.Index, error) {
	var raw []byte
	var err error
	if strings.Contains(from, "://") {
		raw, err = repo.DefaultFetchers().Fetch(ctx, from)
		if errors.Is(err, source.ErrNotFound) {
			return nil, nil
		}
	} else {
		raw, err = os.ReadFile(from)
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return repo.Parse(raw)
}
