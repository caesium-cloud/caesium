#!/usr/bin/env bash
# G2 coverage collector: instrumented CLI/server binaries, labelled GOCOVERDIR
# profiles, graceful shutdown + SIGUSR2 flush, merge, and check.
#
# The script is the command (G6 owns any later justfile recipe). It never
# starts caesium-server-test, never publishes a host port, never runs
# just integration-up / ui-e2e / performance, and never treats a killed
# process or missing GOCOVERDIR as 0% success.
#
#   CAESIUM_COVERAGE_ARTIFACTS=/tmp/cov \
#   CANDIDATE_SHA=$(git rev-parse HEAD) \
#     bash scripts/integration-coverage.sh          # collect (default)
#     bash scripts/integration-coverage.sh build
#     bash scripts/integration-coverage.sh check    # hermetic; no docker
#
# Optional:
#   CAESIUM_COVERAGE_IMAGE     default caesiumcloud/caesium-coverage:latest
#   CAESIUM_BUILDER_IMAGE      default caesiumcloud/caesium-builder:latest
#   CAESIUM_COVERAGE_UNIT_PROFILE / CAESIUM_COVERAGE_BROWSER_DIR
#   CAESIUM_COVERAGE_BROWSER_PROFILE
#   CAESIUM_COVERAGE_RATCHET / CAESIUM_COVERAGE_WRITE_BASELINE
#   CAESIUM_COVERAGE_CHANGED_PATHS
#   CAESIUM_COVERAGE_SKIP_BUILD=1
#   CAESIUM_COVERAGE_KEEP=1
#   CAESIUM_COVERAGE_REQUIRE_BROWSER=1
#   CAESIUM_COVERAGE_STRICT=1
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

log() { printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }
die() { log "ERROR: $*"; exit 1; }

require_cmd() { command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"; }

CMD="${1:-collect}"
case "$CMD" in
  collect|build|check|merge) ;;
  *) die "unknown command '$CMD' (want collect|build|check|merge)" ;;
esac

require_cmd python3
CHECKER="$ROOT/scripts/check-coverage.py"
test -f "$CHECKER" || die "missing $CHECKER"
DOCKERFILE="$ROOT/build/Dockerfile.coverage"
test -f "$DOCKERFILE" || die "missing $DOCKERFILE"

ARTIFACTS="${CAESIUM_COVERAGE_ARTIFACTS:-}"
[[ -n "$ARTIFACTS" ]] || die "CAESIUM_COVERAGE_ARTIFACTS is required"
mkdir -p "$ARTIFACTS"
ARTIFACTS="$(cd "$ARTIFACTS" && pwd)"

CANDIDATE_SHA="${CANDIDATE_SHA:-${CAESIUM_COVERAGE_SHA:-}}"
if [[ -z "$CANDIDATE_SHA" ]] && command -v git >/dev/null 2>&1 && git -C "$ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  CANDIDATE_SHA="$(git -C "$ROOT" rev-parse HEAD)"
fi
[[ -n "$CANDIDATE_SHA" ]] || die "CANDIDATE_SHA is required"

ID="${CAESIUM_COVERAGE_ID:-}"
if [[ -z "$ID" ]]; then
  ID="cov-$(printf '%s' "$CANDIDATE_SHA" | tr '[:upper:]' '[:lower:]' | tr -cd 'a-z0-9' | tail -c 12)"
fi
if [[ ! "$ID" =~ ^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$ ]]; then
  die "CAESIUM_COVERAGE_ID must be a lowercase DNS-1123 name of at most 40 characters, got '$ID'"
fi

# Never collide with the integration-up server or bind its host port.
[[ "$ID" != "caesium-server-test" ]] || die "refusing to use caesium-server-test as the coverage id"
SERVER_NAME="${ID}-server"
NETWORK="${ID}-net"
IMAGE="${CAESIUM_COVERAGE_IMAGE:-caesiumcloud/caesium-coverage:latest}"
case "$IMAGE" in
  *:latest-test|*-test|*-test:*|*performance*|*perf*|*robustness*)
    die "coverage image must not be a test/performance/robustness artifact, got '$IMAGE'"
    ;;
esac
BUILDER_IMAGE="${CAESIUM_BUILDER_IMAGE:-caesiumcloud/caesium-builder:latest}"

PODMAN="${CAESIUM_PODMAN:-false}"
if [[ "$PODMAN" == "true" ]]; then
  CONTAINER_CLI="${CAESIUM_CONTAINER_CLI:-podman}"
else
  CONTAINER_CLI="${CAESIUM_CONTAINER_CLI:-docker}"
fi

case "$(uname -m)" in
  x86_64|amd64) HOST_ARCH=amd64 ;;
  aarch64|arm64) HOST_ARCH=arm64 ;;
  *) HOST_ARCH="$(uname -m)" ;;
esac
PLATFORM="${CAESIUM_PLATFORM:-linux/$HOST_ARCH}"

PROFILES="$ARTIFACTS/profiles"
RAW="$ARTIFACTS/raw"
AUDIT="$ARTIFACTS/audit"
mkdir -p "$PROFILES" "$RAW/cli" "$RAW/server" "$RAW/browser" "$RAW/integration" "$AUDIT"

# An aborted collect must leave a NON-passing record, never a stale pass and
# never a synthesised 0% profile.
placeholder_report() {
  local dest="$1"
  local reason="$2"
  PLACEHOLDER_SHA="$CANDIDATE_SHA" PLACEHOLDER_REASON="$reason" PLACEHOLDER_DEST="$dest" python3 - <<'PY'
import json, os, pathlib
pathlib.Path(os.environ["PLACEHOLDER_DEST"]).write_text(json.dumps({
    "schema_version": 1,
    "candidate_sha": os.environ["PLACEHOLDER_SHA"],
    "verdict": "incomplete",
    "reason": os.environ["PLACEHOLDER_REASON"],
    "contributions": {
        "unit": {"status": "incomplete", "percent": None},
        "cli": {"status": "incomplete", "percent": None},
        "server": {"status": "incomplete", "percent": None},
        "integration": {"status": "incomplete", "percent": None},
        "browser": {"status": "incomplete", "percent": None},
        "reagents": {"status": "incomplete", "percent": None},
    },
    "write_to_read": {"id": "jobdef-apply-export", "covered": False, "status": "incomplete"},
}, indent=2) + "\n")
PY
}

placeholder_report "$ARTIFACTS/report.json" "collection did not finish"

write_provenance() {
  local dest="$1"
  python3 -c 'import json, pathlib, sys; pathlib.Path(sys.argv[1]).write_text(json.dumps(json.loads(sys.stdin.read()), indent=2) + "\n")' "$dest"
}

gocoverdir_complete() {
  local dir="$1"
  local meta counters
  meta=$(find "$dir" -maxdepth 1 -name 'covmeta.*' 2>/dev/null | wc -l | tr -d ' ')
  counters=$(find "$dir" -maxdepth 1 -name 'covcounters.*' 2>/dev/null | wc -l | tr -d ' ')
  [[ "$meta" -gt 0 && "$counters" -gt 0 ]]
}

textfmt_dir() {
  local src="$1"
  local dest="$2"
  if ! gocoverdir_complete "$src"; then
    return 1
  fi
  require_cmd "$CONTAINER_CLI"
  "$CONTAINER_CLI" run --rm --platform "$PLATFORM" \
    -v "$src:/in:ro" \
    -v "$(dirname "$dest"):/out" \
    -w / \
    "$BUILDER_IMAGE" \
    go tool covdata textfmt -i=/in -o="/out/$(basename "$dest")"
}

merge_gocoverdirs() {
  local dest="$1"
  shift
  local inputs=()
  local arg
  for arg in "$@"; do
    if gocoverdir_complete "$arg"; then
      inputs+=("$arg")
    fi
  done
  if [[ "${#inputs[@]}" -eq 0 ]]; then
    return 1
  fi
  require_cmd "$CONTAINER_CLI"
  rm -rf "$dest"
  mkdir -p "$dest"
  local i_flags=()
  local mount_flags=()
  local idx=0
  local joined=""
  for arg in "${inputs[@]}"; do
    mount_flags+=(-v "$arg:/in$idx:ro")
    if [[ -n "$joined" ]]; then
      joined="${joined},/in${idx}"
    else
      joined="/in${idx}"
    fi
    idx=$((idx + 1))
  done
  "$CONTAINER_CLI" run --rm --platform "$PLATFORM" \
    "${mount_flags[@]}" \
    -v "$dest:/out" \
    "$BUILDER_IMAGE" \
    go tool covdata merge -i="$joined" -o=/out
}

copy_optional_unit() {
  if [[ -n "${CAESIUM_COVERAGE_UNIT_PROFILE:-}" ]]; then
    if [[ ! -f "$CAESIUM_COVERAGE_UNIT_PROFILE" ]]; then
      write_provenance "$PROFILES/unit.provenance.json" <<EOF
{"schema_version":1,"source":"unit","kind":"coverprofile","module":"github.com/caesium-cloud/caesium","candidate_sha":"$CANDIDATE_SHA","complete":false,"missing":true,"killed":false}
EOF
      return
    fi
    cp "$CAESIUM_COVERAGE_UNIT_PROFILE" "$PROFILES/unit.out"
    write_provenance "$PROFILES/unit.provenance.json" <<EOF
{"schema_version":1,"source":"unit","kind":"coverprofile","module":"github.com/caesium-cloud/caesium","candidate_sha":"$CANDIDATE_SHA","complete":true,"missing":false,"killed":false,"collection":"unit-test"}
EOF
  fi
}

copy_optional_browser() {
  local dir="${CAESIUM_COVERAGE_BROWSER_DIR:-}"
  local profile="${CAESIUM_COVERAGE_BROWSER_PROFILE:-}"
  if [[ -z "$dir" && -z "$profile" ]]; then
    if [[ -f "$PROFILES/browser.out" || -f "$PROFILES/browser.provenance.json" ]]; then
      return
    fi
    write_provenance "$PROFILES/browser.provenance.json" <<EOF
{"schema_version":1,"source":"browser","kind":"gocoverdir","module":"github.com/caesium-cloud/caesium","candidate_sha":"$CANDIDATE_SHA","complete":false,"missing":true,"killed":false,"collection":"not-provided"}
EOF
    return
  fi
  if [[ -n "$profile" ]]; then
    if [[ ! -f "$profile" ]]; then
      write_provenance "$PROFILES/browser.provenance.json" <<EOF
{"schema_version":1,"source":"browser","kind":"coverprofile","module":"github.com/caesium-cloud/caesium","candidate_sha":"$CANDIDATE_SHA","complete":false,"missing":true,"killed":false}
EOF
      return
    fi
    cp "$profile" "$PROFILES/browser.out"
    write_provenance "$PROFILES/browser.provenance.json" <<EOF
{"schema_version":1,"source":"browser","kind":"coverprofile","module":"github.com/caesium-cloud/caesium","candidate_sha":"$CANDIDATE_SHA","complete":true,"missing":false,"killed":false,"collection":"labelled-profile"}
EOF
    return
  fi
  mkdir -p "$RAW/browser"
  if [[ -d "$dir" ]]; then
    cp -a "$dir"/. "$RAW/browser/" 2>/dev/null || true
  fi
  if gocoverdir_complete "$RAW/browser" && textfmt_dir "$RAW/browser" "$PROFILES/browser.out"; then
    write_provenance "$PROFILES/browser.provenance.json" <<EOF
{"schema_version":1,"source":"browser","kind":"gocoverdir","module":"github.com/caesium-cloud/caesium","candidate_sha":"$CANDIDATE_SHA","complete":true,"missing":false,"killed":false,"collection":"labelled-gocoverdir"}
EOF
  else
    write_provenance "$PROFILES/browser.provenance.json" <<EOF
{"schema_version":1,"source":"browser","kind":"gocoverdir","module":"github.com/caesium-cloud/caesium","candidate_sha":"$CANDIDATE_SHA","complete":false,"missing":true,"killed":false,"collection":"labelled-gocoverdir"}
EOF
  fi
}

run_checker() {
  copy_optional_unit
  copy_optional_browser
  local extra=()
  if [[ -n "${CAESIUM_COVERAGE_RATCHET:-}" ]]; then
    extra+=(--ratchet "$CAESIUM_COVERAGE_RATCHET")
  fi
  if [[ -z "${CAESIUM_COVERAGE_WRITE_BASELINE:-}" ]]; then
    CAESIUM_COVERAGE_WRITE_BASELINE="$ARTIFACTS/ratchet.json"
  fi
  extra+=(--write-baseline "$CAESIUM_COVERAGE_WRITE_BASELINE")
  if [[ -n "${CAESIUM_COVERAGE_CHANGED_PATHS:-}" ]]; then
    extra+=(--changed-paths "$CAESIUM_COVERAGE_CHANGED_PATHS")
  fi
  if [[ "${CAESIUM_COVERAGE_REQUIRE_BROWSER:-}" == "1" ]]; then
    extra+=(--require-browser)
  fi
  if [[ "${CAESIUM_COVERAGE_REQUIRE_UNIT:-}" == "1" ]]; then
    extra+=(--require-unit)
  fi
  if [[ "${CAESIUM_COVERAGE_REQUIRE_REAGENTS:-}" == "1" ]]; then
    extra+=(--require-reagents)
  fi
  if [[ "${CAESIUM_COVERAGE_STRICT:-}" == "1" ]]; then
    extra+=(--strict)
  fi
  # macOS / bash 3.2: `"${extra[@]}"` is unbound under `set -u` when extra is empty.
  if ((${#extra[@]})); then
    python3 "$CHECKER" \
      --profiles-dir "$PROFILES" \
      --candidate-sha "$CANDIDATE_SHA" \
      --repo-root "$ROOT" \
      --report "$ARTIFACTS/report.json" \
      "${extra[@]}"
  else
    python3 "$CHECKER" \
      --profiles-dir "$PROFILES" \
      --candidate-sha "$CANDIDATE_SHA" \
      --repo-root "$ROOT" \
      --report "$ARTIFACTS/report.json"
  fi
}

build_image() {
  require_cmd "$CONTAINER_CLI"
  log "building coverage image $IMAGE from $DOCKERFILE (builder $BUILDER_IMAGE)"
  "$CONTAINER_CLI" build --platform "$PLATFORM" \
    --build-arg BUILDER_IMAGE="$BUILDER_IMAGE" \
    --target coverage \
    -t "$IMAGE" \
    -f "$DOCKERFILE" \
    "$ROOT"
}

extract_audit() {
  require_cmd "$CONTAINER_CLI"
  local cid
  cid="$("$CONTAINER_CLI" create --platform "$PLATFORM" --entrypoint true "$IMAGE")"
  "$CONTAINER_CLI" cp "$cid":/usr/share/caesium-coverage/. "$AUDIT/" || true
  "$CONTAINER_CLI" rm -f "$cid" >/dev/null 2>&1 || true
}

write_fixture() {
  cat > "$ARTIFACTS/fixture.job.yaml" <<'YAML'
apiVersion: v1
kind: Job
metadata:
  alias: coverage-write-read
trigger:
  type: cron
  configuration:
    cron: "0 2 * * *"
steps:
  - name: noop
    image: alpine:3.23
    command: ["true"]
YAML
}

KEEP_RESOURCES=0
if [[ "${CAESIUM_COVERAGE_KEEP:-}" == "1" ]]; then
  KEEP_RESOURCES=1
fi

cleanup() {
  if [[ "$KEEP_RESOURCES" -eq 1 ]]; then
    log "CAESIUM_COVERAGE_KEEP=1; leaving $SERVER_NAME / $NETWORK in place"
    return
  fi
  if command -v "$CONTAINER_CLI" >/dev/null 2>&1; then
    "$CONTAINER_CLI" rm -f "$SERVER_NAME" >/dev/null 2>&1 || true
    "$CONTAINER_CLI" network rm "$NETWORK" >/dev/null 2>&1 || true
  fi
}

if [[ "$CMD" == "build" ]]; then
  build_image
  log "built $IMAGE"
  exit 0
fi

if [[ "$CMD" == "check" || "$CMD" == "merge" ]]; then
  if [[ "$CMD" == "merge" ]]; then
    if gocoverdir_complete "$RAW/cli"; then
      textfmt_dir "$RAW/cli" "$PROFILES/cli.out" || true
    fi
    if gocoverdir_complete "$RAW/server"; then
      textfmt_dir "$RAW/server" "$PROFILES/server.out" || true
    fi
    if gocoverdir_complete "$RAW/cli" && gocoverdir_complete "$RAW/server"; then
      if merge_gocoverdirs "$RAW/integration" "$RAW/cli" "$RAW/server"; then
        textfmt_dir "$RAW/integration" "$PROFILES/integration.out" || true
      fi
    fi
  fi
  run_checker
  exit $?
fi

# ----- collect -----
require_cmd "$CONTAINER_CLI"
if [[ "${CAESIUM_COVERAGE_SKIP_BUILD:-}" != "1" ]]; then
  build_image
else
  "$CONTAINER_CLI" image inspect "$IMAGE" >/dev/null 2>&1 \
    || die "CAESIUM_COVERAGE_SKIP_BUILD=1 but image $IMAGE is missing"
fi
"$CONTAINER_CLI" image inspect "$BUILDER_IMAGE" >/dev/null 2>&1 \
  || die "builder image $BUILDER_IMAGE is required for go tool covdata"

trap cleanup EXIT

"$CONTAINER_CLI" rm -f "$SERVER_NAME" >/dev/null 2>&1 || true
"$CONTAINER_CLI" network rm "$NETWORK" >/dev/null 2>&1 || true
"$CONTAINER_CLI" network create "$NETWORK" >/dev/null

extract_audit
write_fixture

# Bind-mount GOCOVERDIR so a graceful exit lands counters on the host. 0777
# avoids UID 10001 vs host-root permission misses on Docker Desktop binds.
chmod 0777 "$RAW/cli" "$RAW/server"

IMAGE_ID="$("$CONTAINER_CLI" image inspect --format '{{.Id}}' "$IMAGE")"

log "starting coverage server $SERVER_NAME on network $NETWORK (no host port)"
"$CONTAINER_CLI" run -d \
  --name "$SERVER_NAME" \
  --platform "$PLATFORM" \
  --network "$NETWORK" \
  --network-alias caesium \
  --user 10001:10001 \
  -e GOCOVERDIR=/var/lib/caesium/coverage \
  -e CAESIUM_DATABASE_PATH=/var/lib/caesium/dqlite \
  -e CAESIUM_LOG_LEVEL=info \
  -e CAESIUM_AUTH_MODE=none \
  -v "$RAW/server:/var/lib/caesium/coverage" \
  "$IMAGE" start >/dev/null

healthy=0
for _ in $(seq 1 60); do
  if "$CONTAINER_CLI" run --rm --platform "$PLATFORM" \
      --network "$NETWORK" \
      --user 0:0 \
      --entrypoint wget \
      "$IMAGE" -q -O - http://caesium:8080/health 2>/dev/null | grep -q healthy; then
    healthy=1
    break
  fi
  sleep 1
done
if [[ "$healthy" -ne 1 ]]; then
  log "server never became healthy; logs:"
  "$CONTAINER_CLI" logs "$SERVER_NAME" || true
  write_provenance "$PROFILES/server.provenance.json" <<EOF
{"schema_version":1,"source":"server","kind":"gocoverdir","module":"github.com/caesium-cloud/caesium","candidate_sha":"$CANDIDATE_SHA","image_id":"$IMAGE_ID","complete":false,"missing":true,"killed":false,"collection":"never-healthy"}
EOF
  write_provenance "$PROFILES/cli.provenance.json" <<EOF
{"schema_version":1,"source":"cli","kind":"gocoverdir","module":"github.com/caesium-cloud/caesium","candidate_sha":"$CANDIDATE_SHA","image_id":"$IMAGE_ID","complete":false,"missing":true,"killed":false,"collection":"server-never-healthy"}
EOF
  run_checker
  exit $?
fi

log "running request-to-write-to-read: job apply then job export"
cli_rc=0
"$CONTAINER_CLI" run --rm --platform "$PLATFORM" \
  --network "$NETWORK" \
  --user 0:0 \
  --entrypoint /bin/caesium \
  -e GOCOVERDIR=/coverage \
  -v "$RAW/cli:/coverage" \
  -v "$ARTIFACTS/fixture.job.yaml:/examples/fixture.job.yaml:ro" \
  "$IMAGE" job apply --path /examples/fixture.job.yaml --server http://caesium:8080 \
  || cli_rc=$?
if [[ "$cli_rc" -eq 0 ]]; then
  "$CONTAINER_CLI" run --rm --platform "$PLATFORM" \
    --network "$NETWORK" \
    --user 0:0 \
    --entrypoint /bin/caesium \
    -e GOCOVERDIR=/coverage \
    -v "$RAW/cli:/coverage" \
    "$IMAGE" job export coverage-write-read --server http://caesium:8080 \
    >/dev/null || cli_rc=$?
fi

cli_complete=false
cli_missing=true
cli_killed=false
if [[ "$cli_rc" -eq 0 ]] && gocoverdir_complete "$RAW/cli"; then
  cli_complete=true
  cli_missing=false
  textfmt_dir "$RAW/cli" "$PROFILES/cli.out" || cli_complete=false
elif [[ "$cli_rc" -ge 128 ]]; then
  cli_killed=true
  cli_missing=$(gocoverdir_complete "$RAW/cli" && echo false || echo true)
else
  cli_missing=$(gocoverdir_complete "$RAW/cli" && echo false || echo true)
fi
write_provenance "$PROFILES/cli.provenance.json" <<EOF
{"schema_version":1,"source":"cli","kind":"gocoverdir","module":"github.com/caesium-cloud/caesium","candidate_sha":"$CANDIDATE_SHA","image_id":"$IMAGE_ID","complete":$cli_complete,"missing":$cli_missing,"killed":$cli_killed,"exit_code":$cli_rc,"collection":"cli-exit","flush":"process-exit"}
EOF

# Explicit flush while the server is still running, then graceful SIGTERM.
# docker kill without a signal is SIGKILL and is forbidden here.
log "flushing server coverage via SIGUSR2, then docker stop (SIGTERM)"
"$CONTAINER_CLI" kill --signal=SIGUSR2 "$SERVER_NAME" >/dev/null 2>&1 || true
sleep 1
stop_rc=0
"$CONTAINER_CLI" stop -t 60 "$SERVER_NAME" >/dev/null || stop_rc=$?
inspect_json="$("$CONTAINER_CLI" inspect "$SERVER_NAME" 2>/dev/null || true)"
exit_code="$(printf '%s' "$inspect_json" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d[0]["State"].get("ExitCode", 1) if d else 1)' 2>/dev/null || echo 1)"
oom="$(printf '%s' "$inspect_json" | python3 -c 'import json,sys; d=json.load(sys.stdin); print("true" if d and d[0]["State"].get("OOMKilled") else "false")' 2>/dev/null || echo false)"
killed=false
signal="SIGTERM"
if [[ "$oom" == "true" || "$exit_code" == "137" ]]; then
  killed=true
  signal="SIGKILL"
fi

server_complete=false
server_missing=true
if [[ "$killed" == "true" ]]; then
  server_complete=false
  server_missing=$(gocoverdir_complete "$RAW/server" && echo false || echo true)
elif gocoverdir_complete "$RAW/server"; then
  server_missing=false
  if textfmt_dir "$RAW/server" "$PROFILES/server.out"; then
    server_complete=true
  fi
fi
write_provenance "$PROFILES/server.provenance.json" <<EOF
{"schema_version":1,"source":"server","kind":"gocoverdir","module":"github.com/caesium-cloud/caesium","candidate_sha":"$CANDIDATE_SHA","image_id":"$IMAGE_ID","complete":$server_complete,"missing":$server_missing,"killed":$killed,"exit_code":$exit_code,"signal":"$signal","oom_killed":$oom,"collection":"graceful-shutdown","flush":"sigusr2"}
EOF

if [[ "$cli_complete" == "true" && "$server_complete" == "true" ]]; then
  if merge_gocoverdirs "$RAW/integration" "$RAW/cli" "$RAW/server"; then
    textfmt_dir "$RAW/integration" "$PROFILES/integration.out" || true
    write_provenance "$PROFILES/integration.provenance.json" <<EOF
{"schema_version":1,"source":"integration","kind":"gocoverdir","module":"github.com/caesium-cloud/caesium","candidate_sha":"$CANDIDATE_SHA","image_id":"$IMAGE_ID","complete":true,"missing":false,"killed":false,"sources":["cli","server"],"collection":"covdata-merge"}
EOF
  fi
fi

log "checking labelled coverage"
run_checker
rc=$?
log "coverage checker exit $rc"
exit "$rc"
