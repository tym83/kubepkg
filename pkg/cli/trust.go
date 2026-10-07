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
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/tym83/kubepkg/pkg/repo"
)

func trustCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trust",
		Short: "Manage a repository's root of trust: roles, thresholds, key rotation",
		Long: `A repository with a root publishes root.yaml next to its index, and every
version as root/<version>.yaml. The root names the keys that may sign the
root itself and the index, and how many signatures each needs. Clusters
pin the keys of version 1 (repo add --root-key) and follow later versions,
each signed by enough root keys of the version before and of itself.

  kubepkg trust root new --root-key a.pub --root-key b.pub --root-key c.pub --root-threshold 2 \\
    --index-key ci.pub --index-threshold 1 -o root.yaml
  kubepkg trust sign root.yaml --key a.key      # each signer, on their own machine
  kubepkg trust sign root.yaml --key b.key`,
	}
	root := &cobra.Command{Use: "root", Short: "Make root versions"}
	root.AddCommand(trustRootNewCmd(), trustRootNextCmd())
	cmd.AddCommand(root, trustSignCmd())
	return cmd
}

type roleFlags struct {
	rootKeys, indexKeys           []string
	rootThreshold, indexThreshold int
	expires                       time.Duration
	out                           string
	// Delegations: patterns, key files and thresholds by name.
	delegate, delegateKeys, delegateThreshold []string
	undelegate                                []string
}

// delegations reads the delegation flags, by name in the order given.
func (f *roleFlags) delegations() (names []string, patterns, keys map[string][]string, thresholds map[string]int, err error) {
	patterns, keys, thresholds = map[string][]string{}, map[string][]string{}, map[string]int{}
	split := func(flag, v string) (string, string, error) {
		name, val, ok := strings.Cut(v, "=")
		if !ok || name == "" || val == "" {
			return "", "", fmt.Errorf("--%s %q: want NAME=VALUE", flag, v)
		}
		return name, val, nil
	}
	for _, v := range f.delegate {
		name, val, err := split("delegate", v)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		if _, dup := patterns[name]; !dup {
			names = append(names, name)
		}
		patterns[name] = append(patterns[name], strings.Split(val, ",")...)
	}
	for _, v := range f.delegateKeys {
		name, file, err := split("delegate-key", v)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		if _, ok := patterns[name]; !ok {
			return nil, nil, nil, nil, fmt.Errorf("--delegate-key %s: no --delegate %s=PATTERNS", name, name)
		}
		k, err := readKeys([]string{file})
		if err != nil {
			return nil, nil, nil, nil, err
		}
		keys[name] = append(keys[name], k...)
	}
	for _, v := range f.delegateThreshold {
		name, val, err := split("delegate-threshold", v)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		n, err := strconv.Atoi(val)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("--delegate-threshold %s: %w", name, err)
		}
		thresholds[name] = n
	}
	return names, patterns, keys, thresholds, nil
}

// applyDelegations puts the delegations of the flags into a root, after
// the ones it keeps; a delegation given again replaces the old one.
func (f *roleFlags) applyDelegations(r *repo.Root, keep []repo.Delegation, oldKeys map[string]string) error {
	names, patterns, keys, thresholds, err := f.delegations()
	if err != nil {
		return err
	}
	drop := map[string]bool{}
	for _, n := range append(append([]string(nil), names...), f.undelegate...) {
		drop[n] = true
	}
	for _, d := range keep {
		if drop[d.Name] {
			continue
		}
		var k []string
		for _, id := range d.KeyIDs {
			k = append(k, oldKeys[id])
		}
		if err := r.Delegate(d.Name, d.Packages, k, d.Threshold); err != nil {
			return err
		}
	}
	for _, n := range names {
		t := thresholds[n]
		if t == 0 {
			t = 1
		}
		if err := r.Delegate(n, patterns[n], keys[n], t); err != nil {
			return err
		}
	}
	return nil
}

func (f *roleFlags) bind(cmd *cobra.Command) {
	cmd.Flags().StringArrayVar(&f.rootKeys, "root-key", nil, "public key file allowed to sign the root (repeatable)")
	cmd.Flags().IntVar(&f.rootThreshold, "root-threshold", 0, "root signatures needed")
	cmd.Flags().StringArrayVar(&f.indexKeys, "index-key", nil, "public key file allowed to sign the index (repeatable)")
	cmd.Flags().IntVar(&f.indexThreshold, "index-threshold", 0, "index signatures needed")
	cmd.Flags().DurationVar(&f.expires, "expires", 365*24*time.Hour, "how long the root stays valid")
	cmd.Flags().StringVarP(&f.out, "output", "o", "root.yaml", "where to write the unsigned root")
	cmd.Flags().StringArrayVar(&f.delegate, "delegate", nil, "NAME=PATTERN[,PATTERN]: hand the packages matching the patterns to the delegation's keys (repeatable)")
	cmd.Flags().StringArrayVar(&f.delegateKeys, "delegate-key", nil, "NAME=FILE: public key file of a delegation (repeatable)")
	cmd.Flags().StringArrayVar(&f.delegateThreshold, "delegate-threshold", nil, "NAME=N: signatures a delegation needs on each version (default 1)")
}

func readKeys(files []string) ([]string, error) {
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

func writeRoot(cmd *cobra.Command, sr *repo.SignedRoot, out string) error {
	raw, err := yaml.Marshal(sr)
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, raw, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "wrote %s: root version %d, unsigned; sign it with kubepkg trust sign\n", out, sr.Signed.Version)
	return nil
}

func trustRootNewCmd() *cobra.Command {
	var f roleFlags
	cmd := &cobra.Command{
		Use:   "new",
		Short: "Make version 1 of a root",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rk, err := readKeys(f.rootKeys)
			if err != nil {
				return err
			}
			ik, err := readKeys(f.indexKeys)
			if err != nil {
				return err
			}
			sr, err := repo.NewRoot(rk, f.rootThreshold, ik, f.indexThreshold, time.Now().Add(f.expires))
			if err != nil {
				return err
			}
			if err := f.applyDelegations(&sr.Signed, nil, nil); err != nil {
				return err
			}
			return writeRoot(cmd, sr, f.out)
		},
	}
	f.bind(cmd)
	return cmd
}

func trustRootNextCmd() *cobra.Command {
	var f roleFlags
	cmd := &cobra.Command{
		Use:   "next <current-root.yaml>",
		Short: "Make the next root version, to rotate keys or change thresholds",
		Long: `Next writes the version after the given root. Roles not given keep their
keys and thresholds, and delegations not given again are kept. It must be signed by enough root keys of the current
version and of the new one before clients accept it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			cur, err := repo.ParseRoot(raw)
			if err != nil {
				return err
			}
			keep := func(role string, files []string, threshold int) ([]string, int, error) {
				r := cur.Signed.Roles[role]
				if threshold == 0 {
					threshold = r.Threshold
				}
				if len(files) > 0 {
					k, err := readKeys(files)
					return k, threshold, err
				}
				var k []string
				for _, id := range r.KeyIDs {
					k = append(k, cur.Signed.Keys[id])
				}
				return k, threshold, nil
			}
			rk, rt, err := keep(repo.RoleRoot, f.rootKeys, f.rootThreshold)
			if err != nil {
				return err
			}
			ik, it, err := keep(repo.RoleIndex, f.indexKeys, f.indexThreshold)
			if err != nil {
				return err
			}
			sr, err := repo.NewRoot(rk, rt, ik, it, time.Now().Add(f.expires))
			if err != nil {
				return err
			}
			sr.Signed.Version = cur.Signed.Version + 1
			if err := f.applyDelegations(&sr.Signed, cur.Signed.Delegations, cur.Signed.Keys); err != nil {
				return err
			}
			return writeRoot(cmd, sr, f.out)
		},
	}
	f.bind(cmd)
	cmd.Flags().StringArrayVar(&f.undelegate, "undelegate", nil, "remove this delegation (repeatable)")
	return cmd
}

func trustSignCmd() *cobra.Command {
	var keyFile, keyEnv string
	cmd := &cobra.Command{
		Use:   "sign <root.yaml|index.yaml|packagesources.yaml>",
		Short: "Add one signature to a root, an index or package versions",
		Long: `Sign adds the key's signature to a root, inside the file, or to an index,
in <index>.sig next to it, keeping signatures already there: each signer
signs with their own key, on their own machine.

Given PackageSources, as kubepkg build writes them, it signs each version
with a delegated key, in the kubepkg.dev/signatures annotation; repo index
carries the signatures into the index.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := signingKey(keyFile, keyEnv)
			if err != nil {
				return err
			}
			if key == nil {
				return fmt.Errorf("give --key or --key-env")
			}
			raw, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			if sr, err := repo.ParseRoot(raw); err == nil {
				if err := repo.SignRoot(sr, key); err != nil {
					return err
				}
				out, err := yaml.Marshal(sr)
				if err != nil {
					return err
				}
				if err := os.WriteFile(args[0], out, 0o644); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "root version %d now has %d signatures\n", sr.Signed.Version, len(sr.Signatures))
				return nil
			}
			if bytes.Contains(raw, []byte("kind: PackageSource")) {
				out, signed, err := repo.SignSources(raw, key)
				if err != nil {
					return err
				}
				if err := os.WriteFile(args[0], out, 0o644); err != nil {
					return err
				}
				for _, v := range signed {
					fmt.Fprintf(cmd.OutOrStdout(), "signed %s\n", v)
				}
				return nil
			}
			if _, err := repo.Parse(raw); err != nil {
				return fmt.Errorf("%s is neither a root, an index nor PackageSources", args[0])
			}
			sigFile := args[0] + repo.SignatureSuffix
			existing, err := os.ReadFile(sigFile)
			if err != nil && !os.IsNotExist(err) {
				return err
			}
			sigs, err := repo.SignIndex(raw, existing, key)
			if err != nil {
				return err
			}
			if err := os.WriteFile(sigFile, sigs, 0o644); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "signed %s in %s\n", args[0], sigFile)
			return nil
		},
	}
	cmd.Flags().StringVar(&keyFile, "key", "", "ed25519 private key file")
	cmd.Flags().StringVar(&keyEnv, "key-env", "", "environment variable holding the PEM private key")
	return cmd
}
