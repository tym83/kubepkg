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

package argo

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/tym83/kubepkg/pkg/backend"
)

func component() backend.Component {
	return backend.Component{
		Package: "kubevirt", Name: "operator", ReleaseName: "kubevirt-operator", Namespace: "kubevirt",
		Chart:             &backend.Chart{Repository: "oci://ghcr.io/example/packages/kubevirt", Name: "kubevirt-operator", Version: "1.9.0-2"},
		Values:            map[string]any{"replicas": int64(2)},
		ValuesFromSecrets: []string{"kubepkg-system/platform"},
		Labels:            map[string]string{"kubepkg.dev/package": "kubevirt"},
	}
}

func newBackend(t *testing.T) *Backend {
	t.Helper()
	secrets := kfake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kubepkg-system", Name: "platform"},
		Data:       map[string][]byte{"values.yaml": []byte("domain: example.org\n")},
	})
	return &Backend{Client: fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build(), Secrets: secrets}
}

func nested(t *testing.T, u *unstructured.Unstructured, path ...string) any {
	t.Helper()
	v, ok, err := unstructured.NestedFieldNoCopy(u.Object, path...)
	if err != nil || !ok {
		t.Fatalf("%v missing: %v", path, err)
	}
	return v
}

func TestApplyWritesAnApplication(t *testing.T) {
	b := newBackend(t)
	ctx := context.Background()
	st, err := b.Apply(ctx, component())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Progressing {
		t.Fatalf("a new Application is progressing: %+v", st)
	}
	app := b.empty(component())
	if err := b.Client.Get(ctx, appKey(app), app); err != nil {
		t.Fatal(err)
	}
	if app.GetNamespace() != "argocd" || app.GetName() != "kubevirt-kubevirt-operator" || app.GetFinalizers()[0] != deleteResources {
		t.Errorf("metadata: %s/%s %v", app.GetNamespace(), app.GetName(), app.GetFinalizers())
	}
	if got := nested(t, app, "spec", "source", "repoURL"); got != "ghcr.io/example/packages/kubevirt" {
		t.Errorf("Argo CD wants OCI repositories without the scheme, got %v", got)
	}
	if got := nested(t, app, "spec", "source", "targetRevision"); got != "1.9.0-2" {
		t.Errorf("targetRevision %v", got)
	}
	values := nested(t, app, "spec", "source", "helm", "valuesObject").(map[string]any)
	if values["domain"] != "example.org" || values["replicas"] != int64(2) {
		t.Errorf("platform and package values must both be inline: %v", values)
	}
	if got := nested(t, app, "spec", "destination", "namespace"); got != "kubevirt" {
		t.Errorf("destination %v", got)
	}

	// A new version updates the same Application.
	c := component()
	c.Chart.Version = "1.9.0-3"
	if _, err := b.Apply(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := b.Client.Get(ctx, appKey(app), app); err != nil {
		t.Fatal(err)
	}
	if got := nested(t, app, "spec", "source", "targetRevision"); got != "1.9.0-3" {
		t.Errorf("not updated: %v", got)
	}

	if err := b.Uninstall(ctx, c); err != nil {
		t.Fatal(err)
	}
	// The finalizer keeps the Application until Argo CD has removed what
	// it deployed.
	if err := b.Client.Get(ctx, appKey(app), app); err != nil {
		t.Fatal(err)
	}
	if app.GetDeletionTimestamp() == nil {
		t.Fatal("Application not being deleted")
	}
}

func TestHTTPChartKeepsItsURL(t *testing.T) {
	c := component()
	c.Chart = &backend.Chart{Repository: "https://charts.jetstack.io", Name: "cert-manager", Version: "v1.21.2"}
	app, err := (&Backend{}).Render(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := nested(t, app, "spec", "source", "repoURL"); got != "https://charts.jetstack.io" {
		t.Errorf("repoURL %v", got)
	}
}

func TestStateOf(t *testing.T) {
	app := func(sync, revision, health, phase string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"spec": map[string]any{"source": map[string]any{"targetRevision": "1.9.0-2"}},
			"status": map[string]any{
				"sync":           map[string]any{"status": sync, "revision": revision},
				"health":         map[string]any{"status": health, "message": "pods crashlooping"},
				"operationState": map[string]any{"phase": phase, "message": "one or more objects failed to apply"},
			},
		}}
	}
	cases := []struct {
		name                string
		app                 *unstructured.Unstructured
		ready, failed, prog bool
	}{
		{"synced and healthy", app("Synced", "1.9.0-2", "Healthy", "Succeeded"), true, false, false},
		{"healthy on the old version", app("Synced", "1.9.0-1", "Healthy", "Succeeded"), false, false, true},
		{"syncing", app("OutOfSync", "1.9.0-1", "Healthy", "Running"), false, false, true},
		{"degraded", app("Synced", "1.9.0-2", "Degraded", "Succeeded"), false, true, false},
		{"sync failed", app("OutOfSync", "1.9.0-1", "Healthy", "Failed"), false, true, false},
		{"not looked at yet", &unstructured.Unstructured{Object: map[string]any{}}, false, false, true},
	}
	for _, c := range cases {
		st := stateOf(c.app)
		if st.Ready != c.ready || st.Failed != c.failed || st.Progressing != c.prog {
			t.Errorf("%s: %+v", c.name, st)
		}
	}
}

func appKey(u *unstructured.Unstructured) types.NamespacedName {
	return types.NamespacedName{Namespace: u.GetNamespace(), Name: u.GetName()}
}
