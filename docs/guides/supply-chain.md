# Supply chain: SBOMs and vulnerabilities

A repository's signed index pins every chart and, once recipes list them, every container image of every package by digest. kubepkg can therefore say exactly what a package or a cluster runs, and check it against known vulnerabilities.

## Bills of materials

```bash
kubepkg sbom virtualization --repo https://kuberoot-dev.github.io/kubepkg-recipes/index.yaml --public-key index.pub -o virtualization.cdx.json
kubepkg sbom --cluster -o cluster.cdx.json
```

`kubepkg sbom` writes a [CycloneDX](https://cyclonedx.org/) 1.6 document. It contains:

- one component per package, with its version, build, repository and spec digest;
- one component per Helm chart, with the SHA-256 of the archive;
- one component per container image, with its digest and an OCI package URL (`pkg:oci/cdi-controller@sha256%3A…?repository_url=quay.io/kubevirt/cdi-controller&tag=v1.66.1`);
- dependencies: each package depends on its charts and images and on the packages it requires.

With package names, `sbom` resolves the packages and their requirements from the repositories, as `install` would for an empty cluster. With `--cluster`, it describes what the cluster runs now. The same packages always give the same document, so it can be compared and signed. `--timestamp` fixes the recorded time as well.

Bundles carry the same document as `sbom.cdx.json`. `bundle import --site` puts it next to the served index, as the record of what entered the air gap.

Any tool that reads CycloneDX accepts these documents, for example Dependency-Track, `trivy sbom` or `grype sbom:`.

## Vulnerability reports

```bash
kubepkg scan virtualization --repo https://kuberoot-dev.github.io/kubepkg-recipes/index.yaml --public-key index.pub
kubepkg scan --cluster --fail-on critical
```

`kubepkg scan` runs [Trivy](https://trivy.dev) on every image the packages pin, by digest, and reports the findings by severity for each package and image. An image shared by several packages is scanned once. Real output for the example repository:

```text
--8<-- "examples/scan.txt"
```

- `--fail-on critical` (or `high`, `medium`, `low`) makes `scan` exit non-zero, to gate a CI pipeline or a release of a distribution.
- `--mirror oci://registry.internal/kubepkg` scans the copies in an air-gapped mirror. Trivy then needs its vulnerability database available offline; pass its options with `--trivy-arg`, for example `--trivy-arg=--skip-db-update`.
- Packages that pin no images cannot be scanned and are listed as such. Pin them with `kubepkg images`.

kubepkg does not scan anything itself and keeps no vulnerability data: it knows what to scan, and Trivy knows what is vulnerable.

## Keeping pods on pinned images

The signed index says which image digests a package runs. With the image policy on, the cluster holds pods to it:

```bash
helm upgrade kubepkg oci://ghcr.io/kuberoot-dev/charts/kubepkg -n kubepkg-system --reset-then-reuse-values \
  --set imagePolicy=enforce        # or warn
```

The operator labels the namespaces packages install into (`kubepkg.dev/image-policy`) and serves a pod admission webhook there:

- **A tag a package pins becomes that tag at its pinned digest.** Charts usually refer to images by tag. The pod runs `quay.io/jetstack/cert-manager-controller:v1.21.2@sha256:70f5…`, so nodes pull exactly the image the index vouches for, even if somebody moves the tag in the upstream registry.
- **In `enforce` mode, an image no installed package pins is refused**:

  ```text
  Error from server: admission webhook "pods.images.kubepkg.dev" denied the request: no installed package pins docker.io/library/busybox:1.36; namespace cert-manager runs only images packages pin (kubepkg.dev/image-policy=enforce)
  ```

- **In `warn` mode it runs, with a warning**, which is a safe way to see what enforce would refuse.

Admission warnings go back to whoever creates the pod, which is usually a controller, so people rarely see them. The webhook therefore also leaves an Event on the pod's owner: `UnpinnedImage` in warn mode, `ImageRefused` in enforce mode. `kubectl get events -n <namespace>` shows them. To check a whole cluster at once, before you turn `enforce` on or at any time:

```bash
kubepkg images --cluster
```

```text
NAMESPACE     PACKAGES      OWNER      IMAGE NO PACKAGE PINS
cert-manager  cert-manager  Pod/stray  docker.io/library/busybox:1.36
error: 1 images run unpinned; add them to package.images (kubepkg images <recipe>), or imagePolicy=enforce will refuse them
```

It checks every running container in the namespaces packages install into against what the installed packages pin, as the webhook would, and exits non-zero when something is unpinned.

**Operators start pods of their own.** Envoy for each Gateway, vmagent and vmalert, config reloaders, helper pods: these images appear in no chart, so `kubepkg images <recipe>` cannot find them. Add them to the recipe's `package.images` by hand, or `enforce` refuses them. `kubepkg images --cluster` on a cluster where the operator is running shows what is missing.

A namespace where some package pins no images stays at `warn`, since `enforce` would refuse that package's own pods. Shared system namespaces (`kube-system`, `kube-public`, `kube-node-lease`) are never labelled, even when packages such as CoreDNS install there: the cluster itself and tools other than packages start pods in them, from critical pods to debug pods. Change the list with `imagePolicyExclude`. A namespace an admin labels `kubepkg.dev/image-policy=off` is left alone. Pin them with `kubepkg images`. Namespaces that no package installs into are not touched, and neither are pods that operators create in users' namespaces, such as KubeVirt's virtual machine pods.

With `enforce`, the webhook's failure policy is `Fail`: while no operator replica answers, pods in package namespaces cannot be created. Run two replicas (`replicas: 2`). The operator issues the webhook's certificate itself, shares it between replicas through a Secret, and renews it ahead of expiry, so kubepkg does not depend on a certificate manager it may be the one installing.
