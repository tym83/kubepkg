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
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// registrySecrets reads the Docker configs of the named Secrets. A Secret
// that does not exist means no credentials, with a warning once until it
// appears: a distribution can then pass the flag by default and leave
// creating the Secret to whoever needs it, and public registries keep
// working without it.
func registrySecrets(cs kubernetes.Interface, refs []string, log logr.Logger) func(context.Context) ([][]byte, error) {
	var mu sync.Mutex
	warned := map[string]bool{}
	return func(ctx context.Context) ([][]byte, error) {
		var out [][]byte
		for _, ref := range refs {
			ns, name, ok := strings.Cut(ref, "/")
			if !ok {
				return nil, fmt.Errorf("--registry-secret %q: want namespace/name", ref)
			}
			sec, err := cs.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
			mu.Lock()
			if apierrors.IsNotFound(err) {
				if !warned[ref] {
					log.Info("registry credentials Secret not found; pulling without it until it exists", "secret", ref)
					warned[ref] = true
				}
				mu.Unlock()
				continue
			}
			delete(warned, ref)
			mu.Unlock()
			if err != nil {
				return nil, fmt.Errorf("secret %s: %w", ref, err)
			}
			raw, ok := sec.Data[corev1.DockerConfigJsonKey]
			if !ok {
				return nil, fmt.Errorf("secret %s has no %s", ref, corev1.DockerConfigJsonKey)
			}
			out = append(out, raw)
		}
		return out, nil
	}
}
