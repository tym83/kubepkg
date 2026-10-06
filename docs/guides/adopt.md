# Taking over existing installations

Most clusters already run things: cert-manager installed with `helm install` two years ago, an operator applied with `kubectl apply -f`, a monitoring stack from a vendor's script. `kubepkg adopt` brings them under kubepkg's management **without reinstalling them**. Nothing is deleted and nothing is recreated. The releases are upgraded in place to the package's charts, and from then on they get revisions, rollback, requirements and CRD ownership like any package.

## Look first

```bash
kubepkg adopt cert-manager
```

```text
--8<-- "examples/adopt.txt"
```

This is real output from a test cluster. cert-manager had been installed there with `helm install` from the upstream chart, with `crds.enabled=true` and `replicaCount=2`. After adoption the Deployment is the same object, it still runs two replicas, and the Helm release went from the upstream chart to the package's chart in place (revision 3).

For each component of the package, `adopt` looks for the Helm release the package would create: the same release name, in the same namespace. It shows what it found: the chart, its version, the release revision and status. It changes nothing until you add `--yes`.

## Releases under other names

If a release lives somewhere else, say where:

```bash
kubepkg adopt cert-manager --component cert-manager=security/certs --yes
```

The Package records the placement (`spec.components.cert-manager.namespace` and `releaseName`). The component stays there, now and on every later upgrade.

## Values

The release's current values, the ones it was installed or last upgraded with, are written into the Package as that component's values. The upgrade therefore keeps your configuration. The package's own defaults go underneath them, and the platform values Secret, if any, goes underneath those.

Values are copied as they are. A package built from the same upstream chart understands them. A package that wraps the upstream differently may use other keys, so read the package's chart before you adopt. A wrong key is ignored by most charts, not reported.

## What was not installed with Helm

Objects applied with `kubectl` or a script have no Helm release. Helm normally refuses to install over them ("exists and cannot be imported into the current release"). An adopted package takes them over. For that first revision, the operator installs with Helm's take-ownership (Nelm's force adoption for the werf backend), so the existing objects join the release in place.

Taking over applies to the adopting revision only. The Package carries the annotation `kubepkg.dev/adopt` until that revision has been applied, and the operator then removes it. A later change that happened to collide with some unrelated object fails as it should, instead of silently taking it over.

## Things to know

- Adopt a package's requirements first, or together, since the operator waits for them. `adopt` lists requirements the cluster does not have.
- A release that belongs to another kubepkg package is refused.
- If the installed version is newer than any version the repository offers, adopt with a constraint that matches, or the operator will move the release to an older chart. Read the plan.
- Adoption works with the helm and werf backends. Flux and Argo CD keep their own record of what they deployed and do not take over releases they did not create.
- After adoption, `kubepkg history` starts at revision 1, the adopting revision. What happened before is in the Helm release history.
