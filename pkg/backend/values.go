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

package backend

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	"github.com/tym83/kubepkg/pkg/source"
)

// ResolveValues layers the component's values over the values.yaml of each
// ValuesFromSecrets Secret, named namespace/name or, like Flux valuesFrom,
// by name alone in the release namespace. A missing Secret is an error:
// installing without platform settings would produce a release that looks
// healthy and is configured wrong. Backends whose installer cannot read
// Secrets itself pass the result on as plain values.
func ResolveValues(ctx context.Context, secrets kubernetes.Interface, c Component) (map[string]any, error) {
	out := map[string]any{}
	for _, ref := range c.ValuesFromSecrets {
		ns, name, ok := strings.Cut(ref, "/")
		if !ok {
			ns, name = c.Namespace, ref
		}
		sec, err := secrets.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("values secret %s/%s: %w", ns, name, err)
		}
		var v map[string]any
		if err := yaml.Unmarshal(sec.Data["values.yaml"], &v); err != nil {
			return nil, fmt.Errorf("values secret %s/%s: %w", ns, name, err)
		}
		out = source.MergeValues(out, v)
	}
	return source.MergeValues(out, c.Values), nil
}
