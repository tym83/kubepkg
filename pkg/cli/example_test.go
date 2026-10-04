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

package cli_test

import (
	"github.com/spf13/cobra"

	"github.com/tym83/kubepkg/pkg/cli"
)

// A distribution's own CLI: its name and API group, and a command of its
// own next to the kubepkg ones.
func Example_distribution() {
	opts := cli.DefaultOptions()
	opts.Name = "examplectl"
	opts.APIGroup = "packages.example.com"
	root := cli.NewRootCommand(opts)
	root.AddCommand(&cobra.Command{
		Use:   "upgrade-platform",
		Short: "Upgrade the whole platform to the next release",
		RunE:  func(*cobra.Command, []string) error { return nil },
	})
	_ = root.Execute()
}
