#!/usr/bin/env bash
# End-to-end test of the flux backend: a package built with kubepkg is
# installed by Flux itself, from the Helm chart kubepkg published, with
# the Package's values; removing the Package removes the release and the
# chart source.
#
# Needs docker, kind, kubectl and go, and network access to the Flux
# release. Leaves nothing behind unless KEEP=1.
set -euo pipefail

CLUSTER=kubepkg-flux
REG_NAME=kubepkg-flux-registry
REG_PORT=${REG_PORT:-5005}
# shellcheck source=lib.sh
source "$(dirname "$0")/lib.sh"
HERE=${ROOT}/test/e2e

build_binaries
start_cluster
# The registry must be reachable from inside the cluster by name.
docker network connect kind "${REG_NAME}" >/dev/null

step "1. install Flux"
${K} apply -f https://github.com/fluxcd/flux2/releases/latest/download/install.yaml >/dev/null
${K} -n flux-system wait deploy/source-controller deploy/helm-controller --for=condition=Available --timeout=5m >/dev/null || fail "Flux did not start"

start_operator --backend flux

step "2. build and publish a package"
"${ROOT}/bin/kubepkg" build "${HERE}/recipes/hello" --registry "oci://${REG}/packages" --plain-http -o "${WORK}/dist" 2>/dev/null
# Inside the cluster the registry is known by its container name.
sed "s#oci://${REG}/#oci://${REG_NAME}:5000/#" "${WORK}/dist/hello-1.0.0-1.yaml" | ${K} apply -f - >/dev/null

step "3. Flux installs it"
${K} apply -f - <<EOF >/dev/null
apiVersion: kubepkg.dev/v1alpha1
kind: Package
metadata: {name: hello}
spec:
  components:
    hello: {values: {image: "registry.k8s.io/pause:3.9"}}
EOF
wait_reason hello ReconciliationSucceeded 600
[[ "$(${K} -n e2e-hello get ocirepositories.source.toolkit.fluxcd.io hello -o jsonpath='{.spec.ref.tag}')" == 1.0.0-1 ]] || fail "no OCIRepository for the chart"
[[ "$(${K} -n e2e-hello get helmreleases.helm.toolkit.fluxcd.io hello -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" == True ]] || fail "HelmRelease not ready"
[[ "$(${K} -n e2e-hello get deploy hello -o jsonpath='{.spec.template.spec.containers[0].image}')" == registry.k8s.io/pause:3.9 ]] || fail "Package values did not reach the release"

step "4. remove it"
${K} delete packages.kubepkg.dev hello --wait --timeout 300s >/dev/null || fail "package not removed"
${K} -n e2e-hello get helmreleases.helm.toolkit.fluxcd.io hello >/dev/null 2>&1 && fail "HelmRelease left behind"
${K} -n e2e-hello get ocirepositories.source.toolkit.fluxcd.io hello >/dev/null 2>&1 && fail "OCIRepository left behind"
for ((i = 0; i < 60; i += 3)); do ${K} -n e2e-hello get deploy hello >/dev/null 2>&1 || break; sleep 3; done
${K} -n e2e-hello get deploy hello >/dev/null 2>&1 && fail "deployment left behind"

echo
echo "PASS"
