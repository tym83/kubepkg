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

package operator

import (
	"context"
	"testing"

	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestAMissingRegistrySecretMeansNoCredentials(t *testing.T) {
	present := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "kubepkg-system", Name: "creds"},
		Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)}}
	cs := fake.NewClientset(present)
	var warnings int
	log := funcr.New(func(_, _ string) { warnings++ }, funcr.Options{})
	load := registrySecrets(cs, []string{"kubepkg-system/creds", "kubepkg-system/missing"}, log)
	for i := 0; i < 3; i++ {
		got, err := load(context.Background())
		if err != nil {
			t.Fatalf("a missing Secret failed the pull: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("configs: %d", len(got))
		}
	}
	if warnings != 1 {
		t.Fatalf("warnings: %d, want one until the Secret appears", warnings)
	}
	// Once it exists it is used.
	_, _ = cs.CoreV1().Secrets("kubepkg-system").Create(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "kubepkg-system", Name: "missing"},
		Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)}}, metav1.CreateOptions{})
	if got, _ := load(context.Background()); len(got) != 2 {
		t.Fatalf("configs after the Secret appeared: %d", len(got))
	}
}
