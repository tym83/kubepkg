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
	"errors"
	"testing"
)

type memFetcher map[string][]byte

func (m memFetcher) FetchIndex(_ context.Context, u string) ([]byte, error) {
	if raw, ok := m[u]; ok {
		return raw, nil
	}
	return nil, errors.New("404")
}

func TestSignAndVerify(t *testing.T) {
	priv, pub, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	_, otherPub, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	index := []byte("apiVersion: kubepkg.dev/v1alpha1\nkind: RepositoryIndex\npackages: {}\n")
	sig, err := Sign(index, priv)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := ParsePublicKeys([]string{string(pub)})
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(index, sig, keys); err != nil {
		t.Fatalf("own signature: %v", err)
	}
	other, _ := ParsePublicKeys([]string{string(otherPub)})
	if err := Verify(index, sig, other); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("a signature by another key: %v", err)
	}
	if err := Verify(append(index, ' '), sig, keys); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("a changed index: %v", err)
	}
	both, _ := ParsePublicKeys([]string{string(otherPub), string(pub)})
	if err := Verify(index, sig, both); err != nil {
		t.Fatalf("rotation: any trusted key must do: %v", err)
	}
}

func TestLoadIndexChecksSignatures(t *testing.T) {
	priv, pub, _ := GenerateKey()
	raw := []byte("apiVersion: kubepkg.dev/v1alpha1\nkind: RepositoryIndex\npackages: {}\n")
	sig, _ := Sign(raw, priv)
	f := Fetchers{"mem": memFetcher{"mem://r/index.yaml": raw, "mem://r/index.yaml.sig": sig, "mem://u/index.yaml": raw}}
	ctx := context.Background()
	if _, _, err := LoadIndex(ctx, f, "mem://r/index.yaml", []string{string(pub)}); err != nil {
		t.Fatalf("signed index: %v", err)
	}
	if _, _, err := LoadIndex(ctx, f, "mem://u/index.yaml", []string{string(pub)}); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("unsigned index with keys: %v", err)
	}
	if _, _, err := LoadIndex(ctx, f, "mem://u/index.yaml", nil); err != nil {
		t.Fatalf("unsigned index without keys: %v", err)
	}
	if _, _, err := LoadIndex(ctx, f, "mem://none/index.yaml", nil); !errors.Is(err, ErrFetchFailed) {
		t.Fatalf("missing index: %v", err)
	}
	f["mem"].(memFetcher)["mem://bad/index.yaml"] = []byte("kind: Something\n")
	if _, _, err := LoadIndex(ctx, f, "mem://bad/index.yaml", nil); !errors.Is(err, ErrInvalidIndex) {
		t.Fatalf("not an index: %v", err)
	}
}
