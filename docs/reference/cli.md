# CLI reference

Generated from the commands by `make docs`; do not edit.

Global flags:

```text
      --api-group string   API group the kubepkg types are served under (default "kubepkg.dev")
      --context string     kubeconfig context (default: the current one)
```

## kubepkg build

Build a package from a recipe and publish it

Build fetches the upstream sources a recipe pins, makes charts of them
(a chart is used as is, plain manifests are wrapped into one), applies the
recipe's values and overlays, and writes the package tree. With --registry
it pushes the tree and writes the PackageSource that installs it, pinned
to the pushed digest; feed those files to "kubepkg repo index".

The same recipe always builds the same content. Published versions are
immutable: when the tag already holds that content, the published
artifact is reused, whatever compressor built it; different content
under the same version and build is an error.

```text
kubepkg build <recipe-dir> [flags]

      --cache-dir string         chart download cache (default: the user cache directory)
  -o, --output string            directory for the published PackageSource (default "dist")
      --plain-http               talk to registries without TLS (local registries only)
      --registry string          OCI registry path to publish to, e.g. oci://ghcr.io/example/packages
      --registry-config string   Docker config file with registry credentials
      --work-dir string          where to build; kept for inspection when set
```

## kubepkg bundle create

Bundle packages, their requirements, charts and images into one file

Create resolves the packages and everything they require, as install
would for an empty cluster, and writes one tar file with the repository
files exactly as published (index, signatures, roots), the chart archives,
package trees and container images. Repositories are loaded with the same
checks a cluster applies. Packages come from the indexes given with
--repo, highest priority first, trusted with --public-key or --root-key,
or else from the cluster's repositories.

Images come from each package's pinned images. A package that pins none
gets the images its charts run, pinned as they are now; import refuses
those unless told to accept them.

```text
kubepkg bundle create <package>[@constraint]... -o <file.tar> [flags]

  -o, --output string            bundle file to write
      --plain-http               talk to registries without TLS (local registries only)
      --public-key stringArray   trust indexes signed with this ed25519 public key file (repeatable)
      --registry-config string   Docker config file with registry credentials
      --repo stringArray         repository index URL, highest priority first (repeatable; default: the cluster's repositories)
      --root-key stringArray     trust a repository with a root of trust through this pinned root key file (repeatable)
      --root-threshold int32     how many pinned root keys must have signed version 1 of the root (default 1)
      --variant string           variant whose requirements are resolved (default: default)
```

## kubepkg bundle import

Check a bundle and copy it into a mirror registry

Import checks a bundle against the repository keys you give, never keys
the bundle carries: signatures, the root chain and expiry as a cluster
checks them, then every chart, package tree and image against the signed
packages. Only then does it copy them into the mirror registry, keeping
every digest, and write the repositories' files to --site for serving
inside the air gap. Point the operator at the mirror (chart value mirror)
and the cluster's repositories at the site; --node-config writes the
containerd and Talos settings that send image pulls to the mirror.

```text
kubepkg bundle import <file.tar> --mirror oci://<registry>/<path> --public-key <file> [flags]

      --allow-unpinned-images    accept images that only the bundle pins, not a signed package
      --mirror string            oci:// registry path to copy into, the operator's mirror
      --node-config string       directory for containerd hosts.toml files and a Talos patch that send image pulls to the mirror
      --plain-http               talk to the mirror registry without TLS
      --public-key stringArray   trust indexes signed with this ed25519 public key file (repeatable)
      --registry-config string   Docker config file with credentials for the mirror registry
      --root-key stringArray     trust a repository with a root of trust through this pinned root key file (repeatable)
      --root-threshold int32     how many pinned root keys must have signed version 1 of the root (default 1)
      --site string              directory for the repositories' files, to serve over HTTP
      --work-dir string          where to unpack the bundle (default: the system temporary directory)
```

## kubepkg bundle inspect

List what a bundle carries, without checking it

```text
kubepkg bundle inspect <file.tar>
```

## kubepkg cluster add

Register a member cluster: its kubeconfig goes into a Secret in the hub

```text
kubepkg cluster add <name> --kubeconfig <file> [--member-context <ctx>] [--label k=v]... [flags]

      --kubeconfig string       kubeconfig of the member cluster
      --label stringArray       cluster label k=v for PackageSet selectors (repeatable)
      --member-context string   context in that kubeconfig (default: its current one)
      --namespace string        hub namespace for the kubeconfig Secret (default "kubepkg-system")
```

## kubepkg cluster list

List member clusters

```text
kubepkg cluster list
```

## kubepkg history

Show the revisions of a package

```text
kubepkg history <package>
```

## kubepkg images

Print the images a recipe's package runs, pinned by digest

Images builds the recipe without publishing it, finds the images its
charts run with their default values, adds those package.images already
lists (images an operator deploys on its own appear in no chart), pins
every one by the digest its tag points at now, and prints the
package.images block to paste into the recipe. Images already pinned keep
their digests; a published version whose images change needs a new build
number.

```text
kubepkg images <recipe-dir> [flags]

      --plain-http               talk to registries without TLS (local registries only)
      --registry-config string   Docker config file with registry credentials
```

## kubepkg init

Start a recipe from an upstream chart or release manifests

Init writes <dir>/recipe.yaml with every source pinned: it downloads the
upstream to compute digests, takes the description from the chart, drops
Namespaces from manifests, lists the CRDs they ship, and pins the images
they run by their current digests. Review it, add images an operator in
the package deploys on its own, then run "kubepkg validate".

  kubepkg init recipes/cert-manager --chart https://charts.jetstack.io/cert-manager@v1.21.2
  kubepkg init recipes/kubevirt --version 1.9.0 \\
    --manifest https://github.com/kubevirt/kubevirt/releases/download/v1.9.0/kubevirt-operator.yaml

```text
kubepkg init <dir> [flags]

      --chart string             upstream chart, <repository>/<name>@<version>, e.g. oci://ghcr.io/org/charts/app@1.2.3
      --description string       one line about the package (default: the chart's)
      --manifest stringArray     URL of upstream release manifests (repeatable)
      --name string              package name (default: the directory name)
      --namespace string         install namespace (default: the package name)
      --no-images                do not pin images (no registry access); fill package.images later with kubepkg images
      --plain-http               talk to registries without TLS (local registries only)
      --registry-config string   Docker config file with registry credentials, for pinning private images
      --version string           upstream version (default: the chart's appVersion)
```

## kubepkg install

Install or upgrade packages together with what they require

Install resolves the packages and everything they require against the
cluster's repositories, shows the plan, and on confirmation creates or
updates one Package per package; the operator does the rest. Without a
constraint a package follows patch releases of the version chosen (~X.Y).
Packages installed because another one required them are marked with the
kubepkg.dev/dependency annotation.

```text
kubepkg install <package>[@constraint]... [flags]

      --variant string   variant whose requirements are resolved (default: default)
  -y, --yes              apply without asking
```

## kubepkg list

List installed packages with their versions, revisions and readiness

```text
kubepkg list
```

## kubepkg plan

Show what installing or upgrading packages would change, without changing anything

```text
kubepkg plan <package>[@constraint]... [flags]

      --variant string   variant whose requirements are resolved (default: default)
```

## kubepkg push

Publish a package tree as an OCI artifact

Push packs a package tree into one reproducible gzipped tarball and
publishes it in the Flux artifact format, so kubepkg and Flux can both
read it. The same tree always produces the same digest.

```text
kubepkg push <dir> <oci-ref> [flags]

      --plain-http               talk to the registry without TLS (local registries only)
      --registry-config string   Docker config file with registry credentials
      --revision string          source revision recorded in the artifact
      --source string            source URL recorded in the artifact
```

## kubepkg remove

Remove packages that nothing else requires

Remove deletes the Packages; the operator uninstalls their releases. It
refuses while a package that stays requires one of them, directly or as
the only provider of a capability. With --autoremove it also removes
packages that were installed as requirements and that nothing needs any
more. CRDs a package owns stay unless its crdPolicy is Delete.

```text
kubepkg remove <package>... [flags]

      --autoremove   also remove requirements nothing needs any more
  -y, --yes          remove without asking
```

## kubepkg render

Write what Flux, Argo CD or helmfile need to install packages without the operator

Render resolves the packages and everything they require, as install
would for an empty cluster, and writes them for another delivery tool in
kubepkg's order: Flux sources and HelmReleases with dependsOn, Argo CD
Applications with sync waves, or a helmfile with needs. Commit the output
to Git and let the tool apply it. Packages come from the indexes given
with --repo, highest priority first, or else from the cluster's
repositories. Package defaults apply; set values in the output.

```text
kubepkg render <package>[@constraint]... [flags]

      --argo-namespace string    namespace of the Applications (argo; default argocd)
      --argo-project string      Argo CD project (argo; default default)
      --format string            output format: flux, argo, helmfile (default "flux")
      --insecure                 let Flux pull from registries over plain HTTP (flux)
      --public-key stringArray   with --repo: trust only indexes signed with this ed25519 public key file (repeatable)
      --repo stringArray         repository index URL, highest priority first (repeatable; default: the cluster's repositories)
      --variant string           variant to render (default: default)
```

## kubepkg repo add

Subscribe the cluster to a package repository

```text
kubepkg repo add <name> <index-url> [flags]

      --interval duration        how often the operator refreshes the index (default 10m)
      --priority int32           the highest priority repository carrying a package supplies it
      --public-key stringArray   trust only an index signed with this ed25519 public key file (repeatable)
      --root-key stringArray     pin this root key of a repository with a root of trust (repeatable)
      --root-threshold int32     how many pinned root keys must have signed version 1 of the root (default 1)
```

## kubepkg repo index

Build a repository index from the PackageSources under a directory

Index collects every PackageSource in the YAML files under dir into one
repository index. Each version needs an exact semver version; charts
without a digest are downloaded and pinned in the index, so a published
version always installs the same charts. Serve the index over HTTP as
index.yaml; packages and charts stay in their registries.

```text
kubepkg repo index <dir> [flags]

      --expires duration       how long clients accept the index; required by repositories with a root (e.g. 720h)
      --merge string           published index (file or URL) whose versions are kept; published versions must not change
  -o, --output string          where to write the index, - for stdout (default "index.yaml")
      --plain-http             talk to OCI registries without TLS (local registries only)
      --sign-key stringArray   sign the index with this ed25519 private key file (repeatable); signatures go next to it as .sig
      --sign-key-env string    also sign with the PEM private key in this environment variable, for CI secrets
      --verify                 also download pinned charts and check their digests
```

## kubepkg repo keygen

Make an ed25519 key pair for signing a repository index

Keygen writes <prefix>.key, the private key, readable only by you, and
<prefix>.pub, the public key clusters trust (repo add --public-key). Keep
the private key out of Git: store it in a secret manager or a CI secret.

```text
kubepkg repo keygen <prefix>
```

## kubepkg repo list

List the repositories the cluster uses

```text
kubepkg repo list
```

## kubepkg repo remove

Unsubscribe the cluster from a repository; installed packages stay

```text
kubepkg repo remove <name>
```

## kubepkg rollback

Re-apply an earlier revision of a package

Rollback asks the operator to re-apply an earlier successfully applied
revision. Without --to it picks the newest applied revision before the
current one. The operator records the result as a new revision.

```text
kubepkg rollback <package> [flags]

      --to int   revision to re-apply
```

## kubepkg search

Search the packages in the cluster's repositories

```text
kubepkg search [term]
```

## kubepkg set list

List PackageSets and how far each cluster got

```text
kubepkg set list
```

## kubepkg trust root new

Make version 1 of a root

```text
kubepkg trust root new [flags]

      --expires duration        how long the root stays valid (default 8760h0m0s)
      --index-key stringArray   public key file allowed to sign the index (repeatable)
      --index-threshold int     index signatures needed
  -o, --output string           where to write the unsigned root (default "root.yaml")
      --root-key stringArray    public key file allowed to sign the root (repeatable)
      --root-threshold int      root signatures needed
```

## kubepkg trust root next

Make the next root version, to rotate keys or change thresholds

Next writes the version after the given root. Roles not given keep their
keys and thresholds. It must be signed by enough root keys of the current
version and of the new one before clients accept it.

```text
kubepkg trust root next <current-root.yaml> [flags]

      --expires duration        how long the root stays valid (default 8760h0m0s)
      --index-key stringArray   public key file allowed to sign the index (repeatable)
      --index-threshold int     index signatures needed
  -o, --output string           where to write the unsigned root (default "root.yaml")
      --root-key stringArray    public key file allowed to sign the root (repeatable)
      --root-threshold int      root signatures needed
```

## kubepkg trust sign

Add one signature to a root or an index

Sign adds the key's signature to a root, inside the file, or to an index,
in <index>.sig next to it, keeping signatures already there: each signer
signs with their own key, on their own machine.

```text
kubepkg trust sign <root.yaml|index.yaml> [flags]

      --key string       ed25519 private key file
      --key-env string   environment variable holding the PEM private key
```

## kubepkg validate

Build recipes without publishing and check them

Validate builds each recipe without publishing it, so every source is
fetched and checked against its pin, renders the charts with their default
values, and checks the package against them: a CRD the charts ship but
the package does not declare is an error. A directory without a recipe
is searched for recipes below it.

```text
kubepkg validate <recipe-dir>... [flags]

      --plain-http   talk to registries without TLS (local registries only)
```

## kubepkg version

Print the kubepkg version

```text
kubepkg version
```
