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

	"github.com/spf13/cobra"

	"github.com/tym83/kubepkg/pkg/source"
)

func pushCmd() *cobra.Command {
	var opts source.PushOptions
	cmd := &cobra.Command{
		Use:   "push <dir> <oci-ref>",
		Short: "Publish a package tree as an OCI artifact",
		Long: `Push packs a package tree into one reproducible gzipped tarball and
publishes it in the Flux artifact format, so kubepkg and Flux can both
read it. The same tree always produces the same digest.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := source.Push(cmd.Context(), args[0], args[1], opts)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "pushed %s@%s\n", args[1], res.Digest)
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.Source, "source", "", "source URL recorded in the artifact")
	cmd.Flags().StringVar(&opts.Revision, "revision", "", "source revision recorded in the artifact")
	cmd.Flags().BoolVar(&opts.PlainHTTP, "plain-http", false, "talk to the registry without TLS (local registries only)")
	cmd.Flags().StringVar(&opts.CredentialsFile, "registry-config", "", "Docker config file with registry credentials")
	return cmd
}
