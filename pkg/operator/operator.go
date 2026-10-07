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

// Package operator assembles the kubepkg operator. A distribution that
// embeds kubepkg builds its own operator binary from it: start from
// DefaultOptions, change defaults, register its own backends, and Run.
package operator

import (
	"context"
	"flag"
	"fmt"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"os"
	"path/filepath"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	ctradmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	"sort"
	"strings"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/tym83/kubepkg/api/v1beta1"
	"github.com/tym83/kubepkg/pkg/admission"
	"github.com/tym83/kubepkg/pkg/backend"
	"github.com/tym83/kubepkg/pkg/backend/argo"
	"github.com/tym83/kubepkg/pkg/backend/flux"
	"github.com/tym83/kubepkg/pkg/backend/helm"
	"github.com/tym83/kubepkg/pkg/backend/werf"
	"github.com/tym83/kubepkg/pkg/controller"
	"github.com/tym83/kubepkg/pkg/repo"
	"github.com/tym83/kubepkg/pkg/source"
	"github.com/tym83/kubepkg/pkg/version"
)

// Env is what a backend factory gets to build its backend.
type Env struct {
	Config  *rest.Config
	Client  client.Client
	Options *Options
}

// BackendFactory makes a backend and the preparer that puts charts where
// that backend reads them.
type BackendFactory func(Env) (backend.Backend, controller.Preparer, error)

// Options configure the operator.
type Options struct {
	Profile controller.Profile
	// Backend names the entry of Backends to use.
	Backend  string
	Backends map[string]BackendFactory
	// AddToScheme registers extra types a backend needs.
	AddToScheme []func(*runtime.Scheme) error

	// IndexFetchers fetch repository indexes by URL scheme; Policy admits
	// indexes and versions. A distribution adds transports and trust
	// rules here.
	IndexFetchers repo.Fetchers
	Policy        repo.Policy

	// Mirror, an oci:// registry path, is where every chart and package
	// tree is fetched from instead of where it was published, for
	// air-gapped clusters; kubepkg bundle import fills it.
	Mirror string

	// Webhook settings for the image policy (Profile.ImagePolicy): the
	// Service in front of the operator, its namespace, the Secret for the
	// serving certificate and the MutatingWebhookConfiguration.
	WebhookPort                                                    int
	WebhookService, WebhookNamespace, WebhookSecret, WebhookConfig string

	// NelmBinary is the nelm executable (werf backend).
	NelmBinary string

	// ArgoNamespace and ArgoProject place Applications (argo backend).
	ArgoNamespace string
	ArgoProject   string

	CacheDir       string
	RegistryConfig string
	PlainHTTP      bool

	MetricsAddr string
	ProbeAddr   string
	LeaderElect bool
	// LeaderElectionID defaults to <binary>.<group>.
	LeaderElectionID string
}

// DefaultOptions are a plain kubepkg installation with the helm and flux
// backends.
func DefaultOptions() *Options {
	return &Options{
		Profile:        controller.DefaultProfile(),
		Backend:        "helm",
		Backends:       map[string]BackendFactory{"helm": HelmBackend, "flux": FluxBackend, "argo": ArgoBackend, "werf": WerfBackend},
		AddToScheme:    []func(*runtime.Scheme) error{helmv2.AddToScheme},
		IndexFetchers:  repo.DefaultFetchers(),
		Policy:         repo.AllowAll{},
		CacheDir:       filepath.Join(os.TempDir(), "kubepkg"),
		NelmBinary:     "nelm",
		WebhookPort:    9443,
		WebhookService: "kubepkg-webhook", WebhookSecret: "kubepkg-webhook-tls", WebhookConfig: "kubepkg-images",
		MetricsAddr: ":8080",
		ProbeAddr:   ":8081",
	}
}

// BindFlags registers the standard flags. A distribution may bind them,
// some of them, or none and configure Options in code.
func (o *Options) BindFlags(fs *flag.FlagSet) {
	names := make([]string, 0, len(o.Backends))
	for n := range o.Backends {
		names = append(names, n)
	}
	sort.Strings(names)
	fs.StringVar(&o.Profile.Group, "api-group", o.Profile.Group, "API group the kubepkg types are served under")
	fs.StringVar(&o.Profile.ValuesSecret, "values-secret", o.Profile.ValuesSecret, "namespace/name of a Secret whose values.yaml is layered under every component's values")
	fs.Var((*labelsFlag)(&o.Profile.NamespaceLabels), "namespace-label", "label key=value put on every namespace packages create (repeatable)")
	fs.StringVar(&o.Backend, "backend", o.Backend, "backend: "+strings.Join(names, ", "))
	fs.StringVar(&o.ArgoNamespace, "argo-namespace", o.ArgoNamespace, "namespace Argo CD watches for Applications (argo backend, default argocd)")
	fs.StringVar(&o.ArgoProject, "argo-project", o.ArgoProject, "Argo CD project of the Applications (argo backend, default default)")
	fs.StringVar(&o.Mirror, "mirror", o.Mirror, "oci:// registry path to fetch every chart and package tree from, for air-gapped clusters")
	fs.StringVar(&o.Profile.ImagePolicy, "image-policy", admission.ModeOff, "off, warn or enforce: keep pods in package namespaces on the images their packages pin")
	fs.IntVar(&o.WebhookPort, "webhook-port", o.WebhookPort, "port of the image policy webhook")
	fs.StringVar(&o.WebhookService, "webhook-service", o.WebhookService, "Service in front of the image policy webhook")
	fs.StringVar(&o.WebhookNamespace, "webhook-namespace", os.Getenv("POD_NAMESPACE"), "namespace of that Service and of the certificate Secret")
	fs.StringVar(&o.WebhookSecret, "webhook-secret", o.WebhookSecret, "Secret for the webhook's serving certificate")
	fs.StringVar(&o.WebhookConfig, "webhook-config", o.WebhookConfig, "MutatingWebhookConfiguration to give the CA bundle")
	fs.StringVar(&o.NelmBinary, "nelm-binary", o.NelmBinary, "nelm executable (werf backend)")
	fs.StringVar(&o.CacheDir, "cache-dir", o.CacheDir, "where package trees and charts are kept (helm backend)")
	fs.StringVar(&o.RegistryConfig, "registry-config", o.RegistryConfig, "Docker config file with registry credentials")
	fs.BoolVar(&o.PlainHTTP, "plain-http", o.PlainHTTP, "talk to OCI registries without TLS (local test registries only)")
	fs.StringVar(&o.MetricsAddr, "metrics-bind-address", o.MetricsAddr, "metrics endpoint")
	fs.StringVar(&o.ProbeAddr, "health-probe-bind-address", o.ProbeAddr, "health probe endpoint")
	fs.BoolVar(&o.LeaderElect, "leader-elect", o.LeaderElect, "enable leader election")
}

// HelmBackend installs charts in-process with the Helm SDK.
func HelmBackend(env Env) (backend.Backend, controller.Preparer, error) {
	o := env.Options
	if err := os.MkdirAll(o.CacheDir, 0o755); err != nil {
		return nil, nil, err
	}
	b, err := helm.New(env.Config)
	if err != nil {
		return nil, nil, err
	}
	return b, &controller.OCIPreparer{
		Fetcher: &source.Fetcher{CacheDir: o.CacheDir, PlainHTTP: o.PlainHTTP, CredentialsFile: o.RegistryConfig, Mirror: o.Mirror},
		WorkDir: filepath.Join(o.CacheDir, "composed"),
	}, nil
}

// FluxBackend installs through Flux: an OCIRepository or HelmRepository
// and a HelmRelease per component.
func FluxBackend(env Env) (backend.Backend, controller.Preparer, error) {
	return &flux.Backend{Client: env.Client, Insecure: env.Options.PlainHTTP}, controller.ChartPreparer{Mirror: env.Options.Mirror}, nil
}

// WerfBackend installs through Nelm, werf's deployment engine, with charts
// prepared as for the helm backend.
func WerfBackend(env Env) (backend.Backend, controller.Preparer, error) {
	o := env.Options
	secrets, err := kubernetes.NewForConfig(env.Config)
	if err != nil {
		return nil, nil, err
	}
	b, err := werf.NewBackend(o.NelmBinary, filepath.Join(o.CacheDir, "nelm"), env.Config, secrets)
	if err != nil {
		return nil, nil, err
	}
	return b, &controller.OCIPreparer{
		Fetcher: &source.Fetcher{CacheDir: o.CacheDir, PlainHTTP: o.PlainHTTP, CredentialsFile: o.RegistryConfig, Mirror: o.Mirror},
		WorkDir: filepath.Join(o.CacheDir, "composed"),
	}, nil
}

// ArgoBackend installs through Argo CD: an Application per component.
func ArgoBackend(env Env) (backend.Backend, controller.Preparer, error) {
	secrets, err := kubernetes.NewForConfig(env.Config)
	if err != nil {
		return nil, nil, err
	}
	return &argo.Backend{Client: env.Client, Secrets: secrets, Namespace: env.Options.ArgoNamespace, Project: env.Options.ArgoProject}, controller.ChartPreparer{Mirror: env.Options.Mirror}, nil
}

// Run starts the operator and blocks until ctx is done.
func Run(ctx context.Context, cfg *rest.Config, o *Options) error {
	factory, ok := o.Backends[o.Backend]
	if !ok {
		return fmt.Errorf("unknown backend %q", o.Backend)
	}
	scheme := runtime.NewScheme()
	adds := append([]func(*runtime.Scheme) error{corev1.AddToScheme, admissionregistrationv1.AddToScheme, v1beta1.AddToSchemeForGroup(o.Profile.Group)}, o.AddToScheme...)
	for _, add := range adds {
		if err := add(scheme); err != nil {
			return err
		}
	}
	id := o.LeaderElectionID
	if id == "" {
		id = filepath.Base(os.Args[0]) + "." + o.Profile.Group
	}
	if o.Profile.ImagePolicy == "" {
		o.Profile.ImagePolicy = admission.ModeOff
	}
	var certs *admission.Certs
	var webhookServer webhook.Server
	switch o.Profile.ImagePolicy {
	case admission.ModeOff:
	case admission.ModeWarn, admission.ModeEnforce:
		if o.WebhookNamespace == "" {
			return fmt.Errorf("--image-policy %s needs --webhook-namespace (or POD_NAMESPACE)", o.Profile.ImagePolicy)
		}
		direct, err := client.New(cfg, client.Options{Scheme: scheme})
		if err != nil {
			return err
		}
		certs = &admission.Certs{Client: direct, Namespace: o.WebhookNamespace, Service: o.WebhookService,
			Secret: o.WebhookSecret, Dir: filepath.Join(o.CacheDir, "webhook-certs"), Webhook: o.WebhookConfig}
		// The certificate must exist before the webhook server starts.
		if err := certs.Ensure(ctx); err != nil {
			return fmt.Errorf("webhook certificate: %w", err)
		}
		webhookServer = webhook.NewServer(webhook.Options{Port: o.WebhookPort, CertDir: certs.Dir})
	default:
		return fmt.Errorf("--image-policy %q: off, warn or enforce", o.Profile.ImagePolicy)
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		WebhookServer:          webhookServer,
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: o.MetricsAddr},
		HealthProbeBindAddress: o.ProbeAddr,
		LeaderElection:         o.LeaderElect,
		LeaderElectionID:       id,
	})
	if err != nil {
		return err
	}
	be, pre, err := factory(Env{Config: cfg, Client: mgr.GetClient(), Options: o})
	if err != nil {
		return fmt.Errorf("backend %s: %w", o.Backend, err)
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return err
	}
	repos := &controller.Repositories{Store: repo.NewStore(), Fetchers: o.IndexFetchers, Policy: o.Policy}
	r := &controller.PackageReconciler{
		Client:       mgr.GetClient(),
		Profile:      o.Profile,
		Backend:      be,
		Preparer:     pre,
		APIs:         &controller.DiscoveryAPIs{Client: dc},
		CRDs:         &controller.MetadataCRDs{Client: mgr.GetClient()},
		Repositories: repos,
	}
	if err := r.SetupWithManager(mgr); err != nil {
		return err
	}
	if err := (&controller.RepositoryReconciler{Client: mgr.GetClient(), Repositories: repos}).SetupWithManager(mgr); err != nil {
		return err
	}
	// Hub: Clusters and PackageSets. Kubeconfig Secrets are read straight
	// from the API server, so the operator does not cache every Secret.
	members := &controller.KubeconfigClients{Hub: mgr.GetAPIReader(), Group: o.Profile.Group}
	if err := (&controller.ClusterReconciler{Client: mgr.GetClient(), Members: members}).SetupWithManager(mgr); err != nil {
		return err
	}
	if err := (&controller.PackageSetReconciler{Client: mgr.GetClient(), Members: members}).SetupWithManager(mgr); err != nil {
		return err
	}
	if certs != nil {
		mgr.GetWebhookServer().Register("/mutate-pods", &webhook.Admission{Handler: &admission.Handler{Reader: mgr.GetClient(), Decoder: ctradmission.NewDecoder(scheme)}})
		if err := mgr.Add(certs); err != nil {
			return err
		}
		if err := mgr.AddReadyzCheck("webhook", mgr.GetWebhookServer().StartedChecker()); err != nil {
			return err
		}
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}
	ctrl.Log.WithName("setup").Info("starting", "version", version.Version, "apiGroup", o.Profile.Group, "backend", o.Backend)
	return mgr.Start(ctx)
}

// labelsFlag collects repeated key=value flags into a map.
type labelsFlag map[string]string

func (l *labelsFlag) String() string {
	parts := make([]string, 0, len(*l))
	for k, v := range *l {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func (l *labelsFlag) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if !ok || k == "" {
		return fmt.Errorf("want key=value, got %q", s)
	}
	if *l == nil {
		*l = map[string]string{}
	}
	(*l)[k] = v
	return nil
}
