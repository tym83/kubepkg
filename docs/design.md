# Design

This document specifies the `v1` API and the behaviour of the operator and the CLI. The reasons behind the choices are in [rationale.md](rationale.md).

## Scope

In scope: versioned packages, requirements on packages and capabilities with version constraints, conflicts, CRD ownership, declared permissions, package revisions with whole-package rollback where it is declared safe, readiness conditions and pre-upgrade hooks, a plan before changes, four backends (Helm, werf, Flux and Argo CD), package repositories with a root of trust, a hub for several clusters, metrics and alerts, and settings that let a platform embed kubepkg without changing its code.

Out of scope, on the roadmap: delegated roles for parts of a repository (TUF targets delegation).

## Resources

All resources are cluster-scoped, in the API group `kubepkg.dev`, version `v1` (`v1beta1` and `v1alpha1` are still served, deprecated, with the same schema). A platform that embeds kubepkg may serve the same types under its own group (see [Embedding in a platform](#embedding-in-a-platform)).

### PackageSource

One package at one version: where its charts come from and how they are installed. The name is the package name.

```yaml
apiVersion: kubepkg.dev/v1
kind: PackageSource
metadata:
  name: cert-manager
spec:
  version: 1.16.2                 # semver of this package
  provides:                       # capabilities this package offers
    - cert-manager
    - api:cert-manager.io/v1
  conflicts: []                   # package names or capabilities that must not coexist
  crds:                           # CRDs this package owns
    - certificates.cert-manager.io
    - issuers.cert-manager.io
  permissions:                    # declared, shown in plans, not enforced in v0.1
    clusterWide: true
    rules:
      - apiGroups: ["*"]
        resources: ["*"]
        verbs: ["*"]
  rollback:
    safe: true                    # rolling back to the previous version does not lose data
  variants:
    - name: default
      dependsOn: []               # package names, no version
      requires:                   # structured requirements
        - package: networking
          version: ">=1.0.0 <2.0.0"
        - capability: api:monitoring.coreos.com/v1
          optional: true
      components:
        - name: cert-manager
          chart:                  # the upstream chart, used as is
            repository: https://charts.jetstack.io
            name: cert-manager
            version: v1.16.2
            digest: sha256:...    # of the chart archive; pinned charts are verified
          install:
            namespace: cert-manager
            releaseName: cert-manager
            dependsOn: []
            healthCheckExprs: []
```

A component takes its chart from one of two places:

- `chart` — a chart published in a Helm repository (`https://...`) or an OCI registry (`oci://...`), at an exact version. With `digest` set, a chart whose archive does not match is refused, so a repository that republishes a version cannot change what gets installed; the digest is also the cache key, so a cached pinned chart needs no network. A package made only of such components needs no `sourceRef` at all: wrapping an upstream chart is one PackageSource.
- `path` — a chart directory inside a package tree, given by `sourceRef` (an `OCIArtifact` published with `kubepkg push`). Use it for charts you write yourself; only these can share `libraries` and use `valuesFiles`.

Exactly one of the two is set; the API server rejects anything else.

A missing `version` is treated as the unversioned version `0.0.0-unversioned`, which satisfies only an empty constraint. `dependsOn: [x]` is equivalent to `requires: [{package: x}]`.

### Package

The desired state: this package is installed, in this variant, at a version matching this constraint. The name is the package name.

```yaml
apiVersion: kubepkg.dev/v1
kind: Package
metadata:
  name: cert-manager
spec:
  variant: default
  version: "~1.16"                # constraint the installed PackageSource must satisfy
  components:                     # per-component overrides
    cert-manager:
      values: {}
  ignoreDependencies: []
  upgrade:
    atomic: true                  # roll back the whole package on failure, if the version is rollback-safe
    timeout: 10m
  revisionHistoryLimit: 10
  crdPolicy: Retain               # Retain (default) or Delete when the package is removed
status:
  conditions: []                  # Ready, plus reasons listed below
  dependencies: {}
  version: 1.16.2                 # the version that is applied
  currentRevision: 3
  history:                        # last revisions, newest first
    - revision: 3
      version: 1.16.2
      phase: Applied
```

### PackageRevision

An immutable record of one applied state of a package. Created by the operator; users read it, they do not write it.

```yaml
apiVersion: kubepkg.dev/v1
kind: PackageRevision
metadata:
  name: cert-manager-3
  labels:
    kubepkg.dev/package: cert-manager
spec:
  package: cert-manager
  revision: 3
  version: 1.16.2
  variant: default
  rollbackSafe: true
  components:
    - name: cert-manager
      releaseName: cert-manager
      namespace: cert-manager
      chartDigest: sha256:...      # what was rendered
      valuesDigest: sha256:...
status:
  phase: Applied                  # Pending | Applying | Applied | Failed | RolledBack | Superseded
  components:
    - name: cert-manager
      backendRevision: 7          # the Helm release revision this revision produced
```

A rollback is the operator re-applying an earlier revision's recorded state; it creates a new revision whose spec matches the old one, so history only grows.

## Requirements and capabilities

A requirement is satisfied by:

- `package: X, version: C` — a `Package` named X is Ready and its applied version satisfies C;
- `capability: Y` — a Ready package lists Y in `provides`, or Y has the form `api:<group>/<version>` and the API server serves that group version (discovery), so components installed outside kubepkg still count.

Requirements marked `optional: true` only order installation: if the requirement is present it must be Ready first; if it is absent the package proceeds.

Constraints use Masterminds semver syntax (`>=1.2 <2`, `~1.16`, `^2`).

A package is blocked, with condition `Ready=False`, when:

| Reason | Meaning |
|---|---|
| `VersionMismatch` | the PackageSource version does not satisfy `Package.spec.version` |
| `RequirementsNotMet` | a requirement is missing or not Ready; the message names it |
| `Conflict` | an installed package matches an entry in `conflicts`, in either direction |
| `CRDOwnershipConflict` | a CRD in `crds` is owned by another package |
| `UpgradeFailed` | the latest revision failed and was not rolled back |
| `UpgradeRolledBack` | the latest revision failed and the package was rolled back to the previous one |

## Applying a revision

The operator builds the desired state from Package and PackageSource. If it differs from the current revision (version, variant, component set, chart or values digests), it creates revision N+1 and applies it:

1. Components are applied in dependency order through the backend.
2. Each component must become healthy within `upgrade.timeout` — Helm readiness, plus `healthCheckExprs` when set.
3. If every component is healthy, revision N+1 is `Applied` and N becomes `Superseded`.
4. If a component fails and `upgrade.atomic` is true and revision N is marked `rollbackSafe`, every component already changed is rolled back to the backend revision recorded in N, in reverse order. N+1 becomes `Failed`, a revision N+2 recording the restored state is added, and the package reports `UpgradeRolledBack`.
5. If the rollback is not safe, the operator stops, N+1 stays `Failed`, and the package reports `UpgradeFailed`. Recovery is a forward fix.

`rollbackSafe` is taken from the version being rolled back from: the author of 1.17 knows whether going back to 1.16 is safe after 1.17 ran its migrations.

## CRD ownership

After a revision is applied, every CRD listed in `crds` is annotated `kubepkg.dev/owned-by: <package>`. A package cannot be applied while a CRD it lists is owned by another package, unless the owner's chosen version no longer lists that CRD. Then the CRD moves: it is annotated for the new owner and `helm.sh/resource-policy: keep`, and the new owner's revision takes the object over into its release. So a package that hands its CRDs to a new package it requires (envoy-gateway giving the Gateway API CRDs to gateway-api) does not wait for itself.

Helm deletes what a release no longer renders, CRDs included, and with them every object of their kinds. kubepkg prevents that unless asked for it: before a revision whose version stops listing a CRD the package owns, the CRD is annotated `helm.sh/resource-policy: keep` and let go of. When a package is deleted, its CRDs are kept the same way, before its releases are uninstalled, unless `crdPolicy: Delete`. Kept CRDs lose the ownership annotation, so another package can adopt them.

## Backends

```go
type Backend interface {
	Apply(ctx context.Context, c Component) (Result, error)
	Status(ctx context.Context, c Component) (State, error)
	Rollback(ctx context.Context, c Component, toRevision int) error
	Uninstall(ctx context.Context, c Component) error
}
```

A component can declare `readyWhen`: object conditions that must hold before it counts as ready, for resources whose readiness Helm cannot see, such as an operator's custom resource reporting `Available`. A release that installed but never meets them within the upgrade timeout fails the revision like any other failure.

A component with `install.phase: PreUpgrade` is a hook, typically a chart with a Job that migrates data. It runs only when a revision moves the package to another version, before every other component, with the values `kubepkg.fromVersion` and `kubepkg.toVersion`. A hook that fails stops the upgrade before anything else changes; once the upgrade succeeds its release is removed, so the next upgrade runs it afresh. Hooks are not part of a revision's snapshot, and no other component may depend on one. `kubepkg plan` lists the hooks an upgrade runs.

Every backend gets a package's components one at a time in dependency order: the operator applies the next one only once the one before it is ready, so ordering never depends on the delivery tool supporting it. Synchronous backends report readiness from `Apply`; asynchronous ones are re-checked until they settle.

- **helm** installs charts with the Helm SDK inside the operator. Sources: published charts (HTTP repositories and OCI registries) and `OCIArtifact` package trees. Rollback uses Helm release history. No other controllers are needed.
- **flux** hands each component to Flux: it writes the chart's source, an `OCIRepository` selecting the Helm chart layer for charts in a registry or a `HelmRepository` otherwise, and a `HelmRelease`, and deletes both on uninstall. It needs only Flux's source and helm controllers. Sources: published charts; package trees cannot be handed to Flux. Flux keeps no earlier chart artifacts, so this backend cannot roll back: packages applied through it are recovered by a forward fix, and `UpgradeFailed` is reported instead of `UpgradeRolledBack`. Flux reads the platform values Secret only from the release namespace.
- **werf** installs each component through Nelm, the deployment engine of werf, which ships in the operator image (`--nelm-binary`). Charts are prepared as for the helm backend, values go in as a file, and Nelm keeps Helm-compatible release history, so this backend rolls back like the helm one. It needs cluster-admin for the same reason.
- **argo** hands each component to Argo CD as an automatically synced `Application` in `--argo-namespace` (default `argocd`) and `--argo-project`. Argo CD cannot read values from Secrets, so the operator passes platform and package values inline. A component is ready when the wanted chart version is synced and healthy; deleting it lets Argo CD remove what it deployed. It cannot roll back, like flux.

The backend is chosen per operator installation (`--backend`).

## Building packages

Packages in a repository are built by the repository's maintainers from upstream sources, the way Debian builds from upstream tarballs. The upstream's own distribution format does not matter: release manifests, a chart, an archive of a source tree. Installing a built package never contacts the upstream. A component with `chart` installs an upstream chart directly; that is a quick way to install something, not how repository packages are made.

A recipe is a directory with `recipe.yaml` and whatever files the maintainers add:

```yaml
apiVersion: kubepkg.dev/v1
kind: Recipe
metadata:
  name: kubevirt
  annotations:
    kubepkg.dev/description: Virtual machines on Kubernetes
spec:
  version: 1.9.0                  # upstream version
  build: 1                        # our packaging of it
  sources:                        # every source is pinned
    operator:
      url: https://github.com/kubevirt/kubevirt/releases/download/v1.9.0/kubevirt-operator.yaml
      sha256: f11307ca...
    kubevirt:
      dir: charts/kubevirt        # a chart we maintain, next to the recipe
  charts:                         # what goes into the package tree
    kubevirt-operator:
      from: [operator]            # plain manifests are wrapped into a chart
      exclude: [{kind: Namespace}]
    kubevirt:
      from: [kubevirt]            # a chart source is used as is
  package:                        # the PackageSource spec; components refer to built charts
    provides: [kubevirt, "api:kubevirt.io/v1"]
    rollback: {safe: false}
    variants:
      - name: default
        components:
          - {name: operator, path: kubevirt-operator, install: {namespace: kubevirt}}
          - {name: kubevirt, path: kubevirt, install: {namespace: kubevirt, dependsOn: [operator]}}
```

Sources are a file or `.tar.gz` archive by URL with its sha256 (`path` picks a file or directory inside an archive), a published chart with its digest, or a directory next to the recipe. A chart is made from one chart source, or from manifests, which are wrapped into a chart that applies them verbatim: their content never passes through the template engine. On top of that, `values` sets the package's defaults, `overlay` adds or replaces files, `exclude` drops objects from wrapped manifests, and `patches` change objects of wrapped manifests with JSON merge patches (`null` removes a field), the way a Debian package patches its upstream. A patch that matches no object fails the build, so an upstream rename cannot silently ship the object unpatched.

`kubepkg build <recipe> --registry oci://...` fetches and checks the sources, builds the charts, and publishes each one as an ordinary Helm chart in the registry, `oci://.../<package>/<chart>` at version `<version>-<build>`; the PackageSource it writes refers to them by archive digest, and `kubepkg repo index` over those files makes the index. Because built packages are plain Helm OCI charts, Helm, Flux, Argo CD and werf can install them without kubepkg. The same recipe always builds the same content. A package's identity is that content, the digest of its uncompressed tree, recorded on the artifact: compressed bytes differ between compressors, and so between Go releases. Tags are immutable: when the tag already holds the same content, the published artifact is reused, so rebuilding an unchanged recipe on another toolchain publishes nothing new; different content under the same version and build is refused.

A distribution extends builds with plugins. A source `{plugin: git, with: {...}}` is fetched by a source plugin, and a chart's `steps: [{plugin: relocate-images, with: {...}}]` run step plugins on the finished chart, in order. A plugin is either Go code registered in `build.Options.Plugins`, or an executable on `PATH` named `kubepkg-source-<name>` or `kubepkg-step-<name>`, in any language: it reads `with` as JSON on stdin, finds its directories in `KUBEPKG_OUT` (a source fills it) or `KUBEPKG_CHART` (a step changes it), the package in `KUBEPKG_PACKAGE` and `KUBEPKG_VERSION`, runs in the recipe directory, and fails the build with a non-zero exit. Plugins must be deterministic, or builds stop being reproducible.

`spec.version` is the upstream version and `spec.build` numbers our packagings of it. Constraints match the version; of two builds of one version the higher is newer.

### Meta packages: a distribution as a package

A recipe with no charts builds a meta package: only requirements. A distribution is one such package listing its members and their versions:

```yaml
apiVersion: kubepkg.dev/v1
kind: Recipe
metadata:
  name: mydistro
spec:
  version: 2.0.0
  package:
    variants:
      - name: default
        requires:
          - {package: cilium, version: "~1.18"}
          - {package: cert-manager, version: "~1.21"}
          - {package: kubevirt, version: "~1.10"}
          - {package: cdi}
```

`kubepkg install mydistro` installs every member and the meta package itself. Each member's `Package` gets the constraints the packages requiring it set (all of them must hold), or follows patch releases when none is set, so installing `mydistro@2.0.0` over 1.0.0 moves the members to the versions 2.0.0 asks for. The operator reports the meta package Ready once every member is; it installs nothing of its own. Platform-wide settings reach the members through the platform values Secret.

## Package repositories

A repository is one index file that lists every version of every package it offers, the way a Helm repository index lists charts. Charts are not copied into it: each version points at its charts by repository, version and digest.

Authors keep one PackageSource per package version in a directory, in any layout:

```
recipes/
  cert-manager/1.16.2.yaml
  cert-manager/1.17.0.yaml
  cilium/1.18.1.yaml
```

`kubepkg repo index recipes -o index.yaml` turns them into the index:

```yaml
apiVersion: kubepkg.dev/v1
kind: RepositoryIndex
packages:
  cert-manager:
    description: X.509 certificate management for Kubernetes   # kubepkg.dev/description annotation
    home: https://cert-manager.io                               # kubepkg.dev/home annotation
    versions:                                                   # newest first
      - version: 1.17.0
        digest: sha256:...      # of the spec in canonical JSON
        spec: {...}             # the PackageSource spec, as installed
```

Building the index enforces what makes a repository trustworthy:

- every version is an exact semver version, and a package version is defined once;
- every chart is pinned by digest: charts without one are downloaded and pinned in the index (the recipe files stay as they are), and `--verify` re-downloads pinned charts to check them;
- a version built from a package tree must pin the tree by digest (`oci://...@sha256:...`).
- with `--merge <published index>`, versions published before are kept even when their recipes are gone, and a version rebuilt under the same version and build must come out identical: published versions never change, a changed recipe needs a new build number.

Because every chart is pinned, the spec digest identifies exactly what a version installs. The index is published as a static `index.yaml` over HTTP, signed as described below.

### Signing and trust

There are two ways to trust a repository.

**Plain keys.** The repository signs its index with an ed25519 key, and clusters trust it by public key:

```bash
kubepkg repo keygen release                        # release.key (private), release.pub
kubepkg repo index dist --sign-key release.key     # index.yaml and index.yaml.sig
kubepkg repo add main https://packages.example.org/index.yaml --public-key release.pub
```

**A root of trust**, after TUF, for threshold signing and key rotation. The repository publishes `root.yaml` next to its index, and every version of it as `root/<version>.yaml`. The root names the keys that may sign the root itself and those that may sign the index, with a threshold for each, and expires:

```bash
kubepkg trust root new --root-key a.pub --root-key b.pub --root-key c.pub --root-threshold 2 \
  --index-key ci.pub --index-threshold 1 --expires 8760h -o root.yaml
kubepkg trust sign root.yaml --key a.key           # each signer on their own machine
kubepkg trust sign root.yaml --key c.key
kubepkg repo index dist --expires 720h --sign-key ci.key
kubepkg repo add main https://packages.example.org/index.yaml \
  --root-key a.pub --root-key b.pub --root-key c.pub --root-threshold 2
```

A cluster pins the keys of root version 1 and their threshold. It follows the chain to the current root, accepting each version only when it is signed by enough root keys of the version before and of itself, so keys rotate without clients changing anything: `kubepkg trust root next root.yaml --root-key ...` makes the next version, and both the old and the new root keys sign it. The index must carry enough signatures by the keys the current root names for it (`kubepkg trust sign index.yaml --key ...` adds one), and neither the root nor the index may have expired: a mirror that keeps serving an old index is refused once it expires. A cluster records the root it accepted, refusing an older root and another root under the same version, the fork someone holding stolen old keys could make.

In both modes the index pins every chart by digest, and every chart is verified against its digest when fetched, so the signatures cover everything the repository installs. Every index carries the time it was generated, and a cluster refuses an index older than one it already accepted, so an old index that is validly signed cannot be replayed. A refused index leaves the one accepted before in use. The CLI applies the same checks to the cluster's repositories.

A distribution adds checks of its own, such as signatures from its own key infrastructure or allowed registries, through the admission policy.

### Installing from repositories

A cluster subscribes to repositories with `Repository` resources:

```yaml
apiVersion: kubepkg.dev/v1
kind: Repository
metadata:
  name: main
spec:
  url: https://packages.example.org/index.yaml
  priority: 10                    # the highest priority repository carrying a package shadows the others
  interval: 10m
status:
  packages: 42
  indexDigest: sha256:...
  conditions: []                  # Ready: IndexLoaded, FetchFailed, InvalidIndex, IndexRefused
```

The operator keeps every index loaded. A `Package` with no hand-written `PackageSource` gets one from the repositories. The package is taken from the highest priority repository that carries it at all (of all repositories, or only `spec.repository`); it shadows the others even when it has no version matching `spec.version`, so a package a vendor repository carries never silently comes from a community one. Within that repository the newest version and build matching `spec.version` wins. The operator writes that version's spec as the `PackageSource`, labelled `kubepkg.dev/repository` and owned by the `Package`; installation then proceeds as for any `PackageSource`.

- The constraint is the policy: `~1.9` follows new 1.9.x releases as the index gains them, an exact version stays put.
- A hand-written `PackageSource` always wins, which is how a cluster overrides a package locally.
- Selection waits until every repository has been fetched once, so after a restart a low priority index that loads first cannot win for a moment.
- A failed refresh keeps the index loaded before; a repository that never loaded leaves its packages at `VersionNotAvailable`.
- Requirements are not installed automatically: a package whose requirements are missing reports `RequirementsNotMet`. Installing a package together with what it needs is the CLI's job, so the cluster state stays explicit.

A distribution adds index transports by URL scheme (`http` and `https` are built in) and a policy that admits indexes and versions — signature checks, allowed registries — through `operator.Options`.

## Air-gapped clusters

A package lists the images it runs in `images`, each pinned by digest, so a signed index covers images as it covers charts. Pins live in the recipe: `init` writes them, `kubepkg images` prints them for an existing recipe, and `validate` fails when a chart runs an image the list does not pin.

`kubepkg bundle create` writes one file with the chosen packages and their requirements: the repository files as fetched (index, signatures, roots), chart archives, package trees and images. `kubepkg bundle import` checks the bundle with keys given on the air-gapped side, never keys from the bundle, using the cluster's own checks (signatures, root chain, expiry), then checks every chart, tree and image against the signed packages, copies them into a mirror registry and writes the repositories' files for serving. Images found in charts rather than pinned by a repository are refused unless `--allow-unpinned-images`.

The mirror keeps each location under its own path, `<mirror>/<host>/<path>`, with the same digests. The operator's `--mirror` fetches every chart and tree from there, without changing revisions; `--node-config` writes containerd and Talos settings that send image pulls there.

## Several clusters

A hub installs packages into member clusters, each of which runs kubepkg itself and keeps its own revisions and rollbacks. The hub knows its members as `Cluster` resources, which point at a kubeconfig in a Secret and carry labels, and says what goes where with `PackageSet`s:

```bash
kubepkg cluster add edge-1 --kubeconfig edge-1.kubeconfig --label env=prod --label region=eu
```

```yaml
apiVersion: kubepkg.dev/v1
kind: PackageSet
metadata:
  name: base
spec:
  clusterSelector: {matchLabels: {env: prod}}
  repositories:
    - name: main
      spec: {url: https://packages.example.org/index.yaml, publicKeys: ["..."]}
  packages:
    - name: cert-manager
      spec: {version: "~1.21"}
    - name: virtualization
```

The hub writes the repositories and packages to every selected cluster, labelled `kubepkg.dev/package-set`, and never touches objects it did not create: a Package of the same name made by hand is reported as a conflict and left alone. Dropping a package from the set removes it from the clusters; a cluster that stops matching the selector loses what the set wrote there; deleting the set removes everything it wrote, and waits for unreachable clusters rather than leave packages behind. The set's status counts ready packages per cluster (`kubepkg set list`), and each Cluster reports whether the hub reaches it and whether it runs kubepkg (`kubepkg cluster list`).

A set's `rollout` paces a change across its clusters: canary clusters first, at most `maxInProgress` clusters taking it at once, and a pause once a cluster that took it reports a failed or rolled back upgrade; a new change resumes. Each object the set writes carries the change it belongs to, so a cluster has a change once all its objects carry it, and is done with it once its packages are ready at their current generation. A cluster moving to another set that carries the same package or repository hands the object over instead of deleting it. `kubepkg set status` shows the version of every package on every cluster.

## Observability

The operator exports Prometheus metrics next to controller-runtime's own:

| Metric | Meaning |
|---|---|
| `kubepkg_package_ready{package}` | 1 when the package is ready |
| `kubepkg_package_info{package,version,revision}` | the version and revision a package runs |
| `kubepkg_revisions_total{package,outcome}` | revisions by outcome: `applied`, `failed`, `rolled_back` |
| `kubepkg_repository_ready{repository}` | 1 when the repository's index is loaded and accepted |
| `kubepkg_repository_index_generated_timestamp_seconds{repository}` | when the accepted index was built |

The chart adds a metrics Service and, when enabled, a ServiceMonitor, a PrometheusRule with alerts (a package not ready for 15 minutes, a failed or rolled back upgrade, a repository that cannot be loaded, an index unchanged for longer than `indexMaxAge`, a controller that keeps failing to reconcile) and a Grafana dashboard.

## Other delivery tools without the operator

Built packages are ordinary Helm charts in OCI registries, so any tool that installs Helm charts installs them. `kubepkg render <package>...` does the part those tools lack: it resolves the packages and their requirements, as `install` would for an empty cluster, and writes them in kubepkg's order for the tool to apply from Git.

- `--format flux`: chart sources and `HelmRelease`s with `dependsOn` across components and packages.
- `--format argo`: `Application`s with sync waves, for an app of apps.
- `--format helmfile`: releases with `needs`.

Packages come from `--repo` indexes or the cluster's repositories; package defaults apply, and values are set in the output. What the operator adds on top — revisions, whole-package rollback, CRD ownership, requirement checks at runtime — is not part of the rendered output.

## Embedding in a platform

kubepkg is a mechanism first and a set of binaries second. It runs in any conformant cluster with nothing but its CRDs and the operator, and a distribution adapts it at three levels, from no code to its own binaries.

Configuration — operator flags, all off by default:

| Flag | Does |
|---|---|
| `--api-group` | serves the types under the platform's own group; `make crds-for-group GROUP=... OUT=...` writes matching CRDs. The CLI takes the same flag |
| `--values-secret namespace/name` | layers the Secret's `values.yaml` under every component's values, for cluster-wide settings such as a domain or an issuer; a PackageSource opts out with the `kubepkg.dev/skip-platform-values` annotation. A missing Secret fails the release instead of installing it unconfigured |
| `--namespace-label key=value` | labels every namespace the packages create (repeatable) |
| `--backend helm\|werf\|flux\|argo` | how components are installed; `--argo-namespace` and `--argo-project` place Argo CD Applications, `--nelm-binary` names Nelm for werf |

Releases are labelled `kubepkg.dev/package`; privileged components get `<group>/privileged`.

Plugins — build source and step plugins, as executables on `PATH` (see [Building packages](#building-packages)); a distribution adds its own fetching and processing, such as moving images to its registry, without Go code.

Libraries — everything the binaries do is in importable packages, so a distribution builds its own binaries without forking:

- `pkg/operator`: `DefaultOptions`, the standard flags, a registry of named backends a distribution extends with its own, and `Run`. `kubepkg-operator` is a thin `main` over it.
- `pkg/cli`: `NewRootCommand` with the distribution's name and API group; it adds commands of its own.
- `pkg/backend`: the `Backend` interface the operator installs through; `pkg/controller` the reconciler and its `Preparer`, `APIs` and `CRDOwner` interfaces.
- `pkg/build`, `pkg/repo`, `pkg/resolve`, `pkg/source`: building, indexing, resolving and fetching, each usable alone.

## CLI

`kubepkg` works against the cluster and against files, so the same commands run in CI.

Installing and running:

| Command | Does |
|---|---|
| `repo add <name> <url> [--public-key <file>]`, `repo list`, `repo remove <name>` | subscribe the cluster to repositories, trusting only indexes signed by the given keys |
| `search [term]` | what the repositories offer, from the repository each package is taken from |
| `install <pkg>[@constraint]...` | resolves the packages and their requirements against the cluster's repositories, shows the plan, and writes one Package per package; requirements already installed are kept, packages pulled in as requirements are marked `kubepkg.dev/dependency`, and without a constraint a package follows patch releases (`~X.Y`) |
| `plan <pkg>[@constraint]...` | the same plan without changing anything: installs, upgrades, downgrades, CRDs, permissions, rollback safety |
| `remove <pkg>... [--autoremove]` | deletes Packages, refusing while a package that stays requires one of them, directly or as the only provider of a capability; `--autoremove` also removes packages installed as requirements that nothing needs any more |
| `list` | packages, versions, revisions, readiness |
| `history <pkg>` | revisions of a package |
| `rollback <pkg> [--to N]` | re-applies an earlier revision |

Building and publishing:

| Command | Does |
|---|---|
| `init <dir> --chart <repo>/<name>@<version>` or `--manifest <url>...` | start a recipe with every source pinned, the description taken from the chart, Namespaces dropped from manifests and the shipped CRDs listed |
| `validate <recipe-dir>...` | build without publishing, render the charts with their defaults and check the package against them; an undeclared CRD is an error |
| `build <recipe>` | build a package from upstream sources and publish it |
| `repo index <dir> [--sign-key <file>]` | build a repository index from the PackageSources under a directory, signed |
| `repo keygen <prefix>` | make an ed25519 key pair for signing |
| `trust root new`, `trust root next`, `trust sign` | make root versions and add signatures to roots and indexes |
| `cluster add`, `cluster list`, `set list` | register member clusters with a hub and follow PackageSets |
| `push <dir> <oci-ref>` | publish a package tree as is |
| `render <pkg>...` | write packages for Flux, Argo CD or helmfile, in kubepkg's order |


The CLI resolves with the operator's own view of the cluster and the same repository shadowing and policy, so a plan shows what the operator will do.
