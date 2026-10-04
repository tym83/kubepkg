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
	"flag"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/tym83/kubepkg/pkg/backend"
	"github.com/tym83/kubepkg/pkg/controller"
	"github.com/tym83/kubepkg/pkg/operator"
)

// A distribution's own operator: its API group and platform settings as
// defaults, and a backend of its own next to the standard ones.
func Example_distribution() {
	opts := operator.DefaultOptions()
	opts.Profile.Group = "packages.example.com"
	opts.Profile.ValuesSecret = "example-system/platform"
	opts.Profile.NamespaceLabels = map[string]string{"example.com/managed": "true"}
	opts.Backends["example"] = func(env operator.Env) (backend.Backend, controller.Preparer, error) {
		// Wrap or replace a standard backend, e.g. to install through the
		// distribution's own delivery system.
		return operator.HelmBackend(env)
	}
	opts.Backend = "example"
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	_ = operator.Run(ctrl.SetupSignalHandler(), ctrl.GetConfigOrDie(), opts)
}
