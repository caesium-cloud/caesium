#!/usr/bin/env bash
# Exercise the real bundled Console in Chromium against the instrumented
# coverage server. The caller owns the server and its GOCOVERDIR lifecycle.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BASE_URL="${1:-}"
REPORT="${2:-}"
if [[ ! "$BASE_URL" =~ ^http://127\.0\.0\.1:[0-9]+$ ]]; then
  echo "expected a loopback coverage-server URL, got '$BASE_URL'" >&2
  exit 1
fi
[[ -n "$REPORT" ]] || { echo "a Playwright result path is required" >&2; exit 1; }

cd "$ROOT/ui"
# The lockfile, not a reused node_modules tree, selects the browser runner.
npm ci --prefer-offline
PLAYWRIGHT_BASE_URL="$BASE_URL" \
  ./node_modules/.bin/playwright test \
    e2e/navigation.spec.ts e2e/jobs-management.spec.ts \
    --project=default \
    --grep 'sidebar navigates between every primary control-plane page|operator can pause and unpause a job from the detail page' \
    --workers=1 --retries=0 --reporter=json >"$REPORT"

# Playwright can exit zero for an all-skipped suite. Validate its structured
# result file before the collector calls this a complete browser profile.
python3 "$ROOT/scripts/check-browser-journey.py" "$REPORT"
