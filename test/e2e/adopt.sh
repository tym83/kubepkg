#!/usr/bin/env bash
# End-to-end test of kubepkg adopt on kind: a Helm release installed by
# hand, under another name and with its own values, and a Deployment
# applied with kubectl come under kubepkg's management in place.
set -euo pipefail
source "$(dirname "$0")/lib.sh"

build_binaries
start_cluster
KP=("${ROOT}/bin/kubepkg" --context "${KCTX}")
image_of() { ${K} -n "$1" get deploy "$2" -o jsonpath='{.spec.template.spec.containers[0].image}'; }
uid() { ${K} -n "$1" get deploy "$2" -o jsonpath='{.metadata.uid}'; }

step "charts and a signed repository"
for c in hello raw; do
  mkdir -p "${WORK}/${c}/templates"
  printf 'apiVersion: v2\nname: %s\nversion: 0.1.0\n' "${c}" > "${WORK}/${c}/Chart.yaml"
  printf 'image: registry.k8s.io/pause:3.10\n' > "${WORK}/${c}/values.yaml"
  cat > "${WORK}/${c}/templates/deploy.yaml" <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata: {name: {{ .Release.Name }}}
spec:
  selector: {matchLabels: {app: {{ .Release.Name }}}}
  template:
    metadata: {labels: {app: {{ .Release.Name }}}}
    spec:
      containers: [{name: app, image: {{ .Values.image }}}]
EOF
  helm package "${WORK}/${c}" -d "${WORK}" >/dev/null
  helm push "${WORK}/${c}-0.1.0.tgz" "oci://${REG}/e2e/charts" --plain-http >/dev/null 2>&1 || fail "helm push ${c}"
done
mkdir -p "${WORK}/sources" "${WORK}/www"
for p in hello:hello:e2e-hello raw:raw:e2e-raw raw2:raw:e2e-raw2; do
  IFS=: read -r name chart ns <<< "${p}"
  cat > "${WORK}/sources/${name}.yaml" <<EOF
apiVersion: kubepkg.dev/v1beta1
kind: PackageSource
metadata: {name: ${name}}
spec:
  version: "0.1.0"
  rollback: {safe: true}
  variants:
    - name: default
      components:
        - name: ${name}
          chart: {repository: "oci://${REG}/e2e/charts", name: ${chart}, version: 0.1.0}
          install: {namespace: ${ns}}
EOF
done
"${ROOT}/bin/kubepkg" repo keygen "${WORK}/signing" >/dev/null
"${ROOT}/bin/kubepkg" repo index "${WORK}/sources" --plain-http -o "${WORK}/www/index.yaml" --sign-key "${WORK}/signing.key" 2>/dev/null
python3 -m http.server 5006 --bind 127.0.0.1 --directory "${WORK}/www" >/dev/null 2>&1 &
WWW_PID=$!
for ((i = 0; i < 20; i++)); do curl -sf http://127.0.0.1:5006/index.yaml >/dev/null && break; sleep 0.5; done

step "installed before kubepkg: a Helm release elsewhere, and kubectl-applied Deployments"
helm --kube-context "${KCTX}" install legacy-hello "oci://${REG}/e2e/charts/hello" --version 0.1.0 --plain-http \
  -n legacy --create-namespace --set image=registry.k8s.io/pause:3.9 --wait >/dev/null
for n in raw raw2; do
  ${K} create namespace "e2e-${n}" >/dev/null
  ${K} -n "e2e-${n}" create deployment "${n}" --image=registry.k8s.io/pause:3.10 >/dev/null
done
hello_uid=$(uid legacy legacy-hello)
raw_uid=$(uid e2e-raw raw)

start_operator
"${KP[@]}" repo add main http://127.0.0.1:5006/index.yaml --public-key "${WORK}/signing.pub" >/dev/null
sleep 5

step "without adopt, an existing object blocks the install"
"${KP[@]}" install raw2 --yes >/dev/null
for ((i = 0; i < 60; i += 3)); do
  msg=$(${K} get packages.kubepkg.dev raw2 -o jsonpath='{.status.conditions[0].message}')
  [[ "${msg}" == *"exists and cannot be imported"* ]] && break
  sleep 3
done
[[ "${msg}" == *"exists and cannot be imported"* ]] || fail "installing over a kubectl-applied Deployment was not refused: ${msg}"
echo "  refused, as it should be"

step "adopt the Helm release in place, keeping its values"
plan=$("${KP[@]}" adopt hello --component hello=legacy/legacy-hello)
[[ "${plan}" == *"legacy/legacy-hello"*"revision 1"*"keeping its 1 top-level values"* ]] || fail "unexpected plan: ${plan}"
[[ -z "$(${K} get packages.kubepkg.dev hello --ignore-not-found -o name)" ]] || fail "a plan without --yes changed something"
"${KP[@]}" adopt hello --component hello=legacy/legacy-hello --yes >/dev/null
wait_reason hello ReconciliationSucceeded
[[ "$(uid legacy legacy-hello)" == "${hello_uid}" ]] || fail "the Deployment was recreated"
[[ "$(image_of legacy legacy-hello)" == registry.k8s.io/pause:3.9 ]] || fail "the release lost its values"
[[ "$(helm --kube-context "${KCTX}" -n legacy history legacy-hello -o json | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))')" == 2 ]] || fail "the release was not upgraded in place"
[[ -z "$(${K} get packages.kubepkg.dev hello -o jsonpath='{.metadata.annotations.kubepkg\.dev/adopt}')" ]] || fail "the adopt annotation stayed"
echo "  hello: release revision 2, same Deployment, values kept"

step "adopt a kubectl-applied Deployment"
"${KP[@]}" adopt raw --yes >/dev/null
wait_reason raw ReconciliationSucceeded
[[ "$(uid e2e-raw raw)" == "${raw_uid}" ]] || fail "the Deployment was recreated"
[[ "$(${K} -n e2e-raw get deploy raw -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}')" == raw ]] || fail "the Deployment was not taken into the release"
echo "  raw: same Deployment, now in release raw"

step "clean up"
${K} delete packages.kubepkg.dev hello raw raw2 --wait --timeout 180s >/dev/null
echo
echo "PASS"
