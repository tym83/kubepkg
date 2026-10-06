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

package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Revision phases.
const (
	PhasePending    = "Pending"
	PhaseApplying   = "Applying"
	PhaseApplied    = "Applied"
	PhaseFailed     = "Failed"
	PhaseRolledBack = "RolledBack"
	PhaseSuperseded = "Superseded"
)

// LabelPackage marks objects that belong to a package.
const LabelPackage = "kubepkg.dev/package"

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName={pkgrev}
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Package",type="string",JSONPath=".spec.package"
// +kubebuilder:printcolumn:name="Revision",type="integer",JSONPath=".spec.revision"
// +kubebuilder:printcolumn:name="Version",type="string",JSONPath=".spec.version"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:validation:XValidation:rule="self.spec == oldSelf.spec",message="spec is immutable"

// PackageRevision is an immutable record of one applied state of a package.
// It is written by the operator.
type PackageRevision struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PackageRevisionSpec   `json:"spec,omitempty"`
	Status PackageRevisionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PackageRevisionList contains a list of PackageRevisions
type PackageRevisionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PackageRevision `json:"items"`
}

// PackageRevisionSpec is the recorded state.
type PackageRevisionSpec struct {
	Package  string `json:"package"`
	Revision int64  `json:"revision"`
	// +optional
	Version string `json:"version,omitempty"`
	// +optional
	Variant string `json:"variant,omitempty"`
	// RollbackSafe is copied from the PackageSource at the time of the revision.
	// +optional
	RollbackSafe bool `json:"rollbackSafe,omitempty"`
	// RestoredFrom is set when this revision re-applies an earlier one.
	// +optional
	RestoredFrom int64 `json:"restoredFrom,omitempty"`
	// +optional
	Components []ComponentSnapshot `json:"components,omitempty"`
}

// ComponentSnapshot is what one component was rendered from.
type ComponentSnapshot struct {
	Name        string `json:"name"`
	ReleaseName string `json:"releaseName"`
	Namespace   string `json:"namespace"`
	// +optional
	DependsOn []string `json:"dependsOn,omitempty"`
	// ChartDigest identifies the chart content that was applied.
	// +optional
	ChartDigest string `json:"chartDigest,omitempty"`
	// ValuesDigest identifies the values that were applied.
	// +optional
	ValuesDigest string `json:"valuesDigest,omitempty"`
}

// PackageRevisionStatus is the outcome of applying the revision.
type PackageRevisionStatus struct {
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	Components []ComponentRevisionStatus `json:"components,omitempty"`
}

// ComponentRevisionStatus links a component to the backend state it produced.
type ComponentRevisionStatus struct {
	Name string `json:"name"`
	// BackendRevision is e.g. the Helm release revision, used to roll back.
	// +optional
	BackendRevision int `json:"backendRevision,omitempty"`
}
