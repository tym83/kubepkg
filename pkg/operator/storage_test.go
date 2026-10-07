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
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/tym83/kubepkg/api/v1"
)

func TestObjectsMoveToTheStorageVersion(t *testing.T) {
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(crdGVK)
	crd.SetName("packages.kubepkg.dev")
	_ = unstructured.SetNestedStringSlice(crd.Object, []string{"v1alpha1", "v1beta1", "v1"}, "status", "storedVersions")
	untouched := crd.DeepCopy()
	untouched.SetName("repositories.kubepkg.dev")
	_ = unstructured.SetNestedStringSlice(untouched.Object, []string{"v1"}, "status", "storedVersions")
	sch := runtime.NewScheme()
	if err := v1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	pkg := &v1.Package{}
	pkg.Name = "cert-manager"
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(crd, untouched, pkg).WithStatusSubresource(crd).Build()
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pkg), pkg); err != nil {
		t.Fatal(err)
	}
	before := pkg.ResourceVersion
	m := &storageMigration{Reader: c, Writer: c, Group: "kubepkg.dev", Version: "v1", Kinds: map[string]string{"packages": "Package", "repositories": "Repository"}}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pkg), pkg); err != nil {
		t.Fatal(err)
	}
	if pkg.ResourceVersion == before {
		t.Error("the package was not rewritten")
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(crdGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Name: "packages.kubepkg.dev"}, got); err != nil {
		t.Fatal(err)
	}
	if stored, _, _ := unstructured.NestedStringSlice(got.Object, "status", "storedVersions"); len(stored) != 1 || stored[0] != "v1" {
		t.Errorf("stored versions: %v", stored)
	}
}
