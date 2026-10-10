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

package sbom

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kuberoot-dev/kubepkg/api/v1"
)

func pkgs() []Package {
	d := "sha256:" + strings.Repeat("a", 64)
	return []Package{
		{Name: "virtualization", Version: "1.0.0", Build: 1, Repository: "main", Digest: "sha256:" + strings.Repeat("1", 64),
			Spec: v1.PackageSourceSpec{Variants: []v1.Variant{{Name: "default", Requires: []v1.Requirement{{Package: "kubevirt"}, {Package: "cdi"}}}}}},
		{Name: "kubevirt", Version: "1.9.0", Build: 6, Repository: "main", Digest: "sha256:" + strings.Repeat("2", 64),
			Spec: v1.PackageSourceSpec{
				Images: []string{"quay.io/kubevirt/virt-api:v1.9.0@" + d},
				Variants: []v1.Variant{{Name: "default", Components: []v1.Component{
					{Name: "operator", Chart: &v1.ChartRef{Repository: "oci://ghcr.io/org/packages/kubevirt", Name: "kubevirt-operator", Version: "1.9.0-6", Digest: "sha256:" + strings.Repeat("b", 64)}},
				}}}}},
	}
}

func TestCycloneDX(t *testing.T) {
	at := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	a, err := CycloneDX(pkgs(), Options{ToolVersion: "v0.3.2", Timestamp: at})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := CycloneDX(pkgs(), Options{ToolVersion: "v0.3.2", Timestamp: at})
	if string(a) != string(b) {
		t.Fatal("the same packages gave different documents")
	}
	var doc struct {
		BOMFormat, SpecVersion, SerialNumber string
		Components                           []struct {
			Type, Name, Version, PURL string
			Hashes                    []struct{ Alg, Content string }
		}
		Dependencies []struct {
			Ref       string
			DependsOn []string
		}
	}
	if err := json.Unmarshal(a, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.BOMFormat != "CycloneDX" || doc.SpecVersion != "1.6" || !strings.HasPrefix(doc.SerialNumber, "urn:uuid:") {
		t.Fatalf("header: %+v", doc)
	}
	var image, chart bool
	for _, c := range doc.Components {
		switch c.Type {
		case "container":
			image = c.Name == "quay.io/kubevirt/virt-api" && c.Version == "v1.9.0" &&
				c.PURL == "pkg:oci/virt-api@sha256%3A"+strings.Repeat("a", 64)+"?repository_url=quay.io%2Fkubevirt%2Fvirt-api&tag=v1.9.0" &&
				len(c.Hashes) == 1 && c.Hashes[0].Content == strings.Repeat("a", 64)
		case "application":
			if c.Name == "kubevirt-operator" {
				chart = len(c.Hashes) == 1 && c.Hashes[0].Content == strings.Repeat("b", 64)
			}
		}
	}
	if !image || !chart {
		t.Fatalf("image %v chart %v in %s", image, chart, a)
	}
	deps := map[string][]string{}
	for _, d := range doc.Dependencies {
		deps[d.Ref] = d.DependsOn
	}
	if strings.Join(deps["package:virtualization"], ",") != "package:cdi,package:kubevirt" && strings.Join(deps["package:virtualization"], ",") != "package:kubevirt" {
		t.Fatalf("virtualization depends on %v", deps["package:virtualization"])
	}
	if len(deps["package:kubevirt"]) != 2 {
		t.Fatalf("kubevirt depends on %v", deps["package:kubevirt"])
	}
}
