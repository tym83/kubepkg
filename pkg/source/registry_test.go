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
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// memRegistry is just enough of the OCI distribution API for oras to push
// and resolve.
type memRegistry struct {
	mu        sync.Mutex
	blobs     map[string][]byte
	manifests map[string][]byte // by tag and by digest
	uploads   int
}

func newMemRegistry(t *testing.T) string {
	t.Helper()
	r := &memRegistry{blobs: map[string][]byte{}, manifests: map[string][]byte{}}
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

func (r *memRegistry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	path := req.URL.Path
	switch {
	case path == "/v2/" || path == "/v2":
		w.WriteHeader(http.StatusOK)
	case strings.Contains(path, "/manifests/"):
		ref := path[strings.LastIndex(path, "/")+1:]
		if req.Method == http.MethodPut {
			body, _ := io.ReadAll(req.Body)
			d := digestOf(body)
			r.manifests[ref], r.manifests[d] = body, body
			w.Header().Set("Docker-Content-Digest", d)
			w.WriteHeader(http.StatusCreated)
			return
		}
		body, ok := r.manifests[ref]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", digestOf(body))
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		if req.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
	case strings.HasSuffix(path, "/blobs/uploads/") && req.Method == http.MethodPost:
		r.uploads++
		w.Header().Set("Location", fmt.Sprintf("%s%d", path, r.uploads))
		w.WriteHeader(http.StatusAccepted)
	case strings.Contains(path, "/blobs/uploads/") && req.Method == http.MethodPut:
		body, _ := io.ReadAll(req.Body)
		r.blobs[req.URL.Query().Get("digest")] = body
		w.WriteHeader(http.StatusCreated)
	case strings.Contains(path, "/blobs/"):
		d := path[strings.LastIndex(path, "/")+1:]
		body, ok := r.blobs[d]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.Header().Set("Docker-Content-Digest", d)
		if req.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestImmutablePushFollowsContentNotCompression(t *testing.T) {
	host := newMemRegistry(t)
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "a.yaml"), []byte("kind: ConfigMap\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ref := "oci://" + host + "/packages/app:1.0.0-1"
	opts := PushOptions{PlainHTTP: true, Immutable: true}
	ctx := context.Background()

	first, err := Push(ctx, tree, ref, opts)
	if err != nil {
		t.Fatal(err)
	}
	if first.Reused {
		t.Fatal("first push reported as reused")
	}

	// Another toolchain compresses the same content differently.
	defer func(l int) { gzipLevel = l }(gzipLevel)
	gzipLevel = gzip.BestSpeed
	again, err := Push(ctx, tree, ref, opts)
	if err != nil {
		t.Fatalf("same content, other compression: %v", err)
	}
	if !again.Reused || again.Digest != first.Digest {
		t.Fatalf("published artifact not reused: %+v vs %+v", again, first)
	}

	if err := os.WriteFile(filepath.Join(tree, "a.yaml"), []byte("kind: Secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Push(ctx, tree, ref, opts); !errors.Is(err, ErrTagChanged) {
		t.Fatalf("changed content under the same tag: %v", err)
	}
	if _, err := Push(ctx, tree, ref, PushOptions{PlainHTTP: true}); err != nil {
		t.Fatalf("a mutable push may move the tag: %v", err)
	}
}

func TestImmutablePushReadsUnannotatedArtifacts(t *testing.T) {
	host := newMemRegistry(t)
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "a.yaml"), []byte("kind: ConfigMap\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ref := "oci://" + host + "/packages/app:1.0.0-1"
	ctx := context.Background()

	annotateContent = false
	old, err := Push(ctx, tree, ref, PushOptions{PlainHTTP: true})
	annotateContent = true
	if err != nil {
		t.Fatal(err)
	}
	defer func(l int) { gzipLevel = l }(gzipLevel)
	gzipLevel = gzip.BestSpeed
	again, err := Push(ctx, tree, ref, PushOptions{PlainHTTP: true, Immutable: true})
	if err != nil {
		t.Fatalf("unannotated artifact with the same content: %v", err)
	}
	if !again.Reused || again.Digest != old.Digest {
		t.Fatalf("not reused: %+v vs %+v", again, old)
	}
}
