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

package images

import (
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"nginx":           "docker.io/library/nginx:latest",
		"bitnami/redis:7": "docker.io/bitnami/redis:7",
		"quay.io/jetstack/cert-manager-controller:v1.21.2":     "quay.io/jetstack/cert-manager-controller:v1.21.2",
		"localhost:5000/app@sha256:" + strings.Repeat("a", 64): "localhost:5000/app@sha256:" + strings.Repeat("a", 64),
	} {
		if got, err := Normalize(in); err != nil || got != want {
			t.Errorf("%s -> %s (%v), want %s", in, got, err, want)
		}
	}
	if _, err := Normalize("bad name:1"); err == nil {
		t.Error("an invalid reference was accepted")
	}
}

func TestFromManifestsFindsEveryContainer(t *testing.T) {
	docs := `apiVersion: apps/v1
kind: Deployment
spec:
  template:
    spec:
      initContainers: [{name: init, image: busybox:1.36}]
      containers:
        - {name: app, image: "ghcr.io/org/app:1.0"}
        - {name: sidecar, image: ghcr.io/org/app:1.0}
---
apiVersion: kubevirt.io/v1
kind: VirtualMachine
spec:
  template:
    spec:
      domain: {}
      volumes: [{name: c, containerDisk: {image: not-a-container}}]
---
apiVersion: batch/v1
kind: CronJob
spec:
  jobTemplate:
    spec:
      template:
        spec:
          containers: [{name: job, image: "registry.k8s.io/kubectl:v1.35.0"}]
`
	got, err := FromManifests(docs)
	if err != nil {
		t.Fatal(err)
	}
	want := "docker.io/library/busybox:1.36 ghcr.io/org/app:1.0 registry.k8s.io/kubectl:v1.35.0"
	if strings.Join(got, " ") != want {
		t.Fatalf("got %v\nwant %s", got, want)
	}
}

func TestCovered(t *testing.T) {
	d := "sha256:" + strings.Repeat("b", 64)
	pinned := []string{"ghcr.io/org/app:1.0@" + d}
	if !Covered("ghcr.io/org/app:1.0", pinned) {
		t.Error("same repository and tag")
	}
	if !Covered("ghcr.io/org/app@"+d, pinned) {
		t.Error("same digest")
	}
	if Covered("ghcr.io/org/app:1.1", pinned) || Covered("ghcr.io/other/app:1.0", pinned) {
		t.Error("another tag or repository")
	}
}
