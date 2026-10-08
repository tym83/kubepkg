#!/usr/bin/env bash
# End-to-end test on an existing cluster: a release installed with
# client-side apply, as Helm 3 does, is taken over by a Package and
# upgraded, not failed with "forceConflicts enabled when serverSideApply
# disabled".
#
#   KUBE_CONTEXT   the cluster, kubepkg with the helm backend in kubepkg-system
#   IMAGE_TAG      operator image of the version under test
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
: "${KUBE_CONTEXT:?}" "${IMAGE_TAG:?}"
K="kubectl --context ${KUBE_CONTEXT}"
NS=csa-test

step() { printf '\n=== %s\n' "$*"; }
fail() { echo "FAIL: $*"; exit 1; }
ready() { ${K} get packages.kubepkg.dev csa-ksm -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}'; }

step "operator under test"
helm --kube-context "${KUBE_CONTEXT}" upgrade kubepkg "${ROOT}/charts/kubepkg" -n kubepkg-system --reuse-values --set image.tag="${IMAGE_TAG}" --wait >/dev/null

step "a release installed with client-side apply, as by Helm 3"
helm --kube-context "${KUBE_CONTEXT}" install csa-ksm oci://ghcr.io/tym83/kubepkg-packages/kube-state-metrics/kube-state-metrics \
  --version 2.20.0-3 -n "${NS}" --create-namespace --server-side=false --wait >/dev/null
echo "  apply method: $(${K} -n "${NS}" get secret -l name=csa-ksm,owner=helm -o jsonpath='{.items[0].data.release}' | base64 -d | base64 -d | gunzip | python3 -c 'import sys, json; print(json.load(sys.stdin).get("apply_method") or "csa (unset)")')"

step "a Package takes it over"
${K} apply -f - >/dev/null <<EOF
apiVersion: kubepkg.dev/v1
kind: PackageSource
metadata: {name: csa-ksm}
spec:
  version: 2.20.0
  build: 3
  rollback: {safe: true}
  variants:
    - name: default
      components:
        - name: ksm
          chart: {repository: oci://ghcr.io/tym83/kubepkg-packages/kube-state-metrics, name: kube-state-metrics, version: 2.20.0-3, digest: "sha256:2bf8279f5f6195dfb6332dfb8c1251bdcf5a00754dd97102411a614f685d329e"}
          install: {namespace: ${NS}, releaseName: csa-ksm}
---
apiVersion: kubepkg.dev/v1
kind: Package
metadata: {name: csa-ksm}
spec:
  components:
    ksm:
      values: {replicas: 2}
EOF
for _ in $(seq 60); do [[ "$(ready)" == True ]] && break; sleep 5; done
[[ "$(ready)" == True ]] || fail "$(${K} get packages.kubepkg.dev csa-ksm -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}')"
[[ "$(${K} -n "${NS}" get deploy -l app.kubernetes.io/instance=csa-ksm -o jsonpath='{.items[0].spec.replicas}')" == 2 ]] || fail "the package's values were not applied"
echo "  $(helm --kube-context "${KUBE_CONTEXT}" -n "${NS}" history csa-ksm | tail -1)"

step "clean up"
${K} delete packages.kubepkg.dev csa-ksm --wait --timeout 300s >/dev/null
${K} delete packagesources.kubepkg.dev csa-ksm >/dev/null 2>&1 || true
${K} delete namespace "${NS}" --wait=false >/dev/null 2>&1 || true
echo
echo "PASS"
