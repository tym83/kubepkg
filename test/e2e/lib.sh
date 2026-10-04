#!/usr/bin/env bash
# Shared setup for the end-to-end tests: a kind cluster, a local registry
# and the operator running outside the cluster. Source it, then call
# build_binaries, start_cluster and start_operator.

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
WORK=$(mktemp -d)
CLUSTER=${CLUSTER:-kubepkg-e2e}
REG_NAME=kubepkg-e2e-registry
REG_PORT=${REG_PORT:-5001}
REG=localhost:${REG_PORT}
KCTX=kind-${CLUSTER}
K="kubectl --context ${KCTX}"
OPLOG=${WORK}/operator.log

step() { printf '\n=== %s\n' "$*"; }
fail() { echo "FAIL: $*"; echo "--- operator log (tail)"; tail -n 60 "${OPLOG}" || true; exit 1; }

cleanup() {
  [[ -n "${OP_PID:-}" ]] && kill "${OP_PID}" 2>/dev/null || true
  [[ -n "${WWW_PID:-}" ]] && kill "${WWW_PID}" 2>/dev/null || true
  if [[ "${KEEP:-0}" != 1 ]]; then
    kind delete cluster --name "${CLUSTER}" >/dev/null 2>&1 || true
    docker rm -f "${REG_NAME}" >/dev/null 2>&1 || true
    rm -rf "${WORK}"
  else
    echo "kept: cluster ${CLUSTER}, registry ${REG}, workdir ${WORK}"
  fi
}
trap cleanup EXIT

# condition prints reason|status of a Package's Ready condition.
condition() {
  ${K} get packages.kubepkg.dev "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}|{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true
}

# wait_reason waits until a Package reports the given Ready reason.
wait_reason() {
  local pkg=$1 want=$2
  local timeout=${3:-240} got=""
  for ((i = 0; i < timeout; i += 3)); do
    got=$(condition "${pkg}")
    [[ "${got%%|*}" == "${want}" ]] && { echo "  ${pkg}: ${got}"; return 0; }
    sleep 3
  done
  fail "${pkg}: wanted reason ${want}, got ${got}"
}

build_binaries() {
  step "build"
  (cd "${ROOT}" && go build -o bin/ ./cmd/...)
}

# start_cluster creates the registry and the cluster and installs the CRDs.
start_cluster() {
  step "registry and cluster"
  docker rm -f "${REG_NAME}" >/dev/null 2>&1 || true
  docker run -d --restart=no -p "127.0.0.1:${REG_PORT}:5000" --name "${REG_NAME}" registry:2 >/dev/null
  kind delete cluster --name "${CLUSTER}" >/dev/null 2>&1 || true
  kind create cluster --name "${CLUSTER}" --wait 120s >/dev/null
  ${K} apply -f "${ROOT}/config/crd" >/dev/null
}

# start_operator runs the operator against the cluster; arguments are
# passed on as extra flags.
start_operator() {
  step "start operator"
  kind get kubeconfig --name "${CLUSTER}" > "${WORK}/kubeconfig"
  "${ROOT}/bin/kubepkg-operator" --kubeconfig "${WORK}/kubeconfig" \
    --plain-http --cache-dir "${WORK}/cache" --metrics-bind-address 0 --health-probe-bind-address 0 \
    "$@" >"${OPLOG}" 2>&1 &
  OP_PID=$!
  sleep 3
  kill -0 "${OP_PID}" || fail "operator did not start"
}
