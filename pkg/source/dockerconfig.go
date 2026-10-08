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
	"encoding/json"
	"fmt"
	"strings"

	"oras.land/oras-go/v2/registry/remote/auth"
)

// DockerConfigCredentials returns the credentials for a registry from
// Docker config files, as kubernetes.io/dockerconfigjson Secrets hold them.
// load is called on every lookup, so rotated credentials take effect
// without a restart.
func DockerConfigCredentials(load func(ctx context.Context) ([][]byte, error)) auth.CredentialFunc {
	return func(ctx context.Context, hostport string) (auth.Credential, error) {
		configs, err := load(ctx)
		if err != nil {
			return auth.EmptyCredential, fmt.Errorf("registry credentials: %w", err)
		}
		want := registryHost(hostport)
		for _, raw := range configs {
			var cfg struct {
				Auths map[string]struct {
					Auth          string `json:"auth"`
					Username      string `json:"username"`
					Password      string `json:"password"`
					IdentityToken string `json:"identitytoken"`
				} `json:"auths"`
			}
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return auth.EmptyCredential, fmt.Errorf("registry credentials: %w", err)
			}
			for key, a := range cfg.Auths {
				if registryHost(key) != want {
					continue
				}
				c := auth.Credential{Username: a.Username, Password: a.Password, RefreshToken: a.IdentityToken}
				if a.Auth != "" {
					dec, err := base64.StdEncoding.DecodeString(a.Auth)
					if err != nil {
						return auth.EmptyCredential, fmt.Errorf("registry credentials for %s: %w", key, err)
					}
					c.Username, c.Password, _ = strings.Cut(string(dec), ":")
				}
				return c, nil
			}
		}
		return auth.EmptyCredential, nil
	}
}

// registryHost reduces a Docker config key or a registry address to the
// host it names: https://index.docker.io/v1/ and registry-1.docker.io are
// both Docker Hub.
func registryHost(s string) string {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	s, _, _ = strings.Cut(s, "/")
	switch s {
	case "index.docker.io", "registry-1.docker.io", "docker.io":
		return "docker.io"
	}
	return s
}
