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
	"os"
	"path/filepath"
	"sort"

	"helm.sh/helm/v4/pkg/chart/common"
	"helm.sh/helm/v4/pkg/chart/common/util"
	"helm.sh/helm/v4/pkg/chart/loader"
	"helm.sh/helm/v4/pkg/engine"
	"sigs.k8s.io/yaml"

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
	return rep
}

// chartCRDs lists the CRDs a chart installs with its default values, from
// its templates and its crds/ directory.
func chartCRDs(dir string) ([]string, error) {
	ch, err := loader.Load(dir)
	if err != nil {
		return nil, err
	}
	vals, err := util.ToRenderValues(ch, map[string]any{}, common.ReleaseOptions{Name: "validate", Namespace: "validate", IsInstall: true}, common.DefaultCapabilities)
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
	_ = filepath.WalkDir(filepath.Join(dir, "crds"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if raw, err := os.ReadFile(p); err == nil {
				docs = append(docs, string(raw))
			}
		}
		return nil
	})
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
