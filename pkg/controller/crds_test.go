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
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/tym83/kubepkg/api/v1"
)

// withCRDs gives the env real CRD ownership over CRDs owned as given.
func withCRDs(t *testing.T, e *env, owners map[string]string) {
	t.Helper()
	if err := apiextensionsv1.AddToScheme(e.c.Scheme()); err != nil {
		t.Fatal(err)
	}
	for name, owner := range owners {
		crd := &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if owner != "" {
			crd.Annotations = map[string]string{AnnotationOwnedBy: owner}
		}
		if err := e.c.Create(context.Background(), crd); err != nil {
			t.Fatal(err)
		}
	}
	e.r.CRDs = &MetadataCRDs{Client: e.c}
}

func crdAnnotations(t *testing.T, c client.Client, name string) map[string]string {
	t.Helper()
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, crd); err != nil {
		t.Fatal(err)
	}
	return crd.Annotations
}

func TestCRDsRetainReleaseAndTransfer(t *testing.T) {
	e := newEnv(t)
	withCRDs(t, e, map[string]string{"a.example.org": "app", "b.example.org": "app", "c.example.org": "other", "d.example.org": ""})
	m := e.r.CRDs
	ctx := context.Background()

	if err := m.Retain(ctx, "app", []string{"a.example.org"}); err != nil {
		t.Fatal(err)
	}
	if a := crdAnnotations(t, e.c, "b.example.org"); a[AnnotationOwnedBy] != "" || a[keepAnnotation] != "keep" {
		t.Fatalf("a CRD the package stopped listing: %v", a)
	}
	if a := crdAnnotations(t, e.c, "a.example.org"); a[AnnotationOwnedBy] != "app" || a[keepAnnotation] != "" {
		t.Fatalf("a CRD the package still lists: %v", a)
	}

	if err := m.Transfer(ctx, "c.example.org", "app", "mine"); err != nil {
		t.Fatal(err)
	}
	if a := crdAnnotations(t, e.c, "c.example.org"); a[AnnotationOwnedBy] != "other" {
		t.Fatal("a transfer from the wrong owner changed the CRD")
	}
	if err := m.Transfer(ctx, "c.example.org", "other", "mine"); err != nil {
		t.Fatal(err)
	}
	if a := crdAnnotations(t, e.c, "c.example.org"); a[AnnotationOwnedBy] != "mine" || a[keepAnnotation] != "keep" {
		t.Fatalf("transferred: %v", a)
	}

	if err := m.Release(ctx, "app", false); err != nil {
		t.Fatal(err)
	}
	if a := crdAnnotations(t, e.c, "a.example.org"); a[AnnotationOwnedBy] != "" {
		t.Fatalf("release left the owner: %v", a)
	}
	if err := m.Release(ctx, "mine", true); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Get(ctx, types.NamespacedName{Name: "c.example.org"}, &apiextensionsv1.CustomResourceDefinition{}); !apierrors.IsNotFound(err) {
		t.Fatalf("release with delete kept the CRD: %v", err)
	}
	if a := crdAnnotations(t, e.c, "d.example.org"); a[AnnotationOwnedBy] != "" {
		t.Fatal("an unowned CRD was touched")
	}
}

// envoy-gateway 2 drops the Gateway API CRDs and requires gateway-api,
// which declares them: gateway-api must get them while envoy-gateway 1
// still runs, instead of each waiting for the other.
func TestACRDMovesToThePackageThatNowDeclaresIt(t *testing.T) {
	e := newEnv(t)
	withCRDs(t, e, map[string]string{"gateways.gateway.networking.k8s.io": "envoy-gateway"})
	gw := mkSource("gateway-api", "1.0.0", true, "crds")
	gw.Spec.CRDs = []string{"gateways.gateway.networking.k8s.io"}
	eg := mkSource("envoy-gateway", "1.0.0", true, "envoy")
	eg.Spec.CRDs = []string{"gateways.gateway.networking.k8s.io"}
	e.create(eg, gw, &v1.Package{ObjectMeta: metav1.ObjectMeta{Name: "gateway-api"}})

	// While envoy-gateway still declares them, they stay its own.
	e.reconcile("gateway-api")
	if c := meta.FindStatusCondition(e.pkg("gateway-api").Status.Conditions, "Ready"); c == nil || c.Reason != v1.ReasonCRDOwnershipConflict {
		t.Fatalf("while declared by envoy-gateway: %+v", c)
	}

	// envoy-gateway's chosen version stops declaring them.
	eg = &v1.PackageSource{}
	if err := e.c.Get(context.Background(), types.NamespacedName{Name: "envoy-gateway"}, eg); err != nil {
		t.Fatal(err)
	}
	eg.Spec.CRDs = nil
	if err := e.c.Update(context.Background(), eg); err != nil {
		t.Fatal(err)
	}
	e.reconcile("gateway-api")
	e.reconcile("gateway-api")
	if c := meta.FindStatusCondition(e.pkg("gateway-api").Status.Conditions, "Ready"); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("gateway-api did not get the CRDs: %+v", c)
	}
	if len(e.be.adopted) == 0 {
		t.Fatal("gateway-api did not take the CRD into its release")
	}
	if a := crdAnnotations(t, e.c, "gateways.gateway.networking.k8s.io"); a[AnnotationOwnedBy] != "gateway-api" || a[keepAnnotation] != "keep" {
		t.Fatalf("the CRD after the move: %v", a)
	}
}

func TestAVersionThatDropsACRDKeepsItAndDeletingThePackageReleasesIt(t *testing.T) {
	e := newEnv(t)
	withCRDs(t, e, map[string]string{"widgets.example.org": "", "gadgets.example.org": ""})
	src := mkSource("app", "1.0.0", true, "app")
	src.Spec.CRDs = []string{"widgets.example.org", "gadgets.example.org"}
	e.create(src, &v1.Package{ObjectMeta: metav1.ObjectMeta{Name: "app"}})
	e.reconcile("app")
	e.reconcile("app")
	if a := crdAnnotations(t, e.c, "gadgets.example.org"); a[AnnotationOwnedBy] != "app" {
		t.Fatalf("not claimed: %v", a)
	}

	// 1.1.0 no longer ships gadgets: its release would delete the CRD.
	cur := &v1.PackageSource{}
	if err := e.c.Get(context.Background(), types.NamespacedName{Name: "app"}, cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec.Version, cur.Spec.CRDs = "1.1.0", []string{"widgets.example.org"}
	if err := e.c.Update(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	e.reconcile("app")
	if a := crdAnnotations(t, e.c, "gadgets.example.org"); a[keepAnnotation] != "keep" || a[AnnotationOwnedBy] != "" {
		t.Fatalf("a dropped CRD must be kept and let go: %v", a)
	}

	// Deleting the package releases what it owns, even with its
	// PackageSource gone first.
	if err := e.c.Delete(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Delete(context.Background(), e.pkg("app")); err != nil {
		t.Fatal(err)
	}
	e.reconcile("app")
	if a := crdAnnotations(t, e.c, "widgets.example.org"); a[AnnotationOwnedBy] != "" || a[keepAnnotation] != "keep" {
		t.Fatalf("after deleting the package: %v", a)
	}
}
