#!/usr/bin/env bash
# End-to-end test of the argo backend: a two-component package is
# installed by Argo CD itself, one Application per component, the second
# only after the first is ready, with the Package's values; removing the
# Package removes the Applications and what they deployed.
#
# Needs docker, kind, kubectl, helm, python3 and go, and network access to the
# Argo CD release. Leaves nothing behind unless KEEP=1.
set -euo pipefail

CLUSTER=kubepkg-argo
REG_NAME=kubepkg-argo-registry
REG_PORT=${REG_PORT:-5006}
WWW_PORT=${WWW_PORT:-5007}
# shellcheck source=lib.sh
source "$(dirname "$0")/lib.sh"
HERE=${ROOT}/test/e2e


build_binaries
start_cluster

# CHART_HOST is how the cluster reaches this machine: the lima host under
# colima, the kind network gateway elsewhere.
if [[ -z "${CHART_HOST:-}" ]]; then
  if command -v colima >/dev/null && colima status >/dev/null 2>&1; then
    CHART_HOST=$(colima ssh -- getent hosts host.lima.internal | awk '{print $1}')
  else
    # The first IPv4 gateway: the network may list an IPv6 range first.
    CHART_HOST=$(docker network inspect kind -f '{{range .IPAM.Config}}{{.Gateway}} {{end}}' | tr ' ' '\n' | grep -m1 -v ':' || true)
  fi
fi
[[ -n "${CHART_HOST}" ]] || fail "cannot tell how the cluster reaches this machine; set CHART_HOST"
CHARTS="http://${CHART_HOST}:${WWW_PORT}"

diagnose() {
  ${K} -n argocd get applications.argoproj.io -o jsonpath='{range .items[*]}{.metadata.name}: sync={.status.sync.status} health={.status.health.status} op={.status.operationState.phase} {.status.operationState.message} {.status.conditions}{"\n"}{end}'
  ${K} -n argocd logs deploy/argocd-repo-server --tail=20
  echo "chart host: ${CHARTS}"
}

step "1. a Helm repository the cluster can reach"
mkdir -p "${WORK}/www"
helm package "${HERE}/recipes/hello/charts/hello" -d "${WORK}/www" >/dev/null
helm repo index "${WORK}/www" --url "${CHARTS}" >/dev/null
python3 -m http.server "${WWW_PORT}" --bind 0.0.0.0 --directory "${WORK}/www" >/dev/null 2>&1 &
WWW_PID=$!

step "2. install Argo CD"
${K} create namespace argocd >/dev/null
${K} apply -n argocd --server-side -f https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/install.yaml >/dev/null
${K} -n argocd rollout status deploy/argocd-repo-server --timeout=10m >/dev/null || fail "argocd-repo-server did not start"
${K} -n argocd rollout status statefulset/argocd-application-controller --timeout=10m >/dev/null || fail "argocd-application-controller did not start"

start_operator --backend argo

step "3. Argo CD installs the package"
${K} apply -f - <<EOF >/dev/null
apiVersion: kubepkg.dev/v1alpha1
kind: PackageSource
metadata: {name: pair}
spec:
  version: "1.0.0"
  variants:
    - name: default
      components:
        - name: first
          chart: {repository: "${CHARTS}", name: hello, version: 1.0.0}
          install: {namespace: e2e-argo-first, releaseName: hello}
        - name: second
          chart: {repository: "${CHARTS}", name: hello, version: 1.0.0}
          install: {namespace: e2e-argo-second, releaseName: hello, dependsOn: [first]}
---
apiVersion: kubepkg.dev/v1alpha1
kind: Package
metadata: {name: pair}
spec:
  components:
    second: {values: {image: "registry.k8s.io/pause:3.9"}}
EOF
wait_reason pair ReconciliationSucceeded 600
for app in e2e-argo-first-hello e2e-argo-second-hello; do
  [[ "$(${K} -n argocd get applications.argoproj.io "${app}" -o jsonpath='{.status.health.status}')" == Healthy ]] || fail "Application ${app} not healthy"
done
[[ "$(${K} -n e2e-argo-first get deploy hello -o jsonpath='{.spec.template.spec.containers[0].image}')" == registry.k8s.io/pause:3.10 ]] || fail "first: chart defaults"
[[ "$(${K} -n e2e-argo-second get deploy hello -o jsonpath='{.spec.template.spec.containers[0].image}')" == registry.k8s.io/pause:3.9 ]] || fail "second: Package values did not reach the Application"
first=$(${K} -n argocd get applications.argoproj.io e2e-argo-first-hello -o jsonpath='{.metadata.creationTimestamp}')
second=$(${K} -n argocd get applications.argoproj.io e2e-argo-second-hello -o jsonpath='{.metadata.creationTimestamp}')
[[ "${first}" < "${second}" ]] || fail "second was created before first was ready (${first} / ${second})"

step "4. remove it"
${K} delete packages.kubepkg.dev pair --wait --timeout 300s >/dev/null || fail "package not removed"
for ((i = 0; i < 120; i += 3)); do
  [[ -z "$(${K} -n argocd get applications.argoproj.io -o name 2>/dev/null)" ]] && ! ${K} -n e2e-argo-second get deploy hello >/dev/null 2>&1 && break
  sleep 3
done
[[ -z "$(${K} -n argocd get applications.argoproj.io -o name 2>/dev/null)" ]] || fail "Applications left behind"
${K} -n e2e-argo-second get deploy hello >/dev/null 2>&1 && fail "deployment left behind"

echo
echo "PASS"
