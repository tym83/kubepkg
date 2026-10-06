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

// Package flux is the backend that installs through Flux: for every
// component it writes the chart's source (an OCIRepository for charts in
// a registry, a HelmRepository otherwise) and a HelmRelease, and lets
// source-controller and helm-controller do the rest. It needs only Flux in
// the cluster.
package flux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/tym83/kubepkg/pkg/backend"
)

// ErrRollbackUnsupported is returned by Rollback. Flux keeps no earlier
// chart artifacts to return to, so packages applied through Flux are
// recovered by a forward fix.
var ErrRollbackUnsupported = errors.New("the flux backend cannot roll back to an earlier revision")

// Backend writes chart sources and HelmReleases.
type Backend struct {
	Client client.Client
	// Insecure lets source-controller pull from registries over plain HTTP.
	Insecure      bool
	Interval      time.Duration
	RetryInterval time.Duration
	MaxHistory    int
}

// Name implements backend.Backend.
func (b *Backend) Name() string { return "flux" }

func (b *Backend) defaults() (time.Duration, time.Duration, int) {
	iv, rv, mh := b.Interval, b.RetryInterval, b.MaxHistory
	if iv == 0 {
		iv = 5 * time.Minute
	}
	if rv == 0 {
		rv = time.Minute
	}
	if mh == 0 {
		mh = 5
	}
	return iv, rv, mh
}

// Render builds the HelmRelease for a component. Exported for the CLI's
// render command and for tests.
func (b *Backend) Render(c backend.Component) (*helmv2.HelmRelease, error) {
	if c.Chart == nil {
		return nil, fmt.Errorf("component %s has no published chart", c.Name)
	}
	iv, rv, mh := b.defaults()
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	hr := &helmv2.HelmRelease{
		TypeMeta: metav1.TypeMeta{APIVersion: helmv2.GroupVersion.String(), Kind: helmv2.HelmReleaseKind},
		ObjectMeta: metav1.ObjectMeta{
			Name:        c.ReleaseName,
			Namespace:   c.Namespace,
			Labels:      c.Labels,
			Annotations: c.Annotations,
		},
		Spec: helmv2.HelmReleaseSpec{
			Interval:   metav1.Duration{Duration: iv},
			MaxHistory: &mh,
			Install: &helmv2.Install{
				Timeout:  &metav1.Duration{Duration: timeout},
				Strategy: &helmv2.InstallStrategy{Name: string(helmv2.ActionStrategyRetryOnFailure), RetryInterval: &metav1.Duration{Duration: rv}},
			},
			Upgrade: &helmv2.Upgrade{
				Timeout:  &metav1.Duration{Duration: timeout},
				Strategy: &helmv2.UpgradeStrategy{Name: string(helmv2.ActionStrategyRetryOnFailure), RetryInterval: &metav1.Duration{Duration: rv}},
				CRDs:     helmv2.CRDsPolicy(c.UpgradeCRDs),
			},
			HealthCheckExprs: c.HealthCheckExprs,
			WaitStrategy:     waitStrategy(c),
		},
	}
	if c.Chart.OCI() {
		hr.Spec.ChartRef = &helmv2.CrossNamespaceSourceReference{Kind: "OCIRepository", Name: c.ReleaseName, Namespace: c.Namespace}
	} else {
		hr.Spec.Chart = &helmv2.HelmChartTemplate{Spec: helmv2.HelmChartTemplateSpec{
			Chart:     c.Chart.Name,
			Version:   c.Chart.Version,
			SourceRef: helmv2.CrossNamespaceObjectReference{Kind: "HelmRepository", Name: c.ReleaseName, Namespace: c.Namespace},
		}}
	}
	for _, s := range c.ValuesFromSecrets {
		if strings.Contains(s, "/") {
			return nil, fmt.Errorf("values secret %q: Flux reads values only from the release namespace", s)
		}
		hr.Spec.ValuesFrom = append(hr.Spec.ValuesFrom, helmv2.ValuesReference{Kind: "Secret", Name: s})
	}
	for _, d := range c.DependsOn {
		ns, name, ok := strings.Cut(d, "/")
		if !ok {
			return nil, fmt.Errorf("dependency %q is not namespace/name", d)
		}
		hr.Spec.DependsOn = append(hr.Spec.DependsOn, helmv2.DependencyReference{Name: name, Namespace: ns})
	}
	if len(c.Values) > 0 {
		raw, err := json.Marshal(c.Values)
		if err != nil {
			return nil, err
		}
		hr.Spec.Values = &apiextensionsv1.JSON{Raw: raw}
	}
	return hr, nil
}

// waitStrategy picks the poller when CEL health checks are set: they only
// run under it.
func waitStrategy(c backend.Component) *helmv2.WaitStrategy {
	name := c.WaitStrategy
	if name == "" && len(c.HealthCheckExprs) > 0 {
		name = "poller"
	}
	if name == "" {
		return nil
	}
	return &helmv2.WaitStrategy{Name: helmv2.WaitStrategyName(name)}
}

// Apply creates or updates the HelmRelease. It preserves labels,
// annotations and suspend set by others, because operators of a cluster
// suspend releases by hand during incidents.
// RenderSource builds the Flux source of a component's chart: an
// OCIRepository selecting the Helm chart layer, or a HelmRepository.
// Exported for the render command.
func (b *Backend) RenderSource(c backend.Component) (*unstructured.Unstructured, error) {
	if c.Chart == nil {
		return nil, fmt.Errorf("component %s has no published chart", c.Name)
	}
	iv, _, _ := b.defaults()
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(sourceAPIVersion)
	u.SetName(c.ReleaseName)
	u.SetNamespace(c.Namespace)
	u.SetLabels(c.Labels)
	spec := map[string]any{"interval": iv.String()}
	if c.Chart.OCI() {
		u.SetKind("OCIRepository")
		spec["url"] = strings.TrimSuffix(c.Chart.Repository, "/") + "/" + c.Chart.Name
		spec["ref"] = map[string]any{"tag": strings.ReplaceAll(c.Chart.Version, "+", "_")}
		spec["layerSelector"] = map[string]any{"mediaType": chartMediaType, "operation": "copy"}
	} else {
		u.SetKind("HelmRepository")
		spec["url"] = c.Chart.Repository
	}
	if b.Insecure {
		spec["insecure"] = true
	}
	u.Object["spec"] = spec
	return u, nil
}

const (
	sourceAPIVersion = "source.toolkit.fluxcd.io/v1"
	chartMediaType   = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
)

// applySource creates or updates the component's chart source.
func (b *Backend) applySource(ctx context.Context, c backend.Component) error {
	want, err := b.RenderSource(c)
	if err != nil {
		return err
	}
	have := &unstructured.Unstructured{}
	have.SetGroupVersionKind(want.GroupVersionKind())
	err = b.Client.Get(ctx, types.NamespacedName{Namespace: c.Namespace, Name: c.ReleaseName}, have)
	if apierrors.IsNotFound(err) {
		return b.Client.Create(ctx, want)
	}
	if err != nil {
		return err
	}
	have.Object["spec"] = want.Object["spec"]
	have.SetLabels(want.GetLabels())
	return b.Client.Update(ctx, have)
}

// Apply writes the chart source and the HelmRelease.
func (b *Backend) Apply(ctx context.Context, c backend.Component) (backend.State, error) {
	want, err := b.Render(c)
	if err != nil {
		return backend.State{}, err
	}
	if err := b.applySource(ctx, c); err != nil {
		return backend.State{}, fmt.Errorf("chart source of %s: %w", c.Key(), err)
	}
	have := &helmv2.HelmRelease{}
	err = b.Client.Get(ctx, types.NamespacedName{Namespace: c.Namespace, Name: c.ReleaseName}, have)
	if apierrors.IsNotFound(err) {
		if err := b.Client.Create(ctx, want); err != nil {
			return backend.State{}, err
		}
		return backend.State{Exists: true, Progressing: true, Message: "HelmRelease created"}, nil
	}
	if err != nil {
		return backend.State{}, err
	}
	for k, v := range have.Labels {
		if _, ok := want.Labels[k]; !ok {
			if want.Labels == nil {
				want.Labels = map[string]string{}
			}
			want.Labels[k] = v
		}
	}
	for k, v := range have.Annotations {
		if _, ok := want.Annotations[k]; !ok {
			if want.Annotations == nil {
				want.Annotations = map[string]string{}
			}
			want.Annotations[k] = v
		}
	}
	want.Spec.Suspend = have.Spec.Suspend
	have.Spec = want.Spec
	have.Labels, have.Annotations = want.Labels, want.Annotations
	if err := b.Client.Update(ctx, have); err != nil {
		return backend.State{}, err
	}
	return b.stateOf(have), nil
}

// Status reads the HelmRelease conditions.
func (b *Backend) Status(ctx context.Context, c backend.Component) (backend.State, error) {
	hr := &helmv2.HelmRelease{}
	err := b.Client.Get(ctx, types.NamespacedName{Namespace: c.Namespace, Name: c.ReleaseName}, hr)
	if apierrors.IsNotFound(err) {
		return backend.State{}, nil
	}
	if err != nil {
		return backend.State{}, err
	}
	return b.stateOf(hr), nil
}

func (b *Backend) stateOf(hr *helmv2.HelmRelease) backend.State {
	st := backend.State{Exists: true}
	if len(hr.Status.History) > 0 {
		st.Revision = hr.Status.History[0].Version
	}
	ready := meta.FindStatusCondition(hr.Status.Conditions, "Ready")
	switch {
	case hr.Status.ObservedGeneration < hr.Generation || ready == nil:
		st.Progressing = true
		st.Message = "waiting for helm-controller"
	case ready.Status == metav1.ConditionTrue:
		st.Ready = true
		st.Message = ready.Message
	case ready.Reason == "Progressing" || ready.Status == metav1.ConditionUnknown:
		st.Progressing = true
		st.Message = ready.Message
	default:
		st.Failed = true
		st.Message = ready.Message
	}
	return st
}

// Rollback is not supported by this backend.
func (b *Backend) Rollback(context.Context, backend.Component, int) (backend.State, error) {
	return backend.State{}, ErrRollbackUnsupported
}

// Uninstall deletes the HelmRelease, which helm-controller uninstalls,
// and the chart source.
func (b *Backend) Uninstall(ctx context.Context, c backend.Component) error {
	hr := &helmv2.HelmRelease{ObjectMeta: metav1.ObjectMeta{Name: c.ReleaseName, Namespace: c.Namespace}}
	if err := b.Client.Delete(ctx, hr); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	// The helm controller uninstalls the release before the HelmRelease
	// goes; its source must stay until then.
	switch err := b.Client.Get(ctx, client.ObjectKeyFromObject(hr), &helmv2.HelmRelease{}); {
	case err == nil:
		return backend.ErrUninstalling
	case !apierrors.IsNotFound(err):
		return err
	}
	for _, kind := range []string{"OCIRepository", "HelmRepository"} {
		u := &unstructured.Unstructured{}
		u.SetAPIVersion(sourceAPIVersion)
		u.SetKind(kind)
		u.SetName(c.ReleaseName)
		u.SetNamespace(c.Namespace)
		if err := b.Client.Delete(ctx, u); err != nil && !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
			return err
		}
	}
	return nil
}

var _ backend.Backend = (*Backend)(nil)
