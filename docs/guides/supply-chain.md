# Supply chain: SBOMs and vulnerabilities

A repository's signed index pins every chart and, once recipes list them, every container image of every package by digest. kubepkg can therefore say exactly what a package or a cluster runs, and check it against known vulnerabilities.

## Bills of materials

```bash
kubepkg sbom virtualization --repo https://tym83.github.io/kubepkg-recipes/index.yaml --public-key index.pub -o virtualization.cdx.json
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
kubepkg scan virtualization --repo https://tym83.github.io/kubepkg-recipes/index.yaml --public-key index.pub
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
