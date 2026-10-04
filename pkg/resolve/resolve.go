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

// Package resolve decides which package versions to install and whether a
// package's requirements and conflicts are satisfied.
//
// It deliberately is not a general SAT solver. Requirements are mostly on
// capabilities, constraints are expected to be loose, and a distribution pins
// exact versions; a solver that finds clever combinations nobody tested would
// be a liability. The search below takes the highest acceptable version of
// each package, prefers providers that are already installed, and backtracks
// only within that order.
package resolve

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// Unversioned is the version of a package that declares none.
const Unversioned = "0.0.0-unversioned"

// APIPrefix marks a capability that the API server can satisfy through
// discovery, e.g. "api:cert-manager.io/v1".
const APIPrefix = "api:"

// Requirement is a dependency on a package or a capability.
type Requirement struct {
	Package    string
	Capability string
	Version    string
	Optional   bool
}

func (r Requirement) String() string {
	if r.Package != "" {
		if r.Version != "" {
			return fmt.Sprintf("package %s %s", r.Package, r.Version)
		}
		return "package " + r.Package
	}
	return "capability " + r.Capability
}

// Release is one version of a package as published in a repository.
type Release struct {
	Name      string
	Version   string
	Provides  []string
	Requires  []Requirement
	Conflicts []string
}

// Installed is a package present in the cluster.
type Installed struct {
	Release
	Ready bool
}

// State is what the cluster has: installed packages and served APIs.
type State struct {
	Packages map[string]Installed
	// APIs holds served group versions, e.g. "cert-manager.io/v1".
	APIs map[string]bool
}

// Satisfies reports whether version meets constraint. An empty constraint
// accepts anything; the unversioned version satisfies only an empty one.
func Satisfies(version, constraint string) (bool, error) {
	if strings.TrimSpace(constraint) == "" {
		return true, nil
	}
	if version == "" || version == Unversioned {
		return false, nil
	}
	c, err := semver.NewConstraint(constraint)
	if err != nil {
		return false, fmt.Errorf("invalid version constraint %q: %w", constraint, err)
	}
	v, err := semver.NewVersion(version)
	if err != nil {
		return false, fmt.Errorf("invalid version %q: %w", version, err)
	}
	return c.Check(v), nil
}

// provides reports whether a release offers a capability. A package always
// provides its own name.
func provides(r Release, capability string) bool {
	if r.Name == capability {
		return true
	}
	for _, p := range r.Provides {
		if p == capability {
			return true
		}
	}
	return false
}

// Unmet is a requirement that is not satisfied, with the reason.
type Unmet struct {
	Requirement Requirement
	Reason      string
}

func (u Unmet) String() string { return u.Requirement.String() + ": " + u.Reason }

// Check evaluates requirements against the cluster. A requirement is met
// only by a Ready package, because the operator uses this to gate
// installation: something that exists but is not working yet does not count.
// Optional requirements are reported only when present but not ready.
func Check(reqs []Requirement, st State) ([]Unmet, error) {
	var out []Unmet
	for _, r := range reqs {
		if r.Package != "" {
			p, ok := st.Packages[r.Package]
			if !ok {
				if !r.Optional {
					out = append(out, Unmet{r, "not installed"})
				}
				continue
			}
			if !p.Ready {
				out = append(out, Unmet{r, "not ready"})
				continue
			}
			ok, err := Satisfies(p.Version, r.Version)
			if err != nil {
				return nil, err
			}
			if !ok {
				out = append(out, Unmet{r, fmt.Sprintf("installed version %s does not satisfy %s", p.Version, r.Version)})
			}
			continue
		}
		if strings.HasPrefix(r.Capability, APIPrefix) && st.APIs[strings.TrimPrefix(r.Capability, APIPrefix)] {
			continue
		}
		found, ready := false, false
		for _, name := range sortedKeys(st.Packages) {
			p := st.Packages[name]
			if provides(p.Release, r.Capability) {
				found = true
				if p.Ready {
					ready = true
					break
				}
			}
		}
		switch {
		case ready:
		case found:
			out = append(out, Unmet{r, "provider not ready"})
		case !r.Optional:
			out = append(out, Unmet{r, "no installed package provides it"})
		}
	}
	return out, nil
}

// Conflicts returns the installed packages that conflict with r, in either
// direction: r declares a conflict with them, or they declare one with r.
// The package itself is ignored, so an upgrade does not conflict with its
// previous version.
func Conflicts(r Release, st State) []string {
	var out []string
	for _, name := range sortedKeys(st.Packages) {
		if name == r.Name {
			continue
		}
		other := st.Packages[name].Release
		if clashes(r, other) || clashes(other, r) {
			out = append(out, name)
		}
	}
	return out
}

func clashes(a, b Release) bool {
	for _, c := range a.Conflicts {
		if provides(b, c) {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
