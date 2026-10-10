#!/usr/bin/env bash
# End-to-end test of a hub's rollouts on two real clusters: the hub itself,
# registered as a canary, and a member. A change goes to the canary first,
# then to the member; a change that fails on the canary never reaches the
# member; fixing the set resumes; and the member moves to another set
# without its package being reinstalled.
#
#   HUB_CONTEXT                    hub cluster context (in KUBECONFIG)
#   HUB_KUBECONFIG                 the hub's kubeconfig as members are given it
#   MEMBER_KUBECONFIG, MEMBER_CONTEXT
#   IMAGE_TAG                      operator image tag of the version under test
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
: "${HUB_CONTEXT:?}" "${HUB_KUBECONFIG:?}" "${MEMBER_KUBECONFIG:?}" "${MEMBER_CONTEXT:?}" "${IMAGE_TAG:?}"
INDEX=https://kuberoot-dev.github.io/kubepkg-recipes/index.yaml
PKG=kube-state-metrics
NS=kube-state-metrics
K="kubectl --context ${HUB_CONTEXT}"
M="kubectl --kubeconfig ${MEMBER_KUBECONFIG} --context ${MEMBER_CONTEXT}"
WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT
OUT=${OUT:-${WORK}}

step() { printf '\n=== %s\n' "$*"; }
fail() { echo "FAIL: $*"; ${K} get packagesets.kubepkg.dev -o yaml | grep -A40 '^  status:' | head -60; exit 1; }
until_true() { # description seconds command...
  local what=$1 t=$2; shift 2
  for ((i = 0; i < t; i += 5)); do "$@" >/dev/null 2>&1 && return 0; sleep 5; done
  fail "${what}"
}
member_msg() { ${K} get packagesets.kubepkg.dev "$1" -o jsonpath="{.status.clusters[?(@.name==\"$2\")].message}"; }
replicas() { $1 -n "${NS}" get deploy "${PKG}" -o jsonpath='{.spec.replicas}' 2>/dev/null; }
image() { $1 -n "${NS}" get deploy "${PKG}" -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null; }
set_values() { # set json-values
  ${K} patch packagesets.kubepkg.dev "$1" --type json -p "[{\"op\":\"replace\",\"path\":\"/spec/packages/0/spec/components\",\"value\":{\"${PKG}\":{\"values\":$2}}}]" >/dev/null
}
done_everywhere() { [[ "$(${K} get packagesets.kubepkg.dev "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" == True ]]; }

curl -fsSL https://raw.githubusercontent.com/kuberoot-dev/kubepkg-recipes/main/keys/index.pub -o "${WORK}/index.pub"
(cd "${ROOT}" && go build -o "${WORK}/" ./cmd/kubepkg)
KP=("${WORK}/kubepkg" --context "${HUB_CONTEXT}")

step "both clusters run the version under test"
for target in "--kube-context ${HUB_CONTEXT}" "--kubeconfig ${MEMBER_KUBECONFIG} --kube-context ${MEMBER_CONTEXT}"; do
  # shellcheck disable=SC2086
  helm ${target} upgrade --install kubepkg "${ROOT}/charts/kubepkg" -n kubepkg-system --create-namespace --reuse-values \
    --set image.tag="${IMAGE_TAG}" --set mirror= --set plainHTTP=false --wait >/dev/null
done
${M} -n kubepkg-system delete ciliumnetworkpolicy kubepkg-airgap --ignore-not-found >/dev/null
${K} delete packages.kubepkg.dev "${PKG}" --ignore-not-found --wait --timeout 300s >/dev/null
${M} delete packages.kubepkg.dev --all --wait --timeout 300s >/dev/null

step "the hub registers itself as a canary and the member"
"${KP[@]}" cluster add self --kubeconfig "${HUB_KUBECONFIG}" --member-context "${HUB_CONTEXT}" --label env=prod --label ring=canary >/dev/null
member_cfg=${WORK}/member.kubeconfig
kubectl --kubeconfig "${MEMBER_KUBECONFIG}" config view --raw --minify --context "${MEMBER_CONTEXT}" > "${member_cfg}"
"${KP[@]}" cluster add edge --kubeconfig "${member_cfg}" --label env=prod >/dev/null
until_true "the hub does not reach both clusters" 180 bash -c "[[ \$(${K} get clusters.kubepkg.dev -o jsonpath='{range .items[*]}{.status.conditions[0].status}{end}') == TrueTrue ]]"
"${KP[@]}" cluster list | tee "${OUT}/fleet-cluster-list.txt"

step "a set with a canary, one cluster at a time"
${K} apply -f - >/dev/null <<EOF
apiVersion: kubepkg.dev/v1
kind: PackageSet
metadata: {name: prod}
spec:
  clusterSelector: {matchLabels: {env: prod}}
  rollout:
    canary: {matchLabels: {ring: canary}}
    maxInProgress: 1
  repositories:
    - name: fleet
      spec:
        url: ${INDEX}
        publicKeys:
          - |
$(sed 's/^/            /' "${WORK}/index.pub")
  packages:
    - name: ${PKG}
      spec:
        version: "2.20.0"
        upgrade: {timeout: 90s}
        components: {${PKG}: {values: {replicas: 1}}}
EOF
until_true "the member did not wait for the canary" 120 bash -c "[[ \"\$(${K} get packagesets.kubepkg.dev prod -o jsonpath='{.status.clusters[?(@.name==\"edge\")].message}')\" == 'waiting for the canary clusters' ]]"
echo "  edge: waiting for the canary clusters"
until_true "the set did not get ready on both clusters" 600 done_everywhere prod
echo "  ready on both"

step "a change reaches the canary first, then the member"
set_values prod '{"replicas":2}'
until_true "the canary did not take the change" 300 bash -c "[[ \$(${K} -n ${NS} get deploy ${PKG} -o jsonpath='{.spec.replicas}') == 2 ]]"
[[ "$(replicas "${M}")" == 1 ]] || fail "the member took the change together with the canary"
echo "  canary at 2 replicas, member still at 1"
until_true "the member did not follow" 600 bash -c "[[ \$(${M} -n ${NS} get deploy ${PKG} -o jsonpath='{.spec.replicas}') == 2 ]]"
until_true "the change did not finish" 300 done_everywhere prod
echo "  member followed"

step "a change that fails on the canary never reaches the member"
good=$(image "${M}")
set_values prod '{"replicas":2,"image":{"tag":"v0.0.0-does-not-exist"}}'
until_true "the rollout did not pause" 600 bash -c "[[ \$(${K} get packagesets.kubepkg.dev prod -o jsonpath='{.status.conditions[?(@.type==\"RolloutPaused\")].status}') == True ]]"
${K} get packagesets.kubepkg.dev prod -o jsonpath='{.status.conditions[?(@.type=="RolloutPaused")].message}'; echo
sleep 30
[[ "$(image "${M}")" == "${good}" ]] || fail "the failed change reached the member"
"${KP[@]}" set status prod | tee "${OUT}/fleet-set-status-paused.txt"

step "fixing the set resumes the rollout"
set_values prod '{"replicas":3}'
until_true "the rollout did not resume and finish" 900 bash -c "[[ \$(${M} -n ${NS} get deploy ${PKG} -o jsonpath='{.spec.replicas}') == 3 ]] && [[ \$(${K} get packagesets.kubepkg.dev prod -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].status}') == True ]]"
"${KP[@]}" set status prod | tee "${OUT}/fleet-set-status.txt"
"${KP[@]}" set list | tee "${OUT}/fleet-set-list.txt"

step "the member moves to another set without a reinstall"
uid=$(${M} -n "${NS}" get deploy "${PKG}" -o jsonpath='{.metadata.uid}')
revs=$(${M} get packagerevisions.kubepkg.dev -l "kubepkg.dev/package=${PKG}" -o name | wc -l | tr -d ' ')
${K} apply -f - >/dev/null <<EOF
apiVersion: kubepkg.dev/v1
kind: PackageSet
metadata: {name: staging}
spec:
  clusterSelector: {matchLabels: {env: staging}}
  repositories:
    - name: fleet
      spec:
        url: ${INDEX}
        publicKeys:
          - |
$(sed 's/^/            /' "${WORK}/index.pub")
  packages:
    - name: ${PKG}
      spec:
        version: "2.20.0"
        components: {${PKG}: {values: {replicas: 1}}}
EOF
${K} label clusters.kubepkg.dev edge env=staging --overwrite >/dev/null
until_true "the member did not move to staging" 300 bash -c "[[ \$(${M} get packages.kubepkg.dev ${PKG} -o jsonpath='{.metadata.labels.kubepkg\.dev/package-set}') == staging ]] && [[ \$(${M} -n ${NS} get deploy ${PKG} -o jsonpath='{.spec.replicas}') == 1 ]]"
[[ "$(${M} -n "${NS}" get deploy "${PKG}" -o jsonpath='{.metadata.uid}')" == "${uid}" ]] || fail "the Deployment was recreated"
now=$(${M} get packagerevisions.kubepkg.dev -l "kubepkg.dev/package=${PKG}" -o name | wc -l | tr -d ' ')
(( now >= revs )) || fail "the package history restarted: ${revs} -> ${now} revisions"
echo "  same Deployment, package history kept (${revs} -> ${now} revisions)"

step "clean up"
${K} delete packagesets.kubepkg.dev prod staging --wait --timeout 600s >/dev/null
${K} delete clusters.kubepkg.dev self edge >/dev/null
echo
echo "PASS"
