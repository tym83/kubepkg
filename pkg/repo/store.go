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
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/kuberoot-dev/kubepkg/pkg/resolve"
	"github.com/kuberoot-dev/kubepkg/pkg/source"
)

// maxIndexBytes bounds a fetched repository index.
const maxIndexBytes = 64 << 20

// IndexFetcher downloads a repository index.
type IndexFetcher interface {
	FetchIndex(ctx context.Context, url string) ([]byte, error)
}

// HTTPFetcher fetches indexes over HTTP(S).
type HTTPFetcher struct{}

// FetchIndex implements IndexFetcher.
func (HTTPFetcher) FetchIndex(ctx context.Context, u string) ([]byte, error) {
	return source.Download(ctx, u, maxIndexBytes)
}

// Fetchers pick an IndexFetcher by URL scheme. A distribution adds its own
// schemes, e.g. for an internal artifact store.
type Fetchers map[string]IndexFetcher

// DefaultFetchers handle http:// and https://.
func DefaultFetchers() Fetchers {
	return Fetchers{"http": HTTPFetcher{}, "https": HTTPFetcher{}}
}

// Fetch downloads the index at u with the fetcher for its scheme.
func (f Fetchers) Fetch(ctx context.Context, u string) ([]byte, error) {
	parsed, err := url.Parse(u)
	if err != nil {
		return nil, err
	}
	fetcher, ok := f[parsed.Scheme]
	if !ok {
		return nil, fmt.Errorf("no index fetcher for %s:// URLs", parsed.Scheme)
	}
	return fetcher.FetchIndex(ctx, u)
}

// Policy decides which indexes and versions the operator accepts. It is
// where a distribution checks signatures, allowed registries and the like.
type Policy interface {
	// AdmitIndex runs on every fetched index before it is used; raw is the
	// index as fetched.
	AdmitIndex(ctx context.Context, repository string, raw []byte, idx *Index) error
	// AdmitVersion runs on a version before it is selected for install.
	AdmitVersion(ctx context.Context, repository, pkg string, v Version) error
}

// AllowAll is the default policy.
type AllowAll struct{}

// AdmitIndex implements Policy.
func (AllowAll) AdmitIndex(context.Context, string, []byte, *Index) error { return nil }

// AdmitVersion implements Policy.
func (AllowAll) AdmitVersion(context.Context, string, string, Version) error { return nil }

// ErrNoVersion is returned when no repository offers an acceptable version.
var ErrNoVersion = errors.New("no repository offers an acceptable version")

// Store holds the accepted index of each repository.
type Store struct {
	mu    sync.RWMutex
	repos map[string]stored
	// attempted holds repositories fetched at least once, successfully or
	// not, so selection can wait until every repository had its chance.
	attempted map[string]bool
}

type stored struct {
	priority int32
	idx      *Index
}

// NewStore returns an empty store.
func NewStore() *Store { return &Store{repos: map[string]stored{}, attempted: map[string]bool{}} }

// Set records the index of a repository.
func (s *Store) Set(name string, priority int32, idx *Index) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repos[name] = stored{priority: priority, idx: idx}
	s.attempted[name] = true
}

// Failed records a failed fetch. An index loaded earlier stays in use.
func (s *Store) Failed(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempted[name] = true
}

// Attempted reports whether a repository was fetched at least once.
func (s *Store) Attempted(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.attempted[name]
}

// Delete forgets a repository.
func (s *Store) Delete(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.repos, name)
	delete(s.attempted, name)
}

// Selection is the version chosen for a package.
type Selection struct {
	Repository string
	Version    Version
}

// Offered returns the repository a package is taken from and its
// versions, newest first. The highest priority repository that has the
// package at all shadows the others (ties by name): a package a vendor
// repository carries never silently comes from a community one. only
// restricts the choice to one repository.
func (s *Store) Offered(pkg, only string) (string, []Version, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	best, found := "", false
	for n, r := range s.repos {
		if only != "" && n != only {
			continue
		}
		if _, ok := r.idx.Packages[pkg]; !ok {
			continue
		}
		if !found || r.priority > s.repos[best].priority || (r.priority == s.repos[best].priority && n < best) {
			best, found = n, true
		}
	}
	if !found {
		return "", nil, false
	}
	versions := append([]Version(nil), s.repos[best].idx.Packages[pkg].Versions...)
	sort.SliceStable(versions, func(i, j int) bool { return Newer(versions[i], versions[j]) })
	return best, versions, true
}

// Packages lists every package name in the store, sorted.
func (s *Store) Packages() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := map[string]bool{}
	for _, r := range s.repos {
		for n := range r.idx.Packages {
			seen[n] = true
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Describe returns the description and home of a package from the
// repository it is taken from.
func (s *Store) Describe(pkg string) (description, home string) {
	repo, _, ok := s.Offered(pkg, "")
	if !ok {
		return "", ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	p := s.repos[repo].idx.Packages[pkg]
	return p.Description, p.Home
}

// Select picks the newest version and build of pkg matching constraint
// from the repository Offered names. policy may refuse a version, and the
// next one is tried.
func (s *Store) Select(ctx context.Context, pkg, constraint, only string, policy Policy) (Selection, error) {
	repo, versions, ok := s.Offered(pkg, only)
	if !ok {
		where := "no loaded repository"
		if only != "" {
			where = "repository " + only + " (or it is not loaded)"
		}
		return Selection{}, fmt.Errorf("%w: %s has no package %s", ErrNoVersion, where, pkg)
	}
	var why []string
	for _, v := range versions {
		ok, err := resolve.Satisfies(v.Version, constraint)
		if err != nil {
			return Selection{}, err
		}
		if !ok {
			continue
		}
		if err := policy.AdmitVersion(ctx, repo, pkg, v); err != nil {
			why = append(why, fmt.Sprintf("%s build %d refused: %v", v.Version, v.Build, err))
			continue
		}
		return Selection{Repository: repo, Version: v}, nil
	}
	c := constraint
	if c == "" {
		c = "any version"
	}
	why = append([]string{fmt.Sprintf("repository %s has no %s matching %s", repo, pkg, c)}, why...)
	return Selection{}, fmt.Errorf("%w: %s", ErrNoVersion, strings.Join(why, "; "))
}

// Catalog lists, for every package, the releases of the given variant
// from the repository it is taken from and that policy admits, so the
// resolver sees exactly the versions the operator will select from. Of
// several builds of one version only the newest admitted one is listed.
func (s *Store) Catalog(ctx context.Context, variant string, policy Policy) resolve.Catalog {
	cat := resolve.Catalog{}
	for _, name := range s.Packages() {
		repo, versions, _ := s.Offered(name, "")
		seen := map[string]bool{}
		for i := range versions {
			v := &versions[i]
			if seen[v.Version] || policy.AdmitVersion(ctx, repo, name, *v) != nil {
				continue
			}
			seen[v.Version] = true
			cat[name] = append(cat[name], resolve.ReleaseOf(name, &v.Spec, variant))
		}
	}
	return cat
}
