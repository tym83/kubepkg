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

package resolve

import (
	"github.com/tym83/kubepkg/api/v1beta1"
)

// RequirementsOf turns a variant's dependsOn and requires into
// requirements, minus the packages and capabilities listed in ignore.
func RequirementsOf(v *v1beta1.Variant, ignore []string) []Requirement {
	ignored := map[string]bool{}
	for _, d := range ignore {
		ignored[d] = true
	}
	var out []Requirement
	seen := map[string]bool{}
	for _, d := range v.DependsOn {
		if !ignored[d] && !seen[d] {
			seen[d] = true
			out = append(out, Requirement{Package: d})
		}
	}
	for _, q := range v.Requires {
		if q.Package != "" && ignored[q.Package] {
			continue
		}
		if q.Capability != "" && ignored[q.Capability] {
			continue
		}
		out = append(out, Requirement{Package: q.Package, Capability: q.Capability, Version: q.Version, Optional: q.Optional})
	}
	return out
}

// ReleaseOf describes one variant of a package version for resolving.
// A missing variant leaves the release without requirements.
func ReleaseOf(name string, spec *v1beta1.PackageSourceSpec, variant string) Release {
	r := Release{Name: name, Version: spec.Version, Provides: spec.Provides, Conflicts: spec.Conflicts}
	if r.Version == "" {
		r.Version = Unversioned
	}
	if variant == "" {
		variant = "default"
	}
	for i := range spec.Variants {
		if spec.Variants[i].Name == variant {
			r.Requires = RequirementsOf(&spec.Variants[i], nil)
		}
	}
	return r
}
