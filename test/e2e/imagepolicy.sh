#!/usr/bin/env bash
# End-to-end test of the image policy on an existing cluster with packages
# that pin their images (cert-manager from the example repository): pods
# in package namespaces run their pinned digests, and in enforce mode an
# image no package pins is refused.
#
#   KUBE_CONTEXT   the cluster, kubepkg installed in kubepkg-system
#   IMAGE_TAG      operator image tag of the version under test
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
: "${KUBE_CONTEXT:?}" "${IMAGE_TAG:?}"
K="kubectl --context ${KUBE_CONTEXT}"
NS=cert-manager

step() { printf '\n=== %s\n' "$*"; }
fail() { echo "FAIL: $*"; ${K} -n kubepkg-system logs -l app.kubernetes.io/name=kubepkg --tail=20 --prefix 2>&1 | tail -20; exit 1; }

step "kubepkg with the image policy enforced"
helm --kube-context "${KUBE_CONTEXT}" upgrade kubepkg "${ROOT}/charts/kubepkg" -n kubepkg-system --reuse-values \
  --set image.tag="${IMAGE_TAG}" --set imagePolicy=enforce --wait >/dev/null
for _ in $(seq 60); do
  [[ "$(${K} get ns "${NS}" -o jsonpath='{.metadata.labels.kubepkg\.dev/image-policy}')" == enforce ]] && break
  sleep 5
done
[[ "$(${K} get ns "${NS}" -o jsonpath='{.metadata.labels.kubepkg\.dev/image-policy}')" == enforce ]] || fail "the package namespace is not labelled"
[[ -n "$(${K} get mutatingwebhookconfiguration kubepkg-images -o jsonpath='{.webhooks[0].clientConfig.caBundle}')" ]] || fail "the webhook has no CA bundle"
echo "  ${NS}: enforce, webhook trusted"

step "pods of the package run their pinned digests"
${K} -n "${NS}" rollout restart deploy/cert-manager >/dev/null
${K} -n "${NS}" rollout status deploy/cert-manager --timeout 300s >/dev/null
img=$(${K} -n "${NS}" get pods -l app.kubernetes.io/name=cert-manager -o jsonpath='{.items[0].spec.containers[0].image}')
[[ "${img}" == *"@sha256:"* ]] || fail "the pod runs ${img}, not a digest"
pinned=$(${K} get packagesources.kubepkg.dev cert-manager -o jsonpath='{.spec.images}')
[[ "${pinned}" == *"${img##*@}"* ]] || fail "the pod's digest is not the one the package pins"
echo "  ${img}"

step "an image no package pins is refused"
if out=$(${K} -n "${NS}" run intruder --image=docker.io/library/busybox:1.36 --restart=Never -- sleep 60 2>&1); then
  ${K} -n "${NS}" delete pod intruder --wait=false >/dev/null
  fail "an image no package pins started in an enforced namespace"
fi
[[ "${out}" == *"no installed package pins"* ]] || fail "refused for another reason: ${out}"
echo "  refused: ${out##*: }"

step "a namespace without the policy is left alone"
${K} create namespace imagepolicy-free --dry-run=client -o yaml | ${K} apply -f - >/dev/null
${K} -n imagepolicy-free run free --image=docker.io/library/busybox:1.36 --restart=Never -- sleep 5 >/dev/null || fail "a pod outside package namespaces was refused"
[[ "$(${K} -n imagepolicy-free get pod free -o jsonpath='{.spec.containers[0].image}')" == docker.io/library/busybox:1.36 ]] || fail "a pod outside package namespaces was changed"
${K} delete namespace imagepolicy-free --wait=false >/dev/null
echo "  untouched"

step "back to off"
helm --kube-context "${KUBE_CONTEXT}" upgrade kubepkg "${ROOT}/charts/kubepkg" -n kubepkg-system --reuse-values --set imagePolicy=off --wait >/dev/null
for _ in $(seq 60); do
  [[ -z "$(${K} get ns "${NS}" -o jsonpath='{.metadata.labels.kubepkg\.dev/image-policy}')" ]] && break
  sleep 5
done
[[ -z "$(${K} get ns "${NS}" -o jsonpath='{.metadata.labels.kubepkg\.dev/image-policy}')" ]] || fail "the label stayed"
echo
echo "PASS"
