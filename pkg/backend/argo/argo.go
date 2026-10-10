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

// Package argo is the backend that installs through Argo CD: one
// Application per component, pointing at the component's published chart,
// synced automatically. kubepkg keeps deciding versions, order,
// requirements and revisions; Argo CD does the applying and shows the
// releases next to everything else it manages.
package argo

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kuberoot-dev/kubepkg/pkg/backend"
)

// ErrRollbackUnsupported is returned by Rollback: packages applied through
// Argo CD are recovered by a forward fix.
var ErrRollbackUnsupported = errors.New("the argo backend cannot roll back to an earlier revision")

const (
	applicationAPIVersion = "argoproj.io/v1alpha1"
	// deleteResources makes Argo CD remove what an Application deployed
	// when the Application is deleted.
	deleteResources = "resources-finalizer.argocd.argoproj.io"
)

// Backend writes Argo CD Applications.
type Backend struct {
	Client client.Client
	// Secrets reads the platform values Secret, which Argo CD cannot use
	// itself; its values are passed on inline.
	Secrets kubernetes.Interface
	// Namespace is where Argo CD watches Applications. Default argocd.
	Namespace string
	// Project is the AppProject the Applications belong to. Default default.
	Project string
	// Server is the destination cluster. Default the cluster Argo CD runs in.
	Server string
}

// Name implements backend.Backend.
func (b *Backend) Name() string { return "argo" }

func (b *Backend) namespace() string {
	if b.Namespace == "" {
		return "argocd"
	}
	return b.Namespace
}

// ApplicationName is unique per release: Applications share one namespace.
func ApplicationName(c backend.Component) string {
	return c.Namespace + "-" + c.ReleaseName
}

// Render builds the Application for a component with its final values.
// Exported for the render command.
func (b *Backend) Render(c backend.Component, values map[string]any) (*unstructured.Unstructured, error) {
	if c.Chart == nil {
		return nil, fmt.Errorf("component %s has no published chart", c.Name)
	}
	project, server := b.Project, b.Server
	if project == "" {
		project = "default"
	}
	if server == "" {
		server = "https://kubernetes.default.svc"
	}
	repoURL := c.Chart.Repository
	if c.Chart.OCI() {
		// Argo CD names OCI Helm repositories without the scheme.
		repoURL = strings.TrimPrefix(repoURL, "oci://")
	}
	helm := map[string]any{"releaseName": c.ReleaseName}
	if len(values) > 0 {
		helm["valuesObject"] = values
	}
	app := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"project": project,
			"source": map[string]any{
				"repoURL":        repoURL,
				"chart":          c.Chart.Name,
				"targetRevision": c.Chart.Version,
				"helm":           helm,
			},
			"destination": map[string]any{"server": server, "namespace": c.Namespace},
			"syncPolicy": map[string]any{
				"automated":   map[string]any{"prune": true, "selfHeal": true},
				"syncOptions": []any{"CreateNamespace=true"},
				"retry":       map[string]any{"limit": int64(5)},
			},
		},
	}}
	app.SetAPIVersion(applicationAPIVersion)
	app.SetKind("Application")
	app.SetName(ApplicationName(c))
	app.SetNamespace(b.namespace())
	app.SetLabels(c.Labels)
	app.SetAnnotations(c.Annotations)
	app.SetFinalizers([]string{deleteResources})
	return app, nil
}

func (b *Backend) empty(c backend.Component) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(applicationAPIVersion)
	u.SetKind("Application")
	u.SetName(ApplicationName(c))
	u.SetNamespace(b.namespace())
	return u
}

// Apply creates or updates the Application.
func (b *Backend) Apply(ctx context.Context, c backend.Component) (backend.State, error) {
	values, err := backend.ResolveValues(ctx, b.Secrets, c)
	if err != nil {
		return backend.State{}, err
	}
	want, err := b.Render(c, values)
	if err != nil {
		return backend.State{}, err
	}
	have := b.empty(c)
	err = b.Client.Get(ctx, types.NamespacedName{Namespace: want.GetNamespace(), Name: want.GetName()}, have)
	if apierrors.IsNotFound(err) {
		if err := b.Client.Create(ctx, want); err != nil {
			return backend.State{}, err
		}
		return backend.State{Exists: true, Progressing: true, Message: "Application created"}, nil
	}
	if err != nil {
		return backend.State{}, err
	}
	have.Object["spec"] = want.Object["spec"]
	have.SetLabels(want.GetLabels())
	if err := b.Client.Update(ctx, have); err != nil {
		return backend.State{}, err
	}
	return stateOf(have), nil
}

// Status reads the Application's sync and health.
func (b *Backend) Status(ctx context.Context, c backend.Component) (backend.State, error) {
	app := b.empty(c)
	err := b.Client.Get(ctx, types.NamespacedName{Namespace: app.GetNamespace(), Name: app.GetName()}, app)
	if apierrors.IsNotFound(err) {
		return backend.State{}, nil
	}
	if err != nil {
		return backend.State{}, err
	}
	return stateOf(app), nil
}

// stateOf maps Argo CD's view to kubepkg's. Ready needs the wanted chart
// version synced and healthy, so an update is not reported ready on the
// strength of the version before it.
func stateOf(app *unstructured.Unstructured) backend.State {
	str := func(path ...string) string {
		v, _, _ := unstructured.NestedString(app.Object, path...)
		return v
	}
	st := backend.State{Exists: true}
	want := str("spec", "source", "targetRevision")
	sync, synced := str("status", "sync", "status"), str("status", "sync", "revision")
	health, healthMsg := str("status", "health", "status"), str("status", "health", "message")
	phase, opMsg := str("status", "operationState", "phase"), str("status", "operationState", "message")
	switch {
	case phase == "Failed" || phase == "Error":
		st.Failed, st.Message = true, opMsg
	case health == "Degraded":
		st.Failed, st.Message = true, healthMsg
	case sync == "Synced" && synced == want && health == "Healthy" && phase != "Running":
		st.Ready, st.Message = true, "synced and healthy"
	default:
		st.Progressing = true
		st.Message = strings.TrimSpace(fmt.Sprintf("sync %s, health %s %s", or(sync, "Unknown"), or(health, "Unknown"), opMsg))
	}
	return st
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// Rollback is not supported by this backend.
func (b *Backend) Rollback(context.Context, backend.Component, int) (backend.State, error) {
	return backend.State{}, ErrRollbackUnsupported
}

// Uninstall deletes the Application; its finalizer makes Argo CD remove
// what it deployed.
// Uninstall deletes the Application and reports ErrUninstalling until
// Argo CD has removed what it deployed and the Application is gone.
func (b *Backend) Uninstall(ctx context.Context, c backend.Component) error {
	app := b.empty(c)
	if err := b.Client.Delete(ctx, app); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	switch err := b.Client.Get(ctx, client.ObjectKeyFromObject(app), b.empty(c)); {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return err
	}
	return backend.ErrUninstalling
}

var _ backend.Backend = (*Backend)(nil)
