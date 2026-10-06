# Upgrades, readiness and hooks

This page is for package authors. It covers what a package declares so that its upgrades are safe: the order of its components, when each counts as ready, whether a failed upgrade may be rolled back, and what has to run before the upgrade.

## Order

Components are applied one at a time. A component listed in another's `install.dependsOn` is applied first, and the next one starts only once it is ready. This holds for every backend, including Flux and Argo CD, which have no ordering of their own across releases.

```yaml
components:
  - name: operator
    path: kubevirt-operator
    install: {namespace: kubevirt}
  - name: kubevirt
    path: kubevirt
    install: {namespace: kubevirt, dependsOn: [operator]}
```

Packages are ordered the same way. A package whose requirements are not ready waits with `RequirementsNotMet`. A requirement marked `optional: true` only orders the installation: if that package is installed, it must be ready first, and if it is absent, nothing waits for it.

## Readiness: readyWhen

By default a component is ready when its release is: Helm's readiness checks pass for the Deployments, StatefulSets, Jobs and other workloads it created. Often that is not enough. KubeVirt's chart creates one `KubeVirt` resource and returns at once, and the operator then spends minutes deploying the actual virtualization stack. `readyWhen` says what has to be true before the component counts as ready:

```yaml
- name: kubevirt
  path: kubevirt
  install:
    namespace: kubevirt
    dependsOn: [operator]
    readyWhen:
      - apiVersion: kubevirt.io/v1
        kind: KubeVirt
        name: kubevirt
        condition: Available      # status.conditions[type=Available]
        status: "True"            # the default
```

`namespace` defaults to the component's namespace and is ignored for cluster-scoped kinds. Every listed condition must hold. Until they all do, the package reports `Progressing`, and the next component does not start. If they do not hold within `upgrade.timeout`, the revision fails like any other failure and is rolled back when that is safe.

`readyWhen` works with every backend. With Flux, `healthCheckExprs` (CEL expressions in Flux's own format) are passed to the `HelmRelease` as well.

## Rollback safety

```yaml
package:
  rollback:
    safe: false    # KubeVirt does not support downgrades
```

`rollback.safe` answers one question: once this version has run, can the version before it run again? The answer belongs to the new version, because only its author knows what its migrations did. For a stateless controller it is `true`. For anything that migrates a schema, rewrites stored objects or bumps a storage version, it is usually `false`.

When an upgrade to a version with `safe: false` fails, kubepkg does not roll back. It stops and reports `UpgradeFailed`, and you fix forward. That is the honest outcome: a rollback that corrupts data is worse than a failed upgrade.

## Pre-upgrade hooks

Some upgrades need work done before the new version starts, such as a database migration or a backup. A component with `install.phase: PreUpgrade` is such a hook:

```yaml
components:
  - name: migrate
    path: migrate               # a chart with a Job
    install:
      namespace: app
      phase: PreUpgrade
  - name: app
    path: app
    install: {namespace: app}
```

The hook's chart gets two values: `kubepkg.fromVersion` and `kubepkg.toVersion`. A typical hook is a Job that decides from these what to migrate:

```yaml title="migrate/templates/job.yaml"
apiVersion: batch/v1
kind: Job
metadata:
  name: migrate-{{ .Values.kubepkg.toVersion | replace "." "-" }}
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: migrate
          image: example.org/app-migrate:{{ .Values.kubepkg.toVersion }}
          args: [--from, "{{ .Values.kubepkg.fromVersion }}", --to, "{{ .Values.kubepkg.toVersion }}"]
```

The rules:

- A hook runs only when a revision moves the package to **another version**. It does not run on a first install, on a values change, or on a rollback.
- It runs before every other component, and kubepkg waits for it to be ready. For a Job, that means it has completed.
- If the hook fails, the upgrade stops before anything else has changed. Nothing needs rolling back.
- Once the upgrade has succeeded, the hook's release is uninstalled, so the next upgrade runs it afresh.
- Hooks are not part of a revision's recorded state, and no other component may depend on one.

`kubepkg plan` lists the hooks an upgrade would run.

## Timeouts

`Package.spec.upgrade.timeout` is how long each component has to become ready, default ten minutes. A package that starts slowly can raise it in the Package. Give heavy components, such as a virtualization stack running under emulation, a generous timeout. A timeout that is too short turns a slow start into a rollback.
