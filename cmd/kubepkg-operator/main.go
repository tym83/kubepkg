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

// Command kubepkg-operator runs the kubepkg operator with its standard
// options. Distributions build their own from package operator.
package main

import (
	"flag"
	"os"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/kuberoot-dev/kubepkg/pkg/operator"
)

func main() {
	opts := operator.DefaultOptions()
	opts.BindFlags(flag.CommandLine)
	logOpts := zap.Options{}
	logOpts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&logOpts)))

	if err := operator.Run(ctrl.SetupSignalHandler(), ctrl.GetConfigOrDie(), opts); err != nil {
		ctrl.Log.WithName("setup").Error(err, "kubepkg-operator failed")
		os.Exit(1)
	}
}
