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

package operator_test

import (
	"context"
	"flag"
	"fmt"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/tym83/kubepkg/pkg/cli"
	"github.com/tym83/kubepkg/pkg/operator"
	"github.com/tym83/kubepkg/pkg/repo"
)

// allowedRegistries admits only versions whose charts come from the
// distribution's own registry.
type allowedRegistries struct{ prefix string }

func (allowedRegistries) AdmitIndex(context.Context, string, []byte, *repo.Index) error { return nil }

func (p allowedRegistries) AdmitVersion(_ context.Context, _, pkg string, v repo.Version) error {
	for _, variant := range v.Spec.Variants {
		for _, c := range variant.Components {
			if c.Chart != nil && !strings.HasPrefix(c.Chart.Repository, p.prefix) {
				return fmt.Errorf("%s %s: chart %s is not from %s", pkg, v.Version, c.Chart.Repository, p.prefix)
			}
		}
	}
	return nil
}

// A distribution's operator: its own API group, its own trust rules, the
// standard flags.
func Example_operator() {
	opts := operator.DefaultOptions()
	opts.Profile.Group = "packages.example.org"
	opts.Policy = allowedRegistries{prefix: "oci://registry.example.org/"}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	if err := operator.Run(ctrl.SetupSignalHandler(), ctrl.GetConfigOrDie(), opts); err != nil {
		panic(err)
	}
}

// The matching CLI, with a command of the distribution's own.
func Example_cli() {
	o := cli.DefaultOptions()
	o.Name, o.Short, o.APIGroup = "exctl", "Example Platform packages", "packages.example.org"
	o.Policy = allowedRegistries{prefix: "oci://registry.example.org/"}
	root := cli.NewRootCommand(o)
	if err := root.Execute(); err != nil {
		panic(err)
	}
}
