#!/usr/bin/env bash
# Renders the chart with the values of every released chart in place of
# its own, which is what helm upgrade --reuse-values gives a template:
# settings added since must have defaults in the templates.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT
for tag in $(git -C "${ROOT}" tag -l 'v*'); do
  git -C "${ROOT}" show "${tag}:charts/kubepkg/values.yaml" > "${WORK}/old.yaml" 2>/dev/null || continue
  rm -rf "${WORK}/kubepkg" && cp -r "${ROOT}/charts/kubepkg" "${WORK}/kubepkg"
  cp "${WORK}/old.yaml" "${WORK}/kubepkg/values.yaml"
  helm template t "${WORK}/kubepkg" >/dev/null || { echo "the chart does not render with the values of ${tag}"; exit 1; }
  echo "renders with the values of ${tag}"
done
