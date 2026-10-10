#!/usr/bin/env bash
# End-to-end test on an existing cluster: a Helm install killed mid-way,
# as on a node reboot, leaves its release pending-install. The operator
# waits for the component's timeout, marks the release failed and
# finishes the install.
#
#   KUBE_CONTEXT   the cluster, kubepkg with the helm backend in kubepkg-system
#   IMAGE_TAG      operator image of the version under test
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
: "${KUBE_CONTEXT:?}" "${IMAGE_TAG:?}"
K="kubectl --context ${KUBE_CONTEXT}"
NS=interrupted-test

step() { printf '\n=== %s\n' "$(date +%H:%M:%S) $*"; }
fail() { echo "FAIL: $*"; exit 1; }
release_status() { helm --kube-context "${KUBE_CONTEXT}" -n "${NS}" status slow-ksm -o json 2>/dev/null | python3 -c 'import sys,json; print(json.load(sys.stdin)["info"]["status"])' 2>/dev/null || true; }
ready() { ${K} get packages.kubepkg.dev slow-ksm -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}'; }
message() { ${K} get packages.kubepkg.dev slow-ksm -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}'; }

step "operator under test"
helm --kube-context "${KUBE_CONTEXT}" upgrade kubepkg "${ROOT}/charts/kubepkg" -n kubepkg-system --reuse-values --set image.tag="${IMAGE_TAG}" --wait >/dev/null

step "a Helm install killed with SIGKILL mid-way, as on a node reboot"
${K} create namespace "${NS}" --dry-run=client -o yaml | ${K} apply -f - >/dev/null
helm --kube-context "${KUBE_CONTEXT}" install slow-ksm oci://ghcr.io/kuberoot-dev/kubepkg-packages/kube-state-metrics/kube-state-metrics \
  --version 2.20.0-3 -n "${NS}" --set readinessProbe.initialDelaySeconds=150 --wait --timeout 10m >/dev/null 2>&1 &
helm_pid=$!
for _ in $(seq 60); do [[ "$(release_status)" == pending-install ]] && break; sleep 2; done
kill -9 "${helm_pid}"; wait "${helm_pid}" 2>/dev/null || true
[[ "$(release_status)" == pending-install ]] || fail "the release is $(release_status), not left pending"
echo "  helm killed; release left pending-install"

step "a package for that release, with a 3m timeout"
${K} apply -f - >/dev/null <<EOF
apiVersion: kubepkg.dev/v1
kind: PackageSource
metadata: {name: slow-ksm}
spec:
  version: 2.20.0
  build: 3
  rollback: {safe: true}
  variants:
    - name: default
      components:
        - name: ksm
          chart: {repository: oci://ghcr.io/kuberoot-dev/kubepkg-packages/kube-state-metrics, name: kube-state-metrics, version: 2.20.0-3, digest: "sha256:2bf8279f5f6195dfb6332dfb8c1251bdcf5a00754dd97102411a614f685d329e"}
          install: {namespace: ${NS}, releaseName: slow-ksm}
---
apiVersion: kubepkg.dev/v1
kind: Package
metadata: {name: slow-ksm}
spec:
  upgrade: {timeout: 3m}
  components:
    ksm:
      values: {readinessProbe: {initialDelaySeconds: 150}}
EOF

step "the new operator waits for the timeout, then finishes"
start=$(date +%s); seen_wait=""
while (( $(date +%s) - start < 600 )); do
  m=$(message)
  [[ "${m}" == *"pending-install since"* ]] && seen_wait="${m}"
  [[ "$(ready)" == True ]] && break
  sleep 10
done
[[ "$(ready)" == True ]] || fail "not ready after 10 minutes: $(message); release $(release_status)"
[[ -n "${seen_wait}" ]] || fail "the package never said what it was waiting for"
echo "  while waiting: ${seen_wait}"
desc=$(helm --kube-context "${KUBE_CONTEXT}" -n "${NS}" history slow-ksm -o json | python3 -c 'import sys, json; print("; ".join("%s %s: %s" % (r["revision"], r["status"], r["description"]) for r in json.load(sys.stdin)))')
[[ "${desc}" == *"did not finish"* ]] || fail "the interrupted revision was not marked: ${desc}"
echo "  history: ${desc}"
echo "  package Ready: $(message)"

step "clean up"
${K} delete packages.kubepkg.dev slow-ksm --wait --timeout 300s >/dev/null
${K} delete packagesources.kubepkg.dev slow-ksm >/dev/null 2>&1 || true
${K} delete namespace "${NS}" --wait=false >/dev/null 2>&1 || true
echo
echo "PASS"
