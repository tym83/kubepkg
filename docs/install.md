# Installing kubepkg

kubepkg has two parts. The **operator** runs in the cluster and installs packages. The **CLI** runs wherever you are, against the cluster or against files, and plans, installs, builds and publishes packages. Both are released together, and their versions should match.

## The operator

The operator ships as a Helm chart in an OCI registry:

```bash
helm install kubepkg oci://ghcr.io/tym83/charts/kubepkg \
  --namespace kubepkg-system --create-namespace --wait
```

This installs:

- the CRDs `Package`, `PackageSource`, `PackageRevision`, `Repository`, `Cluster` and `PackageSet`, all in the group `kubepkg.dev`;
- the operator Deployment and its ServiceAccount;
- RBAC for the operator. With the default `helm` backend the operator is cluster-admin; the next section explains why;
- a Service for the operator's Prometheus metrics;
- `Repository` objects for any repositories you list in `repositories`.

The CRDs are annotated `helm.sh/resource-policy: keep`. Uninstalling the chart therefore leaves your packages in the cluster, and the objects of the CRDs those packages own as well.

### Choosing a backend

The backend decides how the operator installs each component of a package. You choose it once per installation:

| `backend` | Installs with | Rolls back | Needs |
|---|---|---|---|
| `helm` (default) | the Helm SDK inside the operator | yes | nothing else |
| `werf` | Nelm, werf's deployment engine, which ships in the operator image | yes | nothing else |
| `flux` | Flux `HelmRelease`s | no, recovery is a forward fix | Flux's source and helm controllers |
| `argo` | Argo CD `Application`s | no, recovery is a forward fix | Argo CD |

```bash
helm install kubepkg oci://ghcr.io/tym83/charts/kubepkg -n kubepkg-system --create-namespace \
  --set backend=flux --set clusterAdmin=false
```

With `helm` and `werf`, the operator itself creates whatever the charts contain, cluster roles included. Kubernetes lets a subject grant only the permissions it already holds, so these two backends need `clusterAdmin: true`. With `flux` and `argo`, Flux or Argo CD does that work, so the operator can run with its own narrow role. [Backends](guides/backends.md) covers each one in detail.

### Subscribing to repositories at install time

A distribution usually installs kubepkg already subscribed to its own repository:

```yaml title="values.yaml"
repositories:
  - name: main
    url: https://packages.example.org/index.yaml
    priority: 10
    publicKeys:
      - |
        -----BEGIN PUBLIC KEY-----
        MCowBQYDK2VwAyEA...
        -----END PUBLIC KEY-----
```

The chart creates these `Repository` objects after its CRDs exist, from a post-install and post-upgrade hook. [Repositories and trust](guides/repositories.md) covers repositories with a root of trust (`trust:`) instead of plain keys.

### Platform-wide values

Some settings belong to the platform rather than to one package, such as a domain, a cluster issuer or an image registry mirror. Put them in a Secret and point the operator at it:

```bash
kubectl -n kubepkg-system create secret generic platform-values \
  --from-literal=values.yaml='global: {domain: example.org}'
helm upgrade kubepkg oci://ghcr.io/tym83/charts/kubepkg -n kubepkg-system --reuse-values \
  --set valuesSecret=kubepkg-system/platform-values
```

The Secret's `values.yaml` goes underneath every component's values. Both the package's defaults and your own values override it. If the Secret is missing, the release fails rather than installing with the settings left out.

### High availability

```bash
helm upgrade kubepkg oci://ghcr.io/tym83/charts/kubepkg -n kubepkg-system --reuse-values --set replicas=2
```

Replicas elect a leader through a `Lease`. If the leader dies, another replica takes over within about fifteen seconds and continues from what the cluster records. A revision that was being applied is resumed from the next component that was not yet ready.

All chart settings are listed in [Helm chart values](reference/chart.md).

## The CLI

Each release publishes binaries for Linux and macOS on amd64 and arm64, with a `SHA256SUMS` file:

```bash
os=$(uname -s | tr A-Z a-z); arch=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
curl -fsSLO "https://github.com/tym83/kubepkg/releases/latest/download/kubepkg-${os}-${arch}"
curl -fsSL https://github.com/tym83/kubepkg/releases/latest/download/SHA256SUMS | grep "kubepkg-${os}-${arch}" | shasum -a 256 -c -
install -m 0755 "kubepkg-${os}-${arch}" /usr/local/bin/kubepkg
kubepkg version
```

Or build it from source with Go:

```bash
go install github.com/tym83/kubepkg/cmd/kubepkg@latest
```

The CLI uses your kubeconfig. `--context` selects a context, and `--api-group` must match the operator's when a platform serves the types under its own group.

## Upgrading kubepkg

```bash
helm upgrade kubepkg oci://ghcr.io/tym83/charts/kubepkg -n kubepkg-system --reset-then-reuse-values --version <new>
```

`--reset-then-reuse-values` (Helm 3.14 and newer) keeps your settings and takes the new chart's defaults for settings it adds. Plain `--reuse-values` also works: every setting the chart has added since v0.1.0 has its default in the templates too, and CI renders the chart with the values of every earlier release to keep it that way.

The chart upgrades the CRDs together with the operator. Installed packages stay as they are. The new operator adopts their current revisions without creating new ones, unless something the packages depend on has actually changed.

## Uninstalling

Remove the packages first if you no longer want what they installed:

```bash
kubectl delete packages.kubepkg.dev --all --wait   # the operator uninstalls each package
helm uninstall kubepkg -n kubepkg-system
```

The CRDs stay in the cluster, because the chart keeps them on purpose. Delete them once nothing uses them any more:

```bash
kubectl get crd -o name | grep '\.kubepkg\.dev$' | xargs kubectl delete
```
