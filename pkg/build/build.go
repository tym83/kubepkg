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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	jsonpatch "github.com/evanphx/json-patch/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/tym83/kubepkg/api/v1alpha1"
	"github.com/tym83/kubepkg/pkg/source"
)

// wrapTemplate renders every file under manifests/ verbatim. .Files.Get
// returns content without evaluating it, so manifests that carry their own
// {{ }} (alerting rules, dashboards) pass through untouched.
const wrapTemplate = `{{- range $path, $_ := .Files.Glob "manifests/*" }}
---
{{ $.Files.Get $path }}
{{- end }}
`

// Options control a build.
type Options struct {
	// Fetcher downloads charts; its cache is reused across builds.
	Fetcher *source.Fetcher
	// WorkDir receives the sources and the package tree.
	WorkDir string
	// Plugins extend the sources and steps a recipe can use.
	Plugins Plugins
}

// Result is a built package before publishing.
type Result struct {
	Recipe *Recipe
	// TreeDir holds one chart directory per built chart.
	TreeDir string
	// Source installs the tree once SourceRef is set by Publish.
	Source v1alpha1.PackageSource
}

type builder struct {
	ctx       context.Context
	recipe    *Recipe
	recipeDir string
	opts      Options
	resolved  map[string]string
}

// Build reads the recipe in dir and builds its package tree.
func Build(ctx context.Context, dir string, opts Options) (*Result, error) {
	// Plugins run with the recipe as their working directory; absolute
	// paths keep what they are told valid there.
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if opts.WorkDir, err = filepath.Abs(opts.WorkDir); err != nil {
		return nil, err
	}
	r, err := LoadRecipe(dir)
	if err != nil {
		return nil, err
	}
	b := &builder{ctx: ctx, recipe: r, recipeDir: dir, opts: opts, resolved: map[string]string{}}
	tree := filepath.Join(opts.WorkDir, "tree")
	if err := os.RemoveAll(opts.WorkDir); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(r.Spec.Charts))
	for n := range r.Spec.Charts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := b.chart(n, r.Spec.Charts[n], filepath.Join(tree, n)); err != nil {
			return nil, fmt.Errorf("chart %s: %w", n, err)
		}
	}
	src := v1alpha1.PackageSource{
		TypeMeta:   metav1TypeMeta(),
		ObjectMeta: *r.Metadata.DeepCopy(),
		Spec:       *r.Spec.Package.DeepCopy(),
	}
	src.Spec.Version = r.Spec.Version
	src.Spec.Build = r.Spec.Build
	return &Result{Recipe: r, TreeDir: tree, Source: src}, nil
}

// Publish pushes every built chart to registry (oci://host/path) as a
// Helm chart, oci://host/path/<package>/<chart>:<version>-<build>, and
// points the package's components at them by archive digest. Helm, Flux,
// Argo CD and werf install such charts as they are, so a built package
// does not need kubepkg to be installed. Tags are immutable: when a tag
// already holds the same content, built perhaps with another compressor,
// the published chart is reused; reused is true when all of them were.
func Publish(ctx context.Context, res *Result, registry string, opts source.PushOptions) (*v1alpha1.PackageSource, bool, error) {
	out := res.Source.DeepCopy()
	if len(res.Recipe.Spec.Charts) == 0 {
		// A meta package: only requirements, nothing to push.
		return out, false, nil
	}
	repository := strings.TrimSuffix(registry, "/") + "/" + res.Recipe.Metadata.Name
	version := fmt.Sprintf("%s-%d", strings.TrimPrefix(res.Recipe.Spec.Version, "v"), res.Recipe.Spec.Build)
	opts.Immutable = true
	names := make([]string, 0, len(res.Recipe.Spec.Charts))
	for n := range res.Recipe.Spec.Charts {
		names = append(names, n)
	}
	sort.Strings(names)
	refs := map[string]*v1alpha1.ChartRef{}
	reused := true
	for _, n := range names {
		pushed, err := source.PushChart(ctx, filepath.Join(res.TreeDir, n), repository, n, version, opts)
		if errors.Is(err, source.ErrTagChanged) {
			return nil, false, fmt.Errorf("%s %s build %d is already published with different content; published versions do not change, give the recipe a new build number", res.Recipe.Metadata.Name, res.Recipe.Spec.Version, res.Recipe.Spec.Build)
		}
		if err != nil {
			return nil, false, fmt.Errorf("chart %s: %w", n, err)
		}
		reused = reused && pushed.Reused
		refs[n] = &v1alpha1.ChartRef{Repository: repository, Name: n, Version: version, Digest: pushed.LayerDigest}
	}
	for vi := range out.Spec.Variants {
		for ci := range out.Spec.Variants[vi].Components {
			c := &out.Spec.Variants[vi].Components[ci]
			c.Chart, c.Path = refs[c.Path], ""
		}
	}
	return out, reused, nil
}

func (b *builder) chart(name string, c Chart, dst string) error {
	paths := make([]string, 0, len(c.From))
	for _, f := range c.From {
		p, err := b.resolve(f)
		if err != nil {
			return fmt.Errorf("source %s: %w", f, err)
		}
		paths = append(paths, p)
	}
	if len(paths) == 1 && isChart(paths[0]) {
		if err := source.CopyTree(paths[0], dst); err != nil {
			return err
		}
		// The chart cache marks finished downloads; the marker is not
		// part of the chart.
		if err := os.Remove(filepath.Join(dst, ".complete")); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else if err := b.wrap(name, paths, c.Exclude, c.Patches, dst); err != nil {
		return err
	}
	if len(c.Values) > 0 {
		if err := mergeValuesFile(filepath.Join(dst, "values.yaml"), c.Values); err != nil {
			return err
		}
	}
	if c.Overlay != "" {
		dir, err := within(b.recipeDir, c.Overlay)
		if err != nil {
			return err
		}
		if err := source.CopyTree(dir, dst); err != nil {
			return fmt.Errorf("overlay: %w", err)
		}
	}
	for _, st := range c.Steps {
		p, err := b.opts.Plugins.step(st.Plugin)
		if err != nil {
			return err
		}
		if err := p.Run(b.ctx, b.input(st.With, "", dst)); err != nil {
			return fmt.Errorf("step %s: %w", st.Plugin, err)
		}
	}
	return nil
}

func (b *builder) input(with map[string]any, out, chart string) PluginInput {
	return PluginInput{With: with, RecipeDir: b.recipeDir, Out: out, Chart: chart, Package: b.recipe.Metadata.Name, Version: b.recipe.Spec.Version}
}

// wrap makes a chart that applies the manifests in paths, in order.
func (b *builder) wrap(name string, paths []string, exclude []Selector, patches []Patch, dst string) error {
	matched := make([]bool, len(patches))
	var files []string
	for _, p := range paths {
		if isChart(p) {
			return fmt.Errorf("%s is a chart; a chart is used alone, not mixed with manifests", p)
		}
		found, err := manifests(p)
		if err != nil {
			return err
		}
		if len(found) == 0 {
			return fmt.Errorf("%s has no manifests", p)
		}
		files = append(files, found...)
	}
	if err := os.MkdirAll(filepath.Join(dst, "manifests"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dst, "templates"), 0o755); err != nil {
		return err
	}
	for i, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if len(exclude) > 0 || len(patches) > 0 {
			if raw, err = editObjects(raw, exclude, patches, matched); err != nil {
				return fmt.Errorf("%s: %w", filepath.Base(f), err)
			}
		}
		// The index prefix keeps the given order through .Files.Glob,
		// which sorts by name.
		out := filepath.Join(dst, "manifests", fmt.Sprintf("%04d-%s", i, filepath.Base(f)))
		if err := os.WriteFile(out, raw, 0o644); err != nil {
			return err
		}
	}
	for i, m := range matched {
		if !m {
			// An upstream that renamed the object would otherwise ship
			// unpatched without anyone noticing.
			return fmt.Errorf("patch %d (%s %s) matched no object", i+1, patches[i].Kind, patches[i].Name)
		}
	}
	chart := fmt.Sprintf("apiVersion: v2\nname: %s\nversion: %s\ntype: application\n", name, strings.TrimPrefix(b.recipe.Spec.Version, "v"))
	for p, body := range map[string]string{
		"Chart.yaml":               chart,
		"values.yaml":              "{}\n",
		"templates/manifests.yaml": wrapTemplate,
	} {
		if err := os.WriteFile(filepath.Join(dst, p), []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

var docSeparator = regexp.MustCompile(`(?m)^---[ \t]*(#.*)?$`)

// editObjects drops the documents matching any exclude selector and
// applies the patches to the ones they match, recording which patches
// matched. Untouched documents are kept byte for byte.
func editObjects(raw []byte, exclude []Selector, patches []Patch, matched []bool) ([]byte, error) {
	var kept [][]byte
	for _, doc := range docSeparator.Split(string(raw), -1) {
		var head struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(doc), &head); err != nil {
			return nil, err
		}
		if strings.TrimSpace(doc) == "" || matches(exclude, head.Kind, head.Metadata.Name) {
			continue
		}
		out := []byte(strings.Trim(doc, "\n"))
		for i, p := range patches {
			if !matches([]Selector{p.Selector}, head.Kind, head.Metadata.Name) {
				continue
			}
			matched[i] = true
			js, err := yaml.YAMLToJSON(out)
			if err != nil {
				return nil, err
			}
			patch, err := json.Marshal(p.Merge)
			if err != nil {
				return nil, err
			}
			if js, err = jsonpatch.MergePatch(js, patch); err != nil {
				return nil, fmt.Errorf("patch %s %s: %w", head.Kind, head.Metadata.Name, err)
			}
			if out, err = yaml.JSONToYAML(js); err != nil {
				return nil, err
			}
			out = []byte(strings.Trim(string(out), "\n"))
		}
		kept = append(kept, out)
	}
	return append(bytes.Join(kept, []byte("\n---\n")), '\n'), nil
}

func matches(sel []Selector, kind, name string) bool {
	for _, e := range sel {
		if (e.Kind == "" || e.Kind == kind) && (e.Name == "" || e.Name == name) {
			return true
		}
	}
	return false
}

// resolve makes a source available on disk and returns its path.
func (b *builder) resolve(name string) (string, error) {
	if p, ok := b.resolved[name]; ok {
		return p, nil
	}
	s := b.recipe.Spec.Sources[name]
	var p string
	switch {
	case s.Dir != "":
		dir, err := within(b.recipeDir, s.Dir)
		if err != nil {
			return "", err
		}
		p = dir
	case s.Plugin != "":
		sp, err := b.opts.Plugins.source(s.Plugin)
		if err != nil {
			return "", err
		}
		dir := filepath.Join(b.opts.WorkDir, "sources", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		if err := sp.Fetch(b.ctx, b.input(s.With, dir, "")); err != nil {
			return "", fmt.Errorf("plugin %s: %w", s.Plugin, err)
		}
		p = dir
	case s.Chart != nil:
		dir, _, err := b.opts.Fetcher.FetchChart(b.ctx, source.Chart{Repository: s.Chart.Repository, Name: s.Chart.Name, Version: s.Chart.Version, Digest: s.Chart.Digest})
		if err != nil {
			return "", err
		}
		p = dir
	default:
		raw, err := source.FetchFile(b.ctx, s.URL, s.SHA256)
		if err != nil {
			return "", err
		}
		dir := filepath.Join(b.opts.WorkDir, "sources", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		u, err := url.Parse(s.URL)
		if err != nil {
			return "", err
		}
		base := path.Base(u.Path)
		if strings.HasSuffix(base, ".tgz") || strings.HasSuffix(base, ".tar.gz") {
			if err := source.Untar(bytes.NewReader(raw), dir); err != nil {
				return "", fmt.Errorf("unpack %s: %w", s.URL, err)
			}
			if p, err = within(dir, s.Path); err != nil {
				return "", err
			}
		} else {
			if s.Path != "" {
				return "", fmt.Errorf("path applies to archives only")
			}
			p = filepath.Join(dir, base)
			if err := os.WriteFile(p, raw, 0o644); err != nil {
				return "", err
			}
		}
	}
	b.resolved[name] = p
	return p, nil
}

func isChart(p string) bool {
	_, err := os.Stat(filepath.Join(p, "Chart.yaml"))
	return err == nil
}

// manifests lists p if it is a file, or the YAML files under p by path.
func manifests(p string) ([]string, error) {
	st, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return []string{p}, nil
	}
	var out []string
	err = filepath.WalkDir(p, func(f string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if ext := filepath.Ext(f); ext == ".yaml" || ext == ".yml" {
			out = append(out, f)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

func mergeValuesFile(file string, over map[string]any) error {
	base := map[string]any{}
	if raw, err := os.ReadFile(file); err == nil {
		if err := yaml.Unmarshal(raw, &base); err != nil {
			return fmt.Errorf("values.yaml: %w", err)
		}
		if base == nil {
			base = map[string]any{}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	raw, err := yaml.Marshal(source.MergeValues(base, over))
	if err != nil {
		return err
	}
	return os.WriteFile(file, raw, 0o644)
}

// within joins rel to root and refuses paths that leave root.
func within(root, rel string) (string, error) {
	p := filepath.Join(root, filepath.Clean("/"+rel))
	r, err := filepath.Rel(root, p)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q leaves %s", rel, root)
	}
	return p, nil
}

func metav1TypeMeta() metav1.TypeMeta {
	return metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "PackageSource"}
}
