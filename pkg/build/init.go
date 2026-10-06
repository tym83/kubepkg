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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"sigs.k8s.io/yaml"

	"github.com/tym83/kubepkg/pkg/images"
	"github.com/tym83/kubepkg/pkg/source"
)

// InitInput describes the recipe Init writes. Exactly one of Chart and
// Manifests is set.
type InitInput struct {
	Name      string
	Version   string // default: the chart's appVersion or version
	Namespace string // default: Name
	// Chart is an upstream chart: repository, name and version.
	Chart *source.Chart
	// Manifests are URLs of upstream release manifests.
	Manifests   []string
	Description string // default: the chart's description
	Home        string // default: the chart's home
	// Images pins the images the upstream runs; nil leaves package.images
	// for kubepkg images to fill.
	Images *images.Resolver
}

type initSource struct {
	Name, URL, SHA256 string
	Chart             *source.Chart
}

type initData struct {
	InitInput
	Sources []initSource
	Exclude bool
	CRDs    []string
	Images  []string
}

var recipeTemplate = template.Must(template.New("recipe").Parse(`apiVersion: kubepkg.dev/v1beta1
kind: Recipe
metadata:
  name: {{.Name}}
  annotations:
    kubepkg.dev/description: {{printf "%q" .Description}}
{{- if .Home}}
    kubepkg.dev/home: {{.Home}}
{{- end}}
spec:
  version: {{.Version}}
  build: 1
  sources:
{{- range .Sources}}
    {{.Name}}:
{{- if .Chart}}
      chart:
        repository: {{.Chart.Repository}}
        name: {{.Chart.Name}}
        version: {{printf "%q" .Chart.Version}}
        digest: {{.Chart.Digest}}
{{- else}}
      url: {{.URL}}
      sha256: {{.SHA256}}
{{- end}}
{{- end}}
  charts:
    {{.Name}}:
      from: [{{range $i, $s := .Sources}}{{if $i}}, {{end}}{{$s.Name}}{{end}}]
{{- if .Exclude}}
      exclude: [{kind: Namespace}]   # kubepkg creates the install namespace
{{- end}}
      # values: {}                   # the package's defaults over the upstream ones
  package:
    provides: [{{.Name}}]
    crds:{{if not .CRDs}} []{{end}}
{{- range .CRDs}}
      - {{.}}
{{- end}}
{{- if .Images}}
    images:   # what the charts run, pinned; add images an operator deploys on its own
{{- range .Images}}
      - {{.}}
{{- end}}
{{- end}}
    rollback:
      safe: false   # true once rolling back to the previous version is known not to lose data
    variants:
      - name: default
        components:
          - name: {{.Name}}
            path: {{.Name}}
            install: {namespace: {{.Namespace}}}
`))

// Init writes dir/recipe.yaml for an upstream chart or release manifests,
// with every source pinned: it downloads them to compute digests, takes
// the description from the chart, drops Namespaces from manifests, and
// lists the CRDs they ship. Review and adjust the result, then run
// Validate.
func Init(ctx context.Context, dir string, in InitInput, fetcher *source.Fetcher) error {
	if (in.Chart == nil) == (len(in.Manifests) == 0) {
		return errors.New("give either a chart or manifests")
	}
	if in.Name == "" {
		return errors.New("a recipe needs a name")
	}
	file := filepath.Join(dir, RecipeFile)
	if _, err := os.Stat(file); err == nil {
		return fmt.Errorf("%s already exists", file)
	}
	if in.Namespace == "" {
		in.Namespace = in.Name
	}
	d := initData{InitInput: in}
	if in.Chart != nil {
		chartDir, digest, err := fetcher.FetchChart(ctx, *in.Chart)
		if err != nil {
			return err
		}
		meta, err := chartMeta(chartDir)
		if err != nil {
			return err
		}
		if d.Version == "" {
			d.Version = strings.TrimPrefix(firstNonEmpty(meta.AppVersion, meta.Version), "v")
		}
		d.Description = firstNonEmpty(d.Description, meta.Description)
		d.Home = firstNonEmpty(d.Home, meta.Home)
		ch := *in.Chart
		ch.Digest = digest
		d.Sources = []initSource{{Name: "upstream", Chart: &ch}}
		docs, err := renderChart(chartDir)
		if err != nil {
			return fmt.Errorf("render the chart with its defaults to find its CRDs and images: %w", err)
		}
		if d.CRDs, err = crdNames(docs); err != nil {
			return err
		}
		if d.Images, err = images.FromManifests(docs...); err != nil {
			return err
		}
	} else {
		if d.Version == "" {
			return errors.New("manifests carry no version: give one")
		}
		crds := map[string]bool{}
		names := map[string]bool{}
		for i, u := range in.Manifests {
			raw, err := source.Download(ctx, u, 256<<20)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(raw)
			name := sourceName(u, i)
			if names[name] {
				name = fmt.Sprintf("%s-%d", name, i+1)
			}
			names[name] = true
			d.Sources = append(d.Sources, initSource{Name: name, URL: u, SHA256: hex.EncodeToString(sum[:])})
			imgs, err := images.FromManifests(string(raw))
			if err != nil {
				return err
			}
			d.Images = append(d.Images, imgs...)
			kinds, names := scanKinds(raw)
			d.Exclude = d.Exclude || kinds["Namespace"]
			for _, n := range names {
				crds[n] = true
			}
		}
		for n := range crds {
			d.CRDs = append(d.CRDs, n)
		}
		sort.Strings(d.CRDs)
	}
	d.Images = dedupe(d.Images)
	if in.Images == nil {
		d.Images = nil
	}
	for i, img := range d.Images {
		pinned, err := in.Images.Pin(ctx, img)
		if err != nil {
			return fmt.Errorf("pin the images the upstream runs: %w", err)
		}
		d.Images[i] = pinned
	}
	if d.Description == "" {
		d.Description = "TODO: one line about " + in.Name
	}
	var buf bytes.Buffer
	if err := recipeTemplate.Execute(&buf, d); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(file, buf.Bytes(), 0o644)
}

type chartMetadata struct {
	Version     string `json:"version"`
	AppVersion  string `json:"appVersion"`
	Description string `json:"description"`
	Home        string `json:"home"`
}

func chartMeta(dir string) (chartMetadata, error) {
	var m chartMetadata
	raw, err := os.ReadFile(filepath.Join(dir, "Chart.yaml"))
	if err != nil {
		return m, err
	}
	return m, yaml.Unmarshal(raw, &m)
}

// scanKinds reports the kinds in a manifest bundle and the names of its
// CRDs.
func scanKinds(raw []byte) (map[string]bool, []string) {
	kinds := map[string]bool{}
	var crds []string
	for _, doc := range docSeparator.Split(string(raw), -1) {
		var head struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if yaml.Unmarshal([]byte(doc), &head) != nil || head.Kind == "" {
			continue
		}
		kinds[head.Kind] = true
		if head.Kind == "CustomResourceDefinition" {
			crds = append(crds, head.Metadata.Name)
		}
	}
	return kinds, crds
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// sourceName names a manifest source after its file.
func sourceName(u string, i int) string {
	if parsed, err := url.Parse(u); err == nil {
		base := strings.TrimSuffix(strings.TrimSuffix(path.Base(parsed.Path), ".yaml"), ".yml")
		if base != "" && base != "." && base != "/" {
			return base
		}
	}
	return fmt.Sprintf("manifests-%d", i+1)
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
