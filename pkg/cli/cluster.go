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
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/tym83/kubepkg/api/v1alpha1"
	"github.com/tym83/kubepkg/pkg/repo"
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
		APIGroup:      v1alpha1.GroupName,
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
	if err := v1alpha1.AddToSchemeForGroup(c.apiGroup)(scheme); err != nil {
		return nil, err
	}
	return client.New(cfg, client.Options{Scheme: scheme})
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
	root.AddCommand(buildCmd(), pushCmd(), repoCmd(cl), searchCmd(cl), installCmd(cl), planCmd(cl), renderCmd(cl), removeCmd(cl), listCmd(cl), historyCmd(cl), rollbackCmd(cl))
	return root
}
