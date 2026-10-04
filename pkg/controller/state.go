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
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/tym83/kubepkg/api/v1alpha1"
	"github.com/tym83/kubepkg/pkg/resolve"
)

// APIs lists group versions the API server serves, for "api:" capabilities.
type APIs interface {
	Served(ctx context.Context) (map[string]bool, error)
}

// DiscoveryAPIs caches discovery for a short while: every reconcile asks,
// and the served API set changes only when CRDs come and go.
type DiscoveryAPIs struct {
	Client discovery.DiscoveryInterface
	TTL    time.Duration

	mu      sync.Mutex
	fetched time.Time
	cache   map[string]bool
}

// Served implements APIs.
func (d *DiscoveryAPIs) Served(context.Context) (map[string]bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ttl := d.TTL
	if ttl == 0 {
		ttl = 30 * time.Second
	}
	if d.cache != nil && time.Since(d.fetched) < ttl {
		return d.cache, nil
	}
	groups, err := d.Client.ServerGroups()
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, g := range groups.Groups {
		for _, v := range g.Versions {
			out[v.GroupVersion] = true
		}
	}
	d.cache, d.fetched = out, time.Now()
	return out, nil
}

func isReady(conds []metav1.Condition) bool {
	c := meta.FindStatusCondition(conds, "Ready")
	return c != nil && c.Status == metav1.ConditionTrue
}

// clusterState gathers installed packages and served APIs for resolve.
func (r *PackageReconciler) clusterState(ctx context.Context) (resolve.State, error) {
	return ClusterState(ctx, r.Client, r.APIs)
}

// ClusterState gathers the installed packages and, when apis is set, the
// served APIs, in the form the resolver takes. The CLI plans with the
// same view the operator acts on.
func ClusterState(ctx context.Context, c client.Reader, apis APIs) (resolve.State, error) {
	st := resolve.State{Packages: map[string]resolve.Installed{}, APIs: map[string]bool{}}
	var pkgs v1alpha1.PackageList
	if err := c.List(ctx, &pkgs); err != nil {
		return st, err
	}
	for i := range pkgs.Items {
		p := &pkgs.Items[i]
		inst := resolve.Installed{Release: resolve.Release{Name: p.Name, Version: p.Status.Version}, Ready: isReady(p.Status.Conditions)}
		var src v1alpha1.PackageSource
		if err := c.Get(ctx, types.NamespacedName{Name: p.Name}, &src); err == nil {
			inst.Provides = src.Spec.Provides
			inst.Conflicts = src.Spec.Conflicts
			if inst.Version == "" {
				inst.Version = sourceVersion(&src)
			}
		} else if !apierrors.IsNotFound(err) {
			return st, err
		}
		st.Packages[p.Name] = inst
	}
	if apis != nil {
		served, err := apis.Served(ctx)
		if err != nil {
			return st, err
		}
		st.APIs = served
	}
	return st, nil
}

// requirementsOf turns a variant's requirements into resolve requirements,
// minus what the Package chose to ignore.
func requirementsOf(pkg *v1alpha1.Package, v *v1alpha1.Variant) []resolve.Requirement {
	return resolve.RequirementsOf(v, pkg.Spec.IgnoreDependencies)
}
