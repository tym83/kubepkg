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
	cmd.AddCommand(repoIndexCmd(), repoKeygenCmd(), repoAddCmd(cl), repoListCmd(cl), repoRemoveCmd(cl))
	return cmd
}

func repoIndexCmd() *cobra.Command {
	var (
		out, merge, signKey, signKeyEnv string
		opts                            repo.BuildOptions
		plainHTTP                       bool
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
			key, err := signingKey(signKey, signKeyEnv)
			if err != nil {
				return err
			}
			var raw bytes.Buffer
			if err := idx.Write(&raw); err != nil {
				return err
			}
			if out == "-" {
				if key != nil {
					return fmt.Errorf("a signed index needs a file to write the signature next to")
				}
				_, err := cmd.OutOrStdout().Write(raw.Bytes())
				return err
			}
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(out, raw.Bytes(), 0o644); err != nil {
				return err
			}
			if key != nil {
				sig, err := repo.Sign(raw.Bytes(), key)
				if err != nil {
					return err
				}
				if err := os.WriteFile(out+repo.SignatureSuffix, sig, 0o644); err != nil {
					return err
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "signed %s%s\n", out, repo.SignatureSuffix)
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
	cmd.Flags().StringVar(&signKey, "sign-key", "", "sign the index with this ed25519 private key file; the signature goes next to it as .sig")
	cmd.Flags().StringVar(&signKeyEnv, "sign-key-env", "", "sign with the PEM private key in this environment variable, for CI secrets")
	return cmd
}

// signingKey reads the private key from a file or an environment
// variable; nil when neither is given.
func signingKey(file, env string) ([]byte, error) {
	switch {
	case file != "" && env != "":
		return nil, fmt.Errorf("give --sign-key or --sign-key-env, not both")
	case file != "":
		return os.ReadFile(file)
	case env != "":
		key := os.Getenv(env)
		if key == "" {
			return nil, fmt.Errorf("environment variable %s is empty", env)
		}
		return []byte(key), nil
	}
	return nil, nil
}

func repoKeygenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "keygen <prefix>",
		Short: "Make an ed25519 key pair for signing a repository index",
		Long: `Keygen writes <prefix>.key, the private key, readable only by you, and
<prefix>.pub, the public key clusters trust (repo add --public-key). Keep
the private key out of Git: store it in a secret manager or a CI secret.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			priv, pub, err := repo.GenerateKey()
			if err != nil {
				return err
			}
			for _, f := range []string{args[0] + ".key", args[0] + ".pub"} {
				if _, err := os.Stat(f); err == nil {
					return fmt.Errorf("%s already exists", f)
				}
			}
			if err := os.WriteFile(args[0]+".key", priv, 0o600); err != nil {
				return err
			}
			if err := os.WriteFile(args[0]+".pub", pub, 0o644); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote %s.key (private) and %s.pub\n", args[0], args[0])
			return nil
		},
	}
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
