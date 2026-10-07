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
	"github.com/fluxcd/pkg/apis/kustomize"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Source kinds a PackageSource can point at.
const (
	SourceKindGitRepository = "GitRepository"
	SourceKindOCIRepository = "OCIRepository"
	SourceKindOCIArtifact   = "OCIArtifact"
)

// UnversionedVersion is the version of a PackageSource that declares none.
// It satisfies only an empty constraint, so unversioned packages keep
// working while anything that asks for a version fails loudly.
const UnversionedVersion = "0.0.0-unversioned"

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName={pks}
// +kubebuilder:deprecatedversion:warning="kubepkg.dev/v1alpha1 is deprecated; use kubepkg.dev/v1"
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Version",type="string",JSONPath=".spec.version",description="Package version"
// +kubebuilder:printcolumn:name="Build",type="integer",JSONPath=".spec.build",description="Packaging build of that version"
// +kubebuilder:printcolumn:name="Repository",type="string",JSONPath=".metadata.labels.kubepkg\\.dev/repository",description="Repository the version was taken from; empty when written by hand"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// PackageSource is one package at one version: where its charts come from,
// what it provides and requires, and how its components are installed.
// The name is the package name.
type PackageSource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PackageSourceSpec   `json:"spec,omitempty"`
	Status PackageSourceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PackageSourceList contains a list of PackageSources
type PackageSourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PackageSource `json:"items"`
}

// PackageSourceSpec defines the desired state of PackageSource
type PackageSourceSpec struct {
	// Version is the semantic version of this package.
	// Empty means unversioned (see UnversionedVersion).
	// +optional
	Version string `json:"version,omitempty"`

	// Build numbers the packagings of one upstream version: a new patch or
	// default in the recipe makes a new build of the same version. Version
	// constraints apply to Version; of two equal versions the higher build
	// is newer.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Build int32 `json:"build,omitempty"`

	// SourceRef is the source reference for the package source charts
	// +optional
	SourceRef *PackageSourceRef `json:"sourceRef,omitempty"`

	// Provides lists capabilities this package offers, e.g. "ingress" or
	// "api:cert-manager.io/v1". The package name is always provided implicitly.
	// +optional
	Provides []string `json:"provides,omitempty"`

	// Conflicts lists package names or capabilities that must not be
	// installed alongside this package.
	// +optional
	Conflicts []string `json:"conflicts,omitempty"`

	// CRDs lists the names of CustomResourceDefinitions this package owns.
	// +optional
	CRDs []string `json:"crds,omitempty"`

	// Permissions declares what the package needs in the cluster. It is shown
	// in plans so the operator of the cluster sees it before installing; it is
	// not enforced yet.
	// +optional
	Permissions *Permissions `json:"permissions,omitempty"`

	// Rollback describes whether going back from this version is safe.
	// +optional
	Rollback *RollbackPolicy `json:"rollback,omitempty"`

	// Variants is a list of package source variants
	// Each variant defines components, applications, dependencies, and libraries for a specific configuration
	// +optional
	Variants []Variant `json:"variants,omitempty"`

	// Images are the container images the package runs, each pinned by
	// digest: registry/repository:tag@sha256:..., including those an
	// operator in the package deploys on its own. A signed index covers
	// them like the charts, and kubepkg bundle copies them for
	// air-gapped clusters.
	// +optional
	Images []string `json:"images,omitempty"`
}

// Permissions declares the access a package needs.
type Permissions struct {
	// ClusterWide is true when the package needs cluster-scoped access.
	// +optional
	ClusterWide bool `json:"clusterWide,omitempty"`

	// Rules are the RBAC rules the package's components need.
	// +optional
	Rules []rbacv1.PolicyRule `json:"rules,omitempty"`
}

// RollbackPolicy describes rollback safety of a package version.
type RollbackPolicy struct {
	// Safe is true when rolling back from this version to the previous one
	// does not lose data. Only then does a failed upgrade roll back
	// automatically.
	// +optional
	Safe bool `json:"safe,omitempty"`
}

// Variant defines a single variant configuration
type Variant struct {
	// Name is the unique identifier for this variant
	// +required
	Name string `json:"name"`

	// DependsOn is a list of package source dependencies
	// For example: "networking"
	// Equivalent to Requires entries with only Package set.
	// +optional
	DependsOn []string `json:"dependsOn,omitempty"`

	// Requires lists packages or capabilities this variant needs.
	// +optional
	Requires []Requirement `json:"requires,omitempty"`

	// Libraries is a list of Helm library charts used by components in this variant
	// +optional
	Libraries []Library `json:"libraries,omitempty"`

	// Components is a list of Helm releases to be installed as part of this variant
	// +optional
	Components []Component `json:"components,omitempty"`
}

// Requirement is a dependency on a package or a capability.
// +kubebuilder:validation:XValidation:rule="has(self.package) != has(self.capability)",message="exactly one of package or capability must be set"
type Requirement struct {
	// Package is the name of a required package.
	// +optional
	Package string `json:"package,omitempty"`

	// Capability is a required capability, e.g. "ingress" or
	// "api:cert-manager.io/v1".
	// +optional
	Capability string `json:"capability,omitempty"`

	// Version is a semver constraint on the required package, e.g. ">=1.2 <2".
	// Only meaningful with Package.
	// +optional
	Version string `json:"version,omitempty"`

	// Optional requirements only order installation: if present, they must
	// be ready first; if absent, the package proceeds.
	// +optional
	Optional bool `json:"optional,omitempty"`
}

// Library defines a Helm library chart
type Library struct {
	// Name is the optional name for library placed in charts
	// +optional
	Name string `json:"name,omitempty"`

	// Path is the path to the library chart directory
	// +required
	Path string `json:"path"`
}

// PackageSourceRef defines the source reference for package source charts
// +kubebuilder:validation:XValidation:rule="self.kind == 'OCIArtifact' ? has(self.url) : (has(self.name) && has(self.namespace))",message="OCIArtifact needs url; GitRepository and OCIRepository need name and namespace"
type PackageSourceRef struct {
	// Kind of the source reference. GitRepository and OCIRepository are Flux
	// sources and need the flux backend; OCIArtifact is fetched by kubepkg.
	// +kubebuilder:validation:Enum=GitRepository;OCIRepository;OCIArtifact
	// +required
	Kind string `json:"kind"`

	// Name of the source reference (Flux kinds)
	// +optional
	Name string `json:"name,omitempty"`

	// Namespace of the source reference (Flux kinds)
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// URL of an OCI artifact holding the package tree, e.g.
	// oci://ghcr.io/example/packages:1.0.0 (OCIArtifact only).
	// +optional
	URL string `json:"url,omitempty"`

	// Path is the base path where packages are located in the source.
	// For GitRepository, defaults to "packages" if not specified.
	// For OCIRepository and OCIArtifact, defaults to empty string (root) if not specified.
	// +optional
	Path string `json:"path,omitempty"`
}

// ComponentInstall defines installation parameters for a component
type ComponentInstall struct {
	// ReleaseName is the name of the HelmRelease resource that will be created
	// If not specified, defaults to the component Name field
	// +optional
	ReleaseName string `json:"releaseName,omitempty"`

	// Namespace is the Kubernetes namespace where the release will be installed
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Privileged indicates whether this release requires privileged access
	// +optional
	Privileged bool `json:"privileged,omitempty"`

	// DependsOn is a list of component names that must be installed before this component
	// +optional
	DependsOn []string `json:"dependsOn,omitempty"`

	// UpgradeCRDs controls how CRDs from the chart's crds/ directory are
	// handled on upgrades. Empty keeps the backend default (Skip).
	// Use "CreateReplace" for operators that evolve their CRD set between
	// versions. Warning: CreateReplace overwrites CRDs and may cause data
	// loss if upstream drops fields from a CRD with live objects.
	// +optional
	// +kubebuilder:validation:Enum=Skip;Create;CreateReplace
	UpgradeCRDs string `json:"upgradeCRDs,omitempty"`

	// WaitStrategy is one of poller|legacy (flux backend).
	// +optional
	// +kubebuilder:validation:Enum=poller;legacy
	WaitStrategy string `json:"waitStrategy,omitempty"`

	// HealthCheckExprs are CEL health expressions for the custom resource(s)
	// this component renders, so the component reports Ready only when the
	// resource is actually healthy.
	// +optional
	HealthCheckExprs []kustomize.CustomHealthCheck `json:"healthCheckExprs,omitempty"`

	// Phase PreUpgrade makes the component a hook: it runs only when the
	// package moves to another version, before every other component,
	// with values kubepkg.fromVersion and kubepkg.toVersion, typically a
	// Job that migrates data. A hook that fails stops the upgrade before
	// anything else changes; it is uninstalled once the upgrade succeeds,
	// so the next upgrade runs it afresh.
	// +optional
	// +kubebuilder:validation:Enum=PreUpgrade
	Phase string `json:"phase,omitempty"`

	// ReadyWhen lists object conditions that must hold before the
	// component counts as ready, for resources whose own readiness Helm
	// cannot see, such as an operator's custom resource reporting
	// Available. Every backend honours it; not meeting it within the
	// upgrade timeout fails the revision.
	// +optional
	ReadyWhen []ReadyCondition `json:"readyWhen,omitempty"`
}

// PhasePreUpgrade marks a component as a pre-upgrade hook.
const PhasePreUpgrade = "PreUpgrade"

// ReadyCondition is a condition an object must report.
type ReadyCondition struct {
	// +required
	APIVersion string `json:"apiVersion"`
	// +required
	Kind string `json:"kind"`
	// +required
	Name string `json:"name"`
	// Namespace defaults to the component's install namespace and is
	// ignored for cluster-scoped kinds.
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// Condition is the type in status.conditions, e.g. Available.
	// +required
	Condition string `json:"condition"`
	// Status is the wanted status of the condition. Default "True".
	// +optional
	Status string `json:"status,omitempty"`
}

// ChartRef points at one version of a published Helm chart.
type ChartRef struct {
	// Repository is an HTTP(S) Helm repository URL, or an oci:// path the
	// chart is pushed under, e.g. oci://ghcr.io/example/charts.
	// +required
	// +kubebuilder:validation:Pattern=`^(https?|oci)://`
	Repository string `json:"repository"`

	// Name is the chart name.
	// +required
	Name string `json:"name"`

	// Version is the exact chart version. Ranges are not accepted: a
	// package version must always install the same chart.
	// +required
	Version string `json:"version"`

	// Digest is the sha256 of the chart archive, sha256:<hex>. When set, a
	// chart that does not match is refused, so a repository that changes a
	// published version cannot change what gets installed.
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	Digest string `json:"digest,omitempty"`
}

// Component defines a single Helm release component within a package source
// +kubebuilder:validation:XValidation:rule="has(self.path) != has(self.chart)",message="set exactly one of path and chart"
// +kubebuilder:validation:XValidation:rule="!has(self.chart) || (!has(self.libraries) && !has(self.valuesFiles))",message="libraries and valuesFiles apply to charts in the package tree only"
type Component struct {
	// Name is the unique identifier for this component within the package source
	// +required
	Name string `json:"name"`

	// Path is the chart directory inside the package tree.
	// +optional
	Path string `json:"path,omitempty"`

	// Chart is a chart published in a Helm repository, used as is. A
	// package made only of such components needs no package tree.
	// +optional
	Chart *ChartRef `json:"chart,omitempty"`

	// Install defines installation parameters for this component
	// +optional
	Install *ComponentInstall `json:"install,omitempty"`

	// Libraries is a list of library names that this component depends on
	// These libraries must be defined at the variant level
	// +optional
	Libraries []string `json:"libraries,omitempty"`

	// ValuesFiles is a list of values file names to use
	// +optional
	ValuesFiles []string `json:"valuesFiles,omitempty"`
}

// PackageSourceStatus defines the observed state of PackageSource
type PackageSourceStatus struct {
	// Variants is a comma-separated list of package variant names
	// This field is populated by the controller based on spec.variants keys
	// +optional
	Variants string `json:"variants,omitempty"`

	// Conditions represents the latest available observations of a PackageSource's state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
