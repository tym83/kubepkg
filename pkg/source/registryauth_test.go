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

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// privateRegistry serves one chart and answers only to bot:token.
func privateRegistry(t *testing.T, archive []byte) string {
	t.Helper()
	sum := sha256.Sum256(archive)
	layer := "sha256:" + hex.EncodeToString(sum[:])
	manifest, _ := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{"mediaType": "application/vnd.cncf.helm.config.v1+json", "digest": "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a", "size": 2},
		"layers": []map[string]any{{"mediaType": ChartMediaType, "digest": layer, "size": len(archive)}},
	})
	msum := sha256.Sum256(manifest)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "bot" || p != "token" {
			w.Header().Set("WWW-Authenticate", `Basic realm="private"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/v2/":
		case strings.HasPrefix(r.URL.Path, "/v2/charts/demo/manifests/"):
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", "sha256:"+hex.EncodeToString(msum[:]))
			w.Header().Set("Content-Length", strconv.Itoa(len(manifest)))
			if r.Method != http.MethodHead {
				_, _ = w.Write(manifest)
			}
		case r.URL.Path == "/v2/charts/demo/blobs/"+layer:
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return u.Host
}

func TestPrivateRegistryWithClusterCredentials(t *testing.T) {
	archive := chartArchive(t, "demo", "0.1.0")
	host := privateRegistry(t, archive)
	chart := Chart{Repository: "oci://" + host + "/charts", Name: "demo", Version: "0.1.0"}
	secret := []byte(`{"auths": {"` + host + `": {"auth": "` + base64.StdEncoding.EncodeToString([]byte("bot:token")) + `"}}}`)

	anonymous := &Fetcher{CacheDir: t.TempDir(), PlainHTTP: true}
	if _, _, err := anonymous.ChartArchive(context.Background(), chart); err == nil {
		t.Fatal("pulled from a private registry without credentials")
	}
	withSecret := &Fetcher{CacheDir: t.TempDir(), PlainHTTP: true,
		Credentials: DockerConfigCredentials(func(context.Context) ([][]byte, error) { return [][]byte{secret}, nil })}
	got, _, err := withSecret.ChartArchive(context.Background(), chart)
	if err != nil {
		t.Fatalf("with the Secret's credentials: %v", err)
	}
	if string(got) != string(archive) {
		t.Fatal("wrong archive")
	}
}
