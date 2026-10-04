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

package flux

import (
	"context"
	"errors"
	"testing"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	"github.com/fluxcd/pkg/apis/kustomize"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/tym83/kubepkg/pkg/backend"
)

func component() backend.Component {
	return backend.Component{
		Package: "cert-manager", Name: "cert-manager", ReleaseName: "cert-manager", Namespace: "cert-manager",
		ArtifactName: "cert-manager-default-cert-manager", ArtifactNamespace: "kubepkg-system",
		Values:            map[string]any{"replicas": 2},
		ValuesFromSecrets: []string{"platform-values"},
		DependsOn:         []string{"cilium/cilium"},
		Labels:            map[string]string{"kubepkg.dev/package": "cert-manager"},
		UpgradeCRDs:       "CreateReplace",
		HealthCheckExprs:  []kustomize.CustomHealthCheck{{APIVersion: "v1", Kind: "X", HealthCheckExpressions: kustomize.HealthCheckExpressions{Current: "true"}}},
		Timeout:           5 * time.Minute,
	}
}

func TestRender(t *testing.T) {
	hr, err := (&Backend{}).Render(component())
	if err != nil {
		t.Fatal(err)
	}
	s := hr.Spec
	if s.ChartRef.Kind != "ExternalArtifact" || s.ChartRef.Name != "cert-manager-default-cert-manager" || s.ChartRef.Namespace != "kubepkg-system" {
		t.Errorf("chartRef = %+v", s.ChartRef)
	}
	if s.Install.Strategy.Name != string(helmv2.ActionStrategyRetryOnFailure) || s.Upgrade.Strategy.Name != string(helmv2.ActionStrategyRetryOnFailure) {
		t.Error("failed installs and upgrades must be retried; the strategy must stay RetryOnFailure")
	}
	if s.Upgrade.CRDs != helmv2.CreateReplace {
		t.Errorf("crds = %q", s.Upgrade.CRDs)
	}
	if len(s.ValuesFrom) != 1 || s.ValuesFrom[0].Name != "platform-values" {
		t.Errorf("valuesFrom = %+v", s.ValuesFrom)
	}
	if len(s.DependsOn) != 1 || s.DependsOn[0].Namespace != "cilium" || s.DependsOn[0].Name != "cilium" {
		t.Errorf("dependsOn = %+v", s.DependsOn)
	}
	if s.WaitStrategy == nil || s.WaitStrategy.Name != "poller" {
		t.Error("health checks only run under the poller, so they must imply it")
	}
	if string(s.Values.Raw) != `{"replicas":2}` {
		t.Errorf("values = %s", s.Values.Raw)
	}
}

func TestApplyKeepsSuspendAndForeignLabels(t *testing.T) {
	sch := runtime.NewScheme()
	if err := helmv2.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	existing := &helmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{Name: "cert-manager", Namespace: "cert-manager", Labels: map[string]string{"team": "infra"}},
		Spec:       helmv2.HelmReleaseSpec{Suspend: true},
	}
	cl := fake.NewClientBuilder().WithScheme(sch).WithObjects(existing).Build()
	b := &Backend{Client: cl}
	if _, err := b.Apply(context.Background(), component()); err != nil {
		t.Fatal(err)
	}
	got := &helmv2.HelmRelease{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "cert-manager", Namespace: "cert-manager"}, got); err != nil {
		t.Fatal(err)
	}
	if !got.Spec.Suspend {
		t.Error("a release suspended by hand must stay suspended")
	}
	if got.Labels["team"] != "infra" || got.Labels["kubepkg.dev/package"] != "cert-manager" {
		t.Errorf("labels = %v", got.Labels)
	}
}

func TestStateOf(t *testing.T) {
	b := &Backend{}
	mk := func(gen, observed int64, status metav1.ConditionStatus, reason string) *helmv2.HelmRelease {
		hr := &helmv2.HelmRelease{ObjectMeta: metav1.ObjectMeta{Generation: gen}}
		hr.Status.ObservedGeneration = observed
		hr.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: status, Reason: reason}}
		hr.Status.History = helmv2.Snapshots{{Version: 7}}
		return hr
	}
	if st := b.stateOf(mk(2, 1, metav1.ConditionTrue, "")); !st.Progressing {
		t.Error("a stale Ready from the previous generation must not count")
	}
	if st := b.stateOf(mk(2, 2, metav1.ConditionTrue, "")); !st.Ready || st.Revision != 7 {
		t.Errorf("state = %+v", st)
	}
	if st := b.stateOf(mk(2, 2, metav1.ConditionFalse, "UpgradeFailed")); !st.Failed {
		t.Errorf("state = %+v", st)
	}
}

func TestRollbackUnsupported(t *testing.T) {
	if _, err := (&Backend{}).Rollback(context.Background(), component(), 1); !errors.Is(err, ErrRollbackUnsupported) {
		t.Fatalf("err = %v", err)
	}
}
