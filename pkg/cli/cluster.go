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

// Package cli holds the kubepkg commands. A distribution builds its own
// CLI from NewRootCommand: its own name and API group, plus commands of
// its own.
package cli

import (
	"fmt"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/discovery"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/kuberoot-dev/kubepkg/api/v1"
	"github.com/kuberoot-dev/kubepkg/pkg/repo"
	"github.com/kuberoot-dev/kubepkg/pkg/version"
)

// Options configure the root command.
type Options struct {
	// Name is the binary name shown in help.
	Name string
	// Short is the one-line description.
	Short string
	// APIGroup is the default of --api-group.
	APIGroup string
	// IndexFetchers fetch repository indexes by URL scheme, as in the
	// operator; a distribution adds its transports.
	IndexFetchers repo.Fetchers
	// Policy admits versions, as in the operator, so plans offer only
	// what the operator will install.
	Policy repo.Policy
}

// DefaultOptions are the plain kubepkg CLI.
func DefaultOptions() Options {
	return Options{
		Name:          "kubepkg",
		Short:         "Package manager for Kubernetes platforms and distributions",
		APIGroup:      v1.GroupName,
		IndexFetchers: repo.DefaultFetchers(),
		Policy:        repo.AllowAll{},
	}
}

// cluster holds the flags that select the cluster and the API group.
type cluster struct {
	kubeContext, apiGroup string
	fetchers              repo.Fetchers
	policy                repo.Policy
}

func (c *cluster) config() (*rest.Config, error) {
	return config.GetConfigWithContext(c.kubeContext)
}

func (c *cluster) client() (client.Client, error) {
	cfg, err := c.config()
	if err != nil {
		return nil, err
	}
	scheme := runtime.NewScheme()
	if err := v1.AddToSchemeForGroup(c.apiGroup)(scheme); err != nil {
		return nil, err
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := checkServed(cfg, c.apiGroup); err != nil {
		return nil, err
	}
	return client.New(cfg, client.Options{Scheme: scheme})
}

// checkServed explains a cluster that does not serve this CLI's API
// version, instead of a bare discovery error on the first request.
func checkServed(cfg *rest.Config, group string) error {
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return err
	}
	groups, err := dc.ServerGroups()
	if err != nil {
		return nil // let the request itself report it
	}
	for _, g := range groups.Groups {
		if g.Name != group {
			continue
		}
		var served []string
		for _, v := range g.Versions {
			if v.Version == v1.Version {
				return nil
			}
			served = append(served, v.Version)
		}
		return fmt.Errorf("the cluster serves %s %s, an older kubepkg; this CLI needs %s/%s: upgrade kubepkg in the cluster (helm upgrade kubepkg oci://ghcr.io/kuberoot-dev/charts/kubepkg), or use the CLI release that matches it", group, strings.Join(served, ", "), group, v1.Version)
	}
	return fmt.Errorf("the cluster does not serve %s: kubepkg is not installed there (helm install kubepkg oci://ghcr.io/kuberoot-dev/charts/kubepkg -n kubepkg-system --create-namespace)", group)
}

// NewRootCommand returns the root command with every kubepkg command.
func NewRootCommand(o Options) *cobra.Command {
	root := &cobra.Command{
		Use:           o.Name,
		Short:         o.Short,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cl := &cluster{fetchers: o.IndexFetchers, policy: o.Policy}
	root.PersistentFlags().StringVar(&cl.kubeContext, "context", "", "kubeconfig context (default: the current one)")
	root.PersistentFlags().StringVar(&cl.apiGroup, "api-group", o.APIGroup, "API group the kubepkg types are served under")
	root.Version = version.Version
	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print the kubepkg version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintln(cmd.OutOrStdout(), version.Version)
		},
	})
	root.AddCommand(initCmd(), validateCmd(), imagesCmd(cl), buildCmd(), pushCmd(), repoCmd(cl), trustCmd(), clusterCmd(cl), setCmd(cl), searchCmd(cl), installCmd(cl), adoptCmd(cl), planCmd(cl), renderCmd(cl), bundleCmd(cl), sbomCmd(cl), scanCmd(cl), removeCmd(cl), listCmd(cl), historyCmd(cl), rollbackCmd(cl))
	return root
}
