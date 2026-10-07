# Embedding in a platform

kubepkg is meant to become part of someone else's platform: a Kubernetes distribution, a managed Kubernetes service, an internal developer platform. It needs nothing but its CRDs and its operator, and a platform adapts it at three levels, from configuration to its own binaries. No fork is needed at any level.

## Configuration

Operator flags, set through the chart, all off by default:

| Flag | Chart value | Does |
|---|---|---|
| `--api-group` | `apiGroup` | serves the types under the platform's group, such as `packages.example.org` instead of `kubepkg.dev` |
| `--values-secret ns/name` | `valuesSecret` | layers the Secret's `values.yaml` under every component's values |
| `--namespace-label k=v` | `namespaceLabels` | labels every namespace that packages create, for example to put them under the platform's policies |
| `--backend` | `backend` | `helm`, `werf`, `flux` or `argo` |
| `--argo-namespace`, `--argo-project` | `argo.*` | where Argo CD Applications go |
| `--nelm-binary` | | the `nelm` executable for the werf backend |

Releases are labelled `kubepkg.dev/package: <name>` (under the platform's group when it sets one), and components marked `privileged` also get `<group>/privileged`. Admission policies and cost reports can select on these labels.

### Your own API group

```bash
make crds-for-group GROUP=packages.example.org OUT=dist/crd
helm install kubepkg oci://ghcr.io/tym83/charts/kubepkg -n platform-system \
  --set apiGroup=packages.example.org
kubepkg --api-group packages.example.org list
```

Users then see `packages.example.org/v1` Packages, which fits the platform's own API. `crds-for-group` rewrites the CRDs, and the operator and CLI take the group as a flag.

## Plugins

Build plugins extend how packages are made, in any language, without Go code. A platform typically adds:

- a **source plugin** for its internal artifact store or git server;
- a **step plugin** that moves images to its registry, adds its labels, or injects its sidecars.

See [Building packages](building.md#plugins).

## Go libraries

Everything the binaries do lives in importable packages. A platform builds its own operator and CLI, with its own name, defaults, trust rules and backends.

| Package | Gives you |
|---|---|
| `pkg/operator` | `DefaultOptions`, the standard flags, a registry of named backends, and `Run` |
| `pkg/cli` | `NewRootCommand`, with your name and API group; add your own commands |
| `pkg/backend` | the `Backend` interface the operator installs through |
| `pkg/controller` | the reconcilers, and the `Preparer`, `APIs` and `CRDOwner` interfaces |
| `pkg/build`, `pkg/repo`, `pkg/resolve`, `pkg/source` | building, indexing and trust, resolving, fetching, each usable alone |

A platform's operator that serves its own group and admits only charts from its own registry:

```go
package main

import (
	"context"
	"flag"
	"fmt"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/tym83/kubepkg/pkg/operator"
	"github.com/tym83/kubepkg/pkg/repo"
)

// allowedRegistries admits only versions whose charts come from the
// platform's own registry.
type allowedRegistries struct{ prefix string }

func (allowedRegistries) AdmitIndex(context.Context, string, []byte, *repo.Index) error { return nil }

func (p allowedRegistries) AdmitVersion(_ context.Context, _, pkg string, v repo.Version) error {
	for _, variant := range v.Spec.Variants {
		for _, c := range variant.Components {
			if c.Chart != nil && !strings.HasPrefix(c.Chart.Repository, p.prefix) {
				return fmt.Errorf("%s %s: chart %s is not from %s", pkg, v.Version, c.Chart.Repository, p.prefix)
			}
		}
	}
	return nil
}

func main() {
	opts := operator.DefaultOptions()
	opts.Profile.Group = "packages.example.org"
	opts.Policy = allowedRegistries{prefix: "oci://registry.example.org/"}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	if err := operator.Run(ctrl.SetupSignalHandler(), ctrl.GetConfigOrDie(), opts); err != nil {
		panic(err)
	}
}
```

The matching CLI:

```go
o := cli.DefaultOptions()
o.Name, o.Short, o.APIGroup = "exctl", "Example Platform packages", "packages.example.org"
o.Policy = allowedRegistries{prefix: "oci://registry.example.org/"}
root := cli.NewRootCommand(o)
root.AddCommand(myPlatformCommand())
if err := root.Execute(); err != nil {
	os.Exit(1)
}
```

Both examples are compiled with the tests (`pkg/operator/example_test.go`), so they keep working.

Other extension points in `operator.Options`:

- `Backends`: add your own backend under a name, then select it with `--backend`;
- `IndexFetchers`: fetch indexes over your own URL schemes, such as `s3://` or an internal API;
- `AddToScheme`: register the types your backend writes.

## What a platform should not change

The package format, the repository index and the signature formats are the same everywhere. A package built for kubepkg installs on any platform that embeds it, and a platform's repository can be read by a plain kubepkg. Keep it that way, and your users can take their packages with them.
