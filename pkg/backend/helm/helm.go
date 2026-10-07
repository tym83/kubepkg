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

// Package helm is the backend that installs charts with the Helm SDK
// inside the operator. It needs nothing else in the cluster.
package helm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart/loader"
	"helm.sh/helm/v4/pkg/kube"
	"helm.sh/helm/v4/pkg/release"
	releasev1 "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage/driver"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/tym83/kubepkg/pkg/backend"
)

// Helm release statuses, as strings to avoid depending on the internal
// release version package.
const (
	statusDeployed = "deployed"
	statusFailed   = "failed"
)

// DefaultTimeout applies when a component sets none.
const DefaultTimeout = 10 * time.Minute

// Backend runs Helm actions in-process.
type Backend struct {
	getter  *restGetter
	secrets kubernetes.Interface
	// MaxHistory bounds stored Helm revisions per release. It must exceed
	// the PackageRevision history, or rollbacks would target pruned releases.
	MaxHistory int
	Logger     *slog.Logger

	mu      sync.Mutex
	running map[string]bool // releases this process is operating on
}

// New returns a Helm backend talking to the cluster in cfg.
func New(cfg *rest.Config) (*Backend, error) {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Backend{getter: newRESTGetter(cfg), secrets: cs, MaxHistory: 20, Logger: slog.Default()}, nil
}

// Name implements backend.Backend.
func (b *Backend) Name() string { return "helm" }

func (b *Backend) config(namespace string) (*action.Configuration, error) {
	cfg := action.NewConfiguration(action.ConfigurationSetLogger(b.Logger.Handler()))
	if err := cfg.Init(b.getter.forNamespace(namespace), namespace, "secret"); err != nil {
		return nil, fmt.Errorf("helm configuration for %s: %w", namespace, err)
	}
	return cfg, nil
}

func timeout(c backend.Component) time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return DefaultTimeout
}

// Apply installs or upgrades the release and waits until it is ready or
// the timeout passes.
func (b *Backend) Apply(ctx context.Context, c backend.Component) (backend.State, error) {
	if c.ChartDir == "" {
		return backend.State{}, errors.New("helm backend needs a composed chart directory")
	}
	ch, err := loader.Load(c.ChartDir)
	if err != nil {
		return backend.State{}, fmt.Errorf("load chart %s: %w", c.ChartDir, err)
	}
	cfg, err := b.config(c.Namespace)
	if err != nil {
		return backend.State{}, err
	}
	values, err := backend.ResolveValues(ctx, b.secrets, c)
	if err != nil {
		return backend.State{}, err
	}
	if waiting, err := b.recoverInterrupted(cfg, c, time.Now()); err != nil {
		return backend.State{}, err
	} else if waiting != "" {
		return backend.State{Exists: true, Progressing: true, Message: waiting}, nil
	}
	defer b.begin(c.Key())()

	if _, err := action.NewHistory(cfg).Run(c.ReleaseName); errors.Is(err, driver.ErrReleaseNotFound) {
		in := action.NewInstall(cfg)
		in.ReleaseName = c.ReleaseName
		in.Namespace = c.Namespace
		in.CreateNamespace = true
		in.WaitStrategy = kube.StatusWatcherStrategy
		in.Timeout = timeout(c)
		in.Labels = c.Labels
		// A failed first install is uninstalled by the operator when the
		// package rolls back, so Helm must not uninstall it on its own.
		in.RollbackOnFailure = false
		in.TakeOwnership = c.Adopt
		if _, err := in.RunWithContext(ctx, ch, values); err != nil {
			return b.stateAfter(ctx, c, fmt.Errorf("install %s: %w", c.Key(), err))
		}
		return b.Status(ctx, c)
	} else if err != nil {
		return backend.State{}, fmt.Errorf("history of %s: %w", c.Key(), err)
	}

	up := action.NewUpgrade(cfg)
	up.Namespace = c.Namespace
	up.WaitStrategy = kube.StatusWatcherStrategy
	up.Timeout = timeout(c)
	up.MaxHistory = b.MaxHistory
	up.Labels = c.Labels
	// The package decides about rollbacks for all its components together;
	// a per-release rollback here would leave the package half reverted.
	up.RollbackOnFailure = false
	// The package owns what its charts contain. Operators often rewrite
	// fields of their own CRDs and resources; server-side apply would then
	// refuse every upgrade with a field manager conflict, where Helm 3's
	// client-side apply simply wrote the chart's values. Keep those
	// semantics: the chart's values win.
	up.ForceConflicts = true
	up.TakeOwnership = c.Adopt
	if c.UpgradeCRDs == "Create" || c.UpgradeCRDs == "CreateReplace" {
		if err := applyCRDs(ctx, b.getter, ch, values); err != nil {
			return backend.State{}, fmt.Errorf("upgrade CRDs of %s: %w", c.Key(), err)
		}
	}
	if _, err := up.RunWithContext(ctx, c.ReleaseName, ch, values); err != nil {
		return b.stateAfter(ctx, c, fmt.Errorf("upgrade %s: %w", c.Key(), err))
	}
	return b.Status(ctx, c)
}

// stateAfter returns the release state together with the error that ended
// an action, so the operator records which backend revision failed.
func (b *Backend) stateAfter(ctx context.Context, c backend.Component, actionErr error) (backend.State, error) {
	st, err := b.Status(ctx, c)
	if err != nil {
		return backend.State{Failed: true, Message: actionErr.Error()}, actionErr
	}
	st.Ready = false
	st.Failed = true
	st.Message = actionErr.Error()
	return st, actionErr
}

// Status reports the latest release revision.
func (b *Backend) Status(_ context.Context, c backend.Component) (backend.State, error) {
	cfg, err := b.config(c.Namespace)
	if err != nil {
		return backend.State{}, err
	}
	rel, err := action.NewStatus(cfg).Run(c.ReleaseName)
	if errors.Is(err, driver.ErrReleaseNotFound) {
		return backend.State{}, nil
	}
	if err != nil {
		return backend.State{}, fmt.Errorf("status of %s: %w", c.Key(), err)
	}
	acc, err := release.NewAccessor(rel)
	if err != nil {
		return backend.State{}, err
	}
	st := backend.State{Exists: true, Revision: acc.Version(), Message: acc.Status()}
	switch s := acc.Status(); {
	case s == statusDeployed:
		st.Ready = true
	case s == statusFailed:
		st.Failed = true
	case strings.HasPrefix(s, "pending"):
		st.Progressing = true
	}
	return st, nil
}

// Rollback returns the release to an earlier Helm revision and waits for it.
func (b *Backend) Rollback(ctx context.Context, c backend.Component, toRevision int) (backend.State, error) {
	cfg, err := b.config(c.Namespace)
	if err != nil {
		return backend.State{}, err
	}
	if waiting, err := b.recoverInterrupted(cfg, c, time.Now()); err != nil {
		return backend.State{}, err
	} else if waiting != "" {
		return backend.State{Exists: true, Progressing: true, Message: waiting}, fmt.Errorf("roll back %s: %s", c.Key(), waiting)
	}
	defer b.begin(c.Key())()
	rb := action.NewRollback(cfg)
	rb.Version = toRevision
	rb.WaitStrategy = kube.StatusWatcherStrategy
	rb.Timeout = timeout(c)
	rb.MaxHistory = b.MaxHistory
	rb.ForceConflicts = true // see Apply
	if err := rb.Run(c.ReleaseName); err != nil {
		return b.stateAfter(ctx, c, fmt.Errorf("roll back %s to revision %d: %w", c.Key(), toRevision, err))
	}
	return b.Status(ctx, c)
}

// Uninstall removes the release; a missing release is fine.
func (b *Backend) Uninstall(_ context.Context, c backend.Component) error {
	cfg, err := b.config(c.Namespace)
	if err != nil {
		return err
	}
	un := action.NewUninstall(cfg)
	un.WaitStrategy = kube.StatusWatcherStrategy
	un.Timeout = DefaultTimeout
	un.IgnoreNotFound = true
	if _, err := un.Run(c.ReleaseName); err != nil && !errors.Is(err, driver.ErrReleaseNotFound) {
		return fmt.Errorf("uninstall %s: %w", c.Key(), err)
	}
	return nil
}

var _ backend.Backend = (*Backend)(nil)

// ReleaseInfo is what an existing Helm release holds.
type ReleaseInfo struct {
	Chart, ChartVersion, AppVersion string
	Revision                        int
	Status                          string
	// Values are the values the release was given, not the chart's
	// defaults.
	Values map[string]any
	// ManagedBy is the package label, empty for a release kubepkg did not
	// install.
	ManagedBy string
}

// Release reads the Helm release namespace/name; nil when there is none.
func (b *Backend) Release(namespace, name string) (*ReleaseInfo, error) {
	cfg, err := b.config(namespace)
	if err != nil {
		return nil, err
	}
	rel, err := action.NewGet(cfg).Run(name)
	if errors.Is(err, driver.ErrReleaseNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("release %s/%s: %w", namespace, name, err)
	}
	acc, err := release.NewAccessor(rel)
	if err != nil {
		return nil, err
	}
	info := &ReleaseInfo{Revision: acc.Version(), Status: acc.Status(), ManagedBy: acc.Labels()["kubepkg.dev/package"]}
	if r, ok := rel.(*releasev1.Release); ok && r.Chart != nil && r.Chart.Metadata != nil {
		info.Chart, info.ChartVersion, info.AppVersion = r.Chart.Metadata.Name, r.Chart.Metadata.Version, r.Chart.Metadata.AppVersion
	}
	if info.Values, err = action.NewGetValues(cfg).Run(name); err != nil {
		return nil, fmt.Errorf("values of %s/%s: %w", namespace, name, err)
	}
	return info, nil
}
