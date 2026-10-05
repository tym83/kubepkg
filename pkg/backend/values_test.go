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

package backend

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func secret(ns, name, values string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Data:       map[string][]byte{"values.yaml": []byte(values)},
	}
}

func TestValuesLayerSecretsUnderComponent(t *testing.T) {
	secrets := fake.NewClientset(
		secret("kubepkg-system", "platform", "cluster:\n  domain: example.org\n  issuer: letsencrypt\nreplicas: 1\n"),
		secret("app", "local", "cluster:\n  issuer: internal\n"),
	)
	got, err := ResolveValues(context.Background(), secrets, Component{
		Namespace:         "app",
		ValuesFromSecrets: []string{"kubepkg-system/platform", "local"},
		Values:            map[string]any{"replicas": 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"cluster":  map[string]any{"domain": "example.org", "issuer": "internal"},
		"replicas": 3,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestValuesMissingSecretFails(t *testing.T) {
	secrets := fake.NewClientset()
	_, err := ResolveValues(context.Background(), secrets, Component{Namespace: "app", ValuesFromSecrets: []string{"kubepkg-system/platform"}})
	if err == nil {
		t.Fatal("a missing values secret must fail the release, not install it unconfigured")
	}
}
