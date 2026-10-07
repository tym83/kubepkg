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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/tym83/kubepkg/api/v1beta1"
)

// AnnotationSignatures on a PackageSource carries the signatures of a
// delegated team over that version, as a JSON list; the index copies them
// into the version. Annotations are outside the spec digest, so signing
// changes nothing that is signed.
const AnnotationSignatures = "kubepkg.dev/signatures"

// Delegation hands the packages whose names match its patterns to other
// keys: versions of those packages are trusted only with enough of these
// keys' signatures, whoever signs the index. A team then publishes through
// the repository's CI without the CI's key being able to forge its
// packages.
type Delegation struct {
	Name string `json:"name"`
	// Packages are name patterns, as path.Match takes them: "cdi", "kubevirt-*".
	Packages  []string `json:"packages"`
	KeyIDs    []string `json:"keyids"`
	Threshold int      `json:"threshold"`
}

// Delegate adds a delegation, putting its keys among the root's keys.
func (r *Root) Delegate(name string, packages, keys []string, threshold int) error {
	if name == "" || len(packages) == 0 {
		return fmt.Errorf("delegation %q needs a name and package patterns", name)
	}
	for _, d := range r.Delegations {
		if d.Name == name {
			return fmt.Errorf("delegation %s is given twice", name)
		}
	}
	if threshold < 1 || threshold > len(keys) {
		return fmt.Errorf("delegation %s: threshold %d with %d keys", name, threshold, len(keys))
	}
	d := Delegation{Name: name, Packages: packages, Threshold: threshold}
	for _, k := range keys {
		id, err := KeyID(k)
		if err != nil {
			return fmt.Errorf("delegation %s: %w", name, err)
		}
		r.Keys[id] = strings.TrimSpace(k) + "\n"
		d.KeyIDs = append(d.KeyIDs, id)
	}
	sort.Strings(d.KeyIDs)
	r.Delegations = append(r.Delegations, d)
	return r.checkDelegations()
}

// check refuses a root that names a key under an ID that is not its own,
// which would let one key count twice towards a threshold, or whose
// delegations cannot be applied.
func (r Root) check() error {
	for id, k := range r.Keys {
		got, err := KeyID(k)
		if err != nil {
			return fmt.Errorf("key %s: %w", id, err)
		}
		if got != id {
			return fmt.Errorf("key listed as %s is %s", id, got)
		}
	}
	return r.checkDelegations()
}

// checkDelegations refuses a root whose delegations cannot be applied.
func (r Root) checkDelegations() error {
	for _, d := range r.Delegations {
		if d.Threshold < 1 || d.Threshold > len(d.KeyIDs) {
			return fmt.Errorf("delegation %s: threshold %d with %d keys", d.Name, d.Threshold, len(d.KeyIDs))
		}
		for _, id := range d.KeyIDs {
			if _, ok := r.Keys[id]; !ok {
				return fmt.Errorf("delegation %s: key %s is not among the root's keys", d.Name, id)
			}
		}
		for _, p := range d.Packages {
			if _, err := path.Match(p, ""); err != nil {
				return fmt.Errorf("delegation %s: pattern %q: %w", d.Name, p, err)
			}
		}
	}
	return nil
}

// delegationFor returns the first delegation whose patterns match a
// package; delegations are tried in order.
func (r Root) delegationFor(pkg string) *Delegation {
	for i, d := range r.Delegations {
		for _, p := range d.Packages {
			if ok, _ := path.Match(p, pkg); ok {
				return &r.Delegations[i]
			}
		}
	}
	return nil
}

// versionMessage is what a delegated key signs: the package, its version
// and build, and the digest of its spec, so a signature cannot be moved to
// another package or build.
func versionMessage(pkg string, v Version) []byte {
	raw, _ := json.Marshal(struct {
		Type    string `json:"type"`
		Package string `json:"package"`
		Version string `json:"version"`
		Build   int32  `json:"build"`
		Digest  string `json:"digest"`
	}{"kubepkg.dev/package-version", pkg, v.Version, v.Build, v.Digest})
	return raw
}

// SignVersion signs a version of a package with a delegated key.
func SignVersion(pkg string, v Version, privatePEM []byte) (Signature, error) {
	priv, id, err := publicOf(privatePEM)
	if err != nil {
		return Signature{}, err
	}
	return Signature{KeyID: id, Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, versionMessage(pkg, v)))}, nil
}

// mergeSignatures adds signatures, one per key.
func mergeSignatures(have, add []Signature) []Signature {
	for _, s := range add {
		have = append(dropSignature(have, s.KeyID), s)
	}
	sort.Slice(have, func(i, j int) bool { return have[i].KeyID < have[j].KeyID })
	return have
}

// applyDelegations leaves out of the index every version of a delegated
// package without enough signatures of its delegation, and lists them in
// idx.Untrusted.
func (r Root) applyDelegations(idx *Index) {
	for name, p := range idx.Packages {
		d := r.delegationFor(name)
		if d == nil {
			continue
		}
		role := Role{KeyIDs: d.KeyIDs, Threshold: d.Threshold}
		kept := p.Versions[:0]
		for _, v := range p.Versions {
			if n := countValid(versionMessage(name, v), v.Signatures, r.Keys, role); n < d.Threshold {
				idx.Untrusted = append(idx.Untrusted, fmt.Sprintf("%s %s build %d (delegation %s: %d of %d signatures)", name, v.Version, v.Build, d.Name, n, d.Threshold))
				continue
			}
			kept = append(kept, v)
		}
		if len(kept) == 0 {
			delete(idx.Packages, name)
			continue
		}
		p.Versions = kept
		idx.Packages[name] = p
	}
	sort.Strings(idx.Untrusted)
}

// SignSources adds the key's signature to every PackageSource in a YAML
// file, in its signatures annotation, and returns the file and what was
// signed. Charts must be pinned by digest already, as the index will
// publish them, or the signature would not match.
func SignSources(file, privatePEM []byte) ([]byte, []string, error) {
	dec := utilyaml.NewYAMLReader(bufioReader(file))
	var docs [][]byte
	var signed []string
	for {
		doc, err := dec.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		var head struct{ Kind string }
		if yaml.Unmarshal(doc, &head) != nil || head.Kind != "PackageSource" {
			if len(bytes.TrimSpace(doc)) > 0 {
				docs = append(docs, doc)
			}
			continue
		}
		var src v1beta1.PackageSource
		if err := yaml.UnmarshalStrict(doc, &src); err != nil {
			return nil, nil, err
		}
		if err := checkSource(&src.Spec); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", src.Name, err)
		}
		if err := pinCharts(context.Background(), &src.Spec, nil, false); err != nil {
			return nil, nil, fmt.Errorf("%s: %w; sign the PackageSource kubepkg build wrote, with every chart pinned", src.Name, err)
		}
		d, err := SpecDigest(src.Spec)
		if err != nil {
			return nil, nil, err
		}
		v := Version{Version: src.Spec.Version, Build: src.Spec.Build, Digest: d}
		sig, err := SignVersion(src.Name, v, privatePEM)
		if err != nil {
			return nil, nil, err
		}
		have, err := sourceSignatures(src.Annotations)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", src.Name, err)
		}
		raw, err := json.Marshal(mergeSignatures(have, []Signature{sig}))
		if err != nil {
			return nil, nil, err
		}
		if src.Annotations == nil {
			src.Annotations = map[string]string{}
		}
		src.Annotations[AnnotationSignatures] = string(raw)
		out, err := yaml.Marshal(&src)
		if err != nil {
			return nil, nil, err
		}
		docs = append(docs, out)
		signed = append(signed, fmt.Sprintf("%s %s build %d", src.Name, v.Version, v.Build))
	}
	if len(signed) == 0 {
		return nil, nil, errors.New("no PackageSource to sign")
	}
	return bytes.Join(docs, []byte("---\n")), signed, nil
}
