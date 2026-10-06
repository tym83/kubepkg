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

// Package v1beta1 contains the kubepkg API.
//
// The types are served under kubepkg.dev by default. A platform that embeds
// kubepkg may serve them under its own group instead.
//
// +kubebuilder:object:generate=true
// +groupName=kubepkg.dev
package v1beta1

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// GroupName is the native API group.
	GroupName = "kubepkg.dev"
	// Version is the version clusters store; v1alpha1 is still served,
	// with a deprecation warning, and has the same schema.
	Version = "v1beta1"
)

// GroupVersion is the native group version.
var GroupVersion = schema.GroupVersion{Group: GroupName, Version: Version}

// AddToSchemeForGroup registers the kubepkg types under the given group.
func AddToSchemeForGroup(group string) func(*runtime.Scheme) error {
	gv := schema.GroupVersion{Group: group, Version: Version}
	return func(s *runtime.Scheme) error {
		s.AddKnownTypes(gv,
			&Package{}, &PackageList{},
			&PackageSource{}, &PackageSourceList{},
			&PackageRevision{}, &PackageRevisionList{},
			&Repository{}, &RepositoryList{},
			&Cluster{}, &ClusterList{},
			&PackageSet{}, &PackageSetList{},
		)
		metav1AddToGroupVersion(s, gv)
		return nil
	}
}

// AddToScheme registers the types under the native group.
var AddToScheme = AddToSchemeForGroup(GroupName)
