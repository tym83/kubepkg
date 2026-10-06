#!/usr/bin/env bash
# End-to-end test of an air-gapped installation on an existing cluster
# with Cilium. A pod in the cluster plays both sides: it bundles a package
# from the public repository, then checks and imports the bundle into a
# registry and a web server inside the cluster. The operator, cut off
# from everything outside the cluster by a network policy, must install
# the package from there alone.
#
#   KUBE_CONTEXT   the cluster (in KUBECONFIG)
#   IMAGE_TAG      operator image tag of the version under test
#   PACKAGE        package to install (default cert-manager)
#
# Leaves the namespace airgap and the policy behind unless CLEANUP=1.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
: "${KUBE_CONTEXT:?}" "${IMAGE_TAG:?}"
PACKAGE=${PACKAGE:-cert-manager}
INDEX=https://tym83.github.io/kubepkg-recipes/index.yaml
K="kubectl --context ${KUBE_CONTEXT}"
WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT

step() { printf '\n=== %s\n' "$*"; }
fail() { echo "FAIL: $*"; ${K} -n kubepkg-system logs -l app.kubernetes.io/name=kubepkg --tail=30 --prefix 2>&1 | tail -30 || true; exit 1; }
bench() { ${K} -n airgap exec airgap -c bench -- "$@"; }

step "air-gap side: a registry and a web server in the cluster"
# A fresh pod each run: an old one carries the files of the last run.
${K} -n airgap delete pod airgap --ignore-not-found --wait --timeout 120s >/dev/null 2>&1 || true
${K} apply -f - >/dev/null <<'EOF'
apiVersion: v1
kind: Namespace
metadata: {name: airgap}
---
apiVersion: v1
kind: Pod
metadata: {name: airgap, namespace: airgap, labels: {app: airgap}}
spec:
  containers:
    - name: registry
      image: registry:2
      ports: [{containerPort: 5000}]
      volumeMounts: [{name: registry, mountPath: /var/lib/registry}]
    - name: site
      image: python:3.13-slim
      command: [python3, -m, http.server, "8000", --directory, /site]
      volumeMounts: [{name: site, mountPath: /site}]
    - name: bench
      image: python:3.13-slim
      command: [sleep, infinity]
      volumeMounts: [{name: site, mountPath: /site}, {name: work, mountPath: /work}]
  volumes:
    - {name: registry, emptyDir: {}}
    - {name: site, emptyDir: {}}
    - {name: work, emptyDir: {}}
---
apiVersion: v1
kind: Service
metadata: {name: airgap, namespace: airgap}
spec:
  selector: {app: airgap}
  ports: [{name: registry, port: 5000}, {name: site, port: 8000}]
EOF
${K} -n airgap wait --for=condition=Ready pod/airgap --timeout=300s >/dev/null
(cd "${ROOT}" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "${WORK}/kubepkg" ./cmd/kubepkg)
curl -fsSL https://raw.githubusercontent.com/tym83/kubepkg-recipes/main/keys/index.pub -o "${WORK}/index.pub"
${K} -n airgap cp "${WORK}/kubepkg" airgap:/work/kubepkg -c bench
${K} -n airgap cp "${WORK}/index.pub" airgap:/work/index.pub -c bench
bench chmod +x /work/kubepkg

step "connected side: bundle ${PACKAGE}"
bench /work/kubepkg bundle create "${PACKAGE}" --repo "${INDEX}" --public-key /work/index.pub -o /work/bundle.tar
bench /work/kubepkg bundle inspect /work/bundle.tar

step "air-gap side: a bundle checked with the wrong key is refused"
bench sh -c 'cd /work && ./kubepkg repo keygen other >/dev/null'
if bench /work/kubepkg bundle import /work/bundle.tar --mirror oci://localhost:5000/mirror --plain-http --public-key /work/other.pub --allow-unpinned-images >/dev/null 2>&1; then
  fail "import accepted a bundle under keys that did not sign it"
fi
echo "  refused"

step "air-gap side: check and import"
unpinned=()
bench /work/kubepkg bundle inspect /work/bundle.tar | grep -q 'bundle only' && unpinned=(--allow-unpinned-images)
bench /work/kubepkg bundle import /work/bundle.tar --mirror oci://localhost:5000/mirror --plain-http \
  --public-key /work/index.pub --site /site --node-config /work/node ${unpinned[@]+"${unpinned[@]}"}
bench python3 -c 'import json,urllib.request; print("  mirror holds:", ", ".join(json.load(urllib.request.urlopen("http://localhost:5000/v2/_catalog"))["repositories"]))'
bench sh -c 'ls /work/node/containerd/certs.d; cat /work/node/talos-registries.yaml'

step "the operator, cut off from the internet, uses only the mirror and the site"
${K} apply -f - >/dev/null <<'EOF'
apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata: {name: kubepkg-airgap, namespace: kubepkg-system}
spec:
  endpointSelector: {matchLabels: {app.kubernetes.io/name: kubepkg}}
  egress:
    - toEntities: [kube-apiserver, cluster]
    - toEndpoints: [{matchLabels: {k8s:io.kubernetes.pod.namespace: kube-system, k8s-app: kube-dns}}]
      toPorts: [{ports: [{port: "53", protocol: ANY}]}]
EOF
helm --kube-context "${KUBE_CONTEXT}" upgrade --install kubepkg "${ROOT}/charts/kubepkg" -n kubepkg-system --create-namespace --wait \
  --set image.tag="${IMAGE_TAG}" --set backend=helm --set clusterAdmin=true --set plainHTTP=true \
  --set mirror=oci://airgap.airgap.svc:5000/mirror \
  --set 'repositories[0].name=main' --set 'repositories[0].url=http://airgap.airgap.svc:8000/main/index.yaml' \
  --set-file 'repositories[0].publicKeys[0]'="${WORK}/index.pub" >/dev/null
# Whatever an earlier run installed goes, so the package below can only
# come from the mirror.
${K} delete packages.kubepkg.dev "${PACKAGE}" --ignore-not-found --wait --timeout 600s >/dev/null
${K} apply -f - >/dev/null <<EOF
apiVersion: kubepkg.dev/v1beta1
kind: Repository
metadata: {name: public}
spec: {url: ${INDEX}, priority: -1}
EOF
for _ in $(seq 60); do
  reason=$(${K} get repositories.kubepkg.dev public -o jsonpath='{.status.conditions[0].reason}' 2>/dev/null)
  [[ -n "${reason}" ]] && break
  sleep 5
done
[[ "${reason}" == FetchFailed ]] || fail "the operator reached the public repository (${reason}): it is not cut off"
echo "  public repository: ${reason}, as it should be"
${K} delete repositories.kubepkg.dev public >/dev/null

${K} apply -f - >/dev/null <<EOF
apiVersion: kubepkg.dev/v1beta1
kind: Package
metadata: {name: ${PACKAGE}}
spec: {}
EOF
for _ in $(seq 120); do
  ready=$(${K} get packages.kubepkg.dev "${PACKAGE}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')
  [[ "${ready}" == True ]] && break
  sleep 5
done
[[ "${ready}" == True ]] || fail "${PACKAGE} did not install from the mirror: $(${K} get packages.kubepkg.dev "${PACKAGE}" -o jsonpath='{.status.conditions[0].message}')"
${K} get packages.kubepkg.dev "${PACKAGE}"

if [[ "${CLEANUP:-0}" == 1 ]]; then
  ${K} delete packages.kubepkg.dev "${PACKAGE}" --wait --timeout 600s >/dev/null
  ${K} -n kubepkg-system delete ciliumnetworkpolicy kubepkg-airgap >/dev/null
  ${K} delete namespace airgap >/dev/null
fi
echo
echo "PASS"
