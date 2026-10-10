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

// Package sbom describes packages as CycloneDX software bills of
// materials: what each package is, which charts and container images it
// installs, pinned by digest, and which packages it requires.
package sbom

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuberoot-dev/kubepkg/api/v1"
	"github.com/kuberoot-dev/kubepkg/pkg/images"
)

// Package is one package version to describe.
type Package struct {
	Name, Version string
	Build         int32
	// Repository names where the version came from; Digest is its spec
	// digest in that repository's signed index.
	Repository, Digest string
	Spec               v1.PackageSourceSpec
	// Variant picks the requirements to record; default when empty.
	Variant string
}

// Options shape the document.
type Options struct {
	// ToolVersion is the kubepkg version that made the document.
	ToolVersion string
	// Timestamp is when the described state was made, e.g. the index's
	// generation time, so the same packages give the same document.
	Timestamp time.Time
	// Name describes the whole set, e.g. a cluster or a bundle.
	Name string
}

type bom struct {
	BOMFormat    string       `json:"bomFormat"`
	SpecVersion  string       `json:"specVersion"`
	SerialNumber string       `json:"serialNumber"`
	Version      int          `json:"version"`
	Metadata     metadata     `json:"metadata"`
	Components   []component  `json:"components"`
	Dependencies []dependency `json:"dependencies"`
}

type metadata struct {
	Timestamp string    `json:"timestamp"`
	Tools     tools     `json:"tools"`
	Component component `json:"component"`
}

type tools struct {
	Components []component `json:"components"`
}

type component struct {
	Type         string     `json:"type"`
	BOMRef       string     `json:"bom-ref,omitempty"`
	Name         string     `json:"name"`
	Version      string     `json:"version,omitempty"`
	Description  string     `json:"description,omitempty"`
	PURL         string     `json:"purl,omitempty"`
	Hashes       []hash     `json:"hashes,omitempty"`
	ExternalRefs []extRef   `json:"externalReferences,omitempty"`
	Properties   []property `json:"properties,omitempty"`
}

type hash struct {
	Alg     string `json:"alg"`
	Content string `json:"content"`
}

type extRef struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

type property struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type dependency struct {
	Ref       string   `json:"ref"`
	DependsOn []string `json:"dependsOn"`
}

// CycloneDX writes a CycloneDX 1.6 JSON document for the packages. The
// same packages and options always give the same bytes.
func CycloneDX(pkgs []Package, o Options) ([]byte, error) {
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].Name < pkgs[j].Name })
	name := o.Name
	if name == "" {
		var names []string
		for _, p := range pkgs {
			names = append(names, p.Name)
		}
		name = strings.Join(names, "+")
	}
	b := bom{BOMFormat: "CycloneDX", SpecVersion: "1.6", Version: 1}
	ids := sha256.New()
	byRef := map[string]component{}
	deps := map[string][]string{}
	inSet := map[string]bool{}
	for _, p := range pkgs {
		inSet[p.Name] = true
	}
	var top []string
	for _, p := range pkgs {
		ids.Write([]byte(p.Name + "@" + p.Digest + "\n"))
		ref := "package:" + p.Name
		top = append(top, ref)
		pc := component{Type: "application", BOMRef: ref, Name: p.Name, Version: p.Version,
			Properties: []property{{"kubepkg:kind", "package"}, {"kubepkg:build", itoa(p.Build)}}}
		if p.Repository != "" {
			pc.Properties = append(pc.Properties, property{"kubepkg:repository", p.Repository})
		}
		if p.Digest != "" {
			pc.Hashes = []hash{{"SHA-256", strings.TrimPrefix(p.Digest, "sha256:")}}
		}
		byRef[ref] = pc
		var dependsOn []string
		for _, v := range p.Spec.Variants {
			for _, c := range v.Components {
				if c.Chart == nil {
					continue
				}
				cref := "chart:" + c.Chart.Repository + "/" + c.Chart.Name + "@" + c.Chart.Version
				cc := component{Type: "application", BOMRef: cref, Name: c.Chart.Name, Version: c.Chart.Version,
					Properties: []property{{"kubepkg:kind", "helm-chart"}, {"kubepkg:chart-repository", c.Chart.Repository}}}
				if c.Chart.Digest != "" {
					cc.Hashes = []hash{{"SHA-256", strings.TrimPrefix(c.Chart.Digest, "sha256:")}}
				}
				byRef[cref] = cc
				dependsOn = append(dependsOn, cref)
			}
		}
		for _, img := range p.Spec.Images {
			iref := "image:" + img
			byRef[iref] = imageComponent(iref, img)
			dependsOn = append(dependsOn, iref)
		}
		variant := p.Variant
		if variant == "" {
			variant = "default"
		}
		for _, v := range p.Spec.Variants {
			if v.Name != variant {
				continue
			}
			for _, r := range v.Requires {
				if r.Package != "" && inSet[r.Package] {
					dependsOn = append(dependsOn, "package:"+r.Package)
				}
			}
			for _, d := range v.DependsOn {
				if inSet[d] {
					dependsOn = append(dependsOn, "package:"+d)
				}
			}
		}
		deps[ref] = dedupe(dependsOn)
	}
	b.SerialNumber = "urn:uuid:" + uuid.NewSHA1(uuid.NameSpaceOID, ids.Sum(nil)).String()
	b.Metadata = metadata{
		Timestamp: o.Timestamp.UTC().Truncate(time.Second).Format(time.RFC3339),
		Tools:     tools{Components: []component{{Type: "application", Name: "kubepkg", Version: o.ToolVersion}}},
		Component: component{Type: "application", BOMRef: "set:" + name, Name: name},
	}
	refs := make([]string, 0, len(byRef))
	for r := range byRef {
		refs = append(refs, r)
	}
	sort.Strings(refs)
	for _, r := range refs {
		b.Components = append(b.Components, byRef[r])
	}
	b.Dependencies = append(b.Dependencies, dependency{Ref: "set:" + name, DependsOn: top})
	for _, r := range refs {
		if d, ok := deps[r]; ok {
			b.Dependencies = append(b.Dependencies, dependency{Ref: r, DependsOn: d})
		} else {
			b.Dependencies = append(b.Dependencies, dependency{Ref: r, DependsOn: []string{}})
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // package URLs keep their & readable
	enc.SetIndent("", "  ")
	if err := enc.Encode(b); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// imageComponent describes a pinned image with the OCI package URL.
func imageComponent(ref, img string) component {
	repo, tag, digest := images.Split(img)
	c := component{Type: "container", BOMRef: ref, Name: repo, Version: tag,
		Properties: []property{{"kubepkg:kind", "container-image"}}}
	if digest != "" {
		c.Hashes = []hash{{"SHA-256", strings.TrimPrefix(digest, "sha256:")}}
		q := url.Values{"repository_url": {repo}}
		if tag != "" {
			q.Set("tag", tag)
		}
		c.PURL = "pkg:oci/" + repo[strings.LastIndex(repo, "/")+1:] + "@" + strings.ReplaceAll(digest, ":", "%3A") + "?" + q.Encode()
	}
	return c
}

func itoa(n int32) string { return strconv.Itoa(int(n)) }

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
