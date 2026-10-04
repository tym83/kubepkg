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

package resolve

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func rel(name, version string, opts ...func(*Release)) Release {
	r := Release{Name: name, Version: version}
	for _, o := range opts {
		o(&r)
	}
	return r
}

func needsPkg(name, constraint string) func(*Release) {
	return func(r *Release) { r.Requires = append(r.Requires, Requirement{Package: name, Version: constraint}) }
}

func needsCap(c string) func(*Release) {
	return func(r *Release) { r.Requires = append(r.Requires, Requirement{Capability: c}) }
}

func gives(c ...string) func(*Release) {
	return func(r *Release) { r.Provides = append(r.Provides, c...) }
}

func fights(c ...string) func(*Release) {
	return func(r *Release) { r.Conflicts = append(r.Conflicts, c...) }
}

func installed(rs ...Release) State {
	st := State{Packages: map[string]Installed{}, APIs: map[string]bool{}}
	for _, r := range rs {
		st.Packages[r.Name] = Installed{Release: r, Ready: true}
	}
	return st
}

func summary(p Plan) string {
	var parts []string
	for _, c := range p.Changes {
		parts = append(parts, fmt.Sprintf("%s:%s:%s", c.Name, c.Action, c.To))
	}
	return strings.Join(parts, " ")
}

func TestSatisfies(t *testing.T) {
	cases := []struct {
		v, c string
		want bool
	}{
		{"1.16.2", "", true},
		{"1.16.2", "~1.16", true},
		{"1.17.0", "~1.16", false},
		{"2.0.0", ">=1.2 <2", false},
		{Unversioned, "", true},
		{Unversioned, ">=0.0.0", false},
		{"", ">=1", false},
	}
	for _, c := range cases {
		got, err := Satisfies(c.v, c.c)
		if err != nil || got != c.want {
			t.Errorf("Satisfies(%q,%q) = %v, %v; want %v", c.v, c.c, got, err, c.want)
		}
	}
	if _, err := Satisfies("1.0.0", ">>nonsense"); err == nil {
		t.Error("invalid constraint must be an error, not a silent false")
	}
}

func TestResolvePicksHighestAndPullsDependencies(t *testing.T) {
	cat := Catalog{
		"app":          {rel("app", "1.0.0", needsPkg("cert-manager", ">=1.15")), rel("app", "1.1.0", needsPkg("cert-manager", ">=1.16"))},
		"cert-manager": {rel("cert-manager", "1.15.0"), rel("cert-manager", "1.16.2"), rel("cert-manager", "1.17.0")},
	}
	p, err := Resolve(cat, installed(), []Request{{Name: "app"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := summary(p); got != "app:install:1.1.0 cert-manager:install:1.17.0" {
		t.Fatalf("plan = %s", got)
	}
	if p.Changes[1].Reason != "required by app 1.1.0" {
		t.Fatalf("reason = %q", p.Changes[1].Reason)
	}
}

func TestResolveKeepsInstalledDependency(t *testing.T) {
	// Installing an app must not upgrade a dependency that still satisfies it.
	cat := Catalog{
		"app":          {rel("app", "1.0.0", needsPkg("cert-manager", ">=1.15"))},
		"cert-manager": {rel("cert-manager", "1.15.0"), rel("cert-manager", "1.17.0")},
	}
	p, err := Resolve(cat, installed(rel("cert-manager", "1.15.0")), []Request{{Name: "app"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := summary(p); got != "app:install:1.0.0 cert-manager:keep:1.15.0" {
		t.Fatalf("plan = %s", got)
	}
}

func TestResolveUpgradesRequestedPackage(t *testing.T) {
	cat := Catalog{"cert-manager": {rel("cert-manager", "1.15.0"), rel("cert-manager", "1.17.0")}}
	p, err := Resolve(cat, installed(rel("cert-manager", "1.15.0")), []Request{{Name: "cert-manager"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := summary(p); got != "cert-manager:upgrade:1.17.0" {
		t.Fatalf("plan = %s", got)
	}
}

func TestResolveBacktracksOnConstraint(t *testing.T) {
	// The newest app needs a cert-manager the user excluded; the solver
	// must fall back to the older app instead of failing.
	cat := Catalog{
		"app":          {rel("app", "1.0.0", needsPkg("cert-manager", "<1.17")), rel("app", "2.0.0", needsPkg("cert-manager", ">=1.17"))},
		"cert-manager": {rel("cert-manager", "1.16.0"), rel("cert-manager", "1.17.0")},
	}
	p, err := Resolve(cat, installed(), []Request{{Name: "cert-manager", Version: "<1.17"}, {Name: "app"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := summary(p); got != "app:install:1.0.0 cert-manager:install:1.16.0" {
		t.Fatalf("plan = %s", got)
	}
}

func TestResolveCapabilityPrefersInstalledProvider(t *testing.T) {
	cat := Catalog{
		"app":           {rel("app", "1.0.0", needsCap("ingress"))},
		"ingress-nginx": {rel("ingress-nginx", "4.0.0", gives("ingress"))},
		"traefik":       {rel("traefik", "3.0.0", gives("ingress"))},
	}
	p, err := Resolve(cat, installed(rel("traefik", "3.0.0", gives("ingress"))), []Request{{Name: "app"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := summary(p); got != "app:install:1.0.0" {
		t.Fatalf("an installed provider must be reused, plan = %s", got)
	}
}

func TestResolveCapabilityPullsProvider(t *testing.T) {
	cat := Catalog{
		"app":           {rel("app", "1.0.0", needsCap("ingress"))},
		"ingress-nginx": {rel("ingress-nginx", "4.0.0", gives("ingress"))},
	}
	p, err := Resolve(cat, installed(), []Request{{Name: "app"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := summary(p); got != "app:install:1.0.0 ingress-nginx:install:4.0.0" {
		t.Fatalf("plan = %s", got)
	}
}

func TestResolveAPICapabilityFromDiscovery(t *testing.T) {
	cat := Catalog{"app": {rel("app", "1.0.0", needsCap("api:cert-manager.io/v1"))}}
	st := installed()
	st.APIs["cert-manager.io/v1"] = true
	p, err := Resolve(cat, st, []Request{{Name: "app"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := summary(p); got != "app:install:1.0.0" {
		t.Fatalf("plan = %s", got)
	}
}

func TestResolveConflictPicksOtherProvider(t *testing.T) {
	cat := Catalog{
		"app":           {rel("app", "1.0.0", needsCap("ingress"))},
		"ingress-nginx": {rel("ingress-nginx", "4.0.0", gives("ingress"), fights("legacy-lb"))},
		"traefik":       {rel("traefik", "3.0.0", gives("ingress"))},
	}
	p, err := Resolve(cat, installed(rel("legacy-lb", "1.0.0")), []Request{{Name: "app"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := summary(p); got != "app:install:1.0.0 traefik:install:3.0.0" {
		t.Fatalf("plan = %s", got)
	}
}

func TestResolveExplainsFailure(t *testing.T) {
	cat := Catalog{
		"app":          {rel("app", "1.0.0", needsPkg("cert-manager", ">=2"))},
		"cert-manager": {rel("cert-manager", "1.17.0")},
	}
	_, err := Resolve(cat, installed(), []Request{{Name: "app"}})
	if !errors.Is(err, ErrUnresolvable) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "no release of cert-manager satisfies >=2 (from app 1.0.0)") {
		t.Fatalf("explanation missing: %v", err)
	}
}

func TestResolveCycleTerminates(t *testing.T) {
	cat := Catalog{
		"a": {rel("a", "1.0.0", needsPkg("b", ""))},
		"b": {rel("b", "1.0.0", needsPkg("a", ""))},
	}
	p, err := Resolve(cat, installed(), []Request{{Name: "a"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := summary(p); got != "a:install:1.0.0 b:install:1.0.0" {
		t.Fatalf("plan = %s", got)
	}
}

func TestCheck(t *testing.T) {
	st := installed(rel("cert-manager", "1.16.0"), rel("traefik", "3.0.0", gives("ingress")))
	st.Packages["slow"] = Installed{Release: rel("slow", "1.0.0", gives("queue")), Ready: false}
	st.APIs["monitoring.coreos.com/v1"] = true

	unmet, err := Check([]Requirement{
		{Package: "cert-manager", Version: "~1.16"},
		{Package: "cert-manager", Version: ">=1.17"},
		{Capability: "ingress"},
		{Capability: "api:monitoring.coreos.com/v1"},
		{Capability: "queue"},
		{Capability: "storage"},
		{Capability: "tracing", Optional: true},
		{Package: "missing"},
	}, st)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, u := range unmet {
		got = append(got, u.String())
	}
	want := []string{
		"package cert-manager >=1.17: installed version 1.16.0 does not satisfy >=1.17",
		"capability queue: provider not ready",
		"capability storage: no installed package provides it",
		"package missing: not installed",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unmet:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestConflictsBothDirections(t *testing.T) {
	st := installed(rel("legacy-lb", "1.0.0", fights("ingress")), rel("other", "1.0.0"))
	if got := Conflicts(rel("traefik", "3.0.0", gives("ingress")), st); strings.Join(got, ",") != "legacy-lb" {
		t.Fatalf("conflicts = %v", got)
	}
	if got := Conflicts(rel("traefik", "3.1.0", gives("ingress")), installed(rel("traefik", "3.0.0", fights("traefik")))); len(got) != 0 {
		t.Fatalf("an upgrade must not conflict with its own previous version: %v", got)
	}
}
