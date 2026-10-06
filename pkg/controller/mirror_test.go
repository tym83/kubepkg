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
	"testing"

	"github.com/tym83/kubepkg/api/v1alpha1"
	"github.com/tym83/kubepkg/pkg/backend"
)

func TestChartPreparerMirrorKeepsTheDigest(t *testing.T) {
	comp := &v1alpha1.Component{Name: "app", Chart: &v1alpha1.ChartRef{Repository: "https://charts.example.org", Name: "app", Version: "1.0.0"}}
	src := &v1alpha1.PackageSource{}
	var plain, mirrored backend.Component
	d1, err := ChartPreparer{}.Prepare(context.Background(), src, nil, comp, &plain)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := ChartPreparer{Mirror: "oci://registry.internal/mirror"}.Prepare(context.Background(), src, nil, comp, &mirrored)
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Error("turning a mirror on changes the chart digest, so every package would get a new revision")
	}
	if mirrored.Chart.Repository != "oci://registry.internal/mirror/charts.example.org" || plain.Chart.Repository != "https://charts.example.org" {
		t.Errorf("repositories: %s, %s", plain.Chart.Repository, mirrored.Chart.Repository)
	}
}
