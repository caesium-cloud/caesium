#!/usr/bin/env bash
# Own the cluster outside the browser worker so an interrupted worker cannot leak it.
set -euo pipefail
cd "$(dirname "$0")/../ui"
export CAESIUM_HEALTH_PROJECT="caesium-health-$(node -p 'crypto.randomUUID()')"
compose=("${CAESIUM_CONTAINER_CLI:-docker}" compose -p "$CAESIUM_HEALTH_PROJECT" -f ../test/cluster-health/compose.yaml)
artifacts="${CAESIUM_HEALTH_ARTIFACTS:-../.tmp/stopped-member/$CAESIUM_HEALTH_PROJECT}"
mkdir -p "$artifacts"
cleanup() {
    rc=$?
    if (( rc != 0 )); then
        "${compose[@]}" ps --all > "$artifacts/containers.log" 2>&1 || true
        "${compose[@]}" logs --no-color --tail 300 > "$artifacts/members.log" 2>&1 || true
    fi
    "${compose[@]}" down --volumes || exit 1
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
"${compose[@]}" up -d --wait --wait-timeout 120
PLAYWRIGHT_JSON_OUTPUT_NAME="$artifacts/results.json" PLAYWRIGHT_HTML_OUTPUT_DIR="$artifacts/report" \
    PLAYWRIGHT_HTML_OPEN=never npm run test:e2e -- --project=stopped-member \
    --output="$artifacts/test-results" --reporter=line,json,html
