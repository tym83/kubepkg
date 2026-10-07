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

package controller

import (
	"github.com/tym83/kubepkg/api/v1"
)

// ReleaseName is the release a component becomes.
func ReleaseName(c v1.Component) string { return releaseName(c) }

// ComponentOrder lists component names so every component comes after the
// ones it depends on, keeping PackageSource order otherwise; a cycle is an
// error. Tools that render packages for other installers use the order
// the operator applies them in.
func ComponentOrder(comps []v1.Component) ([]string, error) { return topoOrder(comps) }
