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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CRDOwner tracks which package owns which CRD.
type CRDOwner interface {
	// Conflict returns the owner and name of the first CRD in crds owned by
	// a package other than pkg.
	Conflict(ctx context.Context, pkg string, crds []string) (owner, crd string, err error)
	// Claim marks existing CRDs as owned by pkg.
	Claim(ctx context.Context, pkg string, crds []string) error
	// Release drops pkg's ownership, deleting the CRDs when del is true.
	Release(ctx context.Context, pkg string, crds []string, del bool) error
}

var crdGVK = schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}

// MetadataCRDs implements CRDOwner with metadata-only requests, so the
// operator never reads or rewrites a CRD schema it does not own.
type MetadataCRDs struct {
	Client client.Client
}

func (m *MetadataCRDs) get(ctx context.Context, name string) (*metav1.PartialObjectMetadata, error) {
	o := &metav1.PartialObjectMetadata{}
	o.SetGroupVersionKind(crdGVK)
	if err := m.Client.Get(ctx, types.NamespacedName{Name: name}, o); err != nil {
		return nil, err
	}
	return o, nil
}

// Conflict implements CRDOwner.
func (m *MetadataCRDs) Conflict(ctx context.Context, pkg string, crds []string) (string, string, error) {
	for _, n := range crds {
		o, err := m.get(ctx, n)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return "", "", err
		}
		if owner := o.GetAnnotations()[AnnotationOwnedBy]; owner != "" && owner != pkg {
			return owner, n, nil
		}
	}
	return "", "", nil
}

// Claim implements CRDOwner.
func (m *MetadataCRDs) Claim(ctx context.Context, pkg string, crds []string) error {
	return m.each(ctx, crds, func(o *metav1.PartialObjectMetadata) error {
		if o.GetAnnotations()[AnnotationOwnedBy] == pkg {
			return nil
		}
		patch := client.MergeFrom(o.DeepCopy())
		a := o.GetAnnotations()
		if a == nil {
			a = map[string]string{}
		}
		a[AnnotationOwnedBy] = pkg
		o.SetAnnotations(a)
		return m.Client.Patch(ctx, o, patch)
	})
}

// Release implements CRDOwner.
func (m *MetadataCRDs) Release(ctx context.Context, pkg string, crds []string, del bool) error {
	return m.each(ctx, crds, func(o *metav1.PartialObjectMetadata) error {
		if o.GetAnnotations()[AnnotationOwnedBy] != pkg {
			return nil // never touch a CRD another package owns
		}
		if del {
			return client.IgnoreNotFound(m.Client.Delete(ctx, o))
		}
		patch := client.MergeFrom(o.DeepCopy())
		a := o.GetAnnotations()
		delete(a, AnnotationOwnedBy)
		o.SetAnnotations(a)
		return m.Client.Patch(ctx, o, patch)
	})
}

func (m *MetadataCRDs) each(ctx context.Context, crds []string, fn func(*metav1.PartialObjectMetadata) error) error {
	for _, n := range crds {
		o, err := m.get(ctx, n)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if err := fn(o); err != nil {
			return err
		}
	}
	return nil
}
