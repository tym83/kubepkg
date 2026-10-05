# Rationale

Why kubepkg exists, which problems it takes on, and which popular answers to those problems it deliberately does not adopt.

## The problem

Kubernetes is a kernel, not an operating system. Everyone who ships a usable cluster ships a distribution around it: a CNI, storage, ingress, certificates, monitoring, operators for the services users ask for, and a tested set of versions that work together. Every platform builder writes its own machinery to install and upgrade that set — Deckhouse has modules, OpenShift has OLM, Rancher has its catalog. None of them can be reused by anyone else.

Helm solves templating and single-release installs well. It does not resolve dependencies between releases, does not know which component provides which API, cannot tell you what an upgrade will change before it happens, and has no opinion about CRDs beyond "install them once". Platform builders fill those gaps by hand.

kubepkg is the package layer for people who build platforms and distributions on Kubernetes. It is not a replacement for Helm, Argo CD or Flux.

## What we take from "Kubernetes as a Distro"

Mat Duggan's [Kubernetes as a Distro](https://matduggan.com/kubernetes-as-a-distro/) lists what a Kubernetes package manager should do. We agree with the goals and disagree with several of the proposed mechanisms.

| Proposal | Our position |
|---|---|
| A centralized state store, like a package database | The API server already is the state store. A second database next to it drifts from the live objects — the failure mode Helm already has with release secrets. kubepkg keeps state in its own resources and derives everything else from the live objects. |
| Dependency trees with conflict resolution | A free-form version solver produces combinations nobody tested. Distributions work because they ship tested sets, not because of the solver. kubepkg resolves on capabilities (APIs, CRDs, named capabilities such as `ingress`) and keeps version constraints minimal; a distribution pins exact versions in a channel. |
| Orchestrating changes across interdependent objects | Yes. Ordering only helps if every component defines what "healthy" means, so health checks are part of the package, not an afterthought. |
| Package signing with a centralized trust system | Signing yes, a central root of trust no. Trust is per repository (Sigstore for artifacts, TUF for repository metadata), the way Homebrew taps or Debian archives each carry their own keys. |
| Targeting multiple clusters and namespaces | Out of scope. Fleet management belongs to Argo CD, Flux, OCM or Karmada. kubepkg does one cluster well and stays easy to drive from them. |
| Rollback by snapshotting cluster state | Snapshotting objects does not roll back data: volumes, database schemas after a migration, CRDs after a version was dropped. Even apt does not support downgrades. kubepkg makes rollback safety a declared property of each package version, rolls back automatically only what is declared safe, and runs a pre-upgrade hook (a backup) for the rest. |
| A declarative, GitOps-style desired state | Yes. Plans are computed in CI from the same resources Git holds, not as an interactive step inside the cluster. |
| Built on CRDs | Yes — and CRDs are also the hardest part, which the list does not mention. CRDs are cluster-wide, outlive the release that installed them, and deleting one deletes every object of that type. kubepkg makes CRD ownership a first-class part of a package. |

## Why this has not been done before

Several projects tried: CoreOS KPM, Carvel kapp-controller, OLM, Glasskube, KubeVela. Helm 3 explicitly chose not to become apt. The reasons are not technical:

- The value of Debian is the archive, not apt. A curated, tested set of packages needs maintainers, and nobody funds that.
- Vendors already publish Helm charts and will not repackage them into a new format.
- Helm is good enough for teams that deploy a handful of their own services, and GitOps took over the lifecycle role.
- Every platform vendor owns a catalog, and a neutral standard is not in any single vendor's interest.

kubepkg answers this with three constraints. A package is an existing Helm chart plus a small metadata file, so nothing needs repackaging. kubepkg runs in any cluster and is embedded in a platform through configuration, not a fork. The target audience is platform builders, who feel the pain Helm leaves open, not every team that deploys an app.

## What a package is

A package is a Helm chart, unchanged, and a `kubepkg.yaml` next to it (or in a separate repository that references the chart by version and digest). The metadata declares:

- name and version,
- what the package provides and requires — APIs, CRDs, named capabilities, other packages,
- what it conflicts with,
- the CRDs it owns,
- the cluster permissions it needs,
- whether rolling back to the previous version is safe,
- how to tell that each component is healthy.

## How it fits existing tools

- With Argo CD and Flux, `Package` is an ordinary resource in Git, and kubepkg reports readiness through standard conditions.
- kubepkg can install charts itself or hand them to Flux (`HelmRelease`) or Argo CD (`Application`). The backend is a deployment choice, not a fork of the format.
- Built packages are plain Helm charts in OCI registries: Helm, Flux, Argo CD and werf install them without kubepkg. `kubepkg render` writes a resolved, ordered set of packages for Flux, Argo CD or helmfile to apply from Git.
- In CI, `kubepkg plan` shows what a change will do.
- A distribution is a meta package in a repository: pinned member versions, installed and upgraded as one.

## Non-goals for now

Multi-cluster targeting, a central package registry operated by the project, replacing Helm's templating, and day-2 logic that belongs in operators.
