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
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"
)

// SignatureSuffix is appended to an index URL to find its signature.
const SignatureSuffix = ".sig"

// ErrBadSignature is returned when no trusted key signed an index.
var ErrBadSignature = errors.New("index is not signed by a trusted key")

// GenerateKey makes an ed25519 key pair, PEM encoded: the private key as
// PKCS #8, the public key as PKIX.
func GenerateKey() (private, public []byte, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), nil
}

// Sign signs the index bytes exactly as published. The signature is
// base64 text, served next to the index under SignatureSuffix. Because
// the index pins every chart by digest, it covers the whole repository.
func Sign(index, privatePEM []byte) ([]byte, error) {
	block, _ := pem.Decode(privatePEM)
	if block == nil {
		return nil, errors.New("signing key is not PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("signing key: %w", err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("signing key is not ed25519")
	}
	return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, index)) + "\n"), nil
}

// ParsePublicKeys reads PEM encoded ed25519 public keys.
func ParsePublicKeys(pems []string) ([]ed25519.PublicKey, error) {
	var out []ed25519.PublicKey
	for i, p := range pems {
		block, _ := pem.Decode([]byte(p))
		if block == nil {
			return nil, fmt.Errorf("public key %d is not PEM", i+1)
		}
		key, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("public key %d: %w", i+1, err)
		}
		pub, ok := key.(ed25519.PublicKey)
		if !ok {
			return nil, fmt.Errorf("public key %d is not ed25519", i+1)
		}
		out = append(out, pub)
	}
	return out, nil
}

// Verify checks that one of the keys signed the index bytes. Several keys
// let a repository rotate: trust the new key, then sign with it.
func Verify(index, signature []byte, keys []ed25519.PublicKey) error {
	// A signature file in the list form holds signatures by several keys;
	// any trusted one will do here.
	var list Signatures
	if yaml.Unmarshal(signature, &list) == nil && len(list.Signatures) > 0 {
		for _, s := range list.Signatures {
			raw, err := base64.StdEncoding.DecodeString(s.Sig)
			if err != nil {
				continue
			}
			for _, k := range keys {
				if ed25519.Verify(k, index, raw) {
					return nil
				}
			}
		}
		return ErrBadSignature
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(signature)))
	if err != nil {
		return fmt.Errorf("%w: signature is not base64", ErrBadSignature)
	}
	for _, k := range keys {
		if ed25519.Verify(k, index, sig) {
			return nil
		}
	}
	return ErrBadSignature
}
