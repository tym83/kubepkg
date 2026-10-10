# Compatibility

From 1.0, kubepkg follows [semantic versioning](https://semver.org/). This page says what that covers: what a minor or patch release never breaks, and how things that must change are retired.

## The cluster API

The `Package`, `PackageSource`, `PackageRevision`, `Repository`, `Cluster` and `PackageSet` resources are served as **`kubepkg.dev/v1`**.

- **Nothing breaks within `v1`.** New fields may appear, always optional and with defaults that keep today's behaviour. No field is removed or renamed, and no field changes meaning.
- **Older versions keep working.** `v1beta1` and `v1alpha1` are served with the same schema, and the API server answers requests for them with a deprecation warning. Moving a manifest to `v1` takes nothing but changing its `apiVersion`. Both stay served throughout 1.x and go away in 2.0 at the earliest.
- **Objects are stored as `v1`.** When the operator starts, it rewrites objects that a cluster still keeps in an older version and records that only `v1` is stored. Removing an old version later loses nothing.
- **A breaking change means `v2`**, served next to `v1` for at least one minor release before `v1` is retired.

## Repository files

Indexes, their signatures and roots of trust are read by clusters and CLIs of many releases at once, so they get their own rules:

- **Any 1.x release reads what any earlier release since 0.5 published.**
- **Published versions never change.** Their spec digests stay valid across releases, so a version built by one release installs the same with every later one.
- **A version that uses something a reader does not know is left out, not fatal.** The reader names it in the Repository status and in a CLI warning, and uses the rest of the index.
- **A root that uses something a reader does not know is refused, and says so.** The reader cannot tell what the root means, so it does not guess: it reports that the root needs a newer kubepkg. The release notes name every such root feature and the first release that understands it. Delegations, from 0.5, are the first. Upgrade the clusters before publishing a root that uses one.
- The index format carries `apiVersion: kubepkg.dev/v1alpha1` as its own format version, which is unrelated to the cluster API.

Recipes (`kind: Recipe`) and bundles follow the same rules as the cluster API: within 1.x, a recipe that builds keeps building, and any release reads a bundle made by an earlier one.

## Upgrades

- Any 1.x release upgrades directly to any later 1.x release. Installed packages keep their revisions.
- From 0.x, upgrade from 0.5 or later. Older releases go through 0.5 first.
- The chart's values keep working with `helm upgrade --reuse-values` across all of 1.x. CI renders the chart with the values of every earlier release.
- Downgrades are not supported once a newer release has written objects.

## The CLI and the operator

- Commands and flags of `kubepkg` and of the operator stay within 1.x. A flag may be deprecated with a warning and removed in 2.0.
- Output meant for people, such as tables and messages, may change in any release. Do not parse it.

## Go packages

A platform that embeds kubepkg builds on `api/v1`, `pkg/operator` and `pkg/cli`. Their exported names follow semantic versioning. Other packages may change in any release.

From 1.2.0 the module path is `github.com/kuberoot-dev/kubepkg`; up to 1.1.0 it was `github.com/tym83/kubepkg`, and those versions stay available there. Moving is a change of import paths only:

```bash
go mod edit -droprequire github.com/tym83/kubepkg -require github.com/kuberoot-dev/kubepkg@v1.2.0
grep -rl github.com/tym83/kubepkg --include='*.go' . | xargs sed -i 's#github.com/tym83/kubepkg#github.com/kuberoot-dev/kubepkg#g'
```

## Kubernetes versions

Each release supports the three newest Kubernetes minor versions at the time it ships; for 1.0 these are 1.35 to 1.37. CI runs the end-to-end tests on the oldest and the newest of them. kubepkg needs only CRDs and its operator, so it usually works on older clusters too, but they are not tested.
