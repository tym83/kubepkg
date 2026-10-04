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

// Package backend defines how a package component reaches the cluster.
//
// The operator decides what to apply and in which order; a backend only
// knows how to install, upgrade, inspect, roll back and remove one Helm
// release. Two backends exist: helm, which runs the Helm SDK in-process, and
// flux, which delegates to Flux helm-controller through HelmRelease objects.
package backend

import (
	"context"
	"strings"
	"time"

	"github.com/fluxcd/pkg/apis/kustomize"
)

// Component is one Helm release of a package revision.
type Component struct {
	// Package and Name identify the component; ReleaseName and Namespace
	// identify the release it becomes.
	Package     string
	Name        string
	ReleaseName string
	Namespace   string

	// ChartDir is a composed chart on local disk (helm backend).
	ChartDir string
	// Chart is a published chart, for backends that hand charts to another
	// installer (flux, argo) instead of reading them from disk.
	Chart *Chart

	// Values are the user overrides, merged over the chart's own values.
	Values map[string]any
	// ValuesFromSecrets names Secrets whose values.yaml is layered under
	// Values: namespace/name, or a bare name in the release namespace. The
	// flux backend supports bare names only, a limit of Flux valuesFrom.
	ValuesFromSecrets []string

	// DependsOn lists other releases, as namespace/name, that must be ready
	// before this one (flux backend orders through it; the operator orders
	// the helm backend itself).
	DependsOn []string

	Labels      map[string]string
	Annotations map[string]string

	UpgradeCRDs      string
	WaitStrategy     string
	HealthCheckExprs []kustomize.CustomHealthCheck
	Timeout          time.Duration
}

// Chart is one version of a published Helm chart.
type Chart struct {
	// Repository is an http(s):// Helm repository or an oci:// path; the
	// chart is <repository>/<name> in a registry.
	Repository string
	Name       string
	Version    string
	// Digest is the sha256 of the chart archive, when known.
	Digest string
}

// OCI reports whether the chart lives in an OCI registry.
func (c *Chart) OCI() bool { return strings.HasPrefix(c.Repository, "oci://") }

// Key is the release identity, namespace/name.
func (c Component) Key() string { return c.Namespace + "/" + c.ReleaseName }

// State is what the backend sees of a release.
type State struct {
	// Exists is false when the release has never been installed or was removed.
	Exists bool
	// Ready means the release reached a deployed, healthy state.
	Ready bool
	// Failed means the last operation on the release failed; Ready is false.
	Failed bool
	// Progressing means an operation is still running (flux backend).
	Progressing bool
	// Revision is the backend's revision number of the current release,
	// e.g. the Helm release version. Rollback targets these numbers.
	Revision int
	Message  string
}

// Backend applies components. Every method must be idempotent: the operator
// calls them again after restarts and on every reconcile.
type Backend interface {
	// Name is the backend identifier used in flags and status.
	Name() string
	// Apply installs or upgrades the release so it matches c and returns the
	// state right after the call. The helm backend waits for readiness up to
	// c.Timeout; the flux backend returns at once with Progressing set.
	Apply(ctx context.Context, c Component) (State, error)
	// Status reports the current state of the release.
	Status(ctx context.Context, c Component) (State, error)
	// Rollback returns the release to an earlier backend revision.
	Rollback(ctx context.Context, c Component, toRevision int) (State, error)
	// Uninstall removes the release. Removing a missing release is not an error.
	Uninstall(ctx context.Context, c Component) error
}
