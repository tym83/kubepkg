#!/usr/bin/env bash
# End-to-end test on an existing cluster: while one package installs
# slowly, a package whose requirement became ready goes ahead, instead of
# waiting behind the slow install.
#
#   KUBE_CONTEXT   the cluster, kubepkg with the helm backend in kubepkg-system
#   IMAGE_TAG      operator image of the version under test
#   WORKERS        packageWorkers to run with (default 4)
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
: "${KUBE_CONTEXT:?}" "${IMAGE_TAG:?}"
WORKERS=${WORKERS:-4}
K="kubectl --context ${KUBE_CONTEXT}"

step() { printf '\n=== %s\n' "$(date +%H:%M:%S) $*"; }
fail() { echo "FAIL: $*"; cleanup; exit 1; }
ready() { [[ "$(${K} get packages.kubepkg.dev "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" == True ]]; }
cleanup() {
  ${K} delete packages.kubepkg.dev dep-c quick-b slow-a --wait --timeout 300s >/dev/null 2>&1 || true
  ${K} delete packagesources.kubepkg.dev dep-c quick-b slow-a >/dev/null 2>&1 || true
  ${K} delete namespace dep-a dep-b dep-c --wait=false >/dev/null 2>&1 || true
}
source_yaml() { # name namespace requires
  cat <<EOF
---
apiVersion: kubepkg.dev/v1
kind: PackageSource
metadata: {name: $1}
spec:
  version: 2.20.0
  build: 3
  rollback: {safe: true}
  variants:
    - name: default
      requires: [$3]
      components:
        - name: ksm
          chart: {repository: oci://ghcr.io/tym83/kubepkg-packages/kube-state-metrics, name: kube-state-metrics, version: 2.20.0-3, digest: "sha256:2bf8279f5f6195dfb6332dfb8c1251bdcf5a00754dd97102411a614f685d329e"}
          install: {namespace: $2, releaseName: $1}
EOF
}

step "operator under test, ${WORKERS} package workers"
helm --kube-context "${KUBE_CONTEXT}" upgrade kubepkg "${ROOT}/charts/kubepkg" -n kubepkg-system --reuse-values \
  --set image.tag="${IMAGE_TAG}" --set packageWorkers="${WORKERS}" --wait >/dev/null
cleanup

step "A installs slowly: its pod takes 150s to get ready"
{ source_yaml slow-a dep-a ""; cat <<'EOF'
---
apiVersion: kubepkg.dev/v1
kind: Package
metadata: {name: slow-a}
spec:
  components: {ksm: {values: {readinessProbe: {initialDelaySeconds: 150}}}}
EOF
} | ${K} apply -f - >/dev/null
sleep 15

step "B is quick; C requires B"
{ source_yaml quick-b dep-b ""; source_yaml dep-c dep-c "{package: quick-b}"; cat <<'EOF'
---
apiVersion: kubepkg.dev/v1
kind: Package
metadata: {name: quick-b}
spec: {}
---
apiVersion: kubepkg.dev/v1
kind: Package
metadata: {name: dep-c}
spec: {}
EOF
} | ${K} apply -f - >/dev/null
b_at=""; c_at=""; start=$(date +%s)
while (( $(date +%s) - start < 420 )); do
  [[ -z "${b_at}" ]] && ready quick-b && b_at=$(date +%s)
  [[ -z "${c_at}" ]] && ready dep-c && c_at=$(date +%s)
  [[ -n "${c_at}" ]] && break
  sleep 3
done
[[ -n "${b_at}" && -n "${c_at}" ]] || fail "B ready: ${b_at:-never}, C ready: ${c_at:-never}"
lag=$(( c_at - b_at )); a_ready=no; ready slow-a && a_ready=yes
echo "  B ready after $(( b_at - start ))s, C ${lag}s after B; A ready by then: ${a_ready}"
[[ "${a_ready}" == no ]] || fail "B and C waited for the slow install of A to finish"

step "clean up"
cleanup
echo
echo "PASS"
