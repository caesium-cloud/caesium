#!/usr/bin/env bash
# Bare-host smoke for the statically linked CLI. The binary is mounted into a
# plain ubuntu:24.04 container that carries none of the caesium runtime
# libraries: if it runs here it is genuinely self-contained.
#
# Usage: scripts/ci-cli-smoke.sh <binary> <arch>
set -euo pipefail

bin="${1:?binary path}"
arch="${2:?arch (amd64|arm64)}"

if [ ! -x "$bin" ]; then
  echo "error: $bin is missing or not executable" >&2
  exit 1
fi

run_cli() {
  docker run --rm \
    -v "${bin}:/usr/local/bin/caesium:ro" \
    -v "$PWD/docs/examples:/examples:ro" \
    -e CAESIUM_DATABASE_PATH=/tmp/caesium-db \
    ubuntu:24.04 caesium "$@"
}

run_cli --help
run_cli job lint --path /examples/minimal.job.yaml

# --help and job lint are pure Go and would pass even if the static cgo
# link were broken. This runs the server: `caesium start` opens the embedded
# dqlite catalog, migrates it, and serves `job apply` over HTTP.
docker run --rm \
  -v "${bin}:/usr/local/bin/caesium:ro" \
  -v "$PWD/docs/examples:/examples:ro" \
  -e CAESIUM_DATABASE_PATH=/tmp/caesium-db \
  ubuntu:24.04 sh -ec '
    mkdir -p /tmp/caesium-db
    (timeout 60 caesium start >/tmp/start.log 2>&1 || true) &
    applied=0
    for i in $(seq 1 55); do
      if caesium job apply --path /examples/minimal.job.yaml --server http://127.0.0.1:8080 >/tmp/apply.log 2>&1; then applied=1; break; fi
      sleep 1
    done
    cat /tmp/apply.log
    [ "$applied" = 1 ] || { echo "job apply against the embedded server never succeeded:"; cat /tmp/start.log; exit 1; }
    grep -q "migrating database" /tmp/start.log || echo "note: the start log did not contain the migration line (informational; apply succeeded)"
    echo "OK: embedded dqlite catalog opened, migrated and served job apply"
  '

sha="$(sha256sum "$bin" | awk '{print $1}')"
marker="${bin}.smoke-ok"
{
  echo "binary=$(basename "$bin")"
  echo "sha256=${sha}"
  echo "runner_arch=$(uname -m)"
  echo "runner_os=${RUNNER_OS:-$(uname -s)}"
  echo "smoke=ubuntu:24.04 'caesium --help' + 'caesium job lint' + 'caesium start' serving 'caesium job apply' (embedded dqlite catalog opened+migrated; native, no emulation)"
} >"$marker"
cat "$marker"
