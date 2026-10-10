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
	"errors"
	"fmt"
	"time"

	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/release"
	"helm.sh/helm/v4/pkg/release/common"
	releasev1 "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage/driver"

	"github.com/kuberoot-dev/kubepkg/pkg/backend"
)

// begin marks a release as being operated on by this process until the
// returned function is called.
func (b *Backend) begin(key string) func() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running == nil {
		b.running = map[string]bool{}
	}
	b.running[key] = true
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.running, key)
	}
}

func (b *Backend) isRunning(key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.running[key]
}

// recoverInterrupted deals with a release left pending by an operation
// that never finished, because the operator or its node went down
// mid-install. Helm refuses every further action on a pending release,
// and nothing would ever move it on. A pending release this process is
// not working on, older than the component's timeout, which no Helm
// operation outlives, is marked failed, and the next action proceeds from
// there as after any failure. A younger one may belong to someone else
// still at work, so the caller waits: the returned text says for what.
func (b *Backend) recoverInterrupted(cfg *action.Configuration, c backend.Component, now time.Time) (string, error) {
	last, err := cfg.Releases.Last(c.ReleaseName)
	if errors.Is(err, driver.ErrReleaseNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("last release of %s: %w", c.Key(), err)
	}
	rel, ok := last.(*releasev1.Release)
	if !ok || rel.Info == nil || !rel.Info.Status.IsPending() {
		return "", nil
	}
	status := rel.Info.Status
	if b.isRunning(c.Key()) {
		return fmt.Sprintf("Helm operation %s in progress", status), nil
	}
	if age := now.Sub(rel.Info.LastDeployed); age < timeout(c) {
		return fmt.Sprintf("release %s is %s since %s ago; waiting for that operation, or for %s to pass", c.Key(), status, age.Round(time.Second), timeout(c)), nil
	}
	rel.Info.Status = common.StatusFailed
	rel.Info.Description = fmt.Sprintf("%s did not finish: the process running it ended; marked failed by kubepkg", status)
	if err := cfg.Releases.Update(rel); err != nil {
		return "", fmt.Errorf("mark interrupted release %s failed: %w", c.Key(), err)
	}
	b.Logger.Info("interrupted Helm release marked failed", "release", c.Key(), "was", string(status), "revision", rel.Version)
	return "", nil
}

// appliedServerSide reports whether a release revision, the last one for
// revision 0, was applied with server-side apply, which Helm then keeps
// using for it.
func appliedServerSide(cfg *action.Configuration, name string, revision int) bool {
	var r release.Releaser
	var err error
	if revision == 0 {
		r, err = cfg.Releases.Last(name)
	} else {
		r, err = cfg.Releases.Get(name, revision)
	}
	if err != nil {
		return true
	}
	rel, ok := r.(*releasev1.Release)
	return !ok || rel.ApplyMethod == string(releasev1.ApplyMethodServerSideApply)
}
