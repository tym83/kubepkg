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
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/kuberoot-dev/kubepkg/api/v1"
)

// Roles a root delegates.
const (
	RoleRoot  = "root"
	RoleIndex = "index"
)

// RootFile is where a repository publishes its current root, next to its
// index; every version also lives at root/<version>.yaml.
const RootFile = "root.yaml"

// ErrExpired is returned for a root or an index past its expiry.
var ErrExpired = errors.New("expired")

// Root says which keys may sign for which role, and how many must. It is
// the repository's trust anchor, after TUF: clients pin the first root's
// keys and follow later versions, each signed by enough keys of the root
// before it and of itself.
type Root struct {
	Version int       `json:"version"`
	Expires time.Time `json:"expires"`
	// Keys are PEM encoded ed25519 public keys by key ID.
	Keys  map[string]string `json:"keys"`
	Roles map[string]Role   `json:"roles"`
	// Delegations hand packages to other keys. A kubepkg that does not
	// know them cannot reproduce what was signed and refuses the root,
	// rather than install delegated packages unchecked.
	Delegations []Delegation `json:"delegations,omitempty"`
}

// Role is the keys that sign for a role and how many signatures it takes.
type Role struct {
	KeyIDs    []string `json:"keyids"`
	Threshold int      `json:"threshold"`
}

// Signature is one key's signature.
type Signature struct {
	KeyID string `json:"keyid"`
	Sig   string `json:"sig"`
}

// SignedRoot is a root with the signatures over its canonical form.
type SignedRoot struct {
	Signed     Root        `json:"signed"`
	Signatures []Signature `json:"signatures"`
}

// Signatures is the signature file of an index: several keys may sign.
type Signatures struct {
	Signatures []Signature `json:"signatures"`
}

// KeyID identifies a public key: the hex sha256 of its PKIX encoding.
func KeyID(publicPEM string) (string, error) {
	block, _ := pem.Decode([]byte(publicPEM))
	if block == nil {
		return "", errors.New("public key is not PEM")
	}
	if _, err := x509.ParsePKIXPublicKey(block.Bytes); err != nil {
		return "", err
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:]), nil
}

func publicOf(privatePEM []byte) (ed25519.PrivateKey, string, error) {
	block, _ := pem.Decode(privatePEM)
	if block == nil {
		return nil, "", errors.New("signing key is not PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, "", err
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, "", errors.New("signing key is not ed25519")
	}
	der, err := x509.MarshalPKIXPublicKey(priv.Public())
	if err != nil {
		return nil, "", err
	}
	id, err := KeyID(string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})))
	return priv, id, err
}

// canonical is the JSON form a root is signed in; struct field order and
// sorted map keys make it stable.
func (r Root) canonical() ([]byte, error) { return json.Marshal(r) }

// NewRoot makes version 1 of a root from public keys per role.
func NewRoot(rootKeys []string, rootThreshold int, indexKeys []string, indexThreshold int, expires time.Time) (*SignedRoot, error) {
	r := Root{Version: 1, Expires: expires.UTC().Truncate(time.Second), Keys: map[string]string{}, Roles: map[string]Role{}}
	for role, spec := range map[string]struct {
		keys      []string
		threshold int
	}{RoleRoot: {rootKeys, rootThreshold}, RoleIndex: {indexKeys, indexThreshold}} {
		if spec.threshold < 1 || spec.threshold > len(spec.keys) {
			return nil, fmt.Errorf("role %s: threshold %d with %d keys", role, spec.threshold, len(spec.keys))
		}
		var ids []string
		for _, k := range spec.keys {
			id, err := KeyID(k)
			if err != nil {
				return nil, fmt.Errorf("role %s: %w", role, err)
			}
			r.Keys[id] = strings.TrimSpace(k) + "\n"
			ids = append(ids, id)
		}
		sort.Strings(ids)
		r.Roles[role] = Role{KeyIDs: ids, Threshold: spec.threshold}
	}
	return &SignedRoot{Signed: r}, nil
}

// SignRoot adds the key's signature to a root. Signers can sign one after
// another, on different machines.
func SignRoot(sr *SignedRoot, privatePEM []byte) error {
	priv, id, err := publicOf(privatePEM)
	if err != nil {
		return err
	}
	data, err := sr.Signed.canonical()
	if err != nil {
		return err
	}
	sr.Signatures = append(dropSignature(sr.Signatures, id), Signature{KeyID: id, Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, data))})
	return nil
}

// SignIndex adds the key's signature over the index bytes to a signature
// file, which may be empty or hold signatures by other keys.
func SignIndex(index, sigFile, privatePEM []byte) ([]byte, error) {
	priv, id, err := publicOf(privatePEM)
	if err != nil {
		return nil, err
	}
	sigs, err := parseSignatures(sigFile)
	if err != nil {
		return nil, err
	}
	sigs.Signatures = append(dropSignature(sigs.Signatures, id), Signature{KeyID: id, Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, index))})
	return yaml.Marshal(sigs)
}

// parseSignatures reads a signature file: a list of signatures, or the
// plain base64 signature by one key that every kubepkg release reads,
// which becomes one entry without a key ID.
func parseSignatures(sigFile []byte) (Signatures, error) {
	var sigs Signatures
	text := strings.TrimSpace(string(sigFile))
	if text == "" {
		return sigs, nil
	}
	if yaml.Unmarshal(sigFile, &sigs) == nil && len(sigs.Signatures) > 0 {
		return sigs, nil
	}
	if _, err := base64.StdEncoding.DecodeString(text); err != nil {
		return sigs, errors.New("neither a list of signatures nor a plain signature")
	}
	return Signatures{Signatures: []Signature{{Sig: text}}}, nil
}

func dropSignature(sigs []Signature, id string) []Signature {
	var out []Signature
	for _, s := range sigs {
		if s.KeyID != id {
			out = append(out, s)
		}
	}
	return out
}

// countValid counts distinct keys of a role that signed data.
func countValid(data []byte, sigs []Signature, keys map[string]string, role Role) int {
	allowed := map[string]bool{}
	for _, id := range role.KeyIDs {
		allowed[id] = true
	}
	seen := map[string]bool{}
	for _, s := range sigs {
		if s.KeyID == "" {
			// A plain signature names no key: it counts for the first
			// allowed key it verifies with.
			raw, err := base64.StdEncoding.DecodeString(s.Sig)
			if err != nil {
				continue
			}
			for _, id := range role.KeyIDs {
				pub, err := ParsePublicKeys([]string{keys[id]})
				if err == nil && !seen[id] && ed25519.Verify(pub[0], data, raw) {
					seen[id] = true
					break
				}
			}
			continue
		}
		if !allowed[s.KeyID] || seen[s.KeyID] {
			continue
		}
		pub, err := ParsePublicKeys([]string{keys[s.KeyID]})
		if err != nil {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(s.Sig)
		if err == nil && ed25519.Verify(pub[0], data, raw) {
			seen[s.KeyID] = true
		}
	}
	return len(seen)
}

func meets(data []byte, sigs []Signature, keys map[string]string, role Role, what string) error {
	if n := countValid(data, sigs, keys, role); n < role.Threshold {
		return fmt.Errorf("%w: %s has %d valid signatures, needs %d", ErrBadSignature, what, n, role.Threshold)
	}
	return nil
}

// TrustedRoot is where a client stands: the root it last accepted.
type TrustedRoot struct {
	Root Root
}

// Bootstrap verifies version 1 of a root against keys the client pinned
// out of band and the threshold it expects.
func Bootstrap(sr *SignedRoot, pinned []string, threshold int) (*TrustedRoot, error) {
	if sr.Signed.Version != 1 {
		return nil, fmt.Errorf("bootstrap needs root version 1, got %d", sr.Signed.Version)
	}
	keys := map[string]string{}
	var ids []string
	for _, k := range pinned {
		id, err := KeyID(k)
		if err != nil {
			return nil, err
		}
		keys[id] = k
		ids = append(ids, id)
	}
	if threshold < 1 {
		threshold = 1
	}
	data, err := sr.Signed.canonical()
	if err != nil {
		return nil, err
	}
	if err := meets(data, sr.Signatures, keys, Role{KeyIDs: ids, Threshold: threshold}, "root version 1 (pinned keys)"); err != nil {
		return nil, err
	}
	if err := meets(data, sr.Signatures, sr.Signed.Keys, sr.Signed.Roles[RoleRoot], "root version 1"); err != nil {
		return nil, err
	}
	if err := sr.Signed.check(); err != nil {
		return nil, fmt.Errorf("%w: root version 1: %v", ErrBadSignature, err)
	}
	return &TrustedRoot{Root: sr.Signed}, nil
}

// Update accepts the next root version: it must be exactly one version on,
// signed by enough root keys of the current root and of itself.
func (t *TrustedRoot) Update(next *SignedRoot) error {
	if next.Signed.Version != t.Root.Version+1 {
		return fmt.Errorf("root version %d does not follow %d", next.Signed.Version, t.Root.Version)
	}
	data, err := next.Signed.canonical()
	if err != nil {
		return err
	}
	if err := meets(data, next.Signatures, t.Root.Keys, t.Root.Roles[RoleRoot], fmt.Sprintf("root version %d (by version %d keys)", next.Signed.Version, t.Root.Version)); err != nil {
		return err
	}
	if err := meets(data, next.Signatures, next.Signed.Keys, next.Signed.Roles[RoleRoot], fmt.Sprintf("root version %d (by its own keys)", next.Signed.Version)); err != nil {
		return err
	}
	if err := next.Signed.check(); err != nil {
		return fmt.Errorf("%w: root version %d: %v", ErrBadSignature, next.Signed.Version, err)
	}
	t.Root = next.Signed
	return nil
}

// VerifyIndex checks an index against the index role of the root and the
// expiry of both, and leaves out versions of delegated packages their
// delegation did not sign.
func (t *TrustedRoot) VerifyIndex(index, sigFile []byte, idx *Index, now time.Time) error {
	if now.After(t.Root.Expires) {
		return fmt.Errorf("%w: root version %d expired %s", ErrExpired, t.Root.Version, t.Root.Expires.Format(time.RFC3339))
	}
	sigs, err := parseSignatures(sigFile)
	if err != nil {
		return fmt.Errorf("%w: signature file: %v", ErrBadSignature, err)
	}
	if err := meets(index, sigs.Signatures, t.Root.Keys, t.Root.Roles[RoleIndex], "index"); err != nil {
		return err
	}
	if idx.Expires == nil {
		return fmt.Errorf("%w: a repository with a root must give its index an expiry", ErrBadSignature)
	}
	if now.After(idx.Expires.Time) {
		return fmt.Errorf("%w: index expired %s; the repository has stopped publishing or is being held back", ErrExpired, idx.Expires.Format(time.RFC3339))
	}
	t.Root.applyDelegations(idx)
	return nil
}

// ParseRoot reads a signed root.
func ParseRoot(raw []byte) (*SignedRoot, error) {
	var sr SignedRoot
	if err := yaml.Unmarshal(raw, &sr); err != nil {
		return nil, err
	}
	if sr.Signed.Version < 1 {
		return nil, errors.New("not a root")
	}
	// A root is signed as this release re-encodes it, so a field it does
	// not know would only surface as signatures that do not verify. Say
	// what it is instead.
	if err := yaml.UnmarshalStrict(raw, &SignedRoot{}); err != nil && strings.Contains(err.Error(), "unknown field") {
		return nil, fmt.Errorf("%w: root version %d uses features this kubepkg does not know (%v); upgrade kubepkg", ErrNewerFormat, sr.Signed.Version, err)
	}
	return &sr, nil
}

// ErrNewerFormat is returned for repository files made for a newer kubepkg.
var ErrNewerFormat = errors.New("made for a newer kubepkg")

// Trust is what a client pins: the first root's keys and threshold, and
// the root version it accepted last, so a root cannot be rolled back.
type Trust struct {
	RootKeys      []string
	RootThreshold int
	// RootVersion and RootDigest are the root accepted before; 0 starts
	// from version 1. A different root under an accepted version number is
	// a forked chain, as made with stolen old keys, and is refused.
	RootVersion int
	RootDigest  string
}

// Digest identifies a root's content.
func (r Root) Digest() (string, error) {
	data, err := r.canonical()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// LoadIndexWithRoot fetches the root chain and the index next to url,
// follows the root from version 1 to the current one, checks the index
// against its index role and both expiries, and returns the index and the
// root reached, to be passed back as Trust next time.
func LoadIndexWithRoot(ctx context.Context, fetchers Fetchers, url string, trust Trust, now time.Time) (*Index, []byte, Trust, error) {
	base := url[:strings.LastIndex(url, "/")+1]
	fetchRoot := func(path string) (*SignedRoot, error) {
		raw, err := fetchers.Fetch(ctx, base+path)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrFetchFailed, path, err)
		}
		sr, err := ParseRoot(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrInvalidIndex, path, err)
		}
		return sr, nil
	}
	latest, err := fetchRoot(RootFile)
	if err != nil {
		return nil, nil, trust, err
	}
	if latest.Signed.Version < trust.RootVersion {
		return nil, nil, trust, fmt.Errorf("%w: root version %d is older than %d accepted before", ErrBadSignature, latest.Signed.Version, trust.RootVersion)
	}
	first, err := fetchRoot("root/1.yaml")
	if err != nil {
		return nil, nil, trust, err
	}
	trusted, err := Bootstrap(first, trust.RootKeys, trust.RootThreshold)
	if err != nil {
		return nil, nil, trust, err
	}
	checkAccepted := func() error {
		if trusted.Root.Version != trust.RootVersion || trust.RootDigest == "" {
			return nil
		}
		d, err := trusted.Root.Digest()
		if err != nil {
			return err
		}
		if d != trust.RootDigest {
			return fmt.Errorf("%w: root version %d differs from the one accepted before: a forked root chain", ErrBadSignature, trust.RootVersion)
		}
		return nil
	}
	if err := checkAccepted(); err != nil {
		return nil, nil, trust, err
	}
	for v := 2; v <= latest.Signed.Version; v++ {
		next, err := fetchRoot(fmt.Sprintf("root/%d.yaml", v))
		if err != nil {
			return nil, nil, trust, err
		}
		if err := trusted.Update(next); err != nil {
			return nil, nil, trust, err
		}
		if err := checkAccepted(); err != nil {
			return nil, nil, trust, err
		}
	}
	raw, err := fetchers.Fetch(ctx, url)
	if err != nil {
		return nil, nil, trust, fmt.Errorf("%w: %v", ErrFetchFailed, err)
	}
	sig, err := fetchers.Fetch(ctx, url+SignatureSuffix)
	if err != nil {
		return nil, nil, trust, fmt.Errorf("%w: no signature at %s%s: %v", ErrBadSignature, url, SignatureSuffix, err)
	}
	idx, err := Parse(raw)
	if err != nil {
		return nil, nil, trust, fmt.Errorf("%w: %v", ErrInvalidIndex, err)
	}
	if err := trusted.VerifyIndex(raw, sig, idx, now); err != nil {
		return nil, nil, trust, err
	}
	digest, err := trusted.Root.Digest()
	if err != nil {
		return nil, nil, trust, err
	}
	trust.RootVersion, trust.RootDigest = trusted.Root.Version, digest
	return idx, raw, trust, nil
}

// LoadRepository loads a Repository's index the way its spec asks: through
// its pinned root, with plain public keys, or unchecked. It refuses an
// index older than the one in status and returns the root reached, for
// status, when the repository has a root.
func LoadRepository(ctx context.Context, fetchers Fetchers, spec v1.RepositorySpec, status v1.RepositoryStatus, now time.Time) (*Index, []byte, Trust, error) {
	var (
		idx   *Index
		raw   []byte
		trust Trust
		err   error
	)
	if t := spec.Trust; t != nil {
		trust = Trust{RootKeys: t.RootKeys, RootThreshold: int(t.RootThreshold), RootVersion: int(status.RootVersion), RootDigest: status.RootDigest}
		idx, raw, trust, err = LoadIndexWithRoot(ctx, fetchers, spec.URL, trust, now)
	} else {
		if len(spec.PublicKeys) == 0 && !spec.AllowUnsigned {
			return nil, nil, trust, fmt.Errorf("%w: the repository gives no keys to check its index with; set publicKeys or trust, or allowUnsigned to accept an unsigned index", ErrBadSignature)
		}
		idx, raw, err = LoadIndex(ctx, fetchers, spec.URL, spec.PublicKeys)
	}
	if err != nil {
		return nil, nil, trust, err
	}
	if seen := status.IndexGenerated; seen != nil && (idx.Generated == nil || idx.Generated.Before(seen)) {
		return nil, nil, trust, fmt.Errorf("%w: index is older than the one accepted before (%s): refusing a rollback", ErrBadSignature, seen.UTC().Format(time.RFC3339))
	}
	return idx, raw, trust, nil
}
