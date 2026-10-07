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

package build

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"helm.sh/helm/v4/pkg/chart/common"
	"helm.sh/helm/v4/pkg/chart/common/util"
	"helm.sh/helm/v4/pkg/chart/loader"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	chartutilv2 "helm.sh/helm/v4/pkg/chart/v2/util"
	"helm.sh/helm/v4/pkg/engine"
	"sigs.k8s.io/yaml"

	"github.com/tym83/kubepkg/pkg/images"
	"github.com/tym83/kubepkg/pkg/repo"
)

// Report is what Validate found. Errors make a recipe unfit to publish;
// warnings are worth fixing.
type Report struct {
	Recipe   string
	Errors   []string
	Warnings []string
}

// OK is true when there are no errors.
func (r Report) OK() bool { return len(r.Errors) == 0 }

// Validate builds the recipe in dir without publishing it, so every
// source is fetched and checked, renders its charts with their default
// values, and checks the package metadata against what the charts ship.
func Validate(ctx context.Context, dir string, opts Options) Report {
	rep := Report{Recipe: dir}
	res, err := Build(ctx, dir, opts)
	if err != nil {
		rep.Errors = append(rep.Errors, err.Error())
		return rep
	}
	rep.Recipe = res.Recipe.Metadata.Name
	if res.Recipe.Metadata.Annotations[repo.AnnotationDescription] == "" {
		rep.Warnings = append(rep.Warnings, "no "+repo.AnnotationDescription+" annotation: search shows nothing for the package")
	}
	if len(res.Recipe.Spec.Charts) == 0 {
		return rep // a meta package ships nothing to check
	}
	if res.Recipe.Spec.Package.Rollback == nil {
		rep.Warnings = append(rep.Warnings, "rollback is not declared: a failed upgrade will not be rolled back; declare rollback.safe either way")
	}

	shipped := map[string]string{}
	for _, name := range sortedKeys(res.Recipe.Spec.Charts) {
		crds, err := chartCRDs(filepath.Join(res.TreeDir, name))
		if err != nil {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("chart %s does not render with its default values, so its CRDs were not checked: %v", name, err))
			continue
		}
		for _, c := range crds {
			shipped[c] = name
		}
	}
	declared := map[string]bool{}
	for _, c := range res.Recipe.Spec.Package.CRDs {
		declared[c] = true
	}
	for _, c := range sortedKeys(shipped) {
		if !declared[c] {
			rep.Errors = append(rep.Errors, fmt.Sprintf("chart %s ships CRD %s, which package.crds does not declare: its ownership would not be tracked", shipped[c], c))
		}
	}
	for _, c := range res.Recipe.Spec.Package.CRDs {
		if _, ok := shipped[c]; !ok {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("package.crds declares %s, which no chart ships (fine if an operator in the package creates it)", c))
		}
	}
	checkImages(res, &rep)
	checkCRDUpgrades(res, &rep)
	if HasImageRules(res.Recipe) {
		unchecked, err := VerifyImages(ctx, res.Recipe, dir, opts.VerifyImages)
		if err != nil {
			rep.Errors = append(rep.Errors, err.Error())
		} else if len(unchecked) > 0 {
			rep.Warnings = append(rep.Warnings, "no signature rule covers "+strings.Join(unchecked, ", "))
		}
	}
	return rep
}

// checkCRDUpgrades warns about charts that ship CRDs in crds/, which Helm
// installs but never upgrades, when the component does not ask kubepkg to.
func checkCRDUpgrades(res *Result, rep *Report) {
	for _, v := range res.Recipe.Spec.Package.Variants {
		for _, c := range v.Components {
			if c.Path == "" || (c.Install != nil && c.Install.UpgradeCRDs != "") {
				continue
			}
			loaded, err := loader.Load(filepath.Join(res.TreeDir, c.Path))
			if err != nil {
				continue
			}
			ch, ok := loaded.(*chartv2.Chart)
			// Subcharts their conditions switch off install no CRDs.
			if !ok || chartutilv2.ProcessDependencies(ch, map[string]any{}) != nil {
				continue
			}
			if len(ch.CRDObjects()) > 0 {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("component %s ships CRDs in crds/, which Helm installs but never upgrades; set install.upgradeCRDs: CreateReplace to upgrade them with the package", c.Name))
			}
		}
	}
}

// checkImages compares the images the charts run with package.images.
func checkImages(res *Result, rep *Report) {
	pinned := res.Recipe.Spec.Package.Images
	for _, p := range pinned {
		if n, err := images.Normalize(p); err != nil {
			rep.Errors = append(rep.Errors, err.Error())
		} else if n != p || !images.Pinned(p) {
			rep.Errors = append(rep.Errors, fmt.Sprintf("package.images entry %s must be in full form with a digest, like %s@sha256:...; kubepkg images prints them", p, n))
		}
	}
	running, err := RenderedImages(res)
	if err != nil {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("images were not checked: %v", err))
		return
	}
	if len(pinned) == 0 {
		if len(running) > 0 {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("package.images is empty, so the %d images the charts run are not pinned or signed and kubepkg bundle has to guess them; kubepkg images prints the list", len(running)))
		}
		return
	}
	for _, img := range running {
		if !images.Covered(img, pinned) {
			rep.Errors = append(rep.Errors, fmt.Sprintf("the charts run %s, which package.images does not pin", img))
		}
	}
}

// RenderedImages lists the images the built charts run with their default
// values. Images an operator in the package deploys on its own do not
// appear in any chart; package.images lists them by hand.
func RenderedImages(res *Result) ([]string, error) {
	var docs []string
	for _, name := range sortedKeys(res.Recipe.Spec.Charts) {
		d, err := renderChart(filepath.Join(res.TreeDir, name))
		if err != nil {
			return nil, fmt.Errorf("chart %s does not render with its default values: %w", name, err)
		}
		docs = append(docs, d...)
	}
	return images.FromManifests(docs...)
}

// renderChart renders a chart with its default values, as helm install
// would, and returns the documents it installs: templates of the chart
// and its enabled subcharts, and their crds/ directories.
func renderChart(dir string) ([]string, error) { return RenderChart(dir, nil) }

// RenderChart renders a chart with values over its defaults, as helm
// install would, and returns the documents it installs: templates of the
// chart and its enabled subcharts, and their crds/ directories.
func RenderChart(dir string, values map[string]any) ([]string, error) {
	if values == nil {
		values = map[string]any{}
	}
	loaded, err := loader.Load(dir)
	if err != nil {
		return nil, err
	}
	ch, ok := loaded.(*chartv2.Chart)
	if !ok {
		return nil, fmt.Errorf("%s: unsupported chart API version", dir)
	}
	// Drop subcharts their conditions disable, as helm install does, so
	// their templates and CRDs are not counted.
	if err := chartutilv2.ProcessDependencies(ch, values); err != nil {
		return nil, err
	}
	vals, err := util.ToRenderValues(ch, values, common.ReleaseOptions{Name: "validate", Namespace: "validate", IsInstall: true}, common.DefaultCapabilities)
	if err != nil {
		return nil, err
	}
	rendered, err := engine.Render(ch, vals)
	if err != nil {
		return nil, err
	}
	var docs []string
	for _, out := range rendered {
		docs = append(docs, out)
	}
	// crds/ of the chart and of every enabled subchart, packed or not.
	for _, crd := range ch.CRDObjects() {
		docs = append(docs, string(crd.File.Data))
	}
	return docs, nil
}

// chartCRDs lists the CRDs a chart installs with its default values.
func chartCRDs(dir string) ([]string, error) {
	docs, err := renderChart(dir)
	if err != nil {
		return nil, err
	}
	return crdNames(docs)
}

// crdNames lists the CRDs among rendered documents.
func crdNames(docs []string) ([]string, error) {
	seen := map[string]bool{}
	for _, text := range docs {
		for _, doc := range docSeparator.Split(text, -1) {
			var head struct {
				Kind     string `json:"kind"`
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
			}
			if yaml.Unmarshal([]byte(doc), &head) == nil && head.Kind == "CustomResourceDefinition" && head.Metadata.Name != "" {
				seen[head.Metadata.Name] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
