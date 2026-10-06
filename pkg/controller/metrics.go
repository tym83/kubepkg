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
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/tym83/kubepkg/api/v1alpha1"
)

// Revision outcomes counted by kubepkg_revisions_total.
const (
	outcomeApplied    = "applied"
	outcomeFailed     = "failed"
	outcomeRolledBack = "rolled_back"
)

var (
	packageReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kubepkg_package_ready",
		Help: "Whether the package is ready (1) or not (0).",
	}, []string{"package"})
	packageInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kubepkg_package_info",
		Help: "The version and revision a package runs; always 1.",
	}, []string{"package", "version", "revision"})
	revisionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kubepkg_revisions_total",
		Help: "Package revisions by outcome: applied, failed, rolled_back.",
	}, []string{"package", "outcome"})
	repositoryReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kubepkg_repository_ready",
		Help: "Whether the repository's index is loaded and accepted (1) or not (0).",
	}, []string{"repository"})
	repositoryGenerated = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kubepkg_repository_index_generated_timestamp_seconds",
		Help: "When the accepted index of the repository was built.",
	}, []string{"repository"})
)

func init() {
	metrics.Registry.MustRegister(packageReady, packageInfo, revisionsTotal, repositoryReady, repositoryGenerated)
}

// recordPackage publishes a package's state after a reconcile.
func recordPackage(pkg *v1alpha1.Package) {
	ready := 0.0
	if isReady(pkg.Status.Conditions) {
		ready = 1
	}
	packageReady.WithLabelValues(pkg.Name).Set(ready)
	packageInfo.DeletePartialMatch(prometheus.Labels{"package": pkg.Name})
	if pkg.Status.Version != "" {
		packageInfo.WithLabelValues(pkg.Name, pkg.Status.Version, strconv.FormatInt(pkg.Status.CurrentRevision, 10)).Set(1)
	}
}

// forgetPackage drops a removed package's series.
func forgetPackage(name string) {
	packageReady.DeleteLabelValues(name)
	packageInfo.DeletePartialMatch(prometheus.Labels{"package": name})
	revisionsTotal.DeletePartialMatch(prometheus.Labels{"package": name})
}

func countRevision(pkg, outcome string) {
	revisionsTotal.WithLabelValues(pkg, outcome).Inc()
}

func recordRepository(rp *v1alpha1.Repository, ok bool) {
	v := 0.0
	if ok {
		v = 1
	}
	repositoryReady.WithLabelValues(rp.Name).Set(v)
	if rp.Status.IndexGenerated != nil {
		repositoryGenerated.WithLabelValues(rp.Name).Set(float64(rp.Status.IndexGenerated.Unix()))
	}
}

func forgetRepository(name string) {
	repositoryReady.DeleteLabelValues(name)
	repositoryGenerated.DeleteLabelValues(name)
}
