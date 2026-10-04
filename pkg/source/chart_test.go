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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chartArchive builds a minimal chart archive the way helm package does.
func chartArchive(t *testing.T, name, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	files := map[string]string{
		name + "/Chart.yaml":            fmt.Sprintf("apiVersion: v2\nname: %s\nversion: %s\n", name, version),
		name + "/templates/config.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}\n",
	}
	for _, n := range []string{name + "/Chart.yaml", name + "/templates/config.yaml"} {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(files[n])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(files[n])); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// helmRepo serves index.yaml and archives; indexDigest overrides the digest
// the index advertises.
func helmRepo(t *testing.T, archive []byte, indexDigest string) (*httptest.Server, *int) {
	t.Helper()
	hits := new(int)
	mux := http.NewServeMux()
	mux.HandleFunc("/charts/index.yaml", func(w http.ResponseWriter, _ *http.Request) {
		*hits++
		fmt.Fprintf(w, "apiVersion: v1\nentries:\n  hello:\n  - version: 1.0.0\n    digest: %s\n    urls: [hello-1.0.0.tgz]\n", strings.TrimPrefix(indexDigest, "sha256:"))
	})
	mux.HandleFunc("/charts/hello-1.0.0.tgz", func(w http.ResponseWriter, _ *http.Request) {
		*hits++
		_, _ = w.Write(archive)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, hits
}

func TestFetchChartFromHTTPRepository(t *testing.T) {
	archive := chartArchive(t, "hello", "1.0.0")
	srv, hits := helmRepo(t, archive, sha(archive))
	f := &Fetcher{CacheDir: t.TempDir()}
	c := Chart{Repository: srv.URL + "/charts", Name: "hello", Version: "1.0.0", Digest: sha(archive)}

	dir, digest, err := f.FetchChart(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if digest != sha(archive) {
		t.Fatalf("digest %s, want %s", digest, sha(archive))
	}
	if _, err := os.Stat(filepath.Join(dir, "templates", "config.yaml")); err != nil {
		t.Fatalf("chart not unpacked: %v", err)
	}

	before := *hits
	if again, _, err := f.FetchChart(context.Background(), c); err != nil || again != dir {
		t.Fatalf("cached fetch: %s %v", again, err)
	}
	if *hits != before {
		t.Fatal("a pinned chart already in the cache must not touch the network")
	}
}

func TestFetchChartRefusesWrongDigest(t *testing.T) {
	archive := chartArchive(t, "hello", "1.0.0")
	srv, _ := helmRepo(t, archive, sha(archive))
	f := &Fetcher{CacheDir: t.TempDir()}
	other := sha([]byte("something else"))
	_, _, err := f.FetchChart(context.Background(), Chart{Repository: srv.URL + "/charts", Name: "hello", Version: "1.0.0", Digest: other})
	if err == nil || !strings.Contains(err.Error(), "pins") {
		t.Fatalf("want a pinned digest mismatch, got %v", err)
	}
}

func TestFetchChartRefusesArchiveNotMatchingIndex(t *testing.T) {
	archive := chartArchive(t, "hello", "1.0.0")
	srv, _ := helmRepo(t, archive, sha([]byte("tampered")))
	f := &Fetcher{CacheDir: t.TempDir()}
	_, _, err := f.FetchChart(context.Background(), Chart{Repository: srv.URL + "/charts", Name: "hello", Version: "1.0.0"})
	if err == nil || !strings.Contains(err.Error(), "repository index") {
		t.Fatalf("want an index digest mismatch, got %v", err)
	}
}

func TestFetchChartNeedsExactVersion(t *testing.T) {
	f := &Fetcher{CacheDir: t.TempDir()}
	for _, v := range []string{"~1.0", "1.x", ">=1.0.0", ""} {
		if _, _, err := f.FetchChart(context.Background(), Chart{Repository: "https://example.invalid", Name: "hello", Version: v}); err == nil || !strings.Contains(err.Error(), "exact") {
			t.Errorf("version %q: want an exact-version error, got %v", v, err)
		}
	}
}

func TestFetchChartUnknownVersion(t *testing.T) {
	archive := chartArchive(t, "hello", "1.0.0")
	srv, _ := helmRepo(t, archive, sha(archive))
	f := &Fetcher{CacheDir: t.TempDir()}
	_, _, err := f.FetchChart(context.Background(), Chart{Repository: srv.URL + "/charts", Name: "hello", Version: "2.0.0"})
	if err == nil || !strings.Contains(err.Error(), "no version 2.0.0") {
		t.Fatalf("got %v", err)
	}
}
