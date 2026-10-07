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

package repo

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/tym83/kubepkg/api/v1beta1"
)

type keypair struct{ priv, pub []byte }

func keys(t *testing.T, n int) []keypair {
	t.Helper()
	var out []keypair
	for i := 0; i < n; i++ {
		priv, pub, err := GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, keypair{priv, pub})
	}
	return out
}

func pubs(ks ...keypair) []string {
	var out []string
	for _, k := range ks {
		out = append(out, string(k.pub))
	}
	return out
}

// repoServer holds what a repository publishes, served under mem://r/.
type repoServer struct {
	files memFetcher
	now   time.Time
}

func newRepoServer() *repoServer {
	return &repoServer{files: memFetcher{}, now: time.Now().UTC()}
}

func (s *repoServer) publishRoot(t *testing.T, sr *SignedRoot) {
	t.Helper()
	raw, err := yaml.Marshal(sr)
	if err != nil {
		t.Fatal(err)
	}
	s.files["mem://r/"+RootFile] = raw
	s.files["mem://r/root/"+itoa(sr.Signed.Version)+".yaml"] = raw
}

func itoa(i int) string { return strconv.Itoa(i) }

func (s *repoServer) publishIndex(t *testing.T, expires time.Time, signers ...keypair) {
	t.Helper()
	g := metav1.NewTime(s.now)
	e := metav1.NewTime(expires)
	idx := &Index{APIVersion: "kubepkg.dev/v1alpha1", Kind: IndexKind, Generated: &g, Packages: map[string]Package{}}
	if !expires.IsZero() {
		idx.Expires = &e
	}
	var buf bytes.Buffer
	if err := idx.Write(&buf); err != nil {
		t.Fatal(err)
	}
	var sig []byte
	for _, k := range signers {
		var err error
		if sig, err = SignIndex(buf.Bytes(), sig, k.priv); err != nil {
			t.Fatal(err)
		}
	}
	s.files["mem://r/index.yaml"] = buf.Bytes()
	s.files["mem://r/index.yaml"+SignatureSuffix] = sig
}

func (s *repoServer) load(trust Trust) (Trust, error) {
	_, _, got, err := LoadIndexWithRoot(context.Background(), Fetchers{"mem": s.files}, "mem://r/index.yaml", trust, s.now)
	return got, err
}

func signedRoot(t *testing.T, version int, rootKeys []keypair, rootThreshold int, indexKeys []keypair, indexThreshold int, expires time.Time, signers ...keypair) *SignedRoot {
	t.Helper()
	sr, err := NewRoot(pubs(rootKeys...), rootThreshold, pubs(indexKeys...), indexThreshold, expires)
	if err != nil {
		t.Fatal(err)
	}
	sr.Signed.Version = version
	for _, k := range signers {
		if err := SignRoot(sr, k.priv); err != nil {
			t.Fatal(err)
		}
	}
	return sr
}

func TestThresholdsAndExpiry(t *testing.T) {
	r, i := keys(t, 3), keys(t, 2)
	s := newRepoServer()
	year := s.now.Add(365 * 24 * time.Hour)
	week := s.now.Add(7 * 24 * time.Hour)
	pin := Trust{RootKeys: pubs(r...), RootThreshold: 2}

	s.publishRoot(t, signedRoot(t, 1, r, 2, i, 2, year, r[0]))
	s.publishIndex(t, week, i[0], i[1])
	if _, err := s.load(pin); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("root with 1 of 2 signatures: %v", err)
	}

	s.publishRoot(t, signedRoot(t, 1, r, 2, i, 2, year, r[0], r[2]))
	if got, err := s.load(pin); err != nil || got.RootVersion != 1 || got.RootDigest == "" {
		t.Fatalf("root with 2 of 2 and index with 2 of 2: %+v %v", got, err)
	}
	s.publishIndex(t, week, i[0])
	if _, err := s.load(pin); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("index with 1 of 2 signatures: %v", err)
	}
	s.publishIndex(t, week, i[0], i[0])
	if _, err := s.load(pin); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("the same key twice must count once: %v", err)
	}

	s.publishIndex(t, s.now.Add(-time.Minute), i[0], i[1])
	if _, err := s.load(pin); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired index: %v", err)
	}
	s.publishIndex(t, time.Time{}, i[0], i[1])
	if _, err := s.load(pin); err == nil || !strings.Contains(err.Error(), "expiry") {
		t.Fatalf("index without expiry: %v", err)
	}
	s.publishRoot(t, signedRoot(t, 1, r, 2, i, 2, s.now.Add(-time.Minute), r[0], r[1]))
	s.publishIndex(t, week, i[0], i[1])
	if _, err := s.load(pin); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired root: %v", err)
	}

	other := keys(t, 2)
	s.publishRoot(t, signedRoot(t, 1, r, 2, i, 2, year, r[0], r[1]))
	if _, err := s.load(Trust{RootKeys: pubs(other...), RootThreshold: 1}); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("pinned keys that did not sign: %v", err)
	}
}

func TestRotationRollbackAndFork(t *testing.T) {
	old, fresh, i1, i2 := keys(t, 3), keys(t, 3), keys(t, 1), keys(t, 1)
	s := newRepoServer()
	year := s.now.Add(365 * 24 * time.Hour)
	week := s.now.Add(7 * 24 * time.Hour)
	pin := Trust{RootKeys: pubs(old...), RootThreshold: 2}

	v1 := signedRoot(t, 1, old, 2, i1, 1, year, old[0], old[1])
	s.publishRoot(t, v1)
	s.publishIndex(t, week, i1[0])
	trust, err := s.load(pin)
	if err != nil {
		t.Fatal(err)
	}

	// Rotation only by the new keys is refused.
	s.publishRoot(t, signedRoot(t, 2, fresh, 2, i2, 1, year, fresh[0], fresh[1]))
	if _, err := s.load(trust); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("version 2 without the old keys: %v", err)
	}
	// Signed by enough old and new root keys, it is the new root.
	genuine := signedRoot(t, 2, fresh, 2, i2, 1, year, old[0], old[2], fresh[0], fresh[1])
	s.publishRoot(t, genuine)
	if _, err := s.load(trust); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("an index by the key version 2 dropped: %v", err)
	}
	s.publishIndex(t, week, i2[0])
	if trust, err = s.load(trust); err != nil || trust.RootVersion != 2 {
		t.Fatalf("rotated: %+v %v", trust, err)
	}

	// Serving version 1 again is a rollback.
	s.files["mem://r/"+RootFile] = s.files["mem://r/root/1.yaml"]
	if _, err := s.load(trust); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("root rollback: %v", err)
	}

	// Someone holding the old keys forks version 2 with keys of their own.
	evil := keys(t, 2)
	fork := signedRoot(t, 2, evil, 1, evil, 1, year, old[0], old[1], evil[0])
	s.publishRoot(t, fork)
	s.publishIndex(t, week, evil[1])
	if _, err := s.load(trust); err == nil || !strings.Contains(err.Error(), "forked") {
		t.Fatalf("a forked version 2 was accepted: %v", err)
	}
	if _, err := s.load(pin); err != nil {
		t.Fatal("without the accepted root on record the fork is indistinguishable; the record is what protects")
	}
}

// A plain signature, the one form kubepkg v0.1 reads, works in every
// mode: alone with a root of threshold 1, and as the first of several
// signatures once trust sign adds more.
func TestPlainSignaturesEverywhere(t *testing.T) {
	r, i := keys(t, 1), keys(t, 2)
	s := newRepoServer()
	year := s.now.Add(365 * 24 * time.Hour)
	week := s.now.Add(7 * 24 * time.Hour)
	s.publishRoot(t, signedRoot(t, 1, r, 1, i, 1, year, r[0]))
	s.publishIndex(t, week)
	index := s.files["mem://r/index.yaml"]
	plain, err := Sign(index, i[0].priv)
	if err != nil {
		t.Fatal(err)
	}
	// What a v0.1 client does with the file.
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(plain)))
	if err != nil {
		t.Fatalf("not plain base64: %q", plain)
	}
	pk, _ := ParsePublicKeys([]string{string(i[0].pub)})
	if !ed25519.Verify(pk[0], index, raw) {
		t.Fatal("a v0.1 client would refuse it")
	}
	s.files["mem://r/index.yaml"+SignatureSuffix] = plain
	if _, err := s.load(Trust{RootKeys: pubs(r...), RootThreshold: 1}); err != nil {
		t.Fatalf("a root of threshold 1 refuses a plain signature: %v", err)
	}

	// Threshold 2: the plain signature counts once, a second key adds one.
	s.publishRoot(t, signedRoot(t, 1, r, 1, i, 2, year, r[0]))
	s.files["mem://r/index.yaml"] = index
	s.files["mem://r/index.yaml"+SignatureSuffix] = plain
	if _, err := s.load(Trust{RootKeys: pubs(r...), RootThreshold: 1}); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("one plain signature met a threshold of 2: %v", err)
	}
	both, err := SignIndex(index, plain, i[1].priv)
	if err != nil {
		t.Fatalf("adding to a plain signature: %v", err)
	}
	s.files["mem://r/index.yaml"+SignatureSuffix] = both
	if _, err := s.load(Trust{RootKeys: pubs(r...), RootThreshold: 1}); err != nil {
		t.Fatalf("plain plus one more: %v", err)
	}
	again, err := SignIndex(index, plain, i[0].priv)
	if err != nil {
		t.Fatal(err)
	}
	s.files["mem://r/index.yaml"+SignatureSuffix] = again
	if _, err := s.load(Trust{RootKeys: pubs(r...), RootThreshold: 1}); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("the same key as plain and as listed counted twice: %v", err)
	}
}

func TestUnsignedRepositoriesNeedConsent(t *testing.T) {
	s := newRepoServer()
	s.publishIndex(t, time.Time{})
	spec := v1beta1.RepositorySpec{URL: "mem://r/index.yaml"}
	if _, _, _, err := LoadRepository(context.Background(), Fetchers{"mem": s.files}, spec, v1beta1.RepositoryStatus{}, s.now); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("an unsigned repository without allowUnsigned: %v", err)
	}
	spec.AllowUnsigned = true
	if _, _, _, err := LoadRepository(context.Background(), Fetchers{"mem": s.files}, spec, v1beta1.RepositoryStatus{}, s.now); err != nil {
		t.Fatal(err)
	}
}

func TestARootCannotCountOneKeyTwice(t *testing.T) {
	ks := keys(t, 2)
	sr := signedRoot(t, 1, ks[:1], 1, ks[1:], 1, time.Now().Add(time.Hour))
	// The index key listed again under another ID, threshold 2.
	sr.Signed.Keys["0000"] = sr.Signed.Keys[sr.Signed.Roles[RoleIndex].KeyIDs[0]]
	sr.Signed.Roles[RoleIndex] = Role{KeyIDs: append(sr.Signed.Roles[RoleIndex].KeyIDs, "0000"), Threshold: 2}
	if err := SignRoot(sr, ks[0].priv); err != nil {
		t.Fatal(err)
	}
	if _, err := Bootstrap(sr, pubs(ks[0]), 1); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("a key under a made-up ID: %v", err)
	}
}
