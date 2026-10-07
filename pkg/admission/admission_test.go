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

package admission

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/tym83/kubepkg/api/v1beta1"
)

var digest = "sha256:" + strings.Repeat("d", 64)

func env(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	sch := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, v1beta1.AddToScheme, admissionregistrationv1.AddToScheme} {
		if err := add(sch); err != nil {
			t.Fatal(err)
		}
	}
	src := &v1beta1.PackageSource{ObjectMeta: metav1.ObjectMeta{Name: "cert-manager"}, Spec: v1beta1.PackageSourceSpec{Images: []string{"quay.io/jetstack/cert-manager-controller:v1.21.2@" + digest}}}
	return fake.NewClientBuilder().WithScheme(sch).WithObjects(append(objs, src)...).Build()
}

func ns(name, mode string) *corev1.Namespace {
	n := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if mode != "" {
		n.Labels = map[string]string{LabelImagePolicy: mode}
	}
	return n
}

func admit(t *testing.T, c client.Client, namespace string, images ...string) admission.Response {
	t.Helper()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: namespace}}
	for _, i := range images {
		pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: "c", Image: i})
	}
	raw, _ := json.Marshal(pod)
	sch := runtime.NewScheme()
	_ = corev1.AddToScheme(sch)
	h := &Handler{Reader: c, Decoder: admission.NewDecoder(sch)}
	return h.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Namespace: namespace, Object: runtime.RawExtension{Raw: raw}}})
}

func TestPinnedTagsGetTheirDigest(t *testing.T) {
	c := env(t, ns("cert-manager", ModeEnforce), ns("free", ""), ns("soft", ModeWarn))
	r := admit(t, c, "cert-manager", "quay.io/jetstack/cert-manager-controller:v1.21.2")
	if !r.Allowed || len(r.Patches) != 1 || r.Patches[0].Value != "quay.io/jetstack/cert-manager-controller:v1.21.2@"+digest {
		t.Fatalf("a pinned tag: %+v", r)
	}
	if r := admit(t, c, "cert-manager", "quay.io/jetstack/cert-manager-controller@"+digest); !r.Allowed || len(r.Patches) != 0 {
		t.Fatalf("a pinned digest: %+v", r)
	}
	if r := admit(t, c, "cert-manager", "quay.io/jetstack/cert-manager-controller:v1.21.2", "docker.io/evil/miner:latest"); r.Allowed || !strings.Contains(r.Result.Message, "docker.io/evil/miner:latest") {
		t.Fatalf("an image no package pins, enforced: %+v", r)
	}
	if r := admit(t, c, "cert-manager", "quay.io/jetstack/cert-manager-controller@sha256:"+strings.Repeat("0", 64)); r.Allowed {
		t.Fatal("another digest of a pinned repository was allowed")
	}
	if r := admit(t, c, "soft", "docker.io/evil/miner:latest"); !r.Allowed || len(r.Warnings) != 1 {
		t.Fatalf("warn mode: %+v", r)
	}
	if r := admit(t, c, "free", "docker.io/evil/miner:latest"); !r.Allowed || len(r.Warnings) != 0 || len(r.Patches) != 0 {
		t.Fatalf("a namespace without the policy: %+v", r)
	}
}

func TestCertsAreIssuedSharedAndRenewed(t *testing.T) {
	webhook := &admissionregistrationv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "kubepkg-images"},
		Webhooks: []admissionregistrationv1.MutatingWebhook{{Name: "pods.kubepkg.dev"}}}
	c := env(t, webhook)
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	certs := func(dir string) *Certs {
		return &Certs{Client: c, Namespace: "kubepkg-system", Service: "kubepkg-webhook", Secret: "kubepkg-webhook-tls", Dir: dir, Webhook: "kubepkg-images", Now: func() time.Time { return now }}
	}
	a, b := certs(t.TempDir()), certs(t.TempDir())
	ctx := context.Background()
	if err := a.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	ca := readFile(t, a.Dir+"/tls.crt")
	if readFile(t, b.Dir+"/tls.crt") != ca {
		t.Fatal("two replicas got different certificates")
	}
	got := &admissionregistrationv1.MutatingWebhookConfiguration{}
	if err := c.Get(ctx, client.ObjectKey{Name: "kubepkg-images"}, got); err != nil || len(got.Webhooks[0].ClientConfig.CABundle) == 0 {
		t.Fatalf("CA bundle not set: %v", err)
	}
	// Eleven months on, the certificate is renewed.
	now = now.AddDate(0, 11, 15)
	if err := a.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	if readFile(t, a.Dir+"/tls.crt") == ca {
		t.Fatal("a certificate about to expire was not renewed")
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
