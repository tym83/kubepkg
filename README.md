# kubepkg

A package layer for people who build platforms and distributions on Kubernetes: versioned packages made from ordinary Helm charts, requirements on packages and capabilities, conflicts, CRD ownership, a plan before every change, and whole-package rollback where it is declared safe.

kubepkg works alongside Helm, werf, Flux and Argo CD rather than replacing them.

It runs in any Kubernetes cluster and can be embedded in a platform through configuration, without code changes.

Status: stable; the API is `v1` and changes only in compatible ways.

> **kubepkg moved to the [kuberoot-dev](https://github.com/kuberoot-dev) organization in 1.2.0.** The Go module is now `github.com/kuberoot-dev/kubepkg`; releases up to 1.1.0 stay available under `github.com/tym83/kubepkg`. The operator image and the chart are published under `ghcr.io/kuberoot-dev`, the example repository's index is at `https://kuberoot-dev.github.io/kubepkg-recipes/index.yaml`, and the documentation at the address below. Old git and web links to GitHub redirect.

**Documentation: https://kuberoot-dev.github.io/kubepkg/**, starting with the [quickstart](https://kuberoot-dev.github.io/kubepkg/quickstart/).

- [Why it exists and what it deliberately does differently](docs/rationale.md)
- [Design](docs/design.md)

Licensed under the [Apache License 2.0](LICENSE).
