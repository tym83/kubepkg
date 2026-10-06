#!/usr/bin/env bash
# End-to-end test on kind with the helm backend.
#
# Scenario: install a two-component package, upgrade it, break an upgrade
# (an image that does not exist) and check the whole package rolls back,
# confirm the operator does not retry the same failure, fix forward, install
# a package that depends on it, roll back by request, then delete.
#
# Needs docker, kind, kubectl, helm and go. Leaves nothing behind unless KEEP=1.
set -euo pipefail

# shellcheck source=lib.sh
source "$(dirname "$0")/lib.sh"

revisions() {
  ${K} get packagerevisions.kubepkg.dev -l kubepkg.dev/package="$1" \
    -o jsonpath='{range .items[*]}{.spec.revision}:{.spec.version}:{.status.phase}{" "}{end}' | tr ' ' '\n' | sort -n | tr '\n' ' '
}

image_of() { ${K} -n "$1" get deploy "$2" -o jsonpath='{.spec.template.spec.containers[0].image}'; }

# tree <name> <demo-version> <web-image> builds and pushes a package tree.
tree() {
  local name=$1 img=$2
  local dir=${WORK}/tree-${name}
  rm -rf "${dir}" && cp -R "${ROOT}/test/e2e/tree-base" "${dir}"
  printf 'image: %s\n' "${img}" > "${dir}/packages/demo/web/base.yaml"
  "${ROOT}/bin/kubepkg" push "${dir}/packages" "oci://${REG}/e2e/packages:${name}" --plain-http --source e2e --revision "${name}" >/dev/null
}

# demo_source <version> <tag> applies the demo PackageSource.
demo_source() {
  ${K} apply -f - <<EOF
apiVersion: kubepkg.dev/v1alpha1
kind: PackageSource
metadata: {name: demo}
spec:
  version: "$1"
  sourceRef: {kind: OCIArtifact, url: "oci://${REG}/e2e/packages:$2"}
  provides: [demo-api]
  crds: [widgets.e2e.kubepkg.dev]
  rollback: {safe: true}
  variants:
    - name: default
      libraries: [{name: common, path: library/common}]
      components:
        - name: api
          path: demo/api
          libraries: [common]
          install: {namespace: e2e-demo}
        - name: web
          path: demo/web
          libraries: [common]
          valuesFiles: [values.yaml, base.yaml]
          install: {namespace: e2e-demo, dependsOn: [api]}
EOF
}

build_binaries
start_cluster
${K} create namespace kubepkg-system >/dev/null
${K} -n kubepkg-system create secret generic platform-values \
  --from-literal=values.yaml=$'platform:\n  domain: e2e.example\n' >/dev/null

step "publish trees"
tree v1 registry.k8s.io/pause:3.10
tree v2-broken registry.k8s.io/pause:does-not-exist-e2e
tree v3 registry.k8s.io/pause:3.9

# BACKEND picks the installer: helm (default) or werf (needs nelm on PATH).
start_operator --backend "${BACKEND:-helm}" --values-secret kubepkg-system/platform-values --namespace-label e2e.kubepkg.dev/managed=true

step "1. install demo 1.0.0"
demo_source 1.0.0 v1
${K} apply -f - <<EOF
apiVersion: kubepkg.dev/v1alpha1
kind: Package
metadata: {name: demo}
spec:
  upgrade: {timeout: 60s}
EOF
wait_reason demo ReconciliationSucceeded
[[ "$(image_of e2e-demo web)" == registry.k8s.io/pause:3.10 ]] || fail "web image after install"
[[ "$(${K} get crd widgets.e2e.kubepkg.dev -o jsonpath='{.metadata.annotations.kubepkg\.dev/owned-by}')" == demo ]] || fail "CRD ownership not claimed"
[[ "$(${K} -n e2e-demo get deploy web -o jsonpath='{.metadata.annotations.e2e\.kubepkg\.dev/domain}')" == e2e.example ]] || fail "platform values secret did not reach the release"
[[ "$(${K} get ns e2e-demo -o jsonpath='{.metadata.labels.e2e\.kubepkg\.dev/managed}')" == true ]] || fail "namespace label not set"
echo "  revisions: $(revisions demo)"

step "2. broken upgrade to 1.1.0 rolls the whole package back"
demo_source 1.1.0 v2-broken
wait_reason demo UpgradeRolledBack 300
[[ "$(image_of e2e-demo web)" == registry.k8s.io/pause:3.10 ]] || fail "web not rolled back: $(image_of e2e-demo web)"
echo "  revisions: $(revisions demo)"
[[ "$(revisions demo)" == "1:1.0.0:Superseded 2:1.1.0:Failed 3:1.0.0:Applied " ]] || fail "unexpected revisions"

step "3. the failure is not retried while nothing changes"
sleep 20
[[ "$(revisions demo)" == "1:1.0.0:Superseded 2:1.1.0:Failed 3:1.0.0:Applied " ]] || fail "operator retried a held package: $(revisions demo)"

step "4. fix forward to 1.2.0"
# An operator-like field manager takes over a field the chart sets; the
# upgrade must still apply the chart's value instead of failing on a
# server-side apply conflict.
${K} -n e2e-demo annotate deploy web e2e.kubepkg.dev/domain=taken-over --overwrite --field-manager=intruder >/dev/null
demo_source 1.2.0 v3
wait_reason demo ReconciliationSucceeded
[[ "$(image_of e2e-demo web)" == registry.k8s.io/pause:3.9 ]] || fail "web image after fix"
[[ "$(${K} -n e2e-demo get deploy web -o jsonpath='{.metadata.annotations.e2e\.kubepkg\.dev/domain}')" == e2e.example ]] || fail "the upgrade did not take back a field another manager had changed"
echo "  revisions: $(revisions demo)"

step "5. a dependent package waits for its requirement, then installs"
${K} apply -f - <<EOF
apiVersion: kubepkg.dev/v1alpha1
kind: PackageSource
metadata: {name: app}
spec:
  version: "0.1.0"
  sourceRef: {kind: OCIArtifact, url: "oci://${REG}/e2e/packages:v3"}
  variants:
    - name: default
      libraries: [{name: common, path: library/common}]
      requires:
        - {package: demo, version: ">=1.2"}
        - {capability: "api:apps/v1"}
        - {capability: demo-api}
      components:
        - name: frontend
          path: app/frontend
          libraries: [common]
          install: {namespace: e2e-app}
---
apiVersion: kubepkg.dev/v1alpha1
kind: Package
metadata: {name: app}
EOF
wait_reason app ReconciliationSucceeded
${K} apply -f - <<EOF
apiVersion: kubepkg.dev/v1alpha1
kind: Package
metadata: {name: blocked}
---
apiVersion: kubepkg.dev/v1alpha1
kind: PackageSource
metadata: {name: blocked}
spec:
  version: "0.1.0"
  sourceRef: {kind: OCIArtifact, url: "oci://${REG}/e2e/packages:v3"}
  variants:
    - name: default
      requires: [{package: demo, version: ">=2"}]
EOF
wait_reason blocked RequirementsNotMet

step "6. roll back by request to revision 1"
"${ROOT}/bin/kubepkg" --context "${KCTX}" rollback demo --to 1 >/dev/null
# A rollback someone asked for is ready once it runs.
wait_reason demo RolledBack
[[ "$(image_of e2e-demo web)" == registry.k8s.io/pause:3.10 ]] || fail "manual rollback did not restore 1.0.0"
echo "  revisions: $(revisions demo)"

step "7. the CLI reports packages and history"
"${ROOT}/bin/kubepkg" --context "${KCTX}" list | grep -Eq '^demo +1\.0\.0 +5 ' || fail "list does not show demo 1.0.0 at revision 5"
"${ROOT}/bin/kubepkg" --context "${KCTX}" history demo | grep -Eq '^5 +1\.0\.0 +Applied .*restored from 1' || fail "history does not show the restore"

step "8. a package made of a published chart, with no package tree"
mkdir -p "${WORK}/hello/templates"
printf 'apiVersion: v2\nname: hello\nversion: 0.1.0\n' > "${WORK}/hello/Chart.yaml"
printf 'image: registry.k8s.io/pause:3.10\n' > "${WORK}/hello/values.yaml"
cat > "${WORK}/hello/templates/deploy.yaml" <<'CHART'
apiVersion: apps/v1
kind: Deployment
metadata: {name: hello}
spec:
  selector: {matchLabels: {app: hello}}
  template:
    metadata: {labels: {app: hello}}
    spec:
      containers:
        - name: main
          image: {{ .Values.image }}
          resources: {requests: {cpu: 5m, memory: 8Mi}}
CHART
helm package "${WORK}/hello" -d "${WORK}" >/dev/null
helm push "${WORK}/hello-0.1.0.tgz" "oci://${REG}/e2e/charts" --plain-http >/dev/null 2>&1 || fail "helm push"
HELLO_DIGEST="sha256:$(shasum -a 256 "${WORK}/hello-0.1.0.tgz" | cut -d' ' -f1)"
${K} apply -f - <<EOF
apiVersion: kubepkg.dev/v1alpha1
kind: PackageSource
metadata: {name: hello}
spec:
  version: "0.1.0"
  variants:
    - name: default
      components:
        - name: hello
          chart: {repository: "oci://${REG}/e2e/charts", name: hello, version: 0.1.0, digest: "${HELLO_DIGEST}"}
          install: {namespace: e2e-hello}
---
apiVersion: kubepkg.dev/v1alpha1
kind: Package
metadata: {name: hello}
spec:
  components:
    hello: {values: {image: "registry.k8s.io/pause:3.9"}}
EOF
wait_reason hello ReconciliationSucceeded
[[ "$(image_of e2e-hello hello)" == registry.k8s.io/pause:3.9 ]] || fail "package values did not reach the published chart"
bad=$(${K} apply -f - 2>&1 <<EOF || true
apiVersion: kubepkg.dev/v1alpha1
kind: PackageSource
metadata: {name: both}
spec:
  variants:
    - name: default
      components:
        - {name: x, path: x, chart: {repository: "oci://${REG}/e2e/charts", name: hello, version: 0.1.0}}
EOF
)
[[ "${bad}" == *"exactly one of path and chart"* ]] || fail "a component with both path and chart was accepted: ${bad}"

step "9. install from a signed repository with the CLI, requirements included, then upgrade"
sed -i.bak 's/^version: .*/version: 0.2.0/' "${WORK}/hello/Chart.yaml"
printf 'image: registry.k8s.io/pause:3.9\n' > "${WORK}/hello/values.yaml"
helm package "${WORK}/hello" -d "${WORK}" >/dev/null
helm push "${WORK}/hello-0.2.0.tgz" "oci://${REG}/e2e/charts" --plain-http >/dev/null 2>&1 || fail "helm push 0.2.0"
mkdir -p "${WORK}/recipes"
for v in 0.1.0 0.2.0; do
  cat > "${WORK}/recipes/greeter-${v}.yaml" <<EOF
apiVersion: kubepkg.dev/v1alpha1
kind: PackageSource
metadata:
  name: greeter
  annotations: {kubepkg.dev/description: Says hello}
spec:
  version: "${v}"
  variants:
    - name: default
      requires: [{package: salute}]
      components:
        - name: hello
          chart: {repository: "oci://${REG}/e2e/charts", name: hello, version: ${v}}
          install: {namespace: e2e-greeter}
EOF
done
cat > "${WORK}/recipes/salute-1.0.0.yaml" <<EOF
apiVersion: kubepkg.dev/v1alpha1
kind: PackageSource
metadata: {name: salute}
spec:
  version: "1.0.0"
  variants:
    - name: default
      components:
        - name: hello
          chart: {repository: "oci://${REG}/e2e/charts", name: hello, version: 0.1.0}
          install: {namespace: e2e-salute}
EOF
mkdir -p "${WORK}/www"
"${ROOT}/bin/kubepkg" repo keygen "${WORK}/signing" >/dev/null
"${ROOT}/bin/kubepkg" repo index "${WORK}/recipes" --plain-http -o "${WORK}/www/index.yaml" --sign-key "${WORK}/signing.key" 2>/dev/null
WWW_PORT=${WWW_PORT:-5002}
python3 -m http.server "${WWW_PORT}" --bind 127.0.0.1 --directory "${WORK}/www" >/dev/null 2>&1 &
WWW_PID=$!
KP=("${ROOT}/bin/kubepkg" --context "${KCTX}")
for ((i = 0; i < 20; i++)); do curl -sf "http://127.0.0.1:${WWW_PORT}/index.yaml" >/dev/null && break; sleep 0.5; done
"${KP[@]}" repo add e2e "http://127.0.0.1:${WWW_PORT}/index.yaml" --interval 30s --public-key "${WORK}/signing.pub" >/dev/null
"${KP[@]}" search hello | grep -Eq '^greeter +0\.2\.0 +e2e +Says hello' || fail "search does not find greeter"
plan=$("${KP[@]}" plan greeter@~0.1)
[[ "${plan}" == *"install  salute"*"required by greeter 0.1.0"* && "${plan}" == *"install  greeter"*"requested"* ]] || fail "unexpected plan: ${plan}"
"${KP[@]}" install greeter@~0.1 --yes >/dev/null
wait_reason salute ReconciliationSucceeded
wait_reason greeter ReconciliationSucceeded
[[ -n "$(${K} get packages.kubepkg.dev salute -o jsonpath='{.metadata.annotations.kubepkg\.dev/dependency}')" ]] || fail "salute not marked as a dependency"
[[ "$(${K} get packagesources.kubepkg.dev greeter -o jsonpath='{.spec.version}/{.metadata.labels.kubepkg\.dev/repository}')" == 0.1.0/e2e ]] || fail "greeter source not taken from the repository at 0.1.0"
[[ "$(image_of e2e-greeter hello)" == registry.k8s.io/pause:3.10 ]] || fail "greeter 0.1.0 not installed"
"${KP[@]}" install "greeter@>=0.1" --yes >/dev/null
for ((i = 0; i < 120; i += 3)); do
  [[ "$(image_of e2e-greeter hello)" == registry.k8s.io/pause:3.9 ]] && break
  sleep 3
done
[[ "$(image_of e2e-greeter hello)" == registry.k8s.io/pause:3.9 ]] || fail "greeter did not move to 0.2.0"
wait_reason greeter ReconciliationSucceeded
echo "  greeter: $(${K} get packages.kubepkg.dev greeter -o jsonpath='{.status.version}')"

# An index changed after signing is refused, and the cluster keeps the one
# it accepted.
printf '# tampered\n' >> "${WORK}/www/index.yaml"
for ((i = 0; i < 90; i += 3)); do
  [[ "$(${K} get repositories.kubepkg.dev e2e -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}')" == IndexRefused ]] && break
  sleep 3
done
[[ "$(${K} get repositories.kubepkg.dev e2e -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}')" == IndexRefused ]] || fail "a tampered index was not refused"
"${KP[@]}" search greeter 2>&1 | grep -q "left out" || fail "the CLI used a tampered index"
wait_reason greeter ReconciliationSucceeded
echo "  tampered index refused"
kill "${WWW_PID}" 2>/dev/null || true

step "10. delete"
${K} delete packages.kubepkg.dev app blocked demo hello greeter salute --wait --timeout 180s >/dev/null
${K} -n e2e-hello get deploy hello >/dev/null 2>&1 && fail "deployment hello left behind"
${K} -n e2e-greeter get deploy hello >/dev/null 2>&1 && fail "deployment greeter/hello left behind"
for ((i = 0; i < 30; i += 3)); do
  ${K} get packagesources.kubepkg.dev greeter >/dev/null 2>&1 || break
  sleep 3
done
${K} get packagesources.kubepkg.dev greeter >/dev/null 2>&1 && fail "the PackageSource made from the repository outlived its Package"
for d in api web; do
  ${K} -n e2e-demo get deploy "$d" >/dev/null 2>&1 && fail "deployment $d left behind"
done
${K} get crd widgets.e2e.kubepkg.dev >/dev/null 2>&1 || fail "a retained CRD was deleted"
[[ -z "$(${K} get crd widgets.e2e.kubepkg.dev -o jsonpath='{.metadata.annotations.kubepkg\.dev/owned-by}')" ]] || fail "CRD still claimed after delete"

echo
echo "PASS"
