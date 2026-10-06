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

// Package images finds the container images manifests run, normalises
// their references and pins them by digest.
package images

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/distribution/reference"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"
	"sigs.k8s.io/yaml"
)

// Normalize returns the full form of an image reference:
// docker.io/library/nginx:latest for nginx. A digest is kept, and a tag
// defaults to latest only when there is no digest.
func Normalize(ref string) (string, error) {
	named, err := reference.ParseNormalizedNamed(strings.TrimSpace(ref))
	if err != nil {
		return "", fmt.Errorf("image %q: %w", ref, err)
	}
	if _, ok := named.(reference.Digested); !ok {
		named = reference.TagNameOnly(named)
	}
	return named.String(), nil
}

// Split returns the repository (registry/path), the tag and the digest
// of a normalised reference.
func Split(ref string) (repo, tag, digest string) {
	if i := strings.Index(ref, "@"); i >= 0 {
		ref, digest = ref[:i], ref[i+1:]
	}
	slash := strings.LastIndex(ref, "/")
	if i := strings.LastIndex(ref, ":"); i > slash {
		ref, tag = ref[:i], ref[i+1:]
	}
	return ref, tag, digest
}

// Pinned reports whether a reference carries a digest.
func Pinned(ref string) bool {
	_, _, d := Split(ref)
	return d != ""
}

// Covered reports whether image, as a chart renders it, is one of the
// pinned images: the same repository and digest, or the same repository
// and tag.
func Covered(image string, pinned []string) bool {
	repo, tag, digest := Split(image)
	for _, p := range pinned {
		pr, pt, pd := Split(p)
		if pr != repo {
			continue
		}
		if (digest != "" && digest == pd) || (digest == "" && tag != "" && tag == pt) {
			return true
		}
	}
	return false
}

// containerKeys are the fields that hold containers, in any object: pods,
// pod templates of workloads and of custom resources.
var containerKeys = map[string]bool{"containers": true, "initContainers": true, "ephemeralContainers": true}

var docSeparator = regexp.MustCompile(`(?m)^---[ \t]*(#.*)?$`)

// FromManifests lists, normalised and sorted, the images that containers
// in the given YAML manifests run.
func FromManifests(docs ...string) ([]string, error) {
	seen := map[string]bool{}
	for _, text := range docs {
		for _, doc := range docSeparator.Split(text, -1) {
			var obj any
			if err := yaml.Unmarshal([]byte(doc), &obj); err != nil || obj == nil {
				continue
			}
			var walkErr error
			walk(obj, func(img string) {
				n, err := Normalize(img)
				if err != nil {
					walkErr = err
					return
				}
				seen[n] = true
			})
			if walkErr != nil {
				return nil, walkErr
			}
		}
	}
	out := make([]string, 0, len(seen))
	for i := range seen {
		out = append(out, i)
	}
	sort.Strings(out)
	return out, nil
}

func walk(v any, found func(string)) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if list, ok := child.([]any); ok && containerKeys[k] {
				for _, c := range list {
					if m, ok := c.(map[string]any); ok {
						if img, ok := m["image"].(string); ok && img != "" {
							found(img)
						}
					}
				}
			}
			walk(child, found)
		}
	case []any:
		for _, child := range t {
			walk(child, found)
		}
	}
}

// Resolver looks up the digests of image tags.
type Resolver struct {
	// CredentialsFile is a Docker config with registry credentials;
	// empty means anonymous access.
	CredentialsFile string
	// PlainHTTP talks to registries without TLS (test registries only).
	PlainHTTP bool
}

// Pin returns ref with the digest its tag points at now; a reference that
// already has a digest is returned as it is.
func (r Resolver) Pin(ctx context.Context, ref string) (string, error) {
	n, err := Normalize(ref)
	if err != nil {
		return "", err
	}
	repoName, tag, digest := Split(n)
	if digest != "" {
		return n, nil
	}
	repo, err := r.Repository(repoName)
	if err != nil {
		return "", err
	}
	desc, err := repo.Resolve(ctx, tag)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", n, err)
	}
	return n + "@" + desc.Digest.String(), nil
}

// Repository opens an image repository, registry/path, mapping docker.io
// to its registry host.
func (r Resolver) Repository(name string) (*remote.Repository, error) {
	if rest, ok := strings.CutPrefix(name, "docker.io/"); ok {
		name = "registry-1.docker.io/" + rest
	}
	repo, err := remote.NewRepository(name)
	if err != nil {
		return nil, err
	}
	repo.PlainHTTP = r.PlainHTTP
	client := &auth.Client{Client: retry.DefaultClient, Cache: auth.NewCache()}
	if r.CredentialsFile != "" {
		store, err := credentials.NewStore(r.CredentialsFile, credentials.StoreOptions{})
		if err != nil {
			return nil, fmt.Errorf("load registry credentials: %w", err)
		}
		client.Credential = credentials.Credential(store)
	}
	repo.Client = client
	return repo, nil
}
