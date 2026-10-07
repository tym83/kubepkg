#!/usr/bin/env bash
# End-to-end test of delegations on an existing cluster: a repository with
# a root that hands team-* packages to a team key, served from inside the
# cluster. A version the team signed is installed; versions only the index
# key vouches for, or signed by a key outside the team, are left out; a
# kubepkg before 0.5 refuses the root.
#
#   KUBE_CONTEXT   the cluster, kubepkg >= 0.5 installed in kubepkg-system
#   OLD_KUBEPKG    a kubepkg CLI before 0.5 (optional)
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
: "${KUBE_CONTEXT:?}"
K="kubectl --context ${KUBE_CONTEXT}"
KP="${ROOT}/bin/kubepkg"
W=$(mktemp -d)
NS=deleg-site

step() { printf '\n=== %s\n' "$*"; }
fail() { echo "FAIL: $*"; exit 1; }

step "keys, root with a delegation, and an index"
cd "${W}"
for k in root ci team intruder; do "${KP}" repo keygen "${k}" >/dev/null; done
"${KP}" trust root new --root-key root.pub --root-threshold 1 --index-key ci.pub --index-threshold 1 \
  --delegate 'team=team-*' --delegate-key team=team.pub --expires 24h -o root.yaml >/dev/null
"${KP}" trust sign root.yaml --key root.key >/dev/null
mkdir -p src site/root
source_yaml() { # name build
  cat <<EOF
apiVersion: kubepkg.dev/v1beta1
kind: PackageSource
metadata: {name: $1}
spec:
  version: 2.20.0
  build: $2
  images:
    - registry.k8s.io/kube-state-metrics/kube-state-metrics:v2.20.0@sha256:42cfe3723a5f058171c627537fb57a3ea0f26e4380fa18555a95cb1a1b4cfc5b
  rollback: {safe: true}
  variants:
    - name: default
      components:
        - name: ksm
          chart: {repository: oci://ghcr.io/tym83/kubepkg-packages/kube-state-metrics, name: kube-state-metrics, version: 2.20.0-3, digest: "sha256:2bf8279f5f6195dfb6332dfb8c1251bdcf5a00754dd97102411a614f685d329e"}
          install: {namespace: deleg-test, releaseName: deleg-ksm}
EOF
}
source_yaml team-ksm 1 >src/team-1.yaml
"${KP}" trust sign src/team-1.yaml --key team.key >/dev/null      # the team signs build 1
source_yaml team-ksm 2 >src/team-2.yaml                            # only the index key vouches for build 2
source_yaml team-ksm 3 >src/team-3.yaml
"${KP}" trust sign src/team-3.yaml --key intruder.key >/dev/null  # build 3: a key outside the team
"${KP}" repo index src --sign-key ci.key --expires 24h -o site/index.yaml >/dev/null
cp root.yaml site/root.yaml && cp root.yaml site/root/1.yaml
echo "  index: team-ksm builds 1 (team), 2 (index key only), 3 (intruder)"

if [[ -n "${OLD_KUBEPKG:-}" ]]; then
  step "a kubepkg before 0.5 refuses the root"
  python3 -m http.server 18765 --directory site >/dev/null 2>&1 &
  srv=$!
  trap 'kill ${srv} 2>/dev/null || true' EXIT
  sleep 1
  if out=$("${OLD_KUBEPKG}" sbom team-ksm --repo http://127.0.0.1:18765/index.yaml --root-key root.pub -o /dev/null 2>&1); then
    fail "$("${OLD_KUBEPKG}" version 2>/dev/null | head -1) accepted a root it cannot read"
  fi
  echo "  refused: ${out##*: }"
  "${KP}" sbom team-ksm --repo http://127.0.0.1:18765/index.yaml --root-key root.pub -o sbom.json 2>warn.txt || fail "0.5 cannot read it: $(cat warn.txt)"
  grep -q '"version": "2.20.0' sbom.json || fail "no team-ksm in the SBOM"
  grep -q 'build 2' warn.txt && grep -q 'build 3' warn.txt || fail "the CLI did not warn about builds 2 and 3: $(cat warn.txt)"
  echo "  0.5: reads it, warns: $(head -c 160 warn.txt)…"
  kill ${srv}; wait ${srv} 2>/dev/null || true
fi

step "the repository, served from the cluster"
${K} create namespace "${NS}" --dry-run=client -o yaml | ${K} apply -f - >/dev/null
${K} -n "${NS}" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: {name: site, labels: {app: site}}
spec:
  containers:
    - name: site
      image: python:3.13-alpine
      command: [python3, -m, http.server, "8000", --directory, /site]
      volumeMounts: [{name: site, mountPath: /site}]
  volumes: [{name: site, emptyDir: {}}]
---
apiVersion: v1
kind: Service
metadata: {name: site}
spec:
  selector: {app: site}
  ports: [{port: 8000}]
EOF
${K} -n "${NS}" wait pod/site --for=condition=Ready --timeout=180s >/dev/null
${K} -n "${NS}" cp site/. site:/site >/dev/null
python3 - "$(cat root.pub)" <<'PY' | ${K} apply -f - >/dev/null
import json, sys
print(json.dumps({"apiVersion": "kubepkg.dev/v1beta1", "kind": "Repository", "metadata": {"name": "deleg"},
  "spec": {"url": "http://site.deleg-site.svc:8000/index.yaml", "trust": {"rootKeys": [sys.argv[1]], "rootThreshold": 1}}}))
PY
for _ in $(seq 30); do
  [[ "$(${K} get repositories.kubepkg.dev deleg -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" == True ]] && break
  sleep 5
done
msg=$(${K} get repositories.kubepkg.dev deleg -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}')
[[ "$(${K} get repositories.kubepkg.dev deleg -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" == True ]] || fail "repository not ready: ${msg}"
[[ "${msg}" == *"team-ksm 2.20.0 build 2 (delegation team: 0 of 1"* && "${msg}" == *"team-ksm 2.20.0 build 3 (delegation team: 0 of 1"* ]] || fail "status does not name the versions left out: ${msg}"
echo "  ${msg}"

step "the package installs the build the team signed"
${K} apply -f - >/dev/null <<EOF
apiVersion: kubepkg.dev/v1beta1
kind: Package
metadata: {name: team-ksm}
spec: {}
EOF
for _ in $(seq 60); do
  [[ "$(${K} get packages.kubepkg.dev team-ksm -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" == True ]] && break
  sleep 5
done
[[ "$(${K} get packages.kubepkg.dev team-ksm -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" == True ]] || fail "package not ready: $(${K} get packages.kubepkg.dev team-ksm -o jsonpath='{.status.conditions}')"
build=$(${K} get packagesources.kubepkg.dev team-ksm -o jsonpath='{.spec.build}')
[[ "${build}" == 1 ]] || fail "installed build ${build}, not the one the team signed"
${K} -n deleg-test rollout status deploy -l app.kubernetes.io/instance=deleg-ksm --timeout=180s >/dev/null 2>&1 || ${K} -n deleg-test get pods
echo "  team-ksm 2.20.0 build 1 installed and ready"

step "clean up"
${K} delete packages.kubepkg.dev team-ksm --wait=true --timeout=300s >/dev/null
${K} delete repositories.kubepkg.dev deleg >/dev/null
${K} delete namespace "${NS}" deleg-test --wait=false >/dev/null
echo
echo "PASS"
