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
	"sync"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// restGetter adapts an in-cluster rest.Config to the getter Helm expects.
// Discovery is cached once and shared, because Helm builds a client per
// action and re-discovering the API on every reconcile is slow.
type restGetter struct {
	cfg  *rest.Config
	once sync.Once
	disc discovery.CachedDiscoveryInterface
	err  error
}

func newRESTGetter(cfg *rest.Config) *restGetter { return &restGetter{cfg: cfg} }

func (g *restGetter) discovery() (discovery.CachedDiscoveryInterface, error) {
	g.once.Do(func() {
		d, err := discovery.NewDiscoveryClientForConfig(g.cfg)
		if err != nil {
			g.err = err
			return
		}
		g.disc = memory.NewMemCacheClient(d)
	})
	return g.disc, g.err
}

// invalidate drops cached discovery, e.g. after new CRDs were installed.
func (g *restGetter) invalidate() {
	if d, err := g.discovery(); err == nil {
		d.Invalidate()
	}
}

func (g *restGetter) forNamespace(ns string) genericclioptions.RESTClientGetter {
	return &nsGetter{g: g, ns: ns}
}

type nsGetter struct {
	g  *restGetter
	ns string
}

func (n *nsGetter) ToRESTConfig() (*rest.Config, error) { return rest.CopyConfig(n.g.cfg), nil }

func (n *nsGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	return n.g.discovery()
}

func (n *nsGetter) ToRESTMapper() (meta.RESTMapper, error) {
	d, err := n.g.discovery()
	if err != nil {
		return nil, err
	}
	return restmapper.NewShortcutExpander(restmapper.NewDeferredDiscoveryRESTMapper(d), d, nil), nil
}

func (n *nsGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	return clientcmd.NewDefaultClientConfig(clientcmdapi.Config{}, &clientcmd.ConfigOverrides{
		Context: clientcmdapi.Context{Namespace: n.ns},
	})
}
