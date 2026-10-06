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
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/tym83/kubepkg/api/v1alpha1"
)

func clusterCmd(cl *cluster) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Register member clusters with a hub",
	}
	cmd.AddCommand(clusterAddCmd(cl), clusterListCmd(cl))
	return cmd
}

// coreClient is the hub client with core types, for Secrets.
func (c *cluster) coreClient() (client.Client, error) {
	cfg, err := c.config()
	if err != nil {
		return nil, err
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := v1alpha1.AddToSchemeForGroup(c.apiGroup)(scheme); err != nil {
		return nil, err
	}
	return client.New(cfg, client.Options{Scheme: scheme})
}

func clusterAddCmd(cl *cluster) *cobra.Command {
	var (
		kubeconfig, memberContext, namespace string
		labels                               []string
	)
	cmd := &cobra.Command{
		Use:   "add <name> --kubeconfig <file> [--member-context <ctx>] [--label k=v]...",
		Short: "Register a member cluster: its kubeconfig goes into a Secret in the hub",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := os.ReadFile(kubeconfig)
			if err != nil {
				return err
			}
			// Keep only the member's context, so the Secret holds no more
			// than the hub needs.
			cfg, err := clientcmd.Load(raw)
			if err != nil {
				return err
			}
			if memberContext != "" {
				cfg.CurrentContext = memberContext
			}
			if _, ok := cfg.Contexts[cfg.CurrentContext]; !ok {
				return fmt.Errorf("context %q not in %s", cfg.CurrentContext, kubeconfig)
			}
			if err := clientcmdapi.MinifyConfig(cfg); err != nil {
				return err
			}
			if err := clientcmdapi.FlattenConfig(cfg); err != nil {
				return err
			}
			minimal, err := clientcmd.Write(*cfg)
			if err != nil {
				return err
			}
			lbls := map[string]string{}
			for _, l := range labels {
				k, v, ok := strings.Cut(l, "=")
				if !ok {
					return fmt.Errorf("label %q is not k=v", l)
				}
				lbls[k] = v
			}
			c, err := cl.coreClient()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cluster-" + args[0], Namespace: namespace}}
			if _, err := controllerutil.CreateOrUpdate(ctx, c, sec, func() error {
				sec.Data = map[string][]byte{"kubeconfig": minimal}
				return nil
			}); err != nil {
				return err
			}
			mc := &v1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: args[0]}}
			if _, err := controllerutil.CreateOrUpdate(ctx, c, mc, func() error {
				mc.Labels = lbls
				mc.Spec.KubeconfigSecretRef = v1alpha1.SecretKeyRef{Namespace: namespace, Name: sec.Name, Key: "kubeconfig"}
				return nil
			}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "cluster %s registered; kubepkg must run in it (cluster list shows whether the hub reaches it)\n", args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "kubeconfig of the member cluster")
	cmd.Flags().StringVar(&memberContext, "member-context", "", "context in that kubeconfig (default: its current one)")
	cmd.Flags().StringVar(&namespace, "namespace", "kubepkg-system", "hub namespace for the kubeconfig Secret")
	cmd.Flags().StringArrayVar(&labels, "label", nil, "cluster label k=v for PackageSet selectors (repeatable)")
	_ = cmd.MarkFlagRequired("kubeconfig")
	return cmd
}

func clusterListCmd(cl *cluster) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List member clusters",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := cl.client()
			if err != nil {
				return err
			}
			var list v1alpha1.ClusterList
			if err := c.List(cmd.Context(), &list); err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tKUBERNETES\tREADY\tLABELS\tSTATUS")
			for _, m := range list.Items {
				ready, msg := "Unknown", ""
				if cond := meta.FindStatusCondition(m.Status.Conditions, "Ready"); cond != nil {
					ready, msg = string(cond.Status), cond.Message
				}
				var ls []string
				for k, v := range m.Labels {
					ls = append(ls, k+"="+v)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", m.Name, dash(m.Status.KubernetesVersion), ready, dash(strings.Join(ls, ",")), msg)
			}
			return w.Flush()
		},
	}
}

func setCmd(cl *cluster) *cobra.Command {
	cmd := &cobra.Command{Use: "set", Short: "Inspect PackageSets on a hub"}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List PackageSets and how far each cluster got",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := cl.client()
			if err != nil {
				return err
			}
			var list v1alpha1.PackageSetList
			if err := c.List(cmd.Context(), &list); err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "SET\tCLUSTER\tREADY\tSTATUS")
			for _, s := range list.Items {
				if len(s.Status.Clusters) == 0 {
					fmt.Fprintf(w, "%s\t-\t-\tno clusters selected\n", s.Name)
				}
				for _, cs := range s.Status.Clusters {
					fmt.Fprintf(w, "%s\t%s\t%d/%d\t%s\n", s.Name, cs.Name, cs.Ready, cs.Total, dash(cs.Message))
				}
			}
			return w.Flush()
		},
	})
	return cmd
}
