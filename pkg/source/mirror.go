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
	"fmt"
	"regexp"
	"strings"
)

// A mirror is one OCI registry path that holds copies of everything a
// cluster installs, for clusters that cannot reach the places packages
// were published. Every location keeps its own place under the mirror:
//
//	oci://ghcr.io/org/packages/cdi      -> <mirror>/ghcr.io/org/packages/cdi
//	https://charts.jetstack.io          -> <mirror>/charts.jetstack.io
//
// so one mirror serves any number of upstreams, and copies keep their
// digests: pinned charts and trees verify exactly as from the original.

// MirrorPath is where a published location lives under a mirror.
func MirrorPath(location string) string {
	loc := location
	for _, scheme := range []string{"oci://", "https://", "http://"} {
		loc = strings.TrimPrefix(loc, scheme)
	}
	var parts []string
	for _, p := range strings.Split(strings.Trim(loc, "/"), "/") {
		if p = repoComponent(p); p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "/")
}

var invalidRepoChars = regexp.MustCompile(`[^a-z0-9._-]+`)

// repoComponent turns a path element, such as host:5000, into a valid OCI
// repository path component.
func repoComponent(s string) string {
	s = invalidRepoChars.ReplaceAllString(strings.ToLower(s), "-")
	return strings.Trim(s, "._-")
}

// MirrorChart returns where a published chart lives under mirror. The
// digest stays: the copy is the same archive.
func MirrorChart(mirror string, c Chart) Chart {
	if mirror == "" {
		return c
	}
	c.Repository = strings.TrimSuffix(mirror, "/") + "/" + MirrorPath(c.Repository)
	return c
}

// MirrorRef returns where an oci:// reference (tag, digest or both) lives
// under mirror.
func MirrorRef(mirror, ref string) (string, error) {
	if mirror == "" {
		return ref, nil
	}
	target, ok := strings.CutPrefix(ref, "oci://")
	if !ok {
		return "", fmt.Errorf("%q is not an oci:// reference", ref)
	}
	repo, suffix := splitReference(target)
	return strings.TrimSuffix(mirror, "/") + "/" + MirrorPath(repo) + suffix, nil
}

// splitReference splits host/path:tag@digest into host/path and the rest.
func splitReference(target string) (repo, suffix string) {
	if i := strings.Index(target, "@"); i >= 0 {
		repo, suffix = target[:i], target[i:]
	} else {
		repo = target
	}
	slash := strings.LastIndex(repo, "/")
	if i := strings.LastIndex(repo, ":"); i > slash {
		repo, suffix = repo[:i], repo[i:]+suffix
	}
	return repo, suffix
}
