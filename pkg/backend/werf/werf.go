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

// Package werf is the backend that installs through Nelm, the deployment
// engine of werf: the operator prepares charts as for the helm backend and
// runs nelm for each component. Nelm keeps Helm-compatible release
// history, so this backend can roll back. The nelm binary ships in the
// operator image.
package werf

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/yaml"

	"github.com/kuberoot-dev/kubepkg/pkg/backend"
)

// Runner runs nelm with arguments and returns its combined output.
type Runner func(ctx context.Context, args ...string) ([]byte, error)

// Backend runs nelm.
type Backend struct {
	// Run runs the nelm binary; NewBackend sets it.
	Run Runner
	// Kubeconfig is the file nelm talks to the cluster with.
	Kubeconfig string
	// TempDir receives values files.
	TempDir string
	// Secrets reads the platform values Secret.
	Secrets kubernetes.Interface
	// HistoryLimit bounds stored release revisions; it must exceed the
	// PackageRevision history, or rollbacks would target pruned releases.
	HistoryLimit int
}

// NewBackend writes a kubeconfig for nelm from cfg, so it acts with the
// operator's own identity, and runs binary.
func NewBackend(binary, tempDir string, cfg *rest.Config, secrets kubernetes.Interface) (*Backend, error) {
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		return nil, err
	}
	kc := filepath.Join(tempDir, "kubeconfig")
	if err := writeKubeconfig(cfg, kc); err != nil {
		return nil, err
	}
	run := func(ctx context.Context, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = append(os.Environ(), "NELM_TEMP_DIR="+tempDir)
		return cmd.CombinedOutput()
	}
	return &Backend{Run: run, Kubeconfig: kc, TempDir: tempDir, Secrets: secrets, HistoryLimit: 20}, nil
}

func writeKubeconfig(cfg *rest.Config, path string) error {
	cluster := &clientcmdapi.Cluster{Server: cfg.Host, CertificateAuthority: cfg.CAFile, CertificateAuthorityData: cfg.CAData, InsecureSkipTLSVerify: cfg.Insecure, TLSServerName: cfg.ServerName}
	user := &clientcmdapi.AuthInfo{Token: cfg.BearerToken, TokenFile: cfg.BearerTokenFile, ClientCertificate: cfg.CertFile, ClientCertificateData: cfg.CertData, ClientKey: cfg.KeyFile, ClientKeyData: cfg.KeyData}
	kc := clientcmdapi.NewConfig()
	kc.Clusters["operator"] = cluster
	kc.AuthInfos["operator"] = user
	kc.Contexts["operator"] = &clientcmdapi.Context{Cluster: "operator", AuthInfo: "operator"}
	kc.CurrentContext = "operator"
	return clientcmd.WriteToFile(*kc, path)
}

// Name implements backend.Backend.
func (b *Backend) Name() string { return "werf" }

func (b *Backend) common(c backend.Component) []string {
	return []string{"-n", c.Namespace, "-r", c.ReleaseName, "--kube-config", b.Kubeconfig, "--color-mode", "off", "--log-level", "error"}
}

// Apply installs or upgrades the release with nelm and waits for it.
func (b *Backend) Apply(ctx context.Context, c backend.Component) (backend.State, error) {
	if c.ChartDir == "" {
		return backend.State{}, errors.New("the werf backend needs a prepared chart directory")
	}
	values, err := backend.ResolveValues(ctx, b.Secrets, c)
	if err != nil {
		return backend.State{}, err
	}
	raw, err := yaml.Marshal(values)
	if err != nil {
		return backend.State{}, err
	}
	vf, err := os.CreateTemp(b.TempDir, "values-*.yaml")
	if err != nil {
		return backend.State{}, err
	}
	defer os.Remove(vf.Name())
	if _, err := vf.Write(raw); err != nil {
		vf.Close()
		return backend.State{}, err
	}
	if err := vf.Close(); err != nil {
		return backend.State{}, err
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	args := append([]string{"release", "install"}, b.common(c)...)
	// The package decides about rollbacks for all its components together.
	args = append(args, "--values", vf.Name(), "--timeout", timeout.String(), "--auto-rollback=false",
		"--no-show-progress", "--no-pod-logs", "--release-history-limit", strconv.Itoa(b.HistoryLimit))
	if c.Adopt {
		args = append(args, "--force-adoption")
	}
	for _, k := range sortedKeys(c.Labels) {
		args = append(args, "--release-labels", k+"="+c.Labels[k])
	}
	args = append(args, c.ChartDir)
	out, err := b.Run(ctx, args...)
	if err != nil {
		st, _ := b.Status(ctx, c)
		st.Failed, st.Ready = true, false
		st.Message = lastLine(out)
		return st, fmt.Errorf("nelm release install %s: %s", c.Key(), lastLine(out))
	}
	return b.Status(ctx, c)
}

type releaseGet struct {
	Release *struct {
		Revision int    `json:"revision"`
		Status   string `json:"status"`
	} `json:"release"`
}

// Status reads the release with nelm release get.
func (b *Backend) Status(ctx context.Context, c backend.Component) (backend.State, error) {
	args := append([]string{"release", "get"}, b.common(c)...)
	args = append(args, "--output-format", "json")
	out, err := b.Run(ctx, args...)
	if err != nil {
		if notFound(out) {
			return backend.State{}, nil
		}
		return backend.State{}, fmt.Errorf("nelm release get %s: %s", c.Key(), lastLine(out))
	}
	var rg releaseGet
	if err := json.Unmarshal(jsonPart(out), &rg); err != nil || rg.Release == nil {
		return backend.State{}, fmt.Errorf("nelm release get %s: unexpected output: %s", c.Key(), lastLine(out))
	}
	st := backend.State{Exists: true, Revision: rg.Release.Revision, Message: rg.Release.Status}
	switch rg.Release.Status {
	case "deployed":
		st.Ready = true
	case "failed":
		st.Failed = true
	default:
		st.Progressing = true
	}
	return st, nil
}

// Rollback re-deploys an earlier release revision.
func (b *Backend) Rollback(ctx context.Context, c backend.Component, toRevision int) (backend.State, error) {
	args := append([]string{"release", "rollback"}, b.common(c)...)
	args = append(args, "--no-show-progress", "--no-pod-logs", strconv.Itoa(toRevision))
	if out, err := b.Run(ctx, args...); err != nil {
		return backend.State{}, fmt.Errorf("nelm release rollback %s to %d: %s", c.Key(), toRevision, lastLine(out))
	}
	return b.Status(ctx, c)
}

// Uninstall removes the release; a release that is gone is fine.
func (b *Backend) Uninstall(ctx context.Context, c backend.Component) error {
	args := append([]string{"release", "uninstall"}, b.common(c)...)
	args = append(args, "--no-show-progress", "--no-pod-logs")
	if out, err := b.Run(ctx, args...); err != nil && !notFound(out) {
		return fmt.Errorf("nelm release uninstall %s: %s", c.Key(), lastLine(out))
	}
	return nil
}

func notFound(out []byte) bool {
	s := strings.ToLower(string(out))
	return strings.Contains(s, "not found")
}

// jsonPart drops anything nelm logged before the JSON document.
func jsonPart(out []byte) []byte {
	if i := strings.Index(string(out), "{"); i >= 0 {
		return out[i:]
	}
	return out
}

func lastLine(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return lines[len(lines)-1]
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var _ backend.Backend = (*Backend)(nil)
