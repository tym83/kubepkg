# Helm chart values

The operator's chart is published at `oci://ghcr.io/tym83/charts/kubepkg`, versioned with kubepkg. These are its values and their defaults, taken from the chart itself:

```yaml title="charts/kubepkg/values.yaml"
--8<-- "charts/kubepkg/values.yaml"
```

What the chart installs is described in [Installing kubepkg](../install.md). The chart refuses `backend: helm` or `backend: werf` together with `clusterAdmin: false`, because those backends could not install charts that create cluster roles.
