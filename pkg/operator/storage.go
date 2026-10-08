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
	"maps"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// storageMigration rewrites kubepkg's objects in the version the cluster
// stores and then records that only that version is stored, so an older
// API version can one day stop being served without losing objects
// written in it. The API server re-encodes an object on any update, even
// one that changes nothing.
type storageMigration struct {
	// Reader reads straight from the API server; Writer updates.
	Reader client.Reader
	Writer client.Client
	Group  string
	// Version is the storage version; Kinds by their plural resource.
	Version string
	Kinds   map[string]string
	// Retry is how long to wait before trying failed kinds again; 5m
	// when zero.
	Retry time.Duration
}

var crdGVK = schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}

// Start migrates, retrying what fails until everything is done: an
// operator upgraded before its CRDs, for one, cannot list in the new
// version until they arrive. The manager runs it on the leader.
func (m *storageMigration) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("storage-migration")
	retry := m.Retry
	if retry == 0 {
		retry = 5 * time.Minute
	}
	pending := maps.Clone(m.Kinds)
	for {
		for plural, kind := range pending {
			if err := m.migrate(ctx, plural, kind); err != nil {
				logger.Error(err, "objects not moved to the storage version yet; retrying", "resource", plural, "in", retry)
				continue
			}
			delete(pending, plural)
		}
		if len(pending) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(retry):
		}
	}
}

// NeedLeaderElection is true: one replica migrates.
func (m *storageMigration) NeedLeaderElection() bool { return true }

func (m *storageMigration) migrate(ctx context.Context, plural, kind string) error {
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(crdGVK)
	if err := m.Reader.Get(ctx, client.ObjectKey{Name: plural + "." + m.Group}, crd); err != nil {
		return client.IgnoreNotFound(err)
	}
	stored, _, _ := unstructured.NestedStringSlice(crd.Object, "status", "storedVersions")
	if len(stored) == 0 || slices.Equal(stored, []string{m.Version}) {
		return nil
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: m.Group, Version: m.Version, Kind: kind + "List"})
	if err := m.Reader.List(ctx, list); err != nil {
		return err
	}
	for i := range list.Items {
		key := client.ObjectKeyFromObject(&list.Items[i])
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(schema.GroupVersionKind{Group: m.Group, Version: m.Version, Kind: kind})
			if err := m.Reader.Get(ctx, key, obj); err != nil {
				return err
			}
			return m.Writer.Update(ctx, obj)
		})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("%s %s: %w", kind, key.Name, err)
		}
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := m.Reader.Get(ctx, client.ObjectKey{Name: plural + "." + m.Group}, crd); err != nil {
			return err
		}
		if err := unstructured.SetNestedStringSlice(crd.Object, []string{m.Version}, "status", "storedVersions"); err != nil {
			return err
		}
		return m.Writer.Status().Update(ctx, crd)
	})
}
