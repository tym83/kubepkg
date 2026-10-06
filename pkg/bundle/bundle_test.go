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

package bundle

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tym83/kubepkg/api/v1beta1"
	"github.com/tym83/kubepkg/pkg/images"
	"github.com/tym83/kubepkg/pkg/repo"
	"github.com/tym83/kubepkg/pkg/source"
)

// memRegistry is a minimal OCI distribution server: manifests by
// repository and reference, blobs by digest.
type memRegistry struct {
	mu        sync.Mutex
	blobs     map[string][]byte
	manifests map[string][]byte // repository@reference
	types     map[string]string
	uploads   int
}

func newRegistry(t *testing.T) string {
	t.Helper()
	r := &memRegistry{blobs: map[string][]byte{}, manifests: map[string][]byte{}, types: map[string]string{}}
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func (r *memRegistry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := req.URL.Path
	switch {
	case p == "/v2/" || p == "/v2":
	case strings.Contains(p, "/manifests/"):
		i := strings.Index(p, "/manifests/")
		key := p[len("/v2/"):i] + "@" + p[i+len("/manifests/"):]
		if req.Method == http.MethodPut {
			body, _ := io.ReadAll(req.Body)
			d := digest.FromBytes(body).String()
			repoKey := p[len("/v2/"):i]
			for _, k := range []string{key, repoKey + "@" + d} {
				r.manifests[k], r.types[k] = body, req.Header.Get("Content-Type")
			}
			w.Header().Set("Docker-Content-Digest", d)
			w.WriteHeader(http.StatusCreated)
			return
		}
		body, ok := r.manifests[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", r.types[key])
		w.Header().Set("Docker-Content-Digest", digest.FromBytes(body).String())
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		if req.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
	case strings.HasSuffix(p, "/blobs/uploads/") && req.Method == http.MethodPost:
		r.uploads++
		w.Header().Set("Location", fmt.Sprintf("%s%d", p, r.uploads))
		w.WriteHeader(http.StatusAccepted)
	case strings.Contains(p, "/blobs/uploads/") && req.Method == http.MethodPut:
		body, _ := io.ReadAll(req.Body)
		d := req.URL.Query().Get("digest")
		if digest.FromBytes(body).String() != d {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.blobs[d] = body
		w.WriteHeader(http.StatusCreated)
	case strings.Contains(p, "/blobs/"):
		body, ok := r.blobs[p[strings.LastIndex(p, "/")+1:]]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.Header().Set("Docker-Content-Digest", digest.FromBytes(body).String())
		if req.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// pushImage puts a one-layer image at host/name:tag and returns its
// pinned reference.
func pushImage(t *testing.T, host, name, tag string) string {
	t.Helper()
	ctx := context.Background()
	repo, err := (images.Resolver{PlainHTTP: true}).Repository(host + "/" + name)
	if err != nil {
		t.Fatal(err)
	}
	blob := func(mediaType string, b []byte) ocispec.Descriptor {
		d := ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(b), Size: int64(len(b))}
		if err := repo.Push(ctx, d, bytes.NewReader(b)); err != nil {
			t.Fatal(err)
		}
		return d
	}
	cfg := blob(ocispec.MediaTypeImageConfig, []byte(`{"architecture":"amd64","os":"linux"}`))
	layer := blob(ocispec.MediaTypeImageLayerGzip, []byte("layer of "+name))
	raw, _ := json.Marshal(ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest, Config: cfg, Layers: []ocispec.Descriptor{layer}})
	md := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(raw), Size: int64(len(raw))}
	if err := repo.PushReference(ctx, md, bytes.NewReader(raw), tag); err != nil {
		t.Fatal(err)
	}
	return host + "/" + name + ":" + tag + "@" + md.Digest.String()
}

type fixture struct {
	upstream, indexURL string
	pub                []byte
	ver                repo.Version
	chart              *v1beta1.ChartRef
	image              string
}

// publish makes an upstream registry with a chart and an image, and a
// signed repository whose package pins them, or pins no images.
func publish(t *testing.T, pinImages bool) fixture {
	t.Helper()
	ctx := context.Background()
	f := fixture{upstream: newRegistry(t)}
	f.image = pushImage(t, f.upstream, "org/app", "1.0")
	chartDir := t.TempDir()
	repoName, tag, _ := images.Split(f.image)
	for n, body := range map[string]string{
		"Chart.yaml":            "apiVersion: v2\nname: app\nversion: 1.0.0\n",
		"templates/deploy.yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: app}\nspec:\n  template:\n    spec:\n      containers: [{name: app, image: \"" + repoName + ":" + tag + "\"}]\n",
	} {
		p := filepath.Join(chartDir, n)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pushed, err := source.PushChart(ctx, chartDir, "oci://"+f.upstream+"/packages/app", "app", "1.0.0-1", source.PushOptions{PlainHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	f.chart = &v1beta1.ChartRef{Repository: "oci://" + f.upstream + "/packages/app", Name: "app", Version: "1.0.0-1", Digest: pushed.LayerDigest}
	spec := v1beta1.PackageSourceSpec{Version: "1.0.0", Build: 1, Variants: []v1beta1.Variant{{Name: "default", Components: []v1beta1.Component{{Name: "app", Chart: f.chart}}}}}
	if pinImages {
		spec.Images = []string{f.image}
	}
	d, err := repo.SpecDigest(spec)
	if err != nil {
		t.Fatal(err)
	}
	f.ver = repo.Version{Version: "1.0.0", Build: 1, Digest: d, Spec: spec}
	now := metav1.NewTime(time.Now().UTC().Truncate(time.Second))
	idx := &repo.Index{APIVersion: "kubepkg.dev/v1alpha1", Kind: "RepositoryIndex", Generated: &now, Packages: map[string]repo.Package{"app": {Versions: []repo.Version{f.ver}}}}
	var buf bytes.Buffer
	if err := idx.Write(&buf); err != nil {
		t.Fatal(err)
	}
	priv, pub, err := repo.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	sig, err := repo.Sign(buf.Bytes(), priv)
	if err != nil {
		t.Fatal(err)
	}
	f.pub = pub
	files := map[string][]byte{"/repo/index.yaml": buf.Bytes(), "/repo/index.yaml.sig": sig}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b, ok := files[r.URL.Path]; ok {
			_, _ = w.Write(b)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	f.indexURL = srv.URL + "/repo/index.yaml"
	return f
}

// makeBundle bundles the fixture's package and returns the unpacked copy,
// as the air-gapped side sees it.
func makeBundle(t *testing.T, f fixture) string {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	_, rec, err := Load(ctx, []Source{{Name: "main", Spec: v1beta1.RepositorySpec{URL: f.indexURL, PublicKeys: []string{string(f.pub)}}}}, repo.DefaultFetchers(), now)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	_, err = Create(ctx, dir, rec, []Selection{{Repository: "main", Name: "app", Version: f.ver}}, CreateOptions{
		Charts: &source.Fetcher{CacheDir: t.TempDir(), PlainHTTP: true}, Registry: images.Resolver{PlainHTTP: true}, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	var tarball bytes.Buffer
	if err := Pack(dir, &tarball); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := Unpack(&tarball, out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestBundleRoundTripIntoAMirror(t *testing.T) {
	f := publish(t, true)
	dir := makeBundle(t, f)
	ctx := context.Background()
	trust := v1beta1.RepositorySpec{PublicKeys: []string{string(f.pub)}}

	v, err := Verify(ctx, dir, trust, time.Now(), false)
	if err != nil {
		t.Fatal(err)
	}
	mirrorHost := newRegistry(t)
	mirror := "oci://" + mirrorHost + "/mirror"
	site := t.TempDir()
	res, err := Import(ctx, v, ImportOptions{Mirror: mirror, Push: source.PushOptions{PlainHTTP: true}, SiteDir: site})
	if err != nil {
		t.Fatal(err)
	}
	if res.Charts != 1 || res.Images != 1 || len(res.Registries) != 1 {
		t.Fatalf("result: %+v", res)
	}
	// The operator, pointed at the mirror, fetches the pinned chart.
	fetcher := &source.Fetcher{CacheDir: t.TempDir(), PlainHTTP: true, Mirror: mirror}
	if _, _, err := fetcher.FetchChart(ctx, source.Chart{Repository: f.chart.Repository, Name: f.chart.Name, Version: f.chart.Version, Digest: f.chart.Digest}); err != nil {
		t.Fatalf("the chart is not in the mirror: %v", err)
	}
	// The image keeps its tag and digest under the mirror.
	repoName, tag, want := images.Split(f.image)
	mirrored, err := (images.Resolver{PlainHTTP: true}).Repository(mirrorHost + "/mirror/" + source.MirrorPath(repoName))
	if err != nil {
		t.Fatal(err)
	}
	if desc, err := mirrored.Resolve(ctx, tag); err != nil || desc.Digest.String() != want {
		t.Fatalf("mirrored image: %v %v", desc.Digest, err)
	}
	// The site serves the signed index unchanged.
	idx, _, err := repo.LoadIndex(ctx, repo.Fetchers{"file": fileOnly{}}, "file://"+filepath.Join(site, "main", "index.yaml"), []string{string(f.pub)})
	if err != nil || len(idx.Packages["app"].Versions) != 1 {
		t.Fatalf("site index: %v", err)
	}
	// Importing twice is fine: copies are by digest.
	if _, err := Import(ctx, v, ImportOptions{Mirror: mirror, Push: source.PushOptions{PlainHTTP: true}}); err != nil {
		t.Fatalf("second import: %v", err)
	}
}

type fileOnly struct{}

func (fileOnly) FetchIndex(_ context.Context, u string) ([]byte, error) {
	return os.ReadFile(strings.TrimPrefix(u, "file://"))
}

func TestVerifyRefusesWhatTheRepositoryDidNotSign(t *testing.T) {
	ctx := context.Background()
	f := publish(t, true)
	trust := v1beta1.RepositorySpec{PublicKeys: []string{string(f.pub)}}

	_, other, _ := repo.GenerateKey()
	if _, err := Verify(ctx, makeBundle(t, f), v1beta1.RepositorySpec{PublicKeys: []string{string(other)}}, time.Now(), false); err == nil {
		t.Error("a bundle verified with keys that did not sign its index")
	}
	if _, err := Verify(ctx, makeBundle(t, f), v1beta1.RepositorySpec{}, time.Now(), false); err == nil {
		t.Error("a bundle verified with no keys at all")
	}

	dir := makeBundle(t, f)
	if err := os.WriteFile(chartFile(dir, f.chart.Digest), []byte("not the chart"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(ctx, dir, trust, time.Now(), false); err == nil || !strings.Contains(err.Error(), "the package pins") {
		t.Errorf("a swapped chart: %v", err)
	}

	dir = makeBundle(t, f)
	m, _ := Read(dir)
	m.Packages[0].Digest = "sha256:" + strings.Repeat("0", 64)
	writeManifest(t, dir, m)
	if _, err := Verify(ctx, dir, trust, time.Now(), false); err == nil || !strings.Contains(err.Error(), "not in the signed index") {
		t.Errorf("a package the index does not carry: %v", err)
	}

	dir = makeBundle(t, f)
	m, _ = Read(dir)
	m.Images = nil
	writeManifest(t, dir, m)
	if _, err := Verify(ctx, dir, trust, time.Now(), false); err == nil || !strings.Contains(err.Error(), "lacks image") {
		t.Errorf("a missing image: %v", err)
	}
}

func TestImagesTheRepositoryDoesNotPin(t *testing.T) {
	ctx := context.Background()
	f := publish(t, false)
	dir := makeBundle(t, f)
	m, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Images) != 1 || m.Images[0].Pinned || m.Images[0].Ref != f.image {
		t.Fatalf("images found in the charts: %+v", m.Images)
	}
	trust := v1beta1.RepositorySpec{PublicKeys: []string{string(f.pub)}}
	if _, err := Verify(ctx, dir, trust, time.Now(), false); err == nil || !strings.Contains(err.Error(), "--allow-unpinned-images") {
		t.Fatalf("unpinned images must be refused by default: %v", err)
	}
	if _, err := Verify(ctx, dir, trust, time.Now(), true); err != nil {
		t.Fatalf("allowed: %v", err)
	}
}

func TestUnpackRefusesEscapes(t *testing.T) {
	for _, name := range []string{"../evil", "/etc/passwd", "a/../../b"} {
		if _, err := safePath(t.TempDir(), name); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestNodeMirrors(t *testing.T) {
	c, talos, err := NodeMirrors("oci://registry.internal:5000/mirror", false, []string{"docker.io", "quay.io"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c["quay.io"], `[host."https://registry.internal:5000/v2/mirror/quay.io"]`) || !strings.Contains(c["docker.io"], `server = "https://registry-1.docker.io"`) {
		t.Fatalf("containerd: %v", c)
	}
	if !strings.Contains(talos, "quay.io:\n        endpoints: [\"https://registry.internal:5000/v2/mirror/quay.io\"]\n        overridePath: true") {
		t.Fatalf("talos:\n%s", talos)
	}
}

func writeManifest(t *testing.T, dir string, m *Manifest) {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}
