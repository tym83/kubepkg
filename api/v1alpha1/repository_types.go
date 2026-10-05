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

// LabelRepository marks a PackageSource that kubepkg made from a
// repository index; the value is the repository name. PackageSources
// without it are written by hand and never touched.
const LabelRepository = "kubepkg.dev/repository"

// RepositorySpec says where a repository index is and how much to trust
// it relative to other repositories.
type RepositorySpec struct {
	// URL of the index, e.g. https://packages.example.org/index.yaml. The
	// scheme picks the fetcher; https:// and http:// are built in.
	// +required
	URL string `json:"url"`

	// Priority orders repositories that carry the same package: the
	// highest priority one shadows the others for that package, whatever
	// versions they have. Default 0.
	// +optional
	Priority int32 `json:"priority,omitempty"`

	// Interval between index refreshes. Default 10m.
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`

	// PublicKeys are PEM encoded ed25519 keys the index must be signed
	// with: the signature is fetched from the index URL plus ".sig". With
	// none, the index is not checked. Several keys allow rotation.
	// +optional
	PublicKeys []string `json:"publicKeys,omitempty"`
}

// RepositoryStatus reports the last index fetch.
type RepositoryStatus struct {
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Packages is the number of packages in the index.
	// +optional
	Packages int32 `json:"packages,omitempty"`
	// IndexDigest is the sha256 of the last accepted index.
	// +optional
	IndexDigest string `json:"indexDigest,omitempty"`
	// LastFetched is when the index was last fetched successfully.
	// +optional
	LastFetched *metav1.Time `json:"lastFetched,omitempty"`
	// IndexGenerated is when the accepted index was built; an older one is
	// refused.
	// +optional
	IndexGenerated *metav1.Time `json:"indexGenerated,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName={pkgrepo}
// +kubebuilder:printcolumn:name="URL",type="string",JSONPath=".spec.url"
// +kubebuilder:printcolumn:name="Priority",type="integer",JSONPath=".spec.priority"
// +kubebuilder:printcolumn:name="Packages",type="integer",JSONPath=".status.packages"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].message"

// Repository is a package repository the operator installs packages from.
type Repository struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RepositorySpec   `json:"spec,omitempty"`
	Status RepositoryStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RepositoryList contains a list of Repository.
type RepositoryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Repository `json:"items"`
}
