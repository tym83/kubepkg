# kubepkg

A package layer for people who build platforms and distributions on Kubernetes: versioned packages made from ordinary Helm charts, requirements on packages and capabilities, conflicts, CRD ownership, a plan before every change, and whole-package rollback where it is declared safe.

kubepkg works alongside Helm, werf, Flux and Argo CD rather than replacing them.

It runs in any Kubernetes cluster and can be embedded in a platform through configuration, without code changes.

Status: early; the API is `v1alpha1`.

**Documentation: https://tym83.github.io/kubepkg/**, starting with the [quickstart](https://tym83.github.io/kubepkg/quickstart/).

- [Why it exists and what it deliberately does differently](docs/rationale.md)
- [Design](docs/design.md)

Licensed under the [Apache License 2.0](LICENSE).
