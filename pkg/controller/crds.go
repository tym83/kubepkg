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
	// Transfer gives a CRD from one package to another, keeping it from
	// being deleted when the old owner's release lets go of it.
	Transfer(ctx context.Context, crd, from, to string) error
	// Retain lets go of the CRDs pkg owns and no longer lists in keep,
	// marking them so that removing them from a release does not delete
	// them and their objects.
	Retain(ctx context.Context, pkg string, keep []string) error
	// Release drops pkg's ownership of every CRD it owns, deleting them
	// when del is true.
	Release(ctx context.Context, pkg string, del bool) error
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

// keepAnnotation stops Helm from deleting an object a release no longer
// renders, or a release being uninstalled.
const keepAnnotation = "helm.sh/resource-policy"

// owned lists the CRDs pkg owns.
func (m *MetadataCRDs) owned(ctx context.Context, pkg string) ([]metav1.PartialObjectMetadata, error) {
	list := &metav1.PartialObjectMetadataList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: crdGVK.Group, Version: crdGVK.Version, Kind: crdGVK.Kind + "List"})
	if err := m.Client.List(ctx, list); err != nil {
		return nil, err
	}
	var out []metav1.PartialObjectMetadata
	for _, o := range list.Items {
		if o.GetAnnotations()[AnnotationOwnedBy] == pkg {
			out = append(out, o)
		}
	}
	return out, nil
}

func (m *MetadataCRDs) annotate(ctx context.Context, o *metav1.PartialObjectMetadata, set map[string]string, drop ...string) error {
	patch := client.MergeFrom(o.DeepCopy())
	a := o.GetAnnotations()
	if a == nil {
		a = map[string]string{}
	}
	for k, v := range set {
		a[k] = v
	}
	for _, k := range drop {
		delete(a, k)
	}
	o.SetAnnotations(a)
	return m.Client.Patch(ctx, o, patch)
}

// Transfer implements CRDOwner.
func (m *MetadataCRDs) Transfer(ctx context.Context, crd, from, to string) error {
	return m.each(ctx, []string{crd}, func(o *metav1.PartialObjectMetadata) error {
		if o.GetAnnotations()[AnnotationOwnedBy] != from {
			return nil
		}
		return m.annotate(ctx, o, map[string]string{keepAnnotation: "keep", AnnotationOwnedBy: to})
	})
}

// Retain implements CRDOwner.
func (m *MetadataCRDs) Retain(ctx context.Context, pkg string, keep []string) error {
	listed := map[string]bool{}
	for _, k := range keep {
		listed[k] = true
	}
	owned, err := m.owned(ctx, pkg)
	if err != nil {
		return err
	}
	for i := range owned {
		if listed[owned[i].Name] {
			continue
		}
		if err := m.annotate(ctx, &owned[i], map[string]string{keepAnnotation: "keep"}, AnnotationOwnedBy); err != nil {
			return err
		}
	}
	return nil
}

// Release implements CRDOwner.
func (m *MetadataCRDs) Release(ctx context.Context, pkg string, del bool) error {
	owned, err := m.owned(ctx, pkg)
	if err != nil {
		return err
	}
	for i := range owned {
		o := &owned[i]
		o.SetGroupVersionKind(crdGVK)
		if del {
			if err := client.IgnoreNotFound(m.Client.Delete(ctx, o)); err != nil {
				return err
			}
			continue
		}
		if err := m.annotate(ctx, o, nil, AnnotationOwnedBy); err != nil {
			return err
		}
	}
	return nil
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
