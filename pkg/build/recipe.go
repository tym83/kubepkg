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

// Package build turns recipes into installable packages.
//
// A recipe is to kubepkg what a debian/ directory is to Debian: it names
// upstream sources pinned by digest, says how to make charts of them, adds
// our own values and files on top, and carries the package metadata. The
// result is a package tree of ready charts, published as an OCI artifact,
// and the PackageSource that installs it. Installing never touches the
// upstream again.
package build

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Masterminds/semver/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/tym83/kubepkg/api/v1beta1"
)

// RecipeKind is the kind of a recipe document.
const RecipeKind = "Recipe"

// RecipeFile is the file a recipe directory must contain.
const RecipeFile = "recipe.yaml"

// Recipe describes how to build one version of a package.
type Recipe struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   metav1.ObjectMeta `json:"metadata"`
	Spec       RecipeSpec        `json:"spec"`
}

// RecipeSpec is the body of a recipe.
type RecipeSpec struct {
	// Version is the upstream version being packaged.
	Version string `json:"version"`
	// Build numbers our packagings of that version.
	Build int32 `json:"build,omitempty"`
	// Sources are the upstream inputs, by name.
	Sources map[string]Source `json:"sources"`
	// Charts are built into the package tree, one directory each, by name.
	Charts map[string]Chart `json:"charts"`
	// Package is the PackageSource spec. Its components refer to built
	// charts by path; version, build and sourceRef are filled in by the
	// build.
	Package v1beta1.PackageSourceSpec `json:"package"`
}

// Source is one upstream input. Exactly one of URL, Chart, Dir and Plugin
// is set.
type Source struct {
	// URL is a file or a .tar.gz/.tgz archive, checked against SHA256.
	URL    string `json:"url,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	// Path selects a file or directory inside an archive.
	Path string `json:"path,omitempty"`
	// Chart is a published Helm chart; it is pinned by digest.
	Chart *v1beta1.ChartRef `json:"chart,omitempty"`
	// Dir is a directory next to the recipe, for charts we maintain.
	Dir string `json:"dir,omitempty"`
	// Plugin fetches the source with a source plugin, e.g. from git or an
	// internal artifact store; With is passed to it. A plugin must produce
	// the same files every time for the same With.
	Plugin string         `json:"plugin,omitempty"`
	With   map[string]any `json:"with,omitempty"`
}

// PluginCall runs a named step plugin with parameters.
type PluginCall struct {
	Plugin string         `json:"plugin"`
	With   map[string]any `json:"with,omitempty"`
}

// Chart says how to make one chart of the sources.
type Chart struct {
	// From names the sources. One source that is a chart is used as the
	// chart; anything else must be Kubernetes manifests, which are wrapped
	// into a chart in the order given.
	From []string `json:"from"`
	// Values are merged over the chart's values.yaml: the package's
	// defaults, which users can still override.
	Values map[string]any `json:"values,omitempty"`
	// Overlay is a directory next to the recipe whose files replace or add
	// files in the chart, for changes values cannot express.
	Overlay string `json:"overlay,omitempty"`
	// Exclude drops objects from wrapped manifests, e.g. the Namespace an
	// upstream bundle creates, which kubepkg creates itself.
	Exclude []Selector `json:"exclude,omitempty"`
	// Patches change objects of wrapped manifests with JSON merge patches
	// (RFC 7386; null removes a field): the recipe's equivalent of a
	// Debian patch to the upstream.
	Patches []Patch `json:"patches,omitempty"`
	// Steps run step plugins on the finished chart, in order: what a
	// distribution needs beyond values and overlays, such as moving images
	// to its own registry.
	Steps []PluginCall `json:"steps,omitempty"`
}

// Patch is a JSON merge patch for the objects Selector matches.
type Patch struct {
	Selector `json:",inline"`
	Merge    map[string]any `json:"merge"`
}

// Selector matches manifest objects; empty fields match anything.
type Selector struct {
	Kind string `json:"kind,omitempty"`
	Name string `json:"name,omitempty"`
}

// LoadRecipe reads dir/recipe.yaml and checks it.
func LoadRecipe(dir string) (*Recipe, error) {
	raw, err := os.ReadFile(filepath.Join(dir, RecipeFile))
	if err != nil {
		return nil, err
	}
	var r Recipe
	if err := yaml.UnmarshalStrict(raw, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", RecipeFile, err)
	}
	if err := r.check(); err != nil {
		return nil, fmt.Errorf("recipe %s: %w", r.Metadata.Name, err)
	}
	return &r, nil
}

func (r *Recipe) check() error {
	if r.Kind != RecipeKind {
		return fmt.Errorf("kind is %q, want %s", r.Kind, RecipeKind)
	}
	if r.Metadata.Name == "" {
		return fmt.Errorf("metadata.name is empty")
	}
	if _, err := semver.StrictNewVersion(strings.TrimPrefix(r.Spec.Version, "v")); err != nil {
		return fmt.Errorf("spec.version %q is not an exact semver version", r.Spec.Version)
	}
	for name, s := range r.Spec.Sources {
		set := 0
		for _, ok := range []bool{s.URL != "", s.Chart != nil, s.Dir != "", s.Plugin != ""} {
			if ok {
				set++
			}
		}
		if set != 1 {
			return fmt.Errorf("source %s: set exactly one of url, chart, dir and plugin", name)
		}
		if s.URL != "" && s.SHA256 == "" {
			return fmt.Errorf("source %s: url needs sha256", name)
		}
		if s.Chart != nil && s.Chart.Digest == "" {
			return fmt.Errorf("source %s: chart needs a digest", name)
		}
	}
	for name, c := range r.Spec.Charts {
		if len(c.From) == 0 {
			return fmt.Errorf("chart %s: from is empty", name)
		}
		for i, st := range c.Steps {
			if st.Plugin == "" {
				return fmt.Errorf("chart %s: step %d names no plugin", name, i+1)
			}
		}
		for i, p := range c.Patches {
			if p.Kind == "" && p.Name == "" {
				return fmt.Errorf("chart %s: patch %d selects nothing in particular; give a kind or a name", name, i+1)
			}
			if len(p.Merge) == 0 {
				return fmt.Errorf("chart %s: patch %d is empty", name, i+1)
			}
		}
		if len(c.Exclude) > 0 || len(c.Patches) > 0 {
			for _, f := range c.From {
				if r.Spec.Sources[f].Chart != nil {
					return fmt.Errorf("chart %s: exclude and patches apply to wrapped manifests, not to source %s, which is a chart", name, f)
				}
			}
			for _, e := range c.Exclude {
				if e.Kind == "" && e.Name == "" {
					return fmt.Errorf("chart %s: an exclude entry with no kind and no name would drop everything", name)
				}
			}
		}
		for _, f := range c.From {
			if _, ok := r.Spec.Sources[f]; !ok {
				return fmt.Errorf("chart %s: unknown source %s", name, f)
			}
		}
	}
	if r.Spec.Package.Version != "" || r.Spec.Package.Build != 0 || r.Spec.Package.SourceRef != nil {
		return fmt.Errorf("package: version, build and sourceRef come from the build, do not set them")
	}
	for _, v := range r.Spec.Package.Variants {
		for _, c := range v.Components {
			if c.Chart != nil {
				return fmt.Errorf("component %s: packages in a repository are built; refer to a built chart with path", c.Name)
			}
			if _, ok := r.Spec.Charts[c.Path]; !ok {
				return fmt.Errorf("component %s: path %q is not a chart this recipe builds", c.Name, c.Path)
			}
		}
	}
	return nil
}
