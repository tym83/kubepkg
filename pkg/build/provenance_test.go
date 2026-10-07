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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"helm.sh/helm/v4/pkg/provenance"

	"github.com/tym83/kubepkg/api/v1"
	"github.com/tym83/kubepkg/pkg/source"
)

// gpgKey writes a private and a public keyring for one new key.
func gpgKey(t *testing.T, dir, name string) (secret, public string) {
	t.Helper()
	e, err := openpgp.NewEntity(name, "", name+"@example.org", nil)
	if err != nil {
		t.Fatal(err)
	}
	var sec, pub bytes.Buffer
	if err := e.SerializePrivate(&sec, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.Serialize(&pub); err != nil {
		t.Fatal(err)
	}
	secret, public = filepath.Join(dir, name+".sec"), filepath.Join(dir, name+".gpg")
	if err := os.WriteFile(secret, sec.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(public, pub.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return secret, public
}

func TestVerifyChartsAgainstUpstreamProvenance(t *testing.T) {
	dir := t.TempDir()
	signer, signerPub := gpgKey(t, dir, "upstream")
	_, otherPub := gpgKey(t, dir, "other")
	meta := "apiVersion: v2\nname: app\nversion: 1.0.0\n"
	archive := tgz(t, map[string]string{"app/Chart.yaml": meta})
	tampered := tgz(t, map[string]string{"app/Chart.yaml": meta, "app/values.yaml": "evil: true\n"})
	sig, err := provenance.NewFromFiles(signer, signer)
	if err != nil {
		t.Fatal(err)
	}
	prov, err := sig.ClearSign(archive, "app-1.0.0.tgz", []byte(meta))
	if err != nil {
		t.Fatal(err)
	}
	serve := archive
	mux := http.NewServeMux()
	mux.HandleFunc("/index.yaml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("apiVersion: v1\nentries:\n  app:\n  - version: 1.0.0\n    urls: [app-1.0.0.tgz]\n"))
	})
	mux.HandleFunc("/app-1.0.0.tgz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(serve) })
	mux.HandleFunc("/app-1.0.0.tgz.prov", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(prov)) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	recipe := func(keyring, digest string) *Recipe {
		r := &Recipe{Spec: RecipeSpec{Sources: map[string]Source{}}}
		r.Spec.Sources["chart"] = Source{Chart: &v1.ChartRef{Repository: srv.URL, Name: "app", Version: "1.0.0", Digest: digest}}
		r.Spec.Verify = &Verify{Charts: []ChartSignature{{Sources: []string{"chart"}, Keyring: keyring}}}
		return r
	}
	f := &source.Fetcher{CacheDir: t.TempDir()}
	ctx := context.Background()
	if err := VerifyCharts(ctx, recipe(filepath.Base(signerPub), "sha256:"+sum(archive)), dir, f); err != nil {
		t.Fatalf("a chart signed by the keyring: %v", err)
	}
	if err := VerifyCharts(ctx, recipe(filepath.Base(otherPub), "sha256:"+sum(archive)), dir, f); err == nil {
		t.Fatal("a chart passed with a keyring that did not sign it")
	}
	serve = tampered
	if err := VerifyCharts(ctx, recipe(filepath.Base(signerPub), "sha256:"+sum(tampered)), dir, f); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("an archive the signature does not cover: %v", err)
	}
}
