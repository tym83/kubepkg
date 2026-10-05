#!/usr/bin/env bash
# End-to-end test of kubepkg the way a user installs it, on an existing
# cluster: the operator from its image through the Helm chart, a signed
# repository from the internet, packages installed and removed with the
# CLI. Everything it installs is removed at the end unless KEEP=1.
#
#   KUBECONFIG, KUBE_CONTEXT   the cluster (the context is required)
#   IMAGE_TAG                  operator image tag in ghcr.io/tym83/kubepkg-operator
#   CHART, CHART_VERSION       the chart: default the one in this checkout;
#                              oci://ghcr.io/tym83/charts/kubepkg and a version
#                              for a release
#   INDEX                      repository index URL
#   PUBLIC_KEY                 file with the key the index is signed with
#   PACKAGES                   packages to install (default: cert-manager virtualization)
#
# Needs kubectl, helm and go.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
: "${KUBE_CONTEXT:?set KUBE_CONTEXT}" "${IMAGE_TAG:?set IMAGE_TAG}"
CHART=${CHART:-${ROOT}/charts/kubepkg}
chart_version=()
[[ -n "${CHART_VERSION:-}" ]] && chart_version=(--version "${CHART_VERSION}")
INDEX=${INDEX:-https://tym83.github.io/kubepkg-recipes/index.yaml}
PACKAGES=${PACKAGES:-cert-manager virtualization}
WORK=$(mktemp -d)
K="kubectl --context ${KUBE_CONTEXT}"
H="helm --kube-context ${KUBE_CONTEXT}"
KP=("${ROOT}/bin/kubepkg" --context "${KUBE_CONTEXT}")
NS=kubepkg-system

step() { printf '\n=== %s\n' "$*"; }
fail() {
  echo "FAIL: $*"
  echo "--- operator log (tail)"
  ${K} -n "${NS}" logs deploy/kubepkg --tail=40 2>&1 || true
  exit 1
}
cleanup() {
  if [[ "${KEEP:-0}" != 1 ]]; then
    step "clean up"
    ${K} delete packages.kubepkg.dev --all --wait --timeout 900s >/dev/null 2>&1 || true
    ${H} uninstall kubepkg -n "${NS}" --wait >/dev/null 2>&1 || true
    ${K} delete namespace "${NS}" --wait=false >/dev/null 2>&1 || true
  fi
  rm -rf "${WORK}"
}
trap cleanup EXIT

ready() {
  for ((i = 0; i < ${2:-900}; i += 5)); do
    [[ "$(${K} get packages.kubepkg.dev "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" == True ]] && { echo "  $1: ready"; return 0; }
    sleep 5
  done
  fail "$1 not ready: $(${K} get packages.kubepkg.dev "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}')"
}

step "build the CLI"
(cd "${ROOT}" && go build -o bin/ ./cmd/kubepkg)

step "1. install kubepkg with its chart, subscribed to a signed repository"
key=${PUBLIC_KEY:-${WORK}/index.pub}
[[ -n "${PUBLIC_KEY:-}" ]] || curl -fsSL https://raw.githubusercontent.com/tym83/kubepkg-recipes/main/keys/index.pub -o "${key}"
${H} upgrade --install kubepkg "${CHART}" ${chart_version[@]+"${chart_version[@]}"} -n "${NS}" --create-namespace --wait --timeout 5m \
  --set image.tag="${IMAGE_TAG}" \
  --set 'repositories[0].name=main' --set "repositories[0].url=${INDEX}" \
  --set-file 'repositories[0].publicKeys[0]'="${key}" >/dev/null || fail "chart did not install"
for ((i = 0; i < 120; i += 3)); do
  [[ "$(${K} get repositories.kubepkg.dev main -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}' 2>/dev/null)" == IndexLoaded ]] && break
  sleep 3
done
[[ "$(${K} get repositories.kubepkg.dev main -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}')" == IndexLoaded ]] || fail "the signed index was not loaded"
echo "  operator $(${K} -n "${NS}" get deploy kubepkg -o jsonpath='{.spec.template.spec.containers[0].image}'), repository loaded"

step "2. plan and install"
"${KP[@]}" search
# shellcheck disable=SC2086
"${KP[@]}" plan ${PACKAGES}
# shellcheck disable=SC2086
"${KP[@]}" install ${PACKAGES} --yes
if ${K} get packages.kubepkg.dev kubevirt >/dev/null 2>&1; then
  # Without /dev/kvm on the nodes KubeVirt has to emulate.
  ${K} patch packages.kubepkg.dev kubevirt --type merge -p '{"spec":{"components":{"kubevirt":{"values":{"emulation":true}}}}}' >/dev/null
fi
for p in $(${K} get packages.kubepkg.dev -o jsonpath='{.items[*].metadata.name}'); do ready "${p}"; done
if ${K} get packages.kubepkg.dev kubevirt >/dev/null 2>&1; then
  ${K} -n kubevirt wait kubevirt/kubevirt --for=condition=Available --timeout=15m >/dev/null || fail "KubeVirt not Available"
  ${K} wait cdi/cdi --for=condition=Available --timeout=15m >/dev/null || fail "CDI not Available"
  echo "  KubeVirt and CDI: Available"
fi
"${KP[@]}" list

if ${K} get packages.kubepkg.dev virtualization >/dev/null 2>&1; then
  step "3. remove"
  # remove must refuse; capture first, since its failure is the point.
  out=$("${KP[@]}" remove kubevirt --yes 2>&1 || true)
  [[ "${out}" == *"kubevirt is required by virtualization"* ]] || fail "remove did not refuse a package another one requires: ${out}"
  ${K} get packages.kubepkg.dev kubevirt >/dev/null 2>&1 || fail "remove deleted kubevirt although virtualization requires it"
  "${KP[@]}" remove virtualization --autoremove --yes
  for ((i = 0; i < 600; i += 5)); do
    ${K} get packages.kubepkg.dev kubevirt cdi virtualization >/dev/null 2>&1 || break
    sleep 5
  done
  ${K} get packages.kubepkg.dev kubevirt >/dev/null 2>&1 && fail "autoremove left kubevirt"
  ${K} get packages.kubepkg.dev cdi >/dev/null 2>&1 && fail "autoremove left cdi"
  [[ -z "$(${K} -n kubevirt get deploy -o name 2>/dev/null)" ]] || fail "KubeVirt deployments left behind"
  echo "  virtualization, kubevirt and cdi removed; the rest stays"
  "${KP[@]}" list
fi

echo
echo "PASS"
