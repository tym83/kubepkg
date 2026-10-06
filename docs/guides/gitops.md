# GitOps without the operator

Built packages are ordinary Helm charts in an OCI registry, so any tool that installs Helm charts can install them. What those tools lack is the package layer: resolving requirements, choosing versions from a repository, and ordering components across releases. `kubepkg render` adds that part and writes the result in the tool's own format, for you to commit to Git.

```bash
kubepkg render virtualization --format flux     > clusters/prod/virtualization.yaml
kubepkg render virtualization --format argo     > apps/virtualization.yaml
kubepkg render virtualization --format helmfile > helmfile.yaml
```

`render` resolves the packages and their requirements as `install` would for an empty cluster, and then writes them in kubepkg's order:

| Format | Writes | Ordering through |
|---|---|---|
| `flux` | an `OCIRepository` or `HelmRepository` and a `HelmRelease` per component | `dependsOn` across components and packages |
| `argo` | an `Application` per component | sync waves, for an app of apps |
| `helmfile` | a release per component | `needs` |

Packages come from the cluster's repositories, or from `--repo` indexes when you have no cluster at hand, for example in CI:

```bash
kubepkg render virtualization --format argo \
  --repo https://tym83.github.io/kubepkg-recipes/index.yaml --public-key index.pub
```

```text
--8<-- "examples/render.txt"
```

Package defaults are applied, and values end up in the output, where you can change them in Git.

## What you give up

Rendered output is plain Flux, Argo CD or helmfile configuration. The operator is not involved, so these do not apply:

- package revisions and whole-package rollback;
- CRD ownership checks;
- requirement and conflict checks while the cluster runs, beyond the resolution done by `render`;
- `readyWhen` and pre-upgrade hooks.

If you want GitOps **and** these, commit `Package` objects to Git instead and let the operator install them. Flux or Argo CD applies the `Package` manifests, and kubepkg does the rest:

```yaml title="clusters/prod/packages.yaml"
apiVersion: kubepkg.dev/v1beta1
kind: Package
metadata:
  name: virtualization
spec:
  version: "~1.0"
---
apiVersion: kubepkg.dev/v1beta1
kind: Package
metadata:
  name: kubevirt
spec:
  version: "~1.9"
---
apiVersion: kubepkg.dev/v1beta1
kind: Package
metadata:
  name: cdi
spec:
  version: "~1.66"
```

List the requirements too. The operator never installs a requirement on its own: a package whose requirements are missing waits with `RequirementsNotMet` and names them. That keeps what is in the cluster exactly what is in Git. `kubepkg plan` shows the full list to commit.

This is the usual setup: Git holds the desired packages and versions, and kubepkg decides how to get there safely.
