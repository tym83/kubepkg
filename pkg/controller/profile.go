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
	"github.com/tym83/kubepkg/api/v1alpha1"
)

// Profile holds the settings a platform changes to embed kubepkg. The
// zero value plus Group is a plain installation.
type Profile struct {
	// Group is the API group the resources are served under. A platform may
	// serve the kubepkg types under its own group.
	Group string
	// ValuesSecret, namespace/name, holds a values.yaml layered under every
	// component's values unless the PackageSource sets
	// AnnotationSkipPlatformValues. It lets a platform pass cluster-wide
	// settings to every package.
	ValuesSecret string
	// NamespaceLabels are put on every namespace the packages create.
	NamespaceLabels map[string]string
}

// Annotations kubepkg reads and writes.
const (
	// AnnotationOwnedBy marks a CRD with the package that owns it.
	AnnotationOwnedBy = "kubepkg.dev/owned-by"
	// AnnotationRollbackTo asks the operator to re-apply an earlier revision.
	AnnotationRollbackTo = "kubepkg.dev/rollback-to"
	// AnnotationRollbackRequested marks a revision restored because someone
	// asked for it, not because an upgrade failed.
	AnnotationRollbackRequested = "kubepkg.dev/rollback-requested"
	// AnnotationAdopt on a Package takes over what its components would
	// create but was installed another way: Helm releases of the same
	// name, and objects applied without Helm. It is removed once a
	// revision has been applied, so later changes never take over
	// unrelated objects.
	AnnotationAdopt = "kubepkg.dev/adopt"
	// AnnotationRetry changes the desired state without changing the spec,
	// so a failed revision is attempted again.
	AnnotationRetry = "kubepkg.dev/retry"
	// AnnotationDesiredDigest on a revision records the desired state it was
	// made from; while it matches, the operator does not act again.
	AnnotationDesiredDigest = "kubepkg.dev/desired-digest"
	// AnnotationSkipPlatformValues on a PackageSource keeps the platform
	// values secret out of its components.
	AnnotationSkipPlatformValues = "kubepkg.dev/skip-platform-values"
	// AnnotationValuesFiles on a release lists the values files it was
	// rendered with.
	AnnotationValuesFiles = "kubepkg.dev/values-files"
	// AnnotationHooksDone on a revision records that its pre-upgrade hooks
	// succeeded, so later passes do not run them again.
	AnnotationHooksDone = "kubepkg.dev/hooks-done"
	// FinalizerCleanup removes releases before the Package goes away.
	FinalizerCleanup = "kubepkg.dev/cleanup"
)

// DefaultProfile is a plain kubepkg installation.
func DefaultProfile() Profile {
	return Profile{Group: v1alpha1.GroupName}
}
