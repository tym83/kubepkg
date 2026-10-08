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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/fluxcd/pkg/apis/kustomize"
	"k8s.io/apimachinery/pkg/types"
	"path"
	"path/filepath"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/tym83/kubepkg/api/v1"
	"github.com/tym83/kubepkg/pkg/backend"
	"github.com/tym83/kubepkg/pkg/resolve"
	"github.com/tym83/kubepkg/pkg/source"
)

// Preparer puts a component's chart where the backend can read it.
type Preparer interface {
	// Prepare fills the chart location in c and returns a digest that
	// changes whenever the chart content changes.
	Prepare(ctx context.Context, src *v1.PackageSource, variant *v1.Variant, comp *v1.Component, c *backend.Component) (string, error)
}

// OCIPreparer puts charts on disk for the helm backend: published charts
// are downloaded as they are, charts in a package tree are composed from
// the tree.
type OCIPreparer struct {
	Fetcher *source.Fetcher
	WorkDir string
}

// Prepare implements Preparer.
func (p *OCIPreparer) Prepare(ctx context.Context, src *v1.PackageSource, variant *v1.Variant, comp *v1.Component, c *backend.Component) (string, error) {
	if ch := comp.Chart; ch != nil {
		dir, digest, err := p.Fetcher.FetchChart(ctx, source.Chart{Repository: ch.Repository, Name: ch.Name, Version: ch.Version, Digest: ch.Digest})
		if err != nil {
			return "", err
		}
		c.ChartDir = dir
		return digest, nil
	}
	ref := src.Spec.SourceRef
	if ref == nil || ref.Kind != v1.SourceKindOCIArtifact {
		kind := "none"
		if ref != nil {
			kind = ref.Kind
		}
		return "", fmt.Errorf("source kind %s needs the flux backend; the helm backend reads OCIArtifact sources", kind)
	}
	tree, treeDigest, err := p.Fetcher.Fetch(ctx, ref.URL)
	if err != nil {
		return "", err
	}
	libs := map[string]string{}
	byName := libraryPaths(variant)
	for _, l := range comp.Libraries {
		lp, ok := byName[l]
		if !ok {
			return "", fmt.Errorf("component %s uses library %s, which variant %s does not define", comp.Name, l, variant.Name)
		}
		libs[l] = lp
	}
	short := strings.TrimPrefix(treeDigest, "sha256:")
	if len(short) > 16 {
		short = short[:16]
	}
	dst := filepath.Join(p.WorkDir, src.Name, variant.Name, comp.Name, short)
	dir, digest, err := source.Compose(tree, ref.Path, source.ChartSpec{Path: comp.Path, Libraries: libs, ValuesFiles: comp.ValuesFiles}, dst)
	if err != nil {
		return "", fmt.Errorf("compose %s: %w", comp.Name, err)
	}
	c.ChartDir = dir
	return digest, nil
}

// libraryPaths maps library names to paths; a library without a name is
// known by the last element of its path.
func libraryPaths(v *v1.Variant) map[string]string {
	out := map[string]string{}
	for _, l := range v.Libraries {
		name := l.Name
		if name == "" {
			name = path.Base(strings.TrimSuffix(l.Path, "/"))
		}
		out[name] = l.Path
	}
	return out
}

// ChartPreparer hands published charts to backends that install through
// another tool (flux, argo). Such tools fetch charts themselves, so only
// components with chart can be installed this way; built packages are
// made of published charts.
type ChartPreparer struct {
	// Mirror, when set, is the registry the tool fetches every chart from
	// (see source.MirrorPath).
	Mirror string
}

// Prepare implements Preparer.
func (p ChartPreparer) Prepare(_ context.Context, src *v1.PackageSource, _ *v1.Variant, comp *v1.Component, c *backend.Component) (string, error) {
	ch := comp.Chart
	if ch == nil {
		return "", fmt.Errorf("component %s: this backend installs published charts; build the package or use chart instead of path", comp.Name)
	}
	// The digest below stays that of the published location, so turning a
	// mirror on does not make new revisions.
	m := source.MirrorChart(p.Mirror, source.Chart{Repository: ch.Repository, Name: ch.Name, Version: ch.Version, Digest: ch.Digest})
	c.Chart = &backend.Chart{Repository: m.Repository, Name: ch.Name, Version: ch.Version, Digest: ch.Digest}
	if ch.Digest != "" {
		return ch.Digest, nil
	}
	return digestOf(struct{ Repository, Name, Version string }{ch.Repository, ch.Name, ch.Version})
}

// desiredState is everything one package revision will apply.
type desiredState struct {
	version      string
	variant      string
	rollbackSafe bool
	components   []desiredComponent
	// hooks run before components when the version changes; they are not
	// part of a revision's snapshot, since they are gone once it applied.
	hooks []desiredComponent
	// digest identifies the desired state: Package spec, the retry
	// annotation and the PackageSource spec. Chart content is not part of
	// it; a tag that moves under the same spec is picked up by the
	// component digests instead.
	digest string
}

type desiredComponent struct {
	snapshot  v1.ComponentSnapshot
	backend   backend.Component
	readyWhen []v1.ReadyCondition
}

func digestOf(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func variantName(pkg *v1.Package) string {
	if pkg.Spec.Variant == "" {
		return "default"
	}
	return pkg.Spec.Variant
}

func findVariant(src *v1.PackageSource, name string) *v1.Variant {
	for i := range src.Spec.Variants {
		if src.Spec.Variants[i].Name == name {
			return &src.Spec.Variants[i]
		}
	}
	return nil
}

func sourceVersion(src *v1.PackageSource) string {
	if src.Spec.Version == "" {
		return resolve.Unversioned
	}
	return src.Spec.Version
}

// enabledComponents lists installable components the Package did not turn off.
func enabledComponents(pkg *v1.Package, v *v1.Variant) []v1.Component {
	var out []v1.Component
	for _, c := range v.Components {
		if c.Install == nil {
			continue
		}
		if o, ok := pkg.Spec.Components[c.Name]; ok && o.Enabled != nil && !*o.Enabled {
			continue
		}
		out = append(out, c)
	}
	return out
}

func releaseName(c v1.Component) string {
	if c.Install.ReleaseName != "" {
		return c.Install.ReleaseName
	}
	return c.Name
}

// buildDesired computes the desired state of a package. depReleases maps a
// package this one depends on to its releases (namespace/name), for the flux
// backend's dependsOn.
func (r *PackageReconciler) buildDesired(ctx context.Context, pkg *v1.Package, src *v1.PackageSource, v *v1.Variant, depReleases []string) (*desiredState, error) {
	d := &desiredState{
		version:      sourceVersion(src),
		variant:      v.Name,
		rollbackSafe: src.Spec.Rollback != nil && src.Spec.Rollback.Safe,
	}
	var err error
	d.digest, err = digestOf(struct {
		Package v1.PackageSpec
		Retry   string
		Source  v1.PackageSourceSpec
	}{pkg.Spec, pkg.Annotations[AnnotationRetry], src.Spec})
	if err != nil {
		return nil, err
	}

	comps := enabledComponents(pkg, v)
	byName := map[string]v1.Component{}
	for _, c := range comps {
		if c.Install.Namespace == "" {
			return nil, fmt.Errorf("component %s has empty namespace in Install section", c.Name)
		}
		byName[c.Name] = c
	}
	for _, c := range comps {
		for _, dep := range c.Install.DependsOn {
			if h, ok := byName[dep]; ok && h.Install.Phase == v1.PhasePreUpgrade && c.Install.Phase != v1.PhasePreUpgrade {
				return nil, fmt.Errorf("component %s depends on %s, a pre-upgrade hook, which runs only on upgrades", c.Name, dep)
			}
		}
	}
	order, err := topoOrder(comps)
	if err != nil {
		return nil, err
	}
	timeout := 10 * time.Minute
	if pkg.Spec.Upgrade != nil && pkg.Spec.Upgrade.Timeout != nil {
		timeout = pkg.Spec.Upgrade.Timeout.Duration
	}
	skipValues := src.Annotations[AnnotationSkipPlatformValues] == "true"

	for _, name := range order {
		c := byName[name]
		values := map[string]any{}
		if o, ok := pkg.Spec.Components[c.Name]; ok && o.Values != nil && len(o.Values.Raw) > 0 {
			if err := json.Unmarshal(o.Values.Raw, &values); err != nil {
				return nil, fmt.Errorf("values of component %s: %w", c.Name, err)
			}
		}
		ns, rel := placement(pkg, c)
		bc := backend.Component{
			Package:          pkg.Name,
			Name:             c.Name,
			ReleaseName:      rel,
			Namespace:        ns,
			Adopt:            pkg.Annotations[AnnotationAdopt] == "true",
			Values:           values,
			Labels:           map[string]string{v1.LabelPackage: pkg.Name},
			UpgradeCRDs:      c.Install.UpgradeCRDs,
			WaitStrategy:     c.Install.WaitStrategy,
			HealthCheckExprs: FluxHealthChecks(c.Install.HealthCheckExprs),
			Timeout:          timeout,
		}
		if c.Install.Privileged {
			bc.Labels[r.Profile.Group+"/privileged"] = "true"
		}
		if r.Profile.ValuesSecret != "" && !skipValues {
			bc.ValuesFromSecrets = []string{r.Profile.ValuesSecret}
		}
		if len(c.ValuesFiles) > 0 {
			bc.Annotations = map[string]string{AnnotationValuesFiles: strings.Join(c.ValuesFiles, ",")}
		}
		for _, dep := range c.Install.DependsOn {
			dc, ok := byName[dep]
			if !ok {
				return nil, fmt.Errorf("component %s not found in variant for dependency %s", dep, c.Name)
			}
			dns, drel := placement(pkg, dc)
			bc.DependsOn = append(bc.DependsOn, dns+"/"+drel)
		}
		bc.DependsOn = append(bc.DependsOn, depReleases...)

		comp := c
		chartDigest, err := r.Preparer.Prepare(ctx, src, v, &comp, &bc)
		if err != nil {
			return nil, err
		}
		valuesDigest, err := digestOf(values)
		if err != nil {
			return nil, err
		}
		dc := desiredComponent{
			snapshot: v1.ComponentSnapshot{
				Name:         c.Name,
				ReleaseName:  bc.ReleaseName,
				Namespace:    bc.Namespace,
				DependsOn:    c.Install.DependsOn,
				ChartDigest:  chartDigest,
				ValuesDigest: valuesDigest,
			},
			backend:   bc,
			readyWhen: c.Install.ReadyWhen,
		}
		if c.Install.Phase == v1.PhasePreUpgrade {
			d.hooks = append(d.hooks, dc)
		} else {
			d.components = append(d.components, dc)
		}
	}
	return d, nil
}

// FluxHealthChecks hands health checks to the flux backend in Flux's own
// type, which has the same shape.
func FluxHealthChecks(in []v1.HealthCheck) []kustomize.CustomHealthCheck {
	if len(in) == 0 {
		return nil
	}
	out := make([]kustomize.CustomHealthCheck, 0, len(in))
	for _, h := range in {
		out = append(out, kustomize.CustomHealthCheck{APIVersion: h.APIVersion, Kind: h.Kind, HealthCheckExpressions: kustomize.HealthCheckExpressions{Current: h.Current, InProgress: h.InProgress, Failed: h.Failed}})
	}
	return out
}

// placement is where a component goes: the package's install settings,
// unless the Package overrides them.
func placement(pkg *v1.Package, c v1.Component) (namespace, release string) {
	namespace, release = c.Install.Namespace, releaseName(c)
	if o, ok := pkg.Spec.Components[c.Name]; ok {
		if o.Namespace != "" {
			namespace = o.Namespace
		}
		if o.ReleaseName != "" {
			release = o.ReleaseName
		}
	}
	return namespace, release
}

// topoOrder sorts components so dependencies come first; ties keep the
// PackageSource order. A cycle is an error, not something to guess around.
func topoOrder(comps []v1.Component) ([]string, error) {
	index := map[string]int{}
	for i, c := range comps {
		index[c.Name] = i
	}
	state := map[string]int{} // 0 new, 1 visiting, 2 done
	var out []string
	var visit func(name string, chain []string) error
	visit = func(name string, chain []string) error {
		switch state[name] {
		case 1:
			return fmt.Errorf("components depend on each other in a cycle: %s", strings.Join(append(chain, name), " -> "))
		case 2:
			return nil
		}
		state[name] = 1
		deps := append([]string(nil), comps[index[name]].Install.DependsOn...)
		sort.SliceStable(deps, func(i, j int) bool { return index[deps[i]] < index[deps[j]] })
		for _, d := range deps {
			if _, ok := index[d]; !ok {
				continue // disabled or unknown; reported by buildDesired
			}
			if err := visit(d, append(chain, name)); err != nil {
				return err
			}
		}
		state[name] = 2
		out = append(out, name)
		return nil
	}
	for _, c := range comps {
		if err := visit(c.Name, nil); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// sameComponents reports whether a revision already recorded this state.
func sameComponents(a []v1.ComponentSnapshot, b []desiredComponent) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i].snapshot
		if x.Name != y.Name || x.ReleaseName != y.ReleaseName || x.Namespace != y.Namespace ||
			x.ChartDigest != y.ChartDigest || x.ValuesDigest != y.ValuesDigest {
			return false
		}
	}
	return true
}

// PackageNamespaces maps each namespace installed packages put components
// in to those packages' names.
func PackageNamespaces(ctx context.Context, c client.Reader) (map[string][]string, error) {
	var pkgs v1.PackageList
	if err := c.List(ctx, &pkgs); err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for i := range pkgs.Items {
		p := &pkgs.Items[i]
		src := &v1.PackageSource{}
		if err := c.Get(ctx, types.NamespacedName{Name: p.Name}, src); err != nil {
			continue
		}
		v := findVariant(src, variantName(p))
		if v == nil {
			continue
		}
		for _, comp := range enabledComponents(p, v) {
			ns, _ := placement(p, comp)
			if !slices.Contains(out[ns], p.Name) {
				out[ns] = append(out[ns], p.Name)
			}
		}
	}
	return out, nil
}
