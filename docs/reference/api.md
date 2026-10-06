# API reference

Generated from the CRDs by `make docs`; do not edit.

All resources are cluster-scoped and served as `kubepkg.dev/v1alpha1`, or under the group a platform chooses with `--api-group`.

- [Cluster](#cluster)
- [Package](#package)
- [PackageRevision](#packagerevision)
- [PackageSet](#packageset)
- [PackageSource](#packagesource)
- [Repository](#repository)

## Cluster

Cluster is a member cluster a hub installs packages into; its labels are what PackageSets select.

### Cluster.spec

| Field | Type | Description |
|---|---|---|
| `kubeconfigSecretRef` | object, required | KubeconfigSecretRef holds a kubeconfig for the member, with its current context set. |
| `kubeconfigSecretRef.key` | string | Key defaults to "kubeconfig". |
| `kubeconfigSecretRef.name` | string, required |  |
| `kubeconfigSecretRef.namespace` | string, required |  |

### Cluster.status

| Field | Type | Description |
|---|---|---|
| `conditions` | []object |  |
| `kubernetesVersion` | string | KubernetesVersion of the member. |


## Package

Package is the desired state of one installed package. The name is the package name and matches its PackageSource.

Short names: `pkg`, `pkgs`.

### Package.spec

| Field | Type | Description |
|---|---|---|
| `components` | object | Components is a map of release name to component overrides Allows overriding values and enabling/disabling specific components from the PackageSource |
| `crdPolicy` | string: Retain, Delete | CRDPolicy decides what happens to owned CRDs when the package is removed. Retain (default) keeps them; Delete removes them and every object of their kinds. |
| `ignoreDependencies` | []string | IgnoreDependencies is a list of package source dependencies to ignore Dependencies listed here will not be installed even if they are specified in the PackageSource |
| `repository` | string | Repository restricts version selection to the named Repository. Empty considers all of them, by priority. Ignored when a PackageSource for the package was written by hand. |
| `revisionHistoryLimit` | integer | RevisionHistoryLimit is how many PackageRevisions to keep. Default 10. |
| `upgrade` | object | Upgrade controls how changes are applied. |
| `upgrade.atomic` | boolean | Atomic rolls the whole package back when a revision fails, if the version being left declares rollback as safe. Default true. |
| `upgrade.timeout` | string | Timeout for every component to become healthy. Default 10m. |
| `variant` | string | Variant is the name of the variant to use from the PackageSource If not specified, defaults to "default" |
| `version` | string | Version is a semver constraint the PackageSource version must satisfy, e.g. "~1.16". Empty accepts any version. |

### Package.status

| Field | Type | Description |
|---|---|---|
| `conditions` | []object | Conditions represents the latest available observations of a Package's state |
| `currentRevision` | integer | CurrentRevision is the revision currently applied. |
| `dependencies` | object | Dependencies tracks the readiness status of each dependency Key is the dependency package name, value indicates if the dependency is ready |
| `history` | []object | History lists recent revisions, newest first. |
| `history[].phase` | string |  |
| `history[].revision` | integer, required |  |
| `history[].version` | string |  |
| `version` | string | Version is the package version currently applied. |


## PackageRevision

PackageRevision is an immutable record of one applied state of a package. It is written by the operator.

Short names: `pkgrev`.

### PackageRevision.spec

| Field | Type | Description |
|---|---|---|
| `components` | []object |  |
| `components[].chartDigest` | string | ChartDigest identifies the chart content that was applied. |
| `components[].dependsOn` | []string |  |
| `components[].name` | string, required |  |
| `components[].namespace` | string, required |  |
| `components[].releaseName` | string, required |  |
| `components[].valuesDigest` | string | ValuesDigest identifies the values that were applied. |
| `package` | string, required |  |
| `restoredFrom` | integer | RestoredFrom is set when this revision re-applies an earlier one. |
| `revision` | integer, required |  |
| `rollbackSafe` | boolean | RollbackSafe is copied from the PackageSource at the time of the revision. |
| `variant` | string |  |
| `version` | string |  |

### PackageRevision.status

| Field | Type | Description |
|---|---|---|
| `components` | []object |  |
| `components[].backendRevision` | integer | BackendRevision is e.g. the Helm release revision, used to roll back. |
| `components[].name` | string, required |  |
| `message` | string |  |
| `phase` | string |  |


## PackageSet

PackageSet installs packages on every Cluster it selects.

Short names: `pkgset`.

### PackageSet.spec

| Field | Type | Description |
|---|---|---|
| `clusterSelector` | object | ClusterSelector picks Clusters by label; empty selects all. |
| `clusterSelector.matchExpressions` | []object | matchExpressions is a list of label selector requirements. The requirements are ANDed. |
| `clusterSelector.matchExpressions[].key` | string, required | key is the label key that the selector applies to. |
| `clusterSelector.matchExpressions[].operator` | string, required | operator represents a key's relationship to a set of values. Valid operators are In, NotIn, Exists and DoesNotExist. |
| `clusterSelector.matchExpressions[].values` | []string | values is an array of string values. If the operator is In or NotIn, the values array must be non-empty. If the operator is Exists or DoesNotExist, the values array must be empty. This array is replaced during a strategic merge patch. |
| `clusterSelector.matchLabels` | object | matchLabels is a map of {key,value} pairs. A single {key,value} in the matchLabels map is equivalent to an element of matchExpressions, whose key field is "key", the operator is "In", and the values array contains only "value". The requirements are ANDed. |
| `packages` | []object |  |
| `packages[].name` | string, required |  |
| `packages[].spec` | object | Spec is the Package spec written to every selected cluster. |
| `packages[].spec.components` | object | Components is a map of release name to component overrides Allows overriding values and enabling/disabling specific components from the PackageSource |
| `packages[].spec.crdPolicy` | string: Retain, Delete | CRDPolicy decides what happens to owned CRDs when the package is removed. Retain (default) keeps them; Delete removes them and every object of their kinds. |
| `packages[].spec.ignoreDependencies` | []string | IgnoreDependencies is a list of package source dependencies to ignore Dependencies listed here will not be installed even if they are specified in the PackageSource |
| `packages[].spec.repository` | string | Repository restricts version selection to the named Repository. Empty considers all of them, by priority. Ignored when a PackageSource for the package was written by hand. |
| `packages[].spec.revisionHistoryLimit` | integer | RevisionHistoryLimit is how many PackageRevisions to keep. Default 10. |
| `packages[].spec.upgrade` | object | Upgrade controls how changes are applied. |
| `packages[].spec.upgrade.atomic` | boolean | Atomic rolls the whole package back when a revision fails, if the version being left declares rollback as safe. Default true. |
| `packages[].spec.upgrade.timeout` | string | Timeout for every component to become healthy. Default 10m. |
| `packages[].spec.variant` | string | Variant is the name of the variant to use from the PackageSource If not specified, defaults to "default" |
| `packages[].spec.version` | string | Version is a semver constraint the PackageSource version must satisfy, e.g. "~1.16". Empty accepts any version. |
| `repositories` | []object | Repositories are written to every selected cluster first. |
| `repositories[].name` | string, required |  |
| `repositories[].spec` | object, required | RepositorySpec says where a repository index is and how much to trust it relative to other repositories. |
| `repositories[].spec.interval` | string | Interval between index refreshes. Default 10m. |
| `repositories[].spec.priority` | integer | Priority orders repositories that carry the same package: the highest priority one shadows the others for that package, whatever versions they have. Default 0. |
| `repositories[].spec.publicKeys` | []string | PublicKeys are PEM encoded ed25519 keys the index must be signed with: the signature is fetched from the index URL plus ".sig". With none, the index is not checked. Several keys allow rotation. |
| `repositories[].spec.trust` | object | Trust verifies the repository the way TUF does: the root keys pinned here sign root/1.yaml, each later root version is signed by enough root keys of the one before and of itself, the current root names the keys and threshold the index needs, and both expire. Use it instead of publicKeys for threshold signing and key rotation. |
| `repositories[].spec.trust.rootKeys` | []string, required | RootKeys are PEM encoded ed25519 keys of version 1 of the root. |
| `repositories[].spec.trust.rootThreshold` | integer | RootThreshold is how many of them must have signed it. Default 1. |
| `repositories[].spec.url` | string, required | URL of the index, e.g. https://packages.example.org/index.yaml. The scheme picks the fetcher; https:// and http:// are built in. |

### PackageSet.status

| Field | Type | Description |
|---|---|---|
| `clusters` | []object |  |
| `clusters[].message` | string |  |
| `clusters[].name` | string, required |  |
| `clusters[].ready` | integer, required | Ready and Total count the set's packages on the cluster. |
| `clusters[].total` | integer, required |  |
| `conditions` | []object |  |
| `readyClusters` | integer | ReadyClusters counts clusters where every package is ready. |


## PackageSource

PackageSource is one package at one version: where its charts come from, what it provides and requires, and how its components are installed. The name is the package name.

Short names: `pks`.

### PackageSource.spec

| Field | Type | Description |
|---|---|---|
| `build` | integer | Build numbers the packagings of one upstream version: a new patch or default in the recipe makes a new build of the same version. Version constraints apply to Version; of two equal versions the higher build is newer. |
| `conflicts` | []string | Conflicts lists package names or capabilities that must not be installed alongside this package. |
| `crds` | []string | CRDs lists the names of CustomResourceDefinitions this package owns. |
| `images` | []string | Images are the container images the package runs, each pinned by digest: registry/repository:tag@sha256:..., including those an operator in the package deploys on its own. A signed index covers them like the charts, and kubepkg bundle copies them for air-gapped clusters. |
| `permissions` | object | Permissions declares what the package needs in the cluster. It is shown in plans so the operator of the cluster sees it before installing; it is not enforced yet. |
| `permissions.clusterWide` | boolean | ClusterWide is true when the package needs cluster-scoped access. |
| `permissions.rules` | []object | Rules are the RBAC rules the package's components need. |
| `permissions.rules[].apiGroups` | []string | apiGroups is the name of the APIGroup that contains the resources.  If multiple API groups are specified, any action requested against one of the enumerated resources in any API group will be allowed. "" represents the core API group and "*" represents all API groups. |
| `permissions.rules[].nonResourceURLs` | []string | nonResourceURLs is a set of partial urls that a user should have access to.  *s are allowed, but only as the full, final step in the path Since non-resource URLs are not namespaced, this field is only applicable for ClusterRoles referenced from a ClusterRoleBinding. Rules can either apply to API resources (such as "pods" or "secrets") or non-resource URL paths (such as "/api"),  but not both. |
| `permissions.rules[].resourceNames` | []string | resourceNames is an optional white list of names that the rule applies to.  An empty set means that everything is allowed. |
| `permissions.rules[].resources` | []string | resources is a list of resources this rule applies to. '*' represents all resources. |
| `permissions.rules[].verbs` | []string, required | verbs is a list of Verbs that apply to ALL the ResourceKinds contained in this rule. '*' represents all verbs. |
| `provides` | []string | Provides lists capabilities this package offers, e.g. "ingress" or "api:cert-manager.io/v1". The package name is always provided implicitly. |
| `rollback` | object | Rollback describes whether going back from this version is safe. |
| `rollback.safe` | boolean | Safe is true when rolling back from this version to the previous one does not lose data. Only then does a failed upgrade roll back automatically. |
| `sourceRef` | object | SourceRef is the source reference for the package source charts |
| `sourceRef.kind` | string: GitRepository, OCIRepository, OCIArtifact, required | Kind of the source reference. GitRepository and OCIRepository are Flux sources and need the flux backend; OCIArtifact is fetched by kubepkg. |
| `sourceRef.name` | string | Name of the source reference (Flux kinds) |
| `sourceRef.namespace` | string | Namespace of the source reference (Flux kinds) |
| `sourceRef.path` | string | Path is the base path where packages are located in the source. For GitRepository, defaults to "packages" if not specified. For OCIRepository and OCIArtifact, defaults to empty string (root) if not specified. |
| `sourceRef.url` | string | URL of an OCI artifact holding the package tree, e.g. oci://ghcr.io/example/packages:1.0.0 (OCIArtifact only). |
| `variants` | []object | Variants is a list of package source variants Each variant defines components, applications, dependencies, and libraries for a specific configuration |
| `variants[].components` | []object | Components is a list of Helm releases to be installed as part of this variant |
| `variants[].components[].chart` | object | Chart is a chart published in a Helm repository, used as is. A package made only of such components needs no package tree. |
| `variants[].components[].chart.digest` | string | Digest is the sha256 of the chart archive, sha256:<hex>. When set, a chart that does not match is refused, so a repository that changes a published version cannot change what gets installed. |
| `variants[].components[].chart.name` | string, required | Name is the chart name. |
| `variants[].components[].chart.repository` | string, required | Repository is an HTTP(S) Helm repository URL, or an oci:// path the chart is pushed under, e.g. oci://ghcr.io/example/charts. |
| `variants[].components[].chart.version` | string, required | Version is the exact chart version. Ranges are not accepted: a package version must always install the same chart. |
| `variants[].components[].install` | object | Install defines installation parameters for this component |
| `variants[].components[].install.dependsOn` | []string | DependsOn is a list of component names that must be installed before this component |
| `variants[].components[].install.healthCheckExprs` | []object | HealthCheckExprs are CEL health expressions for the custom resource(s) this component renders, so the component reports Ready only when the resource is actually healthy. |
| `variants[].components[].install.healthCheckExprs[].apiVersion` | string, required | APIVersion of the custom resource under evaluation. |
| `variants[].components[].install.healthCheckExprs[].current` | string, required | Current is the CEL expression that determines if the status of the custom resource has reached the desired state. |
| `variants[].components[].install.healthCheckExprs[].failed` | string | Failed is the CEL expression that determines if the status of the custom resource has failed to reach the desired state. |
| `variants[].components[].install.healthCheckExprs[].inProgress` | string | InProgress is the CEL expression that determines if the status of the custom resource has not yet reached the desired state. |
| `variants[].components[].install.healthCheckExprs[].kind` | string, required | Kind of the custom resource under evaluation. |
| `variants[].components[].install.namespace` | string | Namespace is the Kubernetes namespace where the release will be installed |
| `variants[].components[].install.phase` | string: PreUpgrade | Phase PreUpgrade makes the component a hook: it runs only when the package moves to another version, before every other component, with values kubepkg.fromVersion and kubepkg.toVersion, typically a Job that migrates data. A hook that fails stops the upgrade before anything else changes; it is uninstalled once the upgrade succeeds, so the next upgrade runs it afresh. |
| `variants[].components[].install.privileged` | boolean | Privileged indicates whether this release requires privileged access |
| `variants[].components[].install.readyWhen` | []object | ReadyWhen lists object conditions that must hold before the component counts as ready, for resources whose own readiness Helm cannot see, such as an operator's custom resource reporting Available. Every backend honours it; not meeting it within the upgrade timeout fails the revision. |
| `variants[].components[].install.readyWhen[].apiVersion` | string, required |  |
| `variants[].components[].install.readyWhen[].condition` | string, required | Condition is the type in status.conditions, e.g. Available. |
| `variants[].components[].install.readyWhen[].kind` | string, required |  |
| `variants[].components[].install.readyWhen[].name` | string, required |  |
| `variants[].components[].install.readyWhen[].namespace` | string | Namespace defaults to the component's install namespace and is ignored for cluster-scoped kinds. |
| `variants[].components[].install.readyWhen[].status` | string | Status is the wanted status of the condition. Default "True". |
| `variants[].components[].install.releaseName` | string | ReleaseName is the name of the HelmRelease resource that will be created If not specified, defaults to the component Name field |
| `variants[].components[].install.upgradeCRDs` | string: Skip, Create, CreateReplace | UpgradeCRDs controls how CRDs from the chart's crds/ directory are handled on upgrades. Empty keeps the backend default (Skip). Use "CreateReplace" for operators that evolve their CRD set between versions. Warning: CreateReplace overwrites CRDs and may cause data loss if upstream drops fields from a CRD with live objects. |
| `variants[].components[].install.waitStrategy` | string: poller, legacy | WaitStrategy is one of poller\|legacy (flux backend). |
| `variants[].components[].libraries` | []string | Libraries is a list of library names that this component depends on These libraries must be defined at the variant level |
| `variants[].components[].name` | string, required | Name is the unique identifier for this component within the package source |
| `variants[].components[].path` | string | Path is the chart directory inside the package tree. |
| `variants[].components[].valuesFiles` | []string | ValuesFiles is a list of values file names to use |
| `variants[].dependsOn` | []string | DependsOn is a list of package source dependencies For example: "networking" Equivalent to Requires entries with only Package set. |
| `variants[].libraries` | []object | Libraries is a list of Helm library charts used by components in this variant |
| `variants[].libraries[].name` | string | Name is the optional name for library placed in charts |
| `variants[].libraries[].path` | string, required | Path is the path to the library chart directory |
| `variants[].name` | string, required | Name is the unique identifier for this variant |
| `variants[].requires` | []object | Requires lists packages or capabilities this variant needs. |
| `variants[].requires[].capability` | string | Capability is a required capability, e.g. "ingress" or "api:cert-manager.io/v1". |
| `variants[].requires[].optional` | boolean | Optional requirements only order installation: if present, they must be ready first; if absent, the package proceeds. |
| `variants[].requires[].package` | string | Package is the name of a required package. |
| `variants[].requires[].version` | string | Version is a semver constraint on the required package, e.g. ">=1.2 <2". Only meaningful with Package. |
| `version` | string | Version is the semantic version of this package. Empty means unversioned (see UnversionedVersion). |

### PackageSource.status

| Field | Type | Description |
|---|---|---|
| `conditions` | []object | Conditions represents the latest available observations of a PackageSource's state |
| `variants` | string | Variants is a comma-separated list of package variant names This field is populated by the controller based on spec.variants keys |


## Repository

Repository is a package repository the operator installs packages from.

Short names: `pkgrepo`.

### Repository.spec

| Field | Type | Description |
|---|---|---|
| `interval` | string | Interval between index refreshes. Default 10m. |
| `priority` | integer | Priority orders repositories that carry the same package: the highest priority one shadows the others for that package, whatever versions they have. Default 0. |
| `publicKeys` | []string | PublicKeys are PEM encoded ed25519 keys the index must be signed with: the signature is fetched from the index URL plus ".sig". With none, the index is not checked. Several keys allow rotation. |
| `trust` | object | Trust verifies the repository the way TUF does: the root keys pinned here sign root/1.yaml, each later root version is signed by enough root keys of the one before and of itself, the current root names the keys and threshold the index needs, and both expire. Use it instead of publicKeys for threshold signing and key rotation. |
| `trust.rootKeys` | []string, required | RootKeys are PEM encoded ed25519 keys of version 1 of the root. |
| `trust.rootThreshold` | integer | RootThreshold is how many of them must have signed it. Default 1. |
| `url` | string, required | URL of the index, e.g. https://packages.example.org/index.yaml. The scheme picks the fetcher; https:// and http:// are built in. |

### Repository.status

| Field | Type | Description |
|---|---|---|
| `conditions` | []object |  |
| `indexDigest` | string | IndexDigest is the sha256 of the last accepted index. |
| `indexGenerated` | string | IndexGenerated is when the accepted index was built; an older one is refused. |
| `lastFetched` | string | LastFetched is when the index was last fetched successfully. |
| `packages` | integer | Packages is the number of packages in the index. |
| `rootDigest` | string |  |
| `rootVersion` | integer | RootVersion and RootDigest are the root accepted last; an older root or another root under the same version is refused. |

