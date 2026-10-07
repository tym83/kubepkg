# Recipe reference

A recipe is `recipe.yaml` in a directory, read by `kubepkg init`, `validate` and `build`. Unknown fields are errors, so a typo fails the build instead of being ignored.

```yaml
apiVersion: kubepkg.dev/v1beta1
kind: Recipe
metadata:
  name: <package name>
  annotations:
    kubepkg.dev/description: <one line, shown by search and in the index>
    kubepkg.dev/home: <project URL>
spec:
  version: <upstream semver>
  build: <int>
  sources: {<name>: Source}
  charts: {<name>: Chart}
  package: PackageSourceSpec
```

## spec

| Field | Type | Description |
|---|---|---|
| `version` | string, required | the upstream version being packaged, exact semver |
| `build` | int | numbers your packagings of that version; raise it whenever the built content changes. Of two builds of one version, the higher is newer |
| `sources` | map of Source | the upstream inputs, by name |
| `charts` | map of Chart | the charts built into the package, by name; empty for a meta package |
| `verify` | Verify | how the maintainers check the upstream: image signatures, checked at validate and build time |
| `package` | [PackageSourceSpec](api.md#packagesource) | what the operator installs. Components refer to built charts by `path: <chart name>`. `version`, `build` and the chart digests are filled in by the build |

## Verify

| Field | Type | Description |
|---|---|---|
| `images` | list of ImageSignature | signatures the pinned images must carry, checked with `cosign verify` by digest |
| `charts` | list of ChartSignature | chart sources checked against their upstream's provenance files |

### ChartSignature

| Field | Type | Description |
|---|---|---|
| `sources` | list of string, required | chart sources of the recipe |
| `keyring` | string, required | a GPG public keyring file next to the recipe |

### ImageSignature

| Field | Type | Description |
|---|---|---|
| `repositories` | list of string, required | image repositories the rule covers, `registry/path`, with `*` standing for one path element |
| `key` | string | a public key file next to the recipe |
| `identity`, `issuer` | string | a keyless signature: the certificate subject and its OIDC issuer; give both, or a key |
| `signatureDigest` | string | `sha384` or `sha512` when the upstream does not sign sha256 digests |
| `ignoreTransparencyLog` | bool | accept signatures not recorded in a transparency log; only with a key |

## Source

Exactly one of `url`, `chart`, `dir` or `plugin`.

| Field | Type | Description |
|---|---|---|
| `url` | string | a file, or a `.tar.gz`/`.tgz` archive |
| `sha256` | string, required with `url` | checksum of the download |
| `path` | string | with an archive: the file or directory inside it to use |
| `chart` | object | a published Helm chart: `repository` (`https://...` or `oci://...`), `name`, `version`, `digest` (sha256 of the chart archive) |
| `dir` | string | a directory next to the recipe |
| `plugin` | string | a source plugin: Go code registered in `build.Options.Plugins`, or the executable `kubepkg-source-<plugin>` on `PATH` |
| `with` | object | parameters for the plugin, passed as JSON on standard input |

## Chart

| Field | Type | Description |
|---|---|---|
| `from` | list of source names, required | one source that is a chart, used as the chart; or sources of plain manifests, wrapped into a chart in the order given |
| `values` | object | merged over the chart's `values.yaml`; the package's defaults |
| `overlay` | string | a directory next to the recipe whose files replace or add files in the chart |
| `exclude` | list of Selector | objects dropped from wrapped manifests; for a chart source, objects dropped from the `crds/` directories of the chart and its subcharts, packed ones included. A file left empty is removed |
| `patches` | list of Patch | JSON merge patches applied to objects of wrapped manifests; for a chart source, to its `crds/` directories |
| `crdsDir` | bool | wrapped manifests only: put their CRDs in the chart's `crds/` instead of its templates. Helm keeps templates in a release record of about 1 MiB, so large CRDs fit only in `crds/`. Helm does not upgrade `crds/`; set `install.upgradeCRDs: CreateReplace` on the component |
| `steps` | list of `{plugin, with}` | step plugins run on the finished chart, in order: Go code, or the executable `kubepkg-step-<plugin>` |

### Selector

| Field | Description |
|---|---|
| `kind` | object kind; empty matches any |
| `name` | object name; empty matches any |

### Patch

A Selector plus:

| Field | Description |
|---|---|
| `merge` | a JSON merge patch (RFC 7386): objects merge, other values replace, `null` removes. A patch that matches no object fails the build |

## Plugin environment

Executable plugins run in the recipe directory with:

| Variable | Set for | Value |
|---|---|---|
| `KUBEPKG_OUT` | source plugins | an empty directory to fill with the source |
| `KUBEPKG_CHART` | step plugins | the chart directory to change |
| `KUBEPKG_PACKAGE` | both | the package name |
| `KUBEPKG_VERSION` | both | the package version |

Standard input is `with` as JSON. A non-zero exit fails the build, and the plugin's standard error is shown.
