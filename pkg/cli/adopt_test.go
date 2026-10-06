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

package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tym83/kubepkg/api/v1beta1"
	"github.com/tym83/kubepkg/pkg/backend/helm"
	"github.com/tym83/kubepkg/pkg/controller"
	"github.com/tym83/kubepkg/pkg/repo"
)

type releases map[string]*helm.ReleaseInfo

func (r releases) Release(ns, name string) (*helm.ReleaseInfo, error) { return r[ns+"/"+name], nil }

func adoptStore() *repo.Store {
	spec := v1beta1.PackageSourceSpec{Version: "1.21.2", Variants: []v1beta1.Variant{{
		Name:     "default",
		Requires: []v1beta1.Requirement{{Package: "networking"}},
		Components: []v1beta1.Component{
			{Name: "cert-manager", Chart: &v1beta1.ChartRef{Repository: "oci://r/p", Name: "cert-manager", Version: "1.21.2-2"}, Install: &v1beta1.ComponentInstall{Namespace: "cert-manager"}},
			{Name: "issuers", Chart: &v1beta1.ChartRef{Repository: "oci://r/p", Name: "issuers", Version: "1.21.2-2"}, Install: &v1beta1.ComponentInstall{Namespace: "cert-manager", DependsOn: []string{"cert-manager"}}},
		},
	}}}
	s := repo.NewStore()
	s.Set("main", 0, &repo.Index{Packages: map[string]repo.Package{"cert-manager": {Versions: []repo.Version{{Version: "1.21.2", Build: 2, Spec: spec}}}}})
	return s
}

func TestAdoptCarriesReleasesAndTheirValues(t *testing.T) {
	ctx := context.Background()
	found := releases{"security/certs": {Chart: "cert-manager", ChartVersion: "v1.20.0", AppVersion: "v1.20.0", Revision: 7, Status: "deployed", Values: map[string]any{"replicaCount": 3}}}
	plan, err := PlanAdopt(ctx, adoptStore(), repo.AllowAll{}, found, map[string]bool{}, "cert-manager", "", "", map[string]string{"cert-manager": "security/certs"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Components) != 2 || plan.Components[0].Existing == nil || plan.Components[1].Existing != nil {
		t.Fatalf("components: %+v", plan.Components)
	}
	if strings.Join(plan.Missing, ",") != "networking" {
		t.Fatalf("missing: %v", plan.Missing)
	}
	pkg, err := plan.PackageObject("")
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Annotations[controller.AnnotationAdopt] != "true" || pkg.Spec.Version != "~1.21" || pkg.Spec.Repository != "main" {
		t.Fatalf("package: %+v", pkg)
	}
	cm := pkg.Spec.Components["cert-manager"]
	if cm.Namespace != "security" || cm.ReleaseName != "certs" {
		t.Fatalf("placement: %+v", cm)
	}
	var values map[string]any
	if err := json.Unmarshal(cm.Values.Raw, &values); err != nil || values["replicaCount"] != float64(3) {
		t.Fatalf("values: %s %v", cm.Values.Raw, err)
	}
	if _, ok := pkg.Spec.Components["issuers"]; ok {
		t.Fatal("a component with nothing to carry over got an entry")
	}
}

func TestAdoptRefusesAnotherPackagesRelease(t *testing.T) {
	found := releases{"cert-manager/cert-manager": {ManagedBy: "platform-certs"}}
	_, err := PlanAdopt(context.Background(), adoptStore(), repo.AllowAll{}, found, nil, "cert-manager", "", "", nil)
	if err == nil || !strings.Contains(err.Error(), "belongs to package platform-certs") {
		t.Fatalf("got %v", err)
	}
	_, err = PlanAdopt(context.Background(), adoptStore(), repo.AllowAll{}, releases{}, nil, "cert-manager", "", "", map[string]string{"nope": "a/b"})
	if err == nil || !strings.Contains(err.Error(), "no component nope") {
		t.Fatalf("unknown component: %v", err)
	}
}
