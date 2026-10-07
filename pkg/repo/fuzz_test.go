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
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

// Indexes, roots and signature files come from the network: whatever they
// hold, parsing must not panic, and what it accepts must hold up.

func FuzzParse(f *testing.F) {
	f.Add([]byte("apiVersion: kubepkg.dev/v1alpha1\nkind: PackageIndex\npackages: {}\n"))
	f.Add([]byte(strings.Replace(recipe("1.3.0", "sha256:"+strings.Repeat("a", 64)), "kind: PackageSource", "kind: PackageIndex", 1)))
	f.Add([]byte("packages: {a: {versions: [{version: 1.0.0, digest: x, spec: {version: 1.0.0}}]}}"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		idx, err := Parse(raw)
		if err != nil {
			return
		}
		for name, p := range idx.Packages {
			for _, v := range p.Versions {
				d, err := SpecDigest(v.Spec)
				if err != nil || d != v.Digest {
					t.Fatalf("%s %s: accepted with a digest that does not match its spec", name, v.Version)
				}
			}
		}
	})
}

func FuzzParseRoot(f *testing.F) {
	ks := keys(f, 1)
	sr, err := NewRoot(pubs(ks[0]), 1, pubs(ks[0]), 1, time.Now().Add(time.Hour))
	if err != nil {
		f.Fatal(err)
	}
	_ = SignRoot(sr, ks[0].priv)
	_ = sr.Signed.Delegate("t", []string{"a-*"}, pubs(ks[0]), 1)
	seed, _ := yaml.Marshal(sr)
	f.Add(seed)
	f.Add([]byte("signed: {version: 1}\nsignatures: [{keyid: x, sig: '!!'}]"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		sr, err := ParseRoot(raw)
		if err != nil {
			return
		}
		// Whatever was parsed can be checked without panicking.
		_, _ = Bootstrap(sr, pubs(ks[0]), 1)
		_ = sr.Signed.checkDelegations()
		idx := &Index{Packages: map[string]Package{"a-b": {Versions: []Version{{Version: "1.0.0"}}}}}
		sr.Signed.applyDelegations(idx)
	})
}

func FuzzSignatures(f *testing.F) {
	ks := keys(f, 1)
	data := []byte("index")
	sig, _ := SignIndex(data, nil, ks[0].priv)
	f.Add(sig)
	f.Add([]byte("c2lnbmF0dXJl"))
	f.Add([]byte("signatures: [{keyid: '', sig: ''}]"))
	id, _ := KeyID(string(ks[0].pub))
	keys := map[string]string{id: string(ks[0].pub)}
	f.Fuzz(func(t *testing.T, raw []byte) {
		sigs, err := parseSignatures(raw)
		if err != nil {
			return
		}
		if n := countValid([]byte("other data"), sigs.Signatures, keys, Role{KeyIDs: []string{id}, Threshold: 1}); n != 0 {
			t.Fatalf("a signature of other data counted")
		}
	})
}
