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

package source

import "testing"

func TestMirrorLocations(t *testing.T) {
	m := "oci://registry.internal/mirror/"
	for in, want := range map[string]string{
		"https://charts.jetstack.io":            "oci://registry.internal/mirror/charts.jetstack.io",
		"https://example.org:8443/Helm/stable/": "oci://registry.internal/mirror/example.org-8443/helm/stable",
		"oci://ghcr.io/org/packages/cdi":        "oci://registry.internal/mirror/ghcr.io/org/packages/cdi",
	} {
		if got := MirrorChart(m, Chart{Repository: in, Name: "x"}).Repository; got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"oci://ghcr.io/org/tree:v1":              "oci://registry.internal/mirror/ghcr.io/org/tree:v1",
		"oci://ghcr.io/org/tree@sha256:ab":       "oci://registry.internal/mirror/ghcr.io/org/tree@sha256:ab",
		"oci://localhost:5000/tree:v2@sha256:ab": "oci://registry.internal/mirror/localhost-5000/tree:v2@sha256:ab",
	} {
		if got, err := MirrorRef(m, in); err != nil || got != want {
			t.Errorf("%s -> %s (%v), want %s", in, got, err, want)
		}
	}
	if got := MirrorChart("", Chart{Repository: "https://a"}).Repository; got != "https://a" {
		t.Errorf("no mirror changed %s", got)
	}
}
