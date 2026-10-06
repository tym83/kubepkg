#!/usr/bin/env bash
# End-to-end test of the air-gapped path on kind: a recipe whose images are
# pinned is built into one registry and a signed index, bundled, and
# imported into a second registry. The first registry and the index server
# then go away, and the operator, pointed at the mirror, must install the
# package from the mirror and the imported index alone.
set -euo pipefail
source "$(dirname "$0")/lib.sh"

MIRROR_NAME=${CLUSTER}-mirror
MIRROR_PORT=${MIRROR_PORT:-5003}
MIRROR=localhost:${MIRROR_PORT}
diagnose() { docker ps -a --filter "name=${MIRROR_NAME}" || true; }
trap 'docker rm -f "${MIRROR_NAME}" >/dev/null 2>&1 || true; cleanup' EXIT

build_binaries
start_cluster
KP=("${ROOT}/bin/kubepkg")

step "1. a recipe pins the images its chart runs"
mkdir -p "${WORK}/recipes/web/chart/templates"
cat > "${WORK}/recipes/web/chart/Chart.yaml" <<'EOF'
apiVersion: v2
name: web
version: 1.0.0
EOF
cat > "${WORK}/recipes/web/chart/templates/deploy.yaml" <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata: {name: web}
spec:
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec:
      containers: [{name: web, image: registry.k8s.io/pause:3.10}]
EOF
cat > "${WORK}/recipes/web/recipe.yaml" <<'EOF'
apiVersion: kubepkg.dev/v1alpha1
kind: Recipe
metadata:
  name: web
  annotations: {kubepkg.dev/description: A web server}
spec:
  version: 1.0.0
  build: 1
  sources: {web: {dir: chart}}
  charts: {web: {from: [web]}}
  package:
    rollback: {safe: true}
    # IMAGES
    variants:
      - name: default
        components: [{name: web, path: web, install: {namespace: e2e-web}}]
EOF
"${KP[@]}" validate "${WORK}/recipes/web" | grep -q 'package.images is empty' || fail "validate does not warn about unpinned images"
pinned=$("${KP[@]}" images "${WORK}/recipes/web")
[[ "${pinned}" == *"registry.k8s.io/pause:3.10@sha256:"* ]] || fail "images did not pin pause: ${pinned}"
python3 - "${WORK}/recipes/web/recipe.yaml" "${pinned}" <<'EOF'
import sys
p, block = sys.argv[1], sys.argv[2]
s = open(p).read().replace("    # IMAGES", block)
open(p, "w").write(s)
EOF
"${KP[@]}" validate "${WORK}/recipes/web" >/dev/null || fail "a recipe with pinned images does not validate"

step "2. build, publish and sign"
"${KP[@]}" build "${WORK}/recipes/web" --registry "oci://${REG}/packages" --plain-http -o "${WORK}/dist" >/dev/null
"${KP[@]}" repo keygen "${WORK}/signing" >/dev/null
mkdir -p "${WORK}/www"
"${KP[@]}" repo index "${WORK}/dist" --plain-http -o "${WORK}/www/index.yaml" --sign-key "${WORK}/signing.key" 2>/dev/null
grep -q 'registry.k8s.io/pause:3.10@sha256:' "${WORK}/www/index.yaml" || fail "the signed index does not carry the pinned image"
python3 -m http.server 5004 --bind 127.0.0.1 --directory "${WORK}/www" >/dev/null 2>&1 &
WWW_PID=$!
for ((i = 0; i < 20; i++)); do curl -sf http://127.0.0.1:5004/index.yaml >/dev/null && break; sleep 0.5; done

step "3. bundle"
"${KP[@]}" bundle create web --repo http://127.0.0.1:5004/index.yaml --public-key "${WORK}/signing.pub" --plain-http -o "${WORK}/web.tar"
"${KP[@]}" bundle inspect "${WORK}/web.tar" | grep -q 'repository' || fail "the bundle's image is not pinned by the repository"
kill "${WWW_PID}"

step "4. import into a second registry"
docker rm -f "${MIRROR_NAME}" >/dev/null 2>&1 || true
docker run -d --restart=no -p "127.0.0.1:${MIRROR_PORT}:5000" --name "${MIRROR_NAME}" registry:2 >/dev/null
for ((i = 0; i < 20; i++)); do curl -sf "http://${MIRROR}/v2/" >/dev/null && break; sleep 0.5; done
"${KP[@]}" repo keygen "${WORK}/other" >/dev/null
if "${KP[@]}" bundle import "${WORK}/web.tar" --mirror "oci://${MIRROR}/mirror" --plain-http --public-key "${WORK}/other.pub" >/dev/null 2>&1; then
  fail "import accepted a bundle under keys that did not sign it"
fi
"${KP[@]}" bundle import "${WORK}/web.tar" --mirror "oci://${MIRROR}/mirror" --plain-http \
  --public-key "${WORK}/signing.pub" --site "${WORK}/site" --node-config "${WORK}/node"
curl -sf "http://${MIRROR}/v2/mirror/registry.k8s.io/pause/tags/list" | grep -q '3.10' || fail "the image is not in the mirror"
[[ -f "${WORK}/node/containerd/certs.d/registry.k8s.io/hosts.toml" ]] || fail "no containerd settings for registry.k8s.io"

step "5. the published registry and index go away; install from the mirror"
docker stop "${REG_NAME}" >/dev/null
python3 -m http.server 5005 --bind 127.0.0.1 --directory "${WORK}/site" >/dev/null 2>&1 &
WWW_PID=$!
for ((i = 0; i < 20; i++)); do curl -sf http://127.0.0.1:5005/main/index.yaml >/dev/null && break; sleep 0.5; done
start_operator --mirror "oci://${MIRROR}/mirror"
"${KP[@]}" --context "${KCTX}" repo add main http://127.0.0.1:5005/main/index.yaml --public-key "${WORK}/signing.pub" >/dev/null
sleep 5
"${KP[@]}" --context "${KCTX}" install web --yes >/dev/null
wait_reason web ReconciliationSucceeded
[[ "$(${K} get packagesources.kubepkg.dev web -o jsonpath='{.spec.variants[0].components[0].chart.repository}')" == "oci://${REG}/packages/web" ]] || fail "the package spec should keep the published location"
${K} delete packages.kubepkg.dev web --wait --timeout 180s >/dev/null

echo
echo "PASS"
