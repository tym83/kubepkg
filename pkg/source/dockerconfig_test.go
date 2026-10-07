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
	"context"
	"encoding/base64"
	"testing"

	"oras.land/oras-go/v2/registry/remote/auth"
)

func TestDockerConfigCredentials(t *testing.T) {
	configs := [][]byte{
		[]byte(`{"auths": {"https://index.docker.io/v1/": {"auth": "` + base64.StdEncoding.EncodeToString([]byte("hub:secret")) + `"}}}`),
		[]byte(`{"auths": {"ghcr.io": {"username": "bot", "password": "token"}, "registry.internal:5000": {"identitytoken": "refresh"}}}`),
	}
	creds := DockerConfigCredentials(func(context.Context) ([][]byte, error) { return configs, nil })
	for host, want := range map[string][2]string{
		"registry-1.docker.io":   {"hub", "secret"},
		"ghcr.io":                {"bot", "token"},
		"registry.internal:5000": {"", ""},
		"quay.io":                {"", ""},
	} {
		c, err := creds(context.Background(), host)
		if err != nil {
			t.Fatal(err)
		}
		if c.Username != want[0] || c.Password != want[1] {
			t.Errorf("%s: got %s/%s", host, c.Username, c.Password)
		}
	}
	if c, _ := creds(context.Background(), "registry.internal:5000"); c.RefreshToken != "refresh" {
		t.Error("identity token not taken")
	}
	// The file is the fallback only when the cluster has nothing.
	fromFile := func(context.Context, string) (auth.Credential, error) { return auth.Credential{Username: "file"}, nil }
	both := firstCredential(creds, fromFile)
	if c, _ := both(context.Background(), "ghcr.io"); c.Username != "bot" {
		t.Errorf("ghcr.io: %s; the Secret comes first", c.Username)
	}
	if c, _ := both(context.Background(), "quay.io"); c.Username != "file" {
		t.Errorf("quay.io: %q; the file is the fallback", c.Username)
	}
}
