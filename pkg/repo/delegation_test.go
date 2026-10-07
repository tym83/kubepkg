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
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func pkgSource(name, version string) string {
	return strings.NewReplacer("name: kubevirt", "name: "+name, "requires: [{package: cdi}]", "requires: []").Replace(recipe(version, "sha256:"+strings.Repeat("a", 64)))
}

func TestDelegatedPackagesNeedTheirTeamsSignatures(t *testing.T) {
	ks := keys(t, 5)
	rootKey, ci, teamA, teamB, outsider := ks[0], ks[1], ks[2], ks[3], ks[4]
	now := time.Now().UTC()
	sr := signedRoot(t, 1, []keypair{rootKey}, 1, []keypair{ci}, 1, now.Add(time.Hour))
	if err := sr.Signed.Delegate("virt", []string{"cdi", "kubevirt-*"}, pubs(teamA, teamB), 2); err != nil {
		t.Fatal(err)
	}
	if err := SignRoot(sr, rootKey.priv); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	sign := func(body string, by ...keypair) string {
		raw := []byte(body)
		for _, k := range by {
			var err error
			if raw, _, err = SignSources(raw, k.priv); err != nil {
				t.Fatal(err)
			}
		}
		return string(raw)
	}
	write(t, dir, "cdi.yaml", sign(pkgSource("cdi", "1.0.0"), teamA, teamB))
	write(t, dir, "kubevirt-ui.yaml", sign(pkgSource("kubevirt-ui", "1.0.0"), teamA))
	write(t, dir, "kubevirt-ui-2.yaml", sign(pkgSource("kubevirt-ui", "2.0.0"), teamA, outsider))
	write(t, dir, "free.yaml", pkgSource("free", "1.0.0"))
	idx, err := Build(context.Background(), dir, nil, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// CI, holding only the index key, tries to slip in a version.
	forged := idx.Packages["cdi"]
	evil := forged.Versions[0]
	evil.Version, evil.Spec.Version = "9.9.9", "9.9.9"
	evil.Digest, _ = SpecDigest(evil.Spec)
	forged.Versions = append([]Version{evil}, forged.Versions...)
	idx.Packages["cdi"] = forged
	exp := metav1.NewTime(now.Add(time.Hour))
	idx.Expires = &exp
	var buf bytes.Buffer
	if err := idx.Write(&buf); err != nil {
		t.Fatal(err)
	}
	sig, err := SignIndex(buf.Bytes(), nil, ci.priv)
	if err != nil {
		t.Fatal(err)
	}

	s := newRepoServer()
	s.now = now
	s.publishRoot(t, sr)
	s.files["mem://r/index.yaml"], s.files["mem://r/index.yaml"+SignatureSuffix] = buf.Bytes(), sig
	got, _, _, err := LoadIndexWithRoot(context.Background(), Fetchers{"mem": s.files}, "mem://r/index.yaml", Trust{RootKeys: pubs(rootKey), RootThreshold: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	versions := func(name string) (out []string) {
		for _, v := range got.Packages[name].Versions {
			out = append(out, v.Version)
		}
		return out
	}
	if v := versions("cdi"); len(v) != 1 || v[0] != "1.0.0" {
		t.Errorf("cdi: %v; the version the CI made up must be left out", v)
	}
	if v := versions("kubevirt-ui"); len(v) != 0 {
		t.Errorf("kubevirt-ui: %v; one team signature of two, or a key outside the team, is not enough", v)
	}
	if v := versions("free"); len(v) != 1 {
		t.Errorf("a package no delegation covers: %v", v)
	}
	if len(got.Untrusted) != 3 || !strings.Contains(strings.Join(got.Untrusted, "\n"), "cdi 9.9.9 build 0 (delegation virt: 0 of 2 signatures)") {
		t.Errorf("untrusted: %q", got.Untrusted)
	}
}

func TestASignatureCannotMoveToAnotherBuild(t *testing.T) {
	k := keys(t, 1)[0]
	v := Version{Version: "1.0.0", Build: 1, Digest: "sha256:" + strings.Repeat("b", 64)}
	sig, err := SignVersion("cdi", v, k.priv)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := KeyID(string(k.pub))
	role := Role{KeyIDs: []string{id}, Threshold: 1}
	keys := map[string]string{id: string(k.pub)}
	if countValid(versionMessage("cdi", v), []Signature{sig}, keys, role) != 1 {
		t.Fatal("a valid signature did not count")
	}
	other := v
	other.Build = 2
	for name, msg := range map[string][]byte{"another build": versionMessage("cdi", other), "another package": versionMessage("kubevirt", v)} {
		if countValid(msg, []Signature{sig}, keys, role) != 0 {
			t.Errorf("a signature counted for %s", name)
		}
	}
}

func TestSignaturesAddToAPublishedVersion(t *testing.T) {
	ks := keys(t, 2)
	dir := t.TempDir()
	write(t, dir, "cdi.yaml", pkgSource("cdi", "1.0.0"))
	base, err := Build(context.Background(), dir, nil, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(pkgSource("cdi", "1.0.0"))
	for _, k := range ks {
		if raw, _, err = SignSources(raw, k.priv); err != nil {
			t.Fatal(err)
		}
	}
	write(t, dir, "cdi.yaml", string(raw))
	idx, err := Build(context.Background(), dir, nil, BuildOptions{Base: base})
	if err != nil {
		t.Fatalf("signing a published version counts as changing it: %v", err)
	}
	if n := len(idx.Packages["cdi"].Versions[0].Signatures); n != 2 {
		t.Fatalf("signatures: %d", n)
	}
}

func TestRootsWithBrokenDelegationsAreRefused(t *testing.T) {
	ks := keys(t, 2)
	sr := signedRoot(t, 1, ks[:1], 1, ks[1:], 1, time.Now().Add(time.Hour))
	sr.Signed.Delegations = []Delegation{{Name: "x", Packages: []string{"a"}, KeyIDs: []string{"missing"}, Threshold: 1}}
	if err := SignRoot(sr, ks[0].priv); err != nil {
		t.Fatal(err)
	}
	if _, err := Bootstrap(sr, pubs(ks[0]), 1); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("a delegation to a key the root does not list: %v", err)
	}
}
