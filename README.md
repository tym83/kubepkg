# kubepkg

A package layer for people who build platforms and distributions on Kubernetes: versioned packages made from ordinary Helm charts, requirements on packages and capabilities, conflicts, CRD ownership, a plan before every change, and whole-package rollback where it is declared safe.

kubepkg works alongside Helm, Argo CD, Flux and werf rather than replacing them.

It runs in any Kubernetes cluster and can be embedded in a platform through configuration, without code changes.

Status: early prototype.

- [Why it exists and what it deliberately does differently](docs/rationale.md)
- [Design of v0.1](docs/design.md)

Licensed under the [Apache License 2.0](LICENSE).
