/*
Copyright 2025 The Cozystack Authors.
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

package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Condition reasons a Package reports on its Ready condition.
const (
	ReasonVersionMismatch       = "VersionMismatch"
	ReasonRequirementsNotMet    = "RequirementsNotMet"
	ReasonConflict              = "Conflict"
	ReasonCRDOwnershipConflict  = "CRDOwnershipConflict"
	ReasonUpgradeFailed         = "UpgradeFailed"
	ReasonUpgradeRolledBack     = "UpgradeRolledBack"
	ReasonPackageSourceNotFound = "PackageSourceNotFound"
	ReasonVariantNotFound       = "VariantNotFound"
	ReasonApplied               = "ReconciliationSucceeded"
	ReasonProgressing           = "Progressing"
)

// CRD policies applied when a package is removed.
const (
	CRDPolicyRetain = "Retain"
	CRDPolicyDelete = "Delete"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName={pkg,pkgs}
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Variant",type="string",JSONPath=".spec.variant",description="Selected variant"
// +kubebuilder:printcolumn:name="Version",type="string",JSONPath=".status.version",description="Applied version"
// +kubebuilder:printcolumn:name="Revision",type="integer",JSONPath=".status.currentRevision",description="Current revision"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status",description="Ready status"
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].message",description="Ready message"

// Package is the desired state of one installed package. The name is the
// package name and matches its PackageSource.
type Package struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PackageSpec   `json:"spec,omitempty"`
	Status PackageStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PackageList contains a list of Packages
type PackageList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Package `json:"items"`
}

// PackageSpec defines the desired state of Package
type PackageSpec struct {
	// Variant is the name of the variant to use from the PackageSource
	// If not specified, defaults to "default"
	// +optional
	Variant string `json:"variant,omitempty"`

	// Version is a semver constraint the PackageSource version must satisfy,
	// e.g. "~1.16". Empty accepts any version.
	// +optional
	Version string `json:"version,omitempty"`

	// Repository restricts version selection to the named Repository. Empty
	// considers all of them, by priority. Ignored when a PackageSource for
	// the package was written by hand.
	// +optional
	Repository string `json:"repository,omitempty"`

	// IgnoreDependencies is a list of package source dependencies to ignore
	// Dependencies listed here will not be installed even if they are specified in the PackageSource
	// +optional
	IgnoreDependencies []string `json:"ignoreDependencies,omitempty"`

	// Components is a map of release name to component overrides
	// Allows overriding values and enabling/disabling specific components from the PackageSource
	// +optional
	Components map[string]PackageComponent `json:"components,omitempty"`

	// Upgrade controls how changes are applied.
	// +optional
	Upgrade *UpgradePolicy `json:"upgrade,omitempty"`

	// RevisionHistoryLimit is how many PackageRevisions to keep. Default 10.
	// +optional
	// +kubebuilder:validation:Minimum=1
	RevisionHistoryLimit *int32 `json:"revisionHistoryLimit,omitempty"`

	// CRDPolicy decides what happens to owned CRDs when the package is
	// removed. Retain (default) keeps them; Delete removes them and every
	// object of their kinds.
	// +optional
	// +kubebuilder:validation:Enum=Retain;Delete
	CRDPolicy string `json:"crdPolicy,omitempty"`
}

// UpgradePolicy controls how a new revision is applied.
type UpgradePolicy struct {
	// Atomic rolls the whole package back when a revision fails, if the
	// version being left declares rollback as safe. Default true.
	// +optional
	Atomic *bool `json:"atomic,omitempty"`

	// Timeout for every component to become healthy. Default 10m.
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// PackageComponent defines overrides for a specific component
type PackageComponent struct {
	// Enabled indicates whether this component should be installed
	// If false, the component will be disabled even if it's defined in the PackageSource
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Values contains Helm chart values as a JSON object
	// These values will be merged with the default values from the PackageSource
	// +optional
	Values *apiextensionsv1.JSON `json:"values,omitempty"`
}

// PackageStatus defines the observed state of Package
type PackageStatus struct {
	// Conditions represents the latest available observations of a Package's state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Dependencies tracks the readiness status of each dependency
	// Key is the dependency package name, value indicates if the dependency is ready
	// +optional
	Dependencies map[string]DependencyStatus `json:"dependencies,omitempty"`

	// Version is the package version currently applied.
	// +optional
	Version string `json:"version,omitempty"`

	// CurrentRevision is the revision currently applied.
	// +optional
	CurrentRevision int64 `json:"currentRevision,omitempty"`

	// History lists recent revisions, newest first.
	// +optional
	History []RevisionSummary `json:"history,omitempty"`
}

// DependencyStatus represents the readiness status of a dependency
type DependencyStatus struct {
	// Ready indicates whether the dependency is ready
	Ready bool `json:"ready"`
}

// RevisionSummary is a short record of one revision.
type RevisionSummary struct {
	Revision int64  `json:"revision"`
	Version  string `json:"version,omitempty"`
	Phase    string `json:"phase,omitempty"`
}
