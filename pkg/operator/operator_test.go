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

package operator

import (
	"flag"
	"testing"
)

func TestBindFlags(t *testing.T) {
	o := DefaultOptions()
	o.Profile.NamespaceLabels = map[string]string{"preset": "kept"}
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	o.BindFlags(fs)
	err := fs.Parse([]string{"-api-group", "packages.example.com", "-namespace-label", "a=1", "-namespace-label", "b=2", "-values-secret", "sys/platform"})
	if err != nil {
		t.Fatal(err)
	}
	if o.Profile.Group != "packages.example.com" || o.Profile.ValuesSecret != "sys/platform" {
		t.Errorf("profile: %+v", o.Profile)
	}
	want := map[string]string{"preset": "kept", "a": "1", "b": "2"}
	if len(o.Profile.NamespaceLabels) != len(want) {
		t.Fatalf("labels %v, want %v", o.Profile.NamespaceLabels, want)
	}
	for k, v := range want {
		if o.Profile.NamespaceLabels[k] != v {
			t.Errorf("label %s=%q, want %q", k, o.Profile.NamespaceLabels[k], v)
		}
	}
	if err := fs.Parse([]string{"-namespace-label", "novalue"}); err == nil {
		t.Error("a label without = was accepted")
	}
}

func TestRunRejectsUnknownBackend(t *testing.T) {
	o := DefaultOptions()
	o.Backend = "nope"
	if err := Run(t.Context(), nil, o); err == nil {
		t.Fatal("unknown backend accepted")
	}
}
