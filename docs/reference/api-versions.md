# API versions

kubepkg's cluster API, the `Package`, `PackageSource`, `PackageRevision`, `Repository`, `Cluster` and `PackageSet` resources, is at **`v1beta1`**.

## What beta promises

- **No breaking changes within `v1beta1`.** New fields may appear, always optional and with defaults that keep today's behaviour. Fields are not removed or renamed, and their meaning does not change.
- **Upgrades keep your objects.** A new kubepkg reads every object an older one wrote, and kubepkg's own upgrade test checks it on real clusters: installed packages keep their revisions across an upgrade.
- **A breaking change means a new version.** If one is ever needed, it arrives as a new API version served next to `v1beta1`, with a deprecation period. During that period both versions work.
- **Repository formats are stable.** The index, its signatures and the root of trust keep their formats. Published package versions never change, and their digests stay valid across kubepkg releases.

## v1alpha1

`v1alpha1` is still served, with the same schema, so manifests written for it keep working. The API server answers requests for it with a deprecation warning:

```text
Warning: kubepkg.dev/v1alpha1 is deprecated; use kubepkg.dev/v1beta1
```

Clusters store objects as `v1beta1`. Moving manifests to `v1beta1` takes nothing but changing `apiVersion`. `v1alpha1` will be removed in a future minor release, announced in the release notes at least one minor release ahead.

Two things changed between the versions without changing any schema an object uses:

- `healthCheckExprs` has its own type in `v1beta1` instead of Flux's, with the same fields, so the API no longer depends on Flux.
- `PackageSource.status.variants`, which nothing ever filled, is gone.

## File formats

Recipes (`kind: Recipe`) and repository indexes (`kind: RepositoryIndex`) are files, not cluster resources. kubepkg reads them whatever `apiVersion` they carry. Indexes keep `apiVersion: kubepkg.dev/v1alpha1` as their format version, which is unrelated to the cluster API.
