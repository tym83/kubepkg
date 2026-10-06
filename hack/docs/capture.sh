#!/usr/bin/env bash
# Captures the command transcripts the documentation shows, from a real
# cluster with kubepkg running and from the published package repository.
# Each transcript is docs/examples/<name>.txt: "$ command" lines followed by
# what the command printed. hack/docs/term2svg.py draws the screenshots
# from them.
#
#   KUBE_CONTEXT   cluster with the kubepkg operator
#
# Packages and the "main" repository left from an earlier run are removed
# first, so the transcripts start from a clean cluster. The cluster keeps
# what the transcripts installed last; KEEP=0 removes it.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
OUT=${ROOT}/docs/examples
: "${KUBE_CONTEXT:?}"
INDEX=https://tym83.github.io/kubepkg-recipes/index.yaml
WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT
mkdir -p "${OUT}"

(cd "${ROOT}" && go build -o "${WORK}/bin/" ./cmd/kubepkg)
# What the reader types, run against the test cluster.
kubepkg() { "${WORK}/bin/kubepkg" --context "${KUBE_CONTEXT}" "$@"; }
kubectl() { command kubectl --context "${KUBE_CONTEXT}" "$@"; }

# run <transcript> <command line>: append the command and its output.
run() {
  local file=${OUT}/$1.txt cmd=$2
  printf '$ %s\n' "${cmd}" >> "${file}"
  (cd "${WORK}" && eval "${cmd}") >> "${file}" 2>&1 || true
}
fresh() { : > "${OUT}/$1.txt"; }
ready() { # package...
  for p in "$@"; do
    for _ in $(seq 120); do
      [[ "$(kubectl get packages.kubepkg.dev "$p" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" == True ]] && break
      sleep 5
    done
  done
}
settled() { # package revision: wait until the revision is applied
  for _ in $(seq 120); do
    [[ "$(kubectl get packages.kubepkg.dev "$1" -o jsonpath='{.status.currentRevision}' 2>/dev/null)" -ge "$2" ]] && break
    sleep 5
  done
  ready "$1"
}

kubectl delete packages.kubepkg.dev --all --wait --timeout 900s >/dev/null
kubectl delete repositories.kubepkg.dev main --ignore-not-found >/dev/null

curl -fsSL https://raw.githubusercontent.com/tym83/kubepkg-recipes/main/keys/index.pub -o "${WORK}/index.pub"

fresh repo-add
run repo-add "kubepkg repo add main ${INDEX} --public-key index.pub"
sleep 15
run repo-add "kubepkg repo list"

fresh search
run search "kubepkg search"

fresh plan
run plan "kubepkg plan virtualization"

fresh install
run install "kubepkg install cert-manager kube-state-metrics --yes"
ready cert-manager kube-state-metrics
run install "kubepkg list"

fresh upgrade
run upgrade "kubectl patch package cert-manager --type merge -p '{\"spec\":{\"components\":{\"cert-manager\":{\"values\":{\"replicaCount\":2}}}}}'"
settled cert-manager 2
run upgrade "kubepkg history cert-manager"

fresh rollback
run rollback "kubepkg rollback cert-manager --to 1"
settled cert-manager 3
run rollback "kubepkg history cert-manager"

fresh resources
run resources "kubectl get packages"
run resources "kubectl get packagerevisions"
run resources "kubectl get packagesources"
run resources "kubectl get repositories"

fresh remove
run remove "kubepkg remove kube-state-metrics --yes"

# Offline: the published repository, no cluster.
fresh render
run render "kubepkg render virtualization --format argo --repo ${INDEX} --public-key index.pub | head -40"

fresh trust
run trust "kubepkg repo keygen root-a && kubepkg repo keygen root-b && kubepkg repo keygen ci"
run trust "kubepkg trust root new --root-key root-a.pub --root-key root-b.pub --root-threshold 2 --index-key ci.pub --index-threshold 1 --expires 8760h -o root.yaml"
run trust "kubepkg trust sign root.yaml --key root-a.key"
run trust "kubepkg trust sign root.yaml --key root-b.key"
run trust "sed -n 1,20p root.yaml"

fresh init
run init "kubepkg init podinfo --chart oci://ghcr.io/stefanprodan/charts/podinfo@6.9.2"
run init "cat podinfo/recipe.yaml"
run init "kubepkg validate podinfo"

if [[ "${KEEP:-1}" == 0 ]]; then
  kubectl delete packages.kubepkg.dev --all --wait --timeout 600s >/dev/null
fi
echo "transcripts in ${OUT}"
