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

package werf

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/tym83/kubepkg/pkg/backend"
)

// nelm is a fake nelm: it records calls and answers release get.
type nelm struct {
	calls  [][]string
	values []string
	status string // for release get; "" means not found
	fail   bool   // release install fails
}

func (n *nelm) run(_ context.Context, args ...string) ([]byte, error) {
	n.calls = append(n.calls, args)
	switch args[1] {
	case "install":
		for i, a := range args {
			if a == "--values" {
				raw, _ := os.ReadFile(args[i+1])
				n.values = append(n.values, string(raw))
			}
		}
		if n.fail {
			n.status = "failed"
			return []byte("Error: resources not ready: Deployment/app"), errors.New("exit status 1")
		}
		n.status = "deployed"
	case "get":
		if n.status == "" {
			return []byte("Error: release \"app\" (namespace \"app\") not found"), errors.New("exit status 1")
		}
		return []byte(`{"apiVersion":"v2","release":{"name":"app","namespace":"app","revision":3,"status":"` + n.status + `"}}`), nil
	case "uninstall":
		if n.status == "" {
			return []byte("release not found"), errors.New("exit status 1")
		}
		n.status = ""
	}
	return nil, nil
}

func newBackend(t *testing.T, n *nelm) *Backend {
	secrets := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kubepkg-system", Name: "platform"},
		Data:       map[string][]byte{"values.yaml": []byte("domain: example.org\n")},
	})
	return &Backend{Run: n.run, Kubeconfig: "/kc", TempDir: t.TempDir(), Secrets: secrets, HistoryLimit: 20}
}

func component() backend.Component {
	return backend.Component{
		Package: "app", Name: "app", ReleaseName: "app", Namespace: "app", ChartDir: "/charts/app",
		Values: map[string]any{"replicas": 2}, ValuesFromSecrets: []string{"kubepkg-system/platform"},
		Labels: map[string]string{"kubepkg.dev/package": "app"}, Timeout: 5 * time.Minute,
	}
}

func has(args []string, want ...string) bool {
	s := " " + strings.Join(args, " ") + " "
	for _, w := range want {
		if !strings.Contains(s, " "+w+" ") {
			return false
		}
	}
	return true
}

func TestApplyAndStatus(t *testing.T) {
	n := &nelm{}
	b := newBackend(t, n)
	ctx := context.Background()
	if st, err := b.Status(ctx, component()); err != nil || st.Exists {
		t.Fatalf("before install: %+v %v", st, err)
	}
	st, err := b.Apply(ctx, component())
	if err != nil || !st.Ready || st.Revision != 3 {
		t.Fatalf("apply: %+v %v", st, err)
	}
	install := n.calls[1]
	if !has(install, "release", "install", "-n app", "-r app", "--kube-config /kc", "--timeout 5m0s", "--auto-rollback=false", "--release-labels kubepkg.dev/package=app", "/charts/app") {
		t.Fatalf("install args: %v", install)
	}
	if v := n.values[0]; !strings.Contains(v, "domain: example.org") || !strings.Contains(v, "replicas: 2") {
		t.Fatalf("values file: %s", v)
	}

	n.status = "pending-upgrade"
	if st, _ := b.Status(ctx, component()); !st.Progressing {
		t.Fatalf("pending: %+v", st)
	}
	if _, err := b.Rollback(ctx, component(), 2); err != nil {
		t.Fatal(err)
	}
	if last := n.calls[len(n.calls)-2]; !has(last, "release", "rollback", "-r app", "2") {
		t.Fatalf("rollback args: %v", last)
	}
	if err := b.Uninstall(ctx, component()); err != nil {
		t.Fatal(err)
	}
	if err := b.Uninstall(ctx, component()); err != nil {
		t.Fatalf("uninstalling a release that is gone: %v", err)
	}
}

func TestFailedInstall(t *testing.T) {
	n := &nelm{fail: true}
	st, err := newBackend(t, n).Apply(context.Background(), component())
	if err == nil || !st.Failed || !strings.Contains(err.Error(), "resources not ready") {
		t.Fatalf("got %+v %v", st, err)
	}
}
