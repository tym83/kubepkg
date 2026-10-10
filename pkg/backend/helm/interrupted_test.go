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
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/release/common"
	releasev1 "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage"
	"helm.sh/helm/v4/pkg/storage/driver"

	"github.com/kuberoot-dev/kubepkg/pkg/backend"
)

func TestAReleaseLeftPendingIsRecovered(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	c := backend.Component{Namespace: "app", ReleaseName: "app", Timeout: 5 * time.Minute}
	setup := func(status common.Status, started time.Time) (*Backend, *action.Configuration) {
		cfg := &action.Configuration{Releases: storage.Init(driver.NewMemory())}
		rel := &releasev1.Release{Name: "app", Namespace: "app", Version: 1, Info: &releasev1.Info{Status: status, LastDeployed: started}}
		if err := cfg.Releases.Create(rel); err != nil {
			t.Fatal(err)
		}
		return &Backend{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, cfg
	}
	status := func(cfg *action.Configuration) common.Status {
		last, err := cfg.Releases.Last("app")
		if err != nil {
			t.Fatal(err)
		}
		return last.(*releasev1.Release).Info.Status
	}

	// Left by a process that went down long ago: marked failed.
	b, cfg := setup(common.StatusPendingInstall, now.Add(-time.Hour))
	if waiting, err := b.recoverInterrupted(cfg, c, now); err != nil || waiting != "" {
		t.Fatalf("an old pending install: %q %v", waiting, err)
	}
	if s := status(cfg); s != common.StatusFailed {
		t.Fatalf("status %s, want failed", s)
	}

	// Younger than the timeout: someone may still be at it.
	b, cfg = setup(common.StatusPendingUpgrade, now.Add(-time.Minute))
	if waiting, _ := b.recoverInterrupted(cfg, c, now); !strings.Contains(waiting, "pending-upgrade") || status(cfg) != common.StatusPendingUpgrade {
		t.Fatalf("a recent pending upgrade was touched: %q", waiting)
	}

	// This process is running it: never touched, however old.
	b, cfg = setup(common.StatusPendingInstall, now.Add(-time.Hour))
	end := b.begin(c.Key())
	if waiting, _ := b.recoverInterrupted(cfg, c, now); waiting == "" || status(cfg) != common.StatusPendingInstall {
		t.Fatal("a release this process is installing was marked failed")
	}
	end()

	// Deployed releases are left alone.
	b, cfg = setup(common.StatusDeployed, now.Add(-time.Hour))
	if waiting, err := b.recoverInterrupted(cfg, c, now); waiting != "" || err != nil || status(cfg) != common.StatusDeployed {
		t.Fatal("a deployed release was touched")
	}
}

func TestForceConflictsOnlyForServerSideReleases(t *testing.T) {
	cfg := &action.Configuration{Releases: storage.Init(driver.NewMemory())}
	for v, method := range map[int]string{1: "", 2: "csa", 3: "ssa"} {
		rel := &releasev1.Release{Name: "app", Namespace: "app", Version: v, ApplyMethod: method, Info: &releasev1.Info{Status: common.StatusSuperseded}}
		if err := cfg.Releases.Create(rel); err != nil {
			t.Fatal(err)
		}
	}
	for rev, want := range map[int]bool{1: false, 2: false, 3: true, 0: true} {
		if got := appliedServerSide(cfg, "app", rev); got != want {
			t.Errorf("revision %d: %v, want %v", rev, got, want)
		}
	}
}
