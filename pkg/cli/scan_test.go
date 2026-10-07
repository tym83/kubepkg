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

package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tym83/kubepkg/api/v1beta1"
	"github.com/tym83/kubepkg/pkg/sbom"
)

// fakeTrivy writes a trivy that logs what it scans and reports two
// critical findings (one twice, as trivy does across layers) for images
// named bad, and nothing otherwise.
func fakeTrivy(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "scanned")
	script := "#!/bin/sh\nfor a; do last=\"$a\"; done\necho \"$last\" >> " + log + "\n" +
		"case \"$last\" in *bad*) echo '{\"Results\":[{\"Vulnerabilities\":[{\"VulnerabilityID\":\"CVE-1\",\"Severity\":\"CRITICAL\"},{\"VulnerabilityID\":\"CVE-2\",\"Severity\":\"HIGH\"}]},{\"Vulnerabilities\":[{\"VulnerabilityID\":\"CVE-1\",\"Severity\":\"CRITICAL\"}]}]}' ;;\n*) echo '{\"Results\":[]}' ;; esac\n"
	p := filepath.Join(dir, "trivy")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p, log
}

func TestScanCountsOncePerImageAndFails(t *testing.T) {
	trivy, log := fakeTrivy(t)
	d := "@sha256:" + strings.Repeat("c", 64)
	comps := []v1beta1.Variant{{Name: "default", Components: []v1beta1.Component{{Name: "x"}}}}
	pkgs := []sbom.Package{
		{Name: "a", Spec: v1beta1.PackageSourceSpec{Variants: comps, Images: []string{"quay.io/org/bad:1" + d, "quay.io/org/good:1" + d}}},
		{Name: "b", Spec: v1beta1.PackageSourceSpec{Variants: comps, Images: []string{"quay.io/org/bad:1" + d}}},
		{Name: "c", Spec: v1beta1.PackageSourceSpec{Variants: comps}},
		{Name: "meta"},
	}
	results, unpinned, err := Scan(context.Background(), pkgs, TrivyScanner(trivy, nil), "oci://registry.internal/mirror")
	if err != nil {
		t.Fatal(err)
	}
	scanned, _ := os.ReadFile(log)
	if lines := strings.Fields(string(scanned)); len(lines) != 2 || !strings.HasPrefix(lines[0], "registry.internal/mirror/quay.io/org/bad:1@sha256:") {
		t.Fatalf("scanned: %q", scanned)
	}
	if len(results) != 3 || results[0].Counts["CRITICAL"] != 1 || results[0].Counts["HIGH"] != 1 || results[2].Counts["CRITICAL"] != 1 {
		t.Fatalf("results: %+v", results)
	}
	if strings.Join(unpinned, ",") != "c" {
		t.Fatalf("unpinned: %v", unpinned)
	}
	var out bytes.Buffer
	printScan(&out, results, unpinned)
	if !strings.Contains(out.String(), "quay.io/org/bad:1") || !strings.Contains(out.String(), "not scanned, their images are not pinned: c") {
		t.Fatalf("report:\n%s", out.String())
	}
	if err := failAt(results, "critical"); err == nil {
		t.Fatal("a critical finding did not fail --fail-on critical")
	}
	if err := failAt(results, ""); err != nil {
		t.Fatal(err)
	}
	if err := failAt([]ScanResult{{Counts: map[string]int{"MEDIUM": 3}}}, "high"); err != nil {
		t.Fatalf("medium findings failed --fail-on high: %v", err)
	}
}
