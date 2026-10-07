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

package helm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	chartutilv2 "helm.sh/helm/v4/pkg/chart/v2/util"
	"io"

	"helm.sh/helm/v4/pkg/chart"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
)

var crdGVR = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

// applyCRDs server-side applies the chart's crds/ directory. Helm never
// upgrades CRDs; components that evolve their CRDs opt in with upgradeCRDs.
func applyCRDs(ctx context.Context, g *restGetter, ch chart.Charter, values map[string]any) error {
	crds, err := crdsToApply(ch, values)
	if err != nil {
		return err
	}
	if len(crds) == 0 {
		return nil
	}
	client, err := dynamic.NewForConfig(g.cfg)
	if err != nil {
		return err
	}
	for _, u := range crds {
		data, err := u.MarshalJSON()
		if err != nil {
			return err
		}
		force := true
		if _, err := client.Resource(crdGVR).Patch(ctx, u.GetName(), types.ApplyPatchType, data,
			metav1.PatchOptions{FieldManager: "kubepkg", Force: &force}); err != nil {
			return fmt.Errorf("apply CRD %s: %w", u.GetName(), err)
		}
	}
	g.invalidate()
	return nil
}

// crdsToApply lists the CRDs in crds/ of a chart and of the subcharts the
// values enable: one switched off by its condition is left out, as helm
// install would leave it.
func crdsToApply(ch chart.Charter, values map[string]any) ([]*unstructured.Unstructured, error) {
	c, ok := ch.(*chartv2.Chart)
	if !ok {
		return nil, fmt.Errorf("unsupported chart type %T", ch)
	}
	if values == nil {
		values = map[string]any{}
	}
	if err := chartutilv2.ProcessDependencies(c, values); err != nil {
		return nil, err
	}
	var out []*unstructured.Unstructured
	for _, f := range c.CRDObjects() {
		dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(f.File.Data), 4096)
		for {
			u := &unstructured.Unstructured{}
			if err := dec.Decode(&u.Object); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				return nil, fmt.Errorf("%s: %w", f.Filename, err)
			}
			if len(u.Object) > 0 {
				out = append(out, u)
			}
		}
	}
	return out, nil
}
