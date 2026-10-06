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

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/tym83/kubepkg/api/v1beta1"
)

// readyWhenMet checks a component's readyWhen conditions. It returns the
// first condition not met, described for the Package status, or "".
func (r *PackageReconciler) readyWhenMet(ctx context.Context, conds []v1beta1.ReadyCondition, namespace string) (string, error) {
	for _, c := range conds {
		gv, err := schema.ParseGroupVersion(c.APIVersion)
		if err != nil {
			return "", fmt.Errorf("readyWhen %s %s: %w", c.Kind, c.Name, err)
		}
		gvk := gv.WithKind(c.Kind)
		want := c.Status
		if want == "" {
			want = "True"
		}
		what := fmt.Sprintf("%s %s condition %s=%s", c.Kind, c.Name, c.Condition, want)

		ns := c.Namespace
		if ns == "" {
			ns = namespace
		}
		mapping, err := r.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
		switch {
		case meta.IsNoMatchError(err):
			return "waiting for " + what + " (its kind is not served yet)", nil
		case err != nil:
			return "", err
		case mapping.Scope.Name() == meta.RESTScopeNameRoot:
			ns = ""
		}

		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: c.Name}, u); err != nil {
			if apierrors.IsNotFound(err) {
				return "waiting for " + what + " (not created yet)", nil
			}
			return "", err
		}
		conditions, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
		met := false
		for _, raw := range conditions {
			cond, ok := raw.(map[string]any)
			if ok && cond["type"] == c.Condition && fmt.Sprint(cond["status"]) == want {
				met = true
			}
		}
		if !met {
			return "waiting for " + what, nil
		}
	}
	return "", nil
}
