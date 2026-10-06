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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LabelPackageSet marks objects a PackageSet manages in a member cluster;
// the value is the PackageSet name. Objects without it are never touched.
const LabelPackageSet = "kubepkg.dev/package-set"

// SecretKeyRef points at one key of a Secret.
type SecretKeyRef struct {
	// +required
	Namespace string `json:"namespace"`
	// +required
	Name string `json:"name"`
	// Key defaults to "kubeconfig".
	// +optional
	Key string `json:"key,omitempty"`
}

// ClusterSpec says how a hub reaches a member cluster. The member runs
// kubepkg itself; the hub writes Repositories and Packages there.
type ClusterSpec struct {
	// KubeconfigSecretRef holds a kubeconfig for the member, with its
	// current context set.
	// +required
	KubeconfigSecretRef SecretKeyRef `json:"kubeconfigSecretRef"`
}

// ClusterStatus reports whether the member can be used.
type ClusterStatus struct {
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// KubernetesVersion of the member.
	// +optional
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Kubernetes",type="string",JSONPath=".status.kubernetesVersion"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].message"

// Cluster is a member cluster a hub installs packages into; its labels
// are what PackageSets select.
type Cluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterSpec   `json:"spec,omitempty"`
	Status ClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterList contains a list of Cluster.
type ClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Cluster `json:"items"`
}

// PackageSetPackage is one package a set installs.
type PackageSetPackage struct {
	// +required
	Name string `json:"name"`
	// Spec is the Package spec written to every selected cluster.
	// +optional
	Spec PackageSpec `json:"spec,omitempty"`
}

// PackageSetRepository is one repository a set subscribes members to.
type PackageSetRepository struct {
	// +required
	Name string `json:"name"`
	// +required
	Spec RepositorySpec `json:"spec"`
}

// PackageSetSpec says which packages go to which clusters.
type PackageSetSpec struct {
	// ClusterSelector picks Clusters by label; empty selects all.
	// +optional
	ClusterSelector metav1.LabelSelector `json:"clusterSelector,omitempty"`
	// Repositories are written to every selected cluster first.
	// +optional
	Repositories []PackageSetRepository `json:"repositories,omitempty"`
	// +optional
	Packages []PackageSetPackage `json:"packages,omitempty"`
}

// PackageSetClusterStatus is the set's state on one cluster.
type PackageSetClusterStatus struct {
	Name string `json:"name"`
	// Ready and Total count the set's packages on the cluster.
	Ready int32 `json:"ready"`
	Total int32 `json:"total"`
	// +optional
	Message string `json:"message,omitempty"`
}

// PackageSetStatus aggregates the set over its clusters.
type PackageSetStatus struct {
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	Clusters []PackageSetClusterStatus `json:"clusters,omitempty"`
	// ReadyClusters counts clusters where every package is ready.
	// +optional
	ReadyClusters int32 `json:"readyClusters,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName={pkgset}
// +kubebuilder:printcolumn:name="Clusters",type="integer",JSONPath=".status.readyClusters"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].message"

// PackageSet installs packages on every Cluster it selects.
type PackageSet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PackageSetSpec   `json:"spec,omitempty"`
	Status PackageSetStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PackageSetList contains a list of PackageSet.
type PackageSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PackageSet `json:"items"`
}
