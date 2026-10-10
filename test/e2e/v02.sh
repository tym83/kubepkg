#!/usr/bin/env bash
# End-to-end test of an upgrade from kubepkg v0.1.0, operator failover and
# a hub with a member cluster, on existing clusters. Everything it installs
# is removed at the end unless KEEP=1.
#
#   HUB_CONTEXT                 hub cluster context (in KUBECONFIG)
#   MEMBER_KUBECONFIG           kubeconfig of the member cluster
#   MEMBER_CONTEXT              its context
#   MEMBER_SERVER               (optional) member API server as the hub sees it
#   IMAGE_TAG                   operator image tag of the version under test
#
# Needs kubectl, helm and go.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
: "${HUB_CONTEXT:?}" "${MEMBER_KUBECONFIG:?}" "${MEMBER_CONTEXT:?}" "${IMAGE_TAG:?}"
INDEX=https://kuberoot-dev.github.io/kubepkg-recipes/index.yaml
WORK=$(mktemp -d)
NS=kubepkg-system
K="kubectl --context ${HUB_CONTEXT}"
H="helm --kube-context ${HUB_CONTEXT}"
M="kubectl --kubeconfig ${MEMBER_KUBECONFIG} --context ${MEMBER_CONTEXT}"
MH="helm --kubeconfig ${MEMBER_KUBECONFIG} --kube-context ${MEMBER_CONTEXT}"
KP=("${ROOT}/bin/kubepkg" --context "${HUB_CONTEXT}")

step() { printf '\n=== %s\n' "$*"; }
fail() {
  echo "FAIL: $*"
  ${K} -n "${NS}" logs -l app.kubernetes.io/name=kubepkg --tail=30 --prefix 2>&1 | tail -40 || true
  exit 1
}
cleanup() {
  if [[ "${KEEP:-0}" != 1 ]]; then
    step "clean up"
    ${K} delete packagesets.kubepkg.dev --all --wait --timeout 300s >/dev/null 2>&1 || true
    ${K} delete clusters.kubepkg.dev --all >/dev/null 2>&1 || true
    ${K} delete packages.kubepkg.dev --all --wait --timeout 600s >/dev/null 2>&1 || true
    ${M} delete packages.kubepkg.dev --all --wait --timeout 600s >/dev/null 2>&1 || true
    ${H} uninstall kubepkg -n "${NS}" --wait >/dev/null 2>&1 || true
    ${MH} uninstall kubepkg -n "${NS}" --wait >/dev/null 2>&1 || true
  fi
  rm -rf "${WORK}"
}
trap cleanup EXIT

wait_for() { # description, seconds, command...
  local what=$1 t=$2; shift 2
  for ((i = 0; i < t; i += 5)); do "$@" >/dev/null 2>&1 && return 0; sleep 5; done
  fail "${what}"
}
pkg_ready() { [[ "$($1 get packages.kubepkg.dev "$2" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" == True ]]; }

curl -fsSL https://raw.githubusercontent.com/kuberoot-dev/kubepkg-recipes/main/keys/index.pub -o "${WORK}/index.pub"
(cd "${ROOT}" && go build -o bin/ ./cmd/kubepkg)

step "1. kubepkg v0.1.0 with a package"
${H} upgrade --install kubepkg oci://ghcr.io/kuberoot-dev/charts/kubepkg --version 0.1.0 -n "${NS}" --create-namespace --wait \
  --set 'repositories[0].name=main' --set "repositories[0].url=${INDEX}" --set-file 'repositories[0].publicKeys[0]'="${WORK}/index.pub" >/dev/null
wait_for "repository not loaded" 120 bash -c "${K} get repositories.kubepkg.dev main -o jsonpath='{.status.conditions[0].reason}' | grep -q IndexLoaded"
"${KP[@]}" install cert-manager --yes >/dev/null
wait_for "cert-manager not ready on v0.1.0" 600 pkg_ready "${K}" cert-manager
rev=$(${K} get packages.kubepkg.dev cert-manager -o jsonpath='{.status.currentRevision}')
echo "  cert-manager ready at revision ${rev}"

step "2. upgrade kubepkg to the version under test, two replicas"
${H} upgrade kubepkg "${ROOT}/charts/kubepkg" -n "${NS}" --wait --timeout 5m --set image.tag="${IMAGE_TAG}" --set replicas=2 \
  --set 'repositories[0].name=main' --set "repositories[0].url=${INDEX}" --set-file 'repositories[0].publicKeys[0]'="${WORK}/index.pub" >/dev/null
${K} get crd clusters.kubepkg.dev packagesets.kubepkg.dev >/dev/null || fail "new CRDs missing after the upgrade"
sleep 30
pkg_ready "${K}" cert-manager || fail "cert-manager not ready after upgrading kubepkg"
[[ "$(${K} get packages.kubepkg.dev cert-manager -o jsonpath='{.status.currentRevision}')" == "${rev}" ]] || fail "upgrading kubepkg made a new revision of an unchanged package"
metrics=$(${K} get --raw "/api/v1/namespaces/${NS}/services/kubepkg-metrics:8080/proxy/metrics")
grep -q 'kubepkg_package_ready{package="cert-manager"} 1' <<< "${metrics}" || fail "metrics do not report cert-manager ready"
echo "  unchanged: revision ${rev}; new CRDs present; metrics served"

step "3. the leader dies, the other replica takes over"
lease=$(${K} -n "${NS}" get leases -o name | grep kubepkg-operator)
leader=$(${K} -n "${NS}" get "${lease}" -o jsonpath='{.spec.holderIdentity}' | cut -d_ -f1)
${K} -n "${NS}" delete pod "${leader}" --wait=false >/dev/null
${K} patch packages.kubepkg.dev cert-manager --type merge -p '{"spec":{"components":{"cert-manager":{"values":{"replicaCount":2}}}}}' >/dev/null
wait_for "no new leader" 180 bash -c "h=\$(${K} -n ${NS} get ${lease} -o jsonpath='{.spec.holderIdentity}'); [[ -n \$h && \${h%%_*} != ${leader} ]]"
wait_for "the change was not applied after failover" 600 bash -c "[[ \$(${K} get packages.kubepkg.dev cert-manager -o jsonpath='{.status.currentRevision}') -gt ${rev} ]] && ${K} get packages.kubepkg.dev cert-manager -o jsonpath='{.status.conditions[0].status}' | grep -q True"
echo "  leader ${leader} replaced by $(${K} -n "${NS}" get "${lease}" -o jsonpath='{.spec.holderIdentity}' | cut -d_ -f1); change applied"

step "4. a hub installs into a member cluster"
${MH} upgrade --install kubepkg "${ROOT}/charts/kubepkg" -n "${NS}" --create-namespace --wait --set image.tag="${IMAGE_TAG}" >/dev/null
member_cfg=${WORK}/member.kubeconfig
kubectl --kubeconfig "${MEMBER_KUBECONFIG}" config view --raw --minify --context "${MEMBER_CONTEXT}" > "${member_cfg}"
[[ -n "${MEMBER_SERVER:-}" ]] && kubectl --kubeconfig "${member_cfg}" config set-cluster "$(kubectl --kubeconfig "${member_cfg}" config view -o jsonpath='{.clusters[0].name}')" --server "${MEMBER_SERVER}" >/dev/null
"${KP[@]}" cluster add edge --kubeconfig "${member_cfg}" --label env=edge >/dev/null
wait_for "the hub does not reach the member" 120 bash -c "${K} get clusters.kubepkg.dev edge -o jsonpath='{.status.conditions[0].status}' | grep -q True"
${K} apply -f - >/dev/null <<EOF
apiVersion: kubepkg.dev/v1alpha1
kind: PackageSet
metadata: {name: edge-base}
spec:
  clusterSelector: {matchLabels: {env: edge}}
  repositories:
    - name: main
      spec:
        url: ${INDEX}
        publicKeys:
          - |
$(sed 's/^/            /' "${WORK}/index.pub")
  packages:
    - name: cert-manager
EOF
wait_for "the set did not get ready" 900 bash -c "${K} get packagesets.kubepkg.dev edge-base -o jsonpath='{.status.conditions[0].status}' | grep -q True"
[[ "$(${M} get packages.kubepkg.dev cert-manager -o jsonpath='{.metadata.labels.kubepkg\.dev/package-set}')" == edge-base ]] || fail "member package not labelled by the set"
"${KP[@]}" cluster list
"${KP[@]}" set list
${K} delete packagesets.kubepkg.dev edge-base --wait --timeout 600s >/dev/null
wait_for "the member keeps the set's package" 600 bash -c "! ${M} get packages.kubepkg.dev cert-manager"
echo "  member got cert-manager from the set and lost it when the set went"

echo
echo "PASS"
