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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCosign accepts what is signed per its table and logs its arguments.
func fakeCosign(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\nfor a; do last=\"$a\"; done\ncase \"$last\" in *unsigned*) echo 'Error: no signatures found' >&2; exit 1;; esac\necho '[]'\n"
	p := filepath.Join(dir, "cosign")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p, log
}

func TestVerifyImagesWithCosign(t *testing.T) {
	cosign, log := fakeCosign(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "keys/jetstack.pem"), "-----BEGIN PUBLIC KEY-----\n")
	d := "@sha256:" + strings.Repeat("e", 64)
	r := &Recipe{Spec: RecipeSpec{Verify: &Verify{Images: []ImageSignature{
		{Repositories: []string{"quay.io/jetstack/*"}, Key: "keys/jetstack.pem", SignatureDigest: "sha512", IgnoreTransparencyLog: true},
		{Repositories: []string{"registry.k8s.io/*/*"}, Identity: "krel-trust@k8s-releng-prod.iam.gserviceaccount.com", Issuer: "https://accounts.google.com"},
	}}}}
	r.Spec.Package.Images = []string{
		"quay.io/jetstack/cert-manager-controller:v1.21.2" + d,
		"registry.k8s.io/metrics-server/metrics-server:v0.8.0" + d,
		"quay.io/kubevirt/virt-api:v1.9.0" + d,
	}
	unchecked, err := VerifyImages(context.Background(), r, dir, CosignVerifier(cosign))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(unchecked, ",") != "quay.io/kubevirt/virt-api:v1.9.0"+d {
		t.Fatalf("unchecked: %v", unchecked)
	}
	calls, _ := os.ReadFile(log)
	got := string(calls)
	if !strings.Contains(got, "--signature-digest-algorithm sha512 --insecure-ignore-tlog --key "+filepath.Join(dir, "keys/jetstack.pem")+" quay.io/jetstack/cert-manager-controller:v1.21.2"+d) ||
		!strings.Contains(got, "--certificate-identity krel-trust@k8s-releng-prod.iam.gserviceaccount.com --certificate-oidc-issuer https://accounts.google.com registry.k8s.io/metrics-server/metrics-server") {
		t.Fatalf("cosign calls:\n%s", got)
	}

	r.Spec.Package.Images = append(r.Spec.Package.Images, "quay.io/jetstack/unsigned:1"+d)
	if _, err := VerifyImages(context.Background(), r, dir, CosignVerifier(cosign)); err == nil || !strings.Contains(err.Error(), "no signatures found") {
		t.Fatalf("an unsigned image passed: %v", err)
	}
	if _, err := VerifyImages(context.Background(), r, dir, nil); err == nil || !strings.Contains(err.Error(), "install cosign") {
		t.Fatalf("without cosign: %v", err)
	}
	keylessNoLog := r.Spec.Verify.Images[1]
	keylessNoLog.IgnoreTransparencyLog = true
	if checkRule(keylessNoLog) == nil {
		t.Fatal("a keyless rule without the transparency log was accepted")
	}
	r.Spec.Verify.Images[0].Identity = "someone"
	if _, err := VerifyImages(context.Background(), r, dir, CosignVerifier(cosign)); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("a rule with a key and an identity: %v", err)
	}
	r.Spec.Verify.Images[0] = ImageSignature{Repositories: []string{"quay.io/jetstack/*"}, Key: "../outside.pem"}
	r.Spec.Package.Images = r.Spec.Package.Images[:1]
	if _, err := VerifyImages(context.Background(), r, dir, CosignVerifier(cosign)); err != nil {
		t.Fatal(err)
	}
	calls, _ = os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if last := lines[len(lines)-1]; !strings.Contains(last, "--key "+filepath.Join(dir, "outside.pem")+" ") {
		t.Fatalf("a key path left the recipe directory: %s", last)
	}
}
