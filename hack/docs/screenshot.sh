#!/usr/bin/env bash
# Takes a browser screenshot of a web UI running in a cluster, from a
# Playwright pod in that cluster, so nothing has to be exposed.
#
#   screenshot.sh <kubectl context> <url> <out.png> [width height [wait-ms]]
set -euo pipefail
CTX=$1 URL=$2 OUT=$3 W=${4:-1600} H=${5:-1000} WAIT=${6:-8000}
NS=default POD=docs-screenshot-$$
k() { kubectl --context "${CTX}" -n "${NS}" "$@"; }
k run "${POD}" --image=mcr.microsoft.com/playwright/python:v1.55.0-noble --restart=Never --command -- sleep 900 >/dev/null
trap 'k delete pod "${POD}" --wait=false >/dev/null 2>&1 || true' EXIT
k wait --for=condition=Ready "pod/${POD}" --timeout=300s >/dev/null
k exec "${POD}" -- sh -c "pip install -q playwright==1.55.0 >/dev/null 2>&1; python3 -c '
from playwright.sync_api import sync_playwright
with sync_playwright() as p:
    b = p.chromium.launch()
    pg = b.new_page(viewport={\"width\": ${W}, \"height\": ${H}}, device_scale_factor=2, locale=\"en-US\")
    pg.goto(\"${URL}\", wait_until=\"load\", timeout=60000)
    pg.wait_for_timeout(${WAIT})
    pg.screenshot(path=\"/tmp/shot.png\")
    b.close()
'"
k exec "${POD}" -- cat /tmp/shot.png > "${OUT}"
echo "${OUT}"
