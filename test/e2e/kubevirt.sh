#!/usr/bin/env bash
# End-to-end test of a package built from upstream sources.
#
# Builds KubeVirt from its release manifests with the recipe in
# recipes/kubevirt, publishes it to a local registry, indexes it, installs
# it as a package with emulation (kind has no /dev/kvm), waits until
# KubeVirt reports Available, then removes it.
#
# Needs docker, kind, kubectl, helm and go, and network access to GitHub and the
# KubeVirt image registry. Leaves nothing behind unless KEEP=1.
set -euo pipefail

CLUSTER=kubepkg-kubevirt
# shellcheck source=lib.sh
source "$(dirname "$0")/lib.sh"
HERE=${ROOT}/test/e2e

build_binaries
start_cluster
start_operator

step "1. build kubevirt from upstream and publish it"
"${ROOT}/bin/kubepkg" build "${HERE}/recipes/kubevirt" --registry "oci://${REG}/packages" --plain-http \
  -o "${WORK}/dist" --cache-dir "${WORK}/build-cache"
"${ROOT}/bin/kubepkg" repo index "${WORK}/dist" -o "${WORK}/index.yaml"
grep -q "repository: oci://${REG}/packages/kubevirt$" "${WORK}/index.yaml" || fail "index does not point at the published charts"
helm show chart "oci://${REG}/packages/kubevirt/kubevirt-operator" --version 1.9.0-1 --plain-http >/dev/null 2>&1 || fail "the published chart is not a Helm chart"

step "2. install it"
${K} apply -f "${WORK}/dist/kubevirt-1.9.0-1.yaml" >/dev/null
${K} apply -f - <<EOF
apiVersion: kubepkg.dev/v1
kind: Package
metadata: {name: kubevirt}
spec:
  upgrade: {timeout: 15m}
  components:
    kubevirt: {values: {emulation: true}}
EOF
wait_reason kubevirt ReconciliationSucceeded 900
${K} -n kubevirt wait kubevirt/kubevirt --for=condition=Available --timeout=15m >/dev/null || fail "KubeVirt did not become Available"
echo "  kubevirt: Available"
[[ "$(${K} -n kubevirt get kubevirt kubevirt -o jsonpath='{.spec.configuration.developerConfiguration.useEmulation}')" == true ]] || fail "package values did not reach the KubeVirt resource"
"${ROOT}/bin/kubepkg" --context "${KCTX}" list

step "3. remove it"
${K} delete packages.kubepkg.dev kubevirt --wait --timeout 600s >/dev/null || fail "kubevirt package was not removed"
[[ -z "$(${K} -n kubevirt get deploy -o name 2>/dev/null)" ]] || fail "KubeVirt deployments left behind: $(${K} -n kubevirt get deploy -o name)"

echo
echo "PASS"
