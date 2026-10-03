#!/usr/bin/env bash
# G2 coverage collector: instrumented CLI/server binaries, labelled GOCOVERDIR
# profiles, graceful shutdown + SIGUSR2 flush, merge, and check.
#
# The script is the command (G6 owns any later justfile recipe). It never
# starts caesium-server-test, publishes only an ephemeral loopback port for
# its isolated Chromium journey, never runs just integration-up / ui-e2e /
# performance, and never treats a killed
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
#   CAESIUM_COVERAGE_BROWSER_PROFILE / CAESIUM_COVERAGE_BROWSER_PROVENANCE
#   CAESIUM_COVERAGE_RATCHET / CAESIUM_COVERAGE_WRITE_BASELINE
#   CAESIUM_COVERAGE_CHANGED_PATHS / CAESIUM_COVERAGE_DIFF_BASE
#   CAESIUM_COVERAGE_SKIP_BUILD=1
#   CAESIUM_COVERAGE_KEEP=1
#   Browser evidence is required by collect, check, and merge.
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

# Never collide with the integration-up server or bind its fixed host port.
[[ "$ID" != "caesium-server-test" ]] || die "refusing to use caesium-server-test as the coverage id"
SERVER_NAME="${ID}-server"
CONNECTOR_NAME="${ID}-connectors"
BROWSER_SERVER_NAME="${ID}-browser"
NETWORK="${ID}-net"
IMAGE="${CAESIUM_COVERAGE_IMAGE:-caesiumcloud/caesium-coverage:latest}"
case "$IMAGE" in
  *:latest-test|*-test|*-test:*|*performance*|*perf*|*robustness*)
    die "coverage image must not be a test/performance/robustness artifact, got '$IMAGE'"
    ;;
esac
BUILDER_IMAGE="${CAESIUM_BUILDER_IMAGE:-caesiumcloud/caesium-builder:latest}"
BUILDER_RUN_IMAGE="$BUILDER_IMAGE"

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
    "$BUILDER_RUN_IMAGE" \
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
    "$BUILDER_RUN_IMAGE" \
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
  local provenance="${CAESIUM_COVERAGE_BROWSER_PROVENANCE:-}"
  if [[ -z "$dir" && -z "$profile" ]]; then
    if [[ -f "$PROFILES/browser.out" || -f "$PROFILES/browser.provenance.json" ]]; then
      return
    fi
    write_provenance "$PROFILES/browser.provenance.json" <<EOF
{"schema_version":1,"source":"browser","kind":"gocoverdir","module":"github.com/caesium-cloud/caesium","candidate_sha":"$CANDIDATE_SHA","complete":false,"missing":true,"killed":false,"collection":"not-provided"}
EOF
    return
  fi
  # A supplied profile cannot acquire candidate provenance just because this
  # check command happened to run in a checkout with CANDIDATE_SHA set.
  [[ -n "$provenance" && -f "$provenance" ]] \
    || die "external browser coverage requires CAESIUM_COVERAGE_BROWSER_PROVENANCE"
  if [[ -n "$profile" ]]; then
    [[ -f "$profile" ]] || die "external browser profile is missing: $profile"
    cp "$profile" "$PROFILES/browser.out"
    cp "$provenance" "$PROFILES/browser.provenance.json"
    return
  fi
  if [[ ! -d "$dir" ]] || ! gocoverdir_complete "$dir"; then
    die "external browser GOCOVERDIR is incomplete: $dir"
  fi
  # Stage first so supplying this invocation's raw/browser itself is safe.
  local imported
  imported="$(mktemp -d "$RAW/browser-import.XXXXXX")"
  if ! cp -a "$dir"/. "$imported/"; then
    rm -rf "$imported"
    die "cannot copy external browser GOCOVERDIR: $dir"
  fi
  # Do not overlay supplied evidence on counters from an earlier collect.
  rm -rf "$RAW/browser"
  mv "$imported" "$RAW/browser"
  if ! gocoverdir_complete "$RAW/browser" || ! textfmt_dir "$RAW/browser" "$PROFILES/browser.out"; then
    die "external browser GOCOVERDIR is incomplete: $dir"
  fi
  cp "$provenance" "$PROFILES/browser.provenance.json"
}

run_checker() {
  copy_optional_unit
  copy_optional_browser
  local extra=()
  local ratchet="${CAESIUM_COVERAGE_RATCHET:-$ROOT/scripts/coverage-ratchet.json}"
  if [[ -f "$ratchet" ]]; then
    extra+=(--ratchet "$ratchet")
  else
    die "committed coverage ratchet is missing: $ratchet"
  fi
  if [[ -z "${CAESIUM_COVERAGE_WRITE_BASELINE:-}" ]]; then
    CAESIUM_COVERAGE_WRITE_BASELINE="$ARTIFACTS/ratchet.json"
  fi
  extra+=(--write-baseline "$CAESIUM_COVERAGE_WRITE_BASELINE")
  extra+=(--changed-paths "$CHANGED_PATHS")
  extra+=(--diff-base "$DIFF_BASE" --coverpkg-audit "$AUDIT/coverpkg-packages.txt")
  if [[ -f "$AUDIT/source-inventory.json" ]]; then
    extra+=(--source-inventory "$AUDIT/source-inventory.json")
  fi
  extra+=(--require-browser)
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

GIT_HEAD=""
GIT_DIRTY=false
if command -v git >/dev/null 2>&1 && git -C "$ROOT" rev-parse --git-dir >/dev/null 2>&1; then
  GIT_HEAD="$(git -C "$ROOT" rev-parse HEAD)"
  if [[ -n "$(git -C "$ROOT" status --porcelain)" ]]; then
    GIT_DIRTY=true
  fi
fi

# The diff floor is only meaningful when its file list is real. Bind it to a
# checked-out base, including in check mode, rather than silently treating an
# omitted --changed-paths as an empty diff.
CHANGED_PATHS="${CAESIUM_COVERAGE_CHANGED_PATHS:-$ARTIFACTS/changed-paths.txt}"
DIFF_BASE="${CAESIUM_COVERAGE_DIFF_BASE:-external-changed-paths}"
if [[ -z "${CAESIUM_COVERAGE_CHANGED_PATHS:-}" ]]; then
  [[ -n "$GIT_HEAD" && "$GIT_DIRTY" == false ]] \
    || die "a clean git checkout is required to compute changed paths"
  [[ "$CANDIDATE_SHA" == "$GIT_HEAD" ]] \
    || die "CANDIDATE_SHA $CANDIDATE_SHA does not match the checkout used to compute changed paths ($GIT_HEAD)"
  DIFF_BASE="${CAESIUM_COVERAGE_DIFF_BASE:-master}"
  git -C "$ROOT" rev-parse --verify "${DIFF_BASE}^{commit}" >/dev/null \
    || die "coverage diff base $DIFF_BASE is not a commit"
  DIFF_BASE="$(git -C "$ROOT" merge-base "$DIFF_BASE" "$GIT_HEAD")"
  git -C "$ROOT" diff --name-only --diff-filter=ACMR "$DIFF_BASE" "$GIT_HEAD" -- '*.go' > "$CHANGED_PATHS"
fi
[[ -f "$CHANGED_PATHS" ]] || die "coverage changed-paths file is missing: $CHANGED_PATHS"

IMAGE_PROVENANCE="unknown"
IMAGE_VERIFIED=false

require_clean_checkout() {
  [[ -n "$GIT_HEAD" ]] || die "a git checkout is required to bind the coverage image to $CANDIDATE_SHA"
  if [[ "$GIT_DIRTY" == true ]]; then
    git -C "$ROOT" status --porcelain | head -60 >&2 || true
    die "refusing to build $IMAGE from a dirty working tree: the image would be labelled with the clean SHA $GIT_HEAD it was not built from"
  fi
  [[ "$CANDIDATE_SHA" == "$GIT_HEAD" ]] \
    || die "CANDIDATE_SHA $CANDIDATE_SHA does not match this checkout's HEAD $GIT_HEAD"
}

image_revision() {
  "$CONTAINER_CLI" image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "${1:-$IMAGE}" 2>/dev/null || true
}

resolve_builder_image() {
  "$CONTAINER_CLI" image inspect "$BUILDER_IMAGE" >/dev/null 2>&1 \
    || die "builder image $BUILDER_IMAGE is required for go tool covdata"
  BUILDER_RUN_IMAGE="$("$CONTAINER_CLI" image inspect --format '{{.Id}}' "$BUILDER_IMAGE")"
  [[ "$BUILDER_RUN_IMAGE" =~ ^sha256:[0-9a-f]{64}$ ]] || die "builder image has no immutable identity"
  # Docker FROM needs a named reference: a bare local ID is interpreted as a
  # repository named sha256. Local builders need not have RepoDigests.
  local builder_name="${BUILDER_IMAGE%%@*}" canonical_id
  if [[ "$BUILDER_IMAGE" == *@* && ! "${BUILDER_IMAGE#*@}" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    die "builder image must have a valid named reference"
  fi
  [[ "$builder_name" =~ ^[a-z0-9][a-z0-9._:/-]*(:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?$ \
    && ! "$builder_name" =~ ^sha256:[0-9a-f]{64}$ \
    && ! "$builder_name" =~ ^[0-9a-f]{64}$ ]] \
    || die "builder image must have a valid named reference"
  BUILDER_BUILD_IMAGE="$builder_name@$BUILDER_RUN_IMAGE"
  canonical_id="$("$CONTAINER_CLI" image inspect --format '{{.Id}}' "$BUILDER_BUILD_IMAGE")" \
    || die "pinned canonical builder reference is unavailable: $BUILDER_BUILD_IMAGE"
  [[ "$canonical_id" == "$BUILDER_RUN_IMAGE" ]] \
    || die "pinned canonical builder reference differs from the resolved builder image"
}

build_image() {
  require_cmd "$CONTAINER_CLI"
  require_clean_checkout
  resolve_builder_image
  log "building coverage image $IMAGE from $DOCKERFILE revision=$CANDIDATE_SHA (builder $BUILDER_BUILD_IMAGE)"
  "$CONTAINER_CLI" build --platform "$PLATFORM" \
    --build-arg BUILDER_IMAGE="$BUILDER_BUILD_IMAGE" \
    --build-arg CAESIUM_REVISION="$CANDIDATE_SHA" \
    --target coverage \
    -t "$IMAGE" \
    -f "$DOCKERFILE" \
    "$ROOT"
  local rev
  IMAGE_ID="$("$CONTAINER_CLI" image inspect --format '{{.Id}}' "$IMAGE")"
  rev="$(image_revision "$IMAGE_ID")"
  [[ "$rev" == "$CANDIDATE_SHA" ]] \
    || die "coverage image org.opencontainers.image.revision='$rev' does not match CANDIDATE_SHA $CANDIDATE_SHA"
  IMAGE_PROVENANCE="built-by-this-run"
  IMAGE_VERIFIED=true
}

extract_audit() {
  require_cmd "$CONTAINER_CLI"
  local cid
  cid="$("$CONTAINER_CLI" create --platform "$PLATFORM" --entrypoint true "$IMAGE_ID")"
  if ! "$CONTAINER_CLI" cp "$cid":/usr/share/caesium-coverage/. "$AUDIT/"; then
    "$CONTAINER_CLI" rm -f "$cid" >/dev/null 2>&1 || true
    die "cannot extract the pinned coverage image audit"
  fi
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
  - name: sample
    image: alpine:3.23
    command: ["sh", "-c", "sleep 2; echo coverage-task-log"]
YAML
}

KEEP_RESOURCES=0
if [[ "${CAESIUM_COVERAGE_KEEP:-}" == "1" ]]; then
  KEEP_RESOURCES=1
fi

# Invoked by the EXIT trap below.
# shellcheck disable=SC2329
cleanup() {
  if [[ "$KEEP_RESOURCES" -eq 1 ]]; then
    log "CAESIUM_COVERAGE_KEEP=1; leaving $SERVER_NAME / $BROWSER_SERVER_NAME / $NETWORK in place"
    return
  fi
  if command -v "$CONTAINER_CLI" >/dev/null 2>&1; then
    "$CONTAINER_CLI" rm -f "$SERVER_NAME" >/dev/null 2>&1 || true
    "$CONTAINER_CLI" rm -f "$CONNECTOR_NAME" >/dev/null 2>&1 || true
    "$CONTAINER_CLI" rm -f "$BROWSER_SERVER_NAME" >/dev/null 2>&1 || true
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
    mkdir -p "$PROFILES"
    # Preserve the source records exactly. Raw-file existence says nothing
    # about candidate identity, process shutdown, or verified image origin.
    rm -f "$PROFILES/cli.out" "$PROFILES/server.out" "$PROFILES/integration.out" "$PROFILES/integration.provenance.json" "$AUDIT/merge-provenance.json"
    rm -rf "$RAW/integration"
    mkdir -p "$RAW/integration"
    preflight_rc=0
    python3 "$ROOT/scripts/merge-coverage-provenance.py" \
      --profiles-dir "$PROFILES" --candidate-sha "$CANDIDATE_SHA" \
      --output "$AUDIT/merge-provenance.json" || preflight_rc=$?
    if [[ "$preflight_rc" -ne 0 ]]; then
      placeholder_report "$ARTIFACTS/report.json" "merge input provenance failed validation"
      exit "$preflight_rc"
    fi
    if ! gocoverdir_complete "$RAW/cli" || ! gocoverdir_complete "$RAW/server"; then
      placeholder_report "$ARTIFACTS/report.json" "merge raw CLI/server profile is incomplete"
      exit 2
    fi
    textfmt_dir "$RAW/cli" "$PROFILES/cli.out"
    textfmt_dir "$RAW/server" "$PROFILES/server.out"
    merge_gocoverdirs "$RAW/integration" "$RAW/cli" "$RAW/server"
    textfmt_dir "$RAW/integration" "$PROFILES/integration.out"
    cp "$AUDIT/merge-provenance.json" "$PROFILES/integration.provenance.json"
  fi
  run_checker
  exit $?
fi

# ----- collect -----
require_cmd "$CONTAINER_CLI"
if [[ -n "${CAESIUM_COVERAGE_BROWSER_DIR:-}" || -n "${CAESIUM_COVERAGE_BROWSER_PROFILE:-}" ]]; then
  die "collect runs its own Chromium journey; external browser profiles are accepted only by check/merge"
fi
rm -rf "$RAW/cli" "$RAW/server" "$RAW/browser" "$RAW/integration"
mkdir -p "$RAW/cli" "$RAW/server" "$RAW/browser" "$RAW/integration" "$PROFILES" "$AUDIT"
rm -f "$PROFILES"/*.out "$PROFILES"/*.provenance.json

if [[ "${CAESIUM_COVERAGE_SKIP_BUILD:-}" == "1" ]]; then
  "$CONTAINER_CLI" image inspect "$IMAGE" >/dev/null 2>&1 \
    || die "CAESIUM_COVERAGE_SKIP_BUILD=1 but image $IMAGE is missing"
  IMAGE_ID="$("$CONTAINER_CLI" image inspect --format '{{.Id}}' "$IMAGE")"
  IMAGE_PROVENANCE="supplied/unverified"
  IMAGE_VERIFIED=false
  resolve_builder_image
  log "SKIP_BUILD: $IMAGE is supplied/unverified and is not a provenanced match of $CANDIDATE_SHA"
else
  build_image
fi
# Runtime and builder identities were pinned before the image build; retain
# them for audit extraction and every subsequent collection process.
[[ "$IMAGE_ID" =~ ^sha256:[0-9a-f]{64}$ ]] || die "coverage image has no immutable identity"
IMAGE_GOOS="$("$CONTAINER_CLI" image inspect --format '{{.Os}}' "$IMAGE_ID")"
IMAGE_GOARCH="$("$CONTAINER_CLI" image inspect --format '{{.Architecture}}' "$IMAGE_ID")"
[[ -n "$IMAGE_GOOS" && -n "$IMAGE_GOARCH" ]] || die "coverage image has no target build context"
trap cleanup EXIT

"$CONTAINER_CLI" rm -f "$SERVER_NAME" >/dev/null 2>&1 || true
"$CONTAINER_CLI" rm -f "$BROWSER_SERVER_NAME" >/dev/null 2>&1 || true
"$CONTAINER_CLI" network rm "$NETWORK" >/dev/null 2>&1 || true
"$CONTAINER_CLI" network create "$NETWORK" >/dev/null

extract_audit
# Eligibility is independent of observed counters. Map the image's audited
# root-module packages to source directories without loading imports/embeds.
# Parse all non-test Go source in the builder, including build-excluded files.
# Missing profiles may exempt only proven files with no function body or files
# excluded by the image's target build context.
rm -f "$AUDIT/source-inventory.json"
"$CONTAINER_CLI" run --rm -i --platform "$PLATFORM" \
  -v "$ROOT:/source:ro" -v "$AUDIT:/audit" -w /source \
  -e INVENTORY_SHA="$CANDIDATE_SHA" \
  -e INVENTORY_IMAGE_ID="$IMAGE_ID" \
  -e INVENTORY_GOOS="$IMAGE_GOOS" -e INVENTORY_GOARCH="$IMAGE_GOARCH" \
  "$BUILDER_RUN_IMAGE" sh -s <<'INVENTORY'
set -eu
cat >/tmp/coverage-source-inventory.go <<'GO'
package main

import (
    "crypto/sha256"
    "encoding/hex"
    "encoding/json"
    "go/ast"
    "go/build"
    "go/parser"
    "go/token"
    "os"
    "path/filepath"
    "sort"
    "strings"
)

func main() {
    raw, err := os.ReadFile("/audit/coverpkg-packages.txt")
    if err != nil { panic(err) }
    packages := strings.Fields(string(raw))
    sort.Strings(packages)
    if len(packages) == 0 { panic("empty coverpkg audit") }
    files := map[string]any{}
    packageFiles := map[string][]string{}
    const module = "github.com/caesium-cloud/caesium"
    target := build.Default
    target.GOOS, target.GOARCH = os.Getenv("INVENTORY_GOOS"), os.Getenv("INVENTORY_GOARCH")
    if target.GOOS == "" || target.GOARCH == "" { panic("missing image build context") }
    target.CgoEnabled = true
    target.BuildTags = []string{} // Dockerfile.coverage adds no build tags.
    buildContext := map[string]any{
        "goos": target.GOOS, "goarch": target.GOARCH, "build_tags": target.BuildTags,
        "cgo_enabled": target.CgoEnabled, "compiler": target.Compiler,
        "release_tags": target.ReleaseTags, "tool_tags": target.ToolTags,
    }
    for _, pkg := range packages {
        if pkg != module && !strings.HasPrefix(pkg, module+"/") { panic("foreign audited package: "+pkg) }
        relative := strings.TrimPrefix(strings.TrimPrefix(pkg, module), "/")
        if relative != "" && (filepath.Clean(relative) != relative || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, "../")) {
            panic("invalid audited package path: "+pkg)
        }
        directory := filepath.Join("/source", relative)
        resolved, err := filepath.EvalSymlinks(directory)
        if err != nil { panic(err) }
        if resolved != "/source" && !strings.HasPrefix(resolved, "/source/") { panic("audited package escapes source root: "+pkg) }
        if _, duplicate := packageFiles[pkg]; duplicate { panic("duplicate audited package: "+pkg) }
        packageFiles[pkg] = []string{}
        entries, err := os.ReadDir(directory)
        if err != nil { panic(err) }
        for _, entry := range entries {
            name := entry.Name()
            if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") { continue }
            packageFiles[pkg] = append(packageFiles[pkg], pkg+"/"+name)
            path := filepath.Join(directory, name)
            resolved, err := filepath.EvalSymlinks(path)
            if err != nil { panic(err) }
            if !strings.HasPrefix(resolved, "/source/") { panic("audited source escapes source root: "+path) }
            source, err := os.ReadFile(path)
            if err != nil { panic(err) }
            matched, err := target.MatchFile(directory, name)
            if err != nil { panic(err) }
            tree, err := parser.ParseFile(token.NewFileSet(), path, source, parser.AllErrors)
            if err != nil { panic(err) }
            body, call, initializer := false, false, false
            ast.Inspect(tree, func(node ast.Node) bool {
                switch n := node.(type) {
                case *ast.FuncDecl: body = body || n.Body != nil
                case *ast.FuncLit: body = true
                case *ast.CallExpr: call = true
                case *ast.GenDecl:
                    if n.Tok == token.VAR {
                        for _, spec := range n.Specs {
                            initializer = initializer || len(spec.(*ast.ValueSpec).Values) != 0
                        }
                    }
                }
                return true
            })
            digest := sha256.Sum256(source)
            files[pkg+"/"+name] = map[string]any{
                "parsed": true, "build_matched": matched, "has_function_body": body, "has_call": call,
                "has_var_initializer": initializer, "source_sha256": hex.EncodeToString(digest[:]),
            }
        }
        if len(packageFiles[pkg]) == 0 { panic("audited package has no source files: "+pkg) }
    }
    if len(packageFiles) != len(packages) { panic("partial source package inventory") }
    for _, name := range packages {
        if _, found := packageFiles[name]; !found { panic("missing audited package: "+name) }
    }
    auditDigest := sha256.Sum256([]byte(strings.Join(packages, "\n")+"\n"))
    record := map[string]any{
        "schema_version": 1, "kind": "go-ast-source-inventory", "parser": "go/parser",
        "complete": true, "packages": packageFiles, "build_context": buildContext,
        "candidate_sha": os.Getenv("INVENTORY_SHA"), "image_id": os.Getenv("INVENTORY_IMAGE_ID"),
        "coverpkg_sha256": hex.EncodeToString(auditDigest[:]), "files": files,
    }
    encoded, err := json.MarshalIndent(record, "", "  ")
    if err != nil { panic(err) }
    if err := os.WriteFile("/audit/source-inventory.json", append(encoded, '\n'), 0644); err != nil { panic(err) }
}
GO
go run /tmp/coverage-source-inventory.go
INVENTORY
[[ -s "$AUDIT/source-inventory.json" ]] || die "builder source inventory was not produced"
write_fixture

# Bind-mount GOCOVERDIR so a graceful exit lands counters on the host. 0777
# avoids UID 10001 vs host-root permission misses on Docker Desktop binds.
chmod 0777 "$RAW/cli" "$RAW/server"

BUILD_CONTEXT="$(python3 -c 'import json,sys; print(json.dumps(json.load(open(sys.argv[1]))["build_context"]))' "$AUDIT/source-inventory.json")"

# The journey runs one real task through the Docker engine, so the server
# (still UID 10001) joins the engine socket's group instead of running as root.
SOCK="${CAESIUM_SOCK:-/var/run/docker.sock}"
SOCK_GID="$("$CONTAINER_CLI" run --rm --platform "$PLATFORM" --user 0:0 --entrypoint stat \
  -v "$SOCK:/var/run/docker.sock" "$IMAGE_ID" -c '%g' /var/run/docker.sock 2>/dev/null || true)"
[[ "$SOCK_GID" =~ ^[0-9]+$ ]] || die "could not determine the group of $SOCK inside a container; refusing to guess"

# CAESIUM_RESOURCE_STATS_ENABLED defaults to false; the journey's task run is
# the only thing that reaches the resource sampler and its projections, so the
# feature is enabled here exactly as `just integration-up` enables it.
log "starting coverage server $SERVER_NAME on network $NETWORK (no host port; engine socket group $SOCK_GID)"
"$CONTAINER_CLI" run -d \
  --name "$SERVER_NAME" \
  --platform "$PLATFORM" \
  --network "$NETWORK" \
  --network-alias caesium \
  --user 10001:10001 \
  --group-add "$SOCK_GID" \
  -e GOCOVERDIR=/var/lib/caesium/coverage \
  -e CAESIUM_DATABASE_PATH=/var/lib/caesium/dqlite \
  -e CAESIUM_LOG_LEVEL=info \
  -e CAESIUM_AUTH_MODE=none \
  -e DOCKER_HOST=unix:///var/run/docker.sock \
  -e CAESIUM_RESOURCE_STATS_ENABLED=true \
  -e CAESIUM_RESOURCE_STATS_SAMPLE_INTERVAL=100ms \
  -v "$SOCK:/var/run/docker.sock" \
  -v "$RAW/server:/var/lib/caesium/coverage" \
  "$IMAGE_ID" start >/dev/null

# GET a server path from inside the journey network (the server has no host port).
server_get() {
  "$CONTAINER_CLI" run --rm --platform "$PLATFORM" \
    --network "$NETWORK" \
    --user 0:0 \
    --entrypoint wget \
    "$IMAGE_ID" -q -O - "http://caesium:8080$1"
}

healthy=0
for _ in $(seq 1 60); do
  if server_get /health 2>/dev/null | grep -q healthy; then
    healthy=1
    break
  fi
  sleep 1
done
if [[ "$healthy" -ne 1 ]]; then
  log "server never became healthy; logs:"
  "$CONTAINER_CLI" logs "$SERVER_NAME" || true
  write_provenance "$PROFILES/server.provenance.json" <<EOF
{"schema_version":1,"source":"server","kind":"gocoverdir","module":"github.com/caesium-cloud/caesium","candidate_sha":"$CANDIDATE_SHA","image_id":"$IMAGE_ID","build_context":$BUILD_CONTEXT,"image_provenance":"$IMAGE_PROVENANCE","verified":$IMAGE_VERIFIED,"complete":false,"missing":true,"killed":false,"collection":"never-healthy"}
EOF
  write_provenance "$PROFILES/cli.provenance.json" <<EOF
{"schema_version":1,"source":"cli","kind":"gocoverdir","module":"github.com/caesium-cloud/caesium","candidate_sha":"$CANDIDATE_SHA","image_id":"$IMAGE_ID","build_context":$BUILD_CONTEXT,"image_provenance":"$IMAGE_PROVENANCE","verified":$IMAGE_VERIFIED,"complete":false,"missing":true,"killed":false,"collection":"server-never-healthy"}
EOF
  run_checker
  exit $?
fi

# The main server stays on CAESIUM_AUTH_MODE=none, where the connector gate
# fails closed. A second instrumented start loads one file, then stops. That
# file resolves an env secret and shape-checks Kubernetes and Vault references.
# It does not dial either provider. Counters share the server GOCOVERDIR.
# A start that never logs the fingerprint fails the journey.
load_connector_for_coverage() {
  cat > "$ARTIFACTS/connectors.yaml" <<'YAML'
version: 1
connections:
  - id: primary
    provider: temporal
    endpoint: frontend.temporal.svc:7233
    scope: default
    enabled: true
    credentials:
      secretRefs:
        - secret://env/TEMPORAL_TOKEN?name=TEMPORAL_TOKEN
        - secret://k8s/temporal-creds/api-token
        - secret://vault/kv/data/temporal?field=token
      certificatePaths:
        - /var/run/secrets/caesium/temporal/tls.crt
    bindings:
      - name: publication
        version: "1"
        displayName: Publication
        statusQuery: publication_status
        activityAllowlist:
          - activityType: caesium.start
            jobs: [publish, notify]
        actions:
          - name: approve_publication
            inputSchema:
              type: object
              additionalProperties: false
              properties:
                note:
                  type: string
            resultSchema:
              type: object
              additionalProperties: false
              properties:
                approved:
                  type: boolean
YAML
  chmod 0644 "$ARTIFACTS/connectors.yaml"
  log "loading a connector config on an instrumented start"
  "$CONTAINER_CLI" rm -f "$CONNECTOR_NAME" >/dev/null 2>&1 || true
  if ! "$CONTAINER_CLI" run -d \
    --name "$CONNECTOR_NAME" \
    --platform "$PLATFORM" \
    --network "$NETWORK" \
    --user 10001:10001 \
    --group-add "$SOCK_GID" \
    -e GOCOVERDIR=/var/lib/caesium/coverage \
    -e CAESIUM_DATABASE_PATH=/var/lib/caesium/dqlite \
    -e CAESIUM_LOG_LEVEL=info \
    -e CAESIUM_AUTH_MODE=api-key \
    -e CAESIUM_AUTH_REQUIRE_TLS=false \
    -e CAESIUM_AUTH_KEY_HASH_SECRET=coverage-connector-auth-key-hash-secret-0001 \
    -e CAESIUM_CONNECTORS_ENABLED=true \
    -e CAESIUM_CONNECTORS_CONFIG_FILE=/etc/caesium/connectors/connections.yaml \
    -e TEMPORAL_TOKEN=coverage-connector-token \
    -v "$SOCK:/var/run/docker.sock" \
    -v "$ARTIFACTS/connectors.yaml:/etc/caesium/connectors/connections.yaml:ro" \
    -v "$RAW/server:/var/lib/caesium/coverage" \
    "$IMAGE_ID" start >/dev/null; then
    log "connector start could not be created"
    return 1
  fi
  connector_ready=0
  for _ in $(seq 1 60); do
    # Match a captured string. `docker logs | grep -q` under pipefail exits 141:
    # grep closes the pipe on the first hit while caesium is still writing
    # startup logs, so a present fingerprint line is reported missing.
    connector_logs="$("$CONTAINER_CLI" logs "$CONNECTOR_NAME" 2>&1 || true)"
    if [[ "$connector_logs" == *"connector config loaded"* ]]; then
      connector_ready=1
      break
    fi
    running="$("$CONTAINER_CLI" inspect -f '{{.State.Running}}' "$CONNECTOR_NAME" 2>/dev/null || true)"
    if [[ "$running" == "false" ]]; then
      break
    fi
    sleep 1
  done
  "$CONTAINER_CLI" kill --signal=SIGUSR2 "$CONNECTOR_NAME" >/dev/null 2>&1 || true
  sleep 1
  "$CONTAINER_CLI" stop -t 60 "$CONNECTOR_NAME" >/dev/null 2>&1 || true
  if [[ "$connector_ready" -ne 1 ]]; then
    log "connector start did not log a loaded fingerprint; logs:"
    "$CONTAINER_CLI" logs "$CONNECTOR_NAME" || true
    return 1
  fi
  return 0
}

log "running request-to-write-to-read: job apply then job export"
cli_rc=0
load_connector_for_coverage || cli_rc=1
"$CONTAINER_CLI" run --rm --platform "$PLATFORM" \
  --network "$NETWORK" \
  --user 0:0 \
  --entrypoint /bin/caesium \
  -e GOCOVERDIR=/coverage \
  -v "$RAW/cli:/coverage" \
  -v "$ARTIFACTS/fixture.job.yaml:/examples/fixture.job.yaml:ro" \
  "$IMAGE_ID" job apply --path /examples/fixture.job.yaml --server http://caesium:8080 \
  || cli_rc=$?
if [[ "$cli_rc" -eq 0 ]]; then
  "$CONTAINER_CLI" run --rm --platform "$PLATFORM" \
    --network "$NETWORK" \
    --user 0:0 \
    --entrypoint /bin/caesium \
    -e GOCOVERDIR=/coverage \
    -v "$RAW/cli:/coverage" \
    "$IMAGE_ID" job export coverage-write-read --server http://caesium:8080 \
    >/dev/null || cli_rc=$?
fi

# Resource-right-sizing W1-α (#449): start one run of the applied fixture with
# `caesium run start`, read the run back over HTTP until it is terminal, then
# read its instance through `caesium run partitions --json`. The step must have
# succeeded with exit code 0 and a sampled, nonzero peak-memory observation on
# BOTH reads; anything else (including a run that never finishes) fails the
# journey rather than letting an unreached sampler pass as coverage.
if [[ "$cli_rc" -eq 0 ]]; then
  log "starting a task run with resource stats enabled, then reading its observation back"
  server_get /v1/jobs >"$ARTIFACTS/jobs.json" || cli_rc=1
fi
if [[ "$cli_rc" -eq 0 ]]; then
  job_id="$(python3 -c 'import json,sys
print(next(j["id"] for j in json.load(open(sys.argv[1])) if j.get("alias") == "coverage-write-read"))' \
    "$ARTIFACTS/jobs.json")" || cli_rc=1
fi
if [[ "$cli_rc" -eq 0 ]]; then
  run_id="$("$CONTAINER_CLI" run --rm --platform "$PLATFORM" \
    --network "$NETWORK" \
    --user 0:0 \
    --entrypoint /bin/caesium \
    -e GOCOVERDIR=/coverage \
    -v "$RAW/cli:/coverage" \
    "$IMAGE_ID" run start --job-id "$job_id" --server http://caesium:8080)" || cli_rc=$?
  if [[ "$cli_rc" -eq 0 && ! "$run_id" =~ ^[0-9a-f-]{36}$ ]]; then
    log "caesium run start printed '$run_id', not a run id"
    cli_rc=1
  fi
fi
if [[ "$cli_rc" -eq 0 ]]; then
  run_terminal=0
  for _ in $(seq 1 120); do
    if server_get "/v1/jobs/$job_id/runs/$run_id" >"$ARTIFACTS/task-run.json" 2>/dev/null \
        && python3 -c 'import json,sys
sys.exit(0 if json.load(open(sys.argv[1])).get("status") not in ("pending", "running", "queued") else 1)' \
          "$ARTIFACTS/task-run.json"; then
      run_terminal=1
      break
    fi
    sleep 1
  done
  if [[ "$run_terminal" -ne 1 ]]; then
    log "run $run_id never became terminal"
    cli_rc=1
  fi
fi
if [[ "$cli_rc" -eq 0 ]]; then
  "$CONTAINER_CLI" run --rm --platform "$PLATFORM" \
    --network "$NETWORK" \
    --user 0:0 \
    --entrypoint /bin/caesium \
    -e GOCOVERDIR=/coverage \
    -v "$RAW/cli:/coverage" \
    "$IMAGE_ID" run partitions "$run_id" --job-id "$job_id" --task sample --json --server http://caesium:8080 \
    >"$ARTIFACTS/task-run-partitions.json" || cli_rc=$?
fi
if [[ "$cli_rc" -eq 0 ]] && ! python3 -c 'import json,sys
run = json.load(open(sys.argv[1]))
rows = json.load(open(sys.argv[2])).get("partitions") or []
problems = []
if run.get("status") != "succeeded":
    problems.append("run status %r, not succeeded" % run.get("status"))
for label, items in (("run", run.get("tasks") or []), ("partitions", rows)):
    if len(items) != 1:
        problems.append("%s read has %d task instances, not 1" % (label, len(items)))
        continue
    item = items[0]
    if item.get("exit_code") != 0:
        problems.append("%s read exit_code %r, not 0" % (label, item.get("exit_code")))
    if item.get("stats_source") != "sampled":
        problems.append("%s read stats_source %r, not sampled" % (label, item.get("stats_source")))
    if not isinstance(item.get("peak_memory_bytes"), int) or item["peak_memory_bytes"] <= 0:
        problems.append("%s read peak_memory_bytes %r is not a measurement" % (label, item.get("peak_memory_bytes")))
for problem in problems:
    print("task run observation: " + problem, file=sys.stderr)
sys.exit(1 if problems else 0)' "$ARTIFACTS/task-run.json" "$ARTIFACTS/task-run-partitions.json"; then
  log "run $run_id did not succeed with a sampled resource observation on both reads"
  cli_rc=1
fi

# Exercise the actual retained-log endpoint for the task that just ran. Live
# deadline/redaction behavior is qualified by the integration stream scenarios.
if [[ "$cli_rc" -eq 0 ]]; then
  task_id="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["tasks"][0]["id"])' \
    "$ARTIFACTS/task-run.json")" || cli_rc=1
fi
if [[ "$cli_rc" -eq 0 ]]; then
  server_get "/v1/jobs/$job_id/runs/$run_id/logs?task_id=$task_id" >"$ARTIFACTS/task-run.log" || cli_rc=1
  if ! grep -q 'coverage-task-log' "$ARTIFACTS/task-run.log"; then
    log "task's retained log did not contain its actual output"
    cli_rc=1
  fi
fi

# H1 operator surface on the same single-node server, read-only by design:
# list the dqlite members, then ask to remove an ID that is not one. The
# documented answer is a non-zero exit with a not_a_member refusal on stdout;
# anything else (success, another reason, a docker error) fails the journey.
if [[ "$cli_rc" -eq 0 ]]; then
  log "listing dqlite members, then requesting a refused non-member removal"
  "$CONTAINER_CLI" run --rm --platform "$PLATFORM" \
    --network "$NETWORK" \
    --user 0:0 \
    --entrypoint /bin/caesium \
    -e GOCOVERDIR=/coverage \
    -v "$RAW/cli:/coverage" \
    "$IMAGE_ID" system nodes list --json --server http://caesium:8080 \
    >"$ARTIFACTS/system-nodes.json" || cli_rc=$?
fi
if [[ "$cli_rc" -eq 0 ]]; then
  non_member="$(python3 -c 'import json,sys
ids = {str(n.get("id", "")) for n in json.load(open(sys.argv[1]))}
n = 42
while str(n) in ids: n += 1
print(n)' "$ARTIFACTS/system-nodes.json")" || cli_rc=1
fi
if [[ "$cli_rc" -eq 0 ]]; then
  refusal_rc=0
  "$CONTAINER_CLI" run --rm --platform "$PLATFORM" \
    --network "$NETWORK" \
    --user 0:0 \
    --entrypoint /bin/caesium \
    -e GOCOVERDIR=/coverage \
    -v "$RAW/cli:/coverage" \
    "$IMAGE_ID" system nodes remove "$non_member" --json --server http://caesium:8080 \
    >"$ARTIFACTS/system-nodes-remove.json" || refusal_rc=$?
  if [[ "$refusal_rc" -eq 0 || "$refusal_rc" -ge 125 ]] || ! python3 -c 'import json,sys
a = json.load(open(sys.argv[1]))
sys.exit(0 if (a.get("status"), a.get("reason")) == ("refused", "not_a_member") else 1)' \
      "$ARTIFACTS/system-nodes-remove.json"; then
    log "system nodes remove $non_member exited $refusal_rc without a not_a_member refusal"
    cli_rc=1
  fi
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
{"schema_version":1,"source":"cli","kind":"gocoverdir","module":"github.com/caesium-cloud/caesium","candidate_sha":"$CANDIDATE_SHA","image_id":"$IMAGE_ID","build_context":$BUILD_CONTEXT,"image_provenance":"$IMAGE_PROVENANCE","verified":$IMAGE_VERIFIED,"complete":$cli_complete,"missing":$cli_missing,"killed":$cli_killed,"exit_code":$cli_rc,"collection":"cli-exit","flush":"process-exit"}
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
server_abnormal=false
if [[ "$stop_rc" -ne 0 ]]; then
  server_abnormal=true
fi
if [[ "$oom" == "true" ]]; then
  killed=true
  server_abnormal=true
  signal="SIGKILL"
fi
if [[ "$exit_code" != "0" && "$exit_code" != "143" ]]; then
  server_abnormal=true
  if [[ "$exit_code" == "137" ]]; then
    killed=true
    signal="SIGKILL"
  fi
fi

server_complete=false
server_missing=true
if [[ "$killed" == "true" || "$server_abnormal" == "true" ]]; then
  server_complete=false
  server_missing=$(gocoverdir_complete "$RAW/server" && echo false || echo true)
elif gocoverdir_complete "$RAW/server"; then
  server_missing=false
  if textfmt_dir "$RAW/server" "$PROFILES/server.out"; then
    server_complete=true
  fi
fi
write_provenance "$PROFILES/server.provenance.json" <<EOF
{"schema_version":1,"source":"server","kind":"gocoverdir","module":"github.com/caesium-cloud/caesium","candidate_sha":"$CANDIDATE_SHA","image_id":"$IMAGE_ID","build_context":$BUILD_CONTEXT,"image_provenance":"$IMAGE_PROVENANCE","verified":$IMAGE_VERIFIED,"complete":$server_complete,"missing":$server_missing,"killed":$killed,"exit_code":$exit_code,"stop_rc":$stop_rc,"signal":"$signal","oom_killed":$oom,"collection":"graceful-shutdown","flush":"sigusr2"}
EOF

if [[ "$cli_complete" == "true" && "$server_complete" == "true" ]]; then
  if merge_gocoverdirs "$RAW/integration" "$RAW/cli" "$RAW/server"; then
    textfmt_dir "$RAW/integration" "$PROFILES/integration.out" || true
    write_provenance "$PROFILES/integration.provenance.json" <<EOF
{"schema_version":1,"source":"integration","kind":"gocoverdir","module":"github.com/caesium-cloud/caesium","candidate_sha":"$CANDIDATE_SHA","image_id":"$IMAGE_ID","build_context":$BUILD_CONTEXT,"image_provenance":"$IMAGE_PROVENANCE","verified":$IMAGE_VERIFIED,"complete":true,"missing":false,"killed":false,"sources":["cli","server"],"collection":"covdata-merge"}
EOF
  fi
fi

# A separate process and GOCOVERDIR keep the browser contribution distinct
# from the CLI/server write-to-read path. The live Console bundle is served by
# the same instrumented image; Playwright drives Chromium over a loopback-only
# ephemeral host port and must record both expected first-attempt passes.
"$CONTAINER_CLI" rm -f "$SERVER_NAME" >/dev/null 2>&1 || true
chmod 0777 "$RAW/browser"
log "starting isolated browser coverage server $BROWSER_SERVER_NAME"
"$CONTAINER_CLI" run -d \
  --name "$BROWSER_SERVER_NAME" \
  --platform "$PLATFORM" \
  --network "$NETWORK" \
  -p 127.0.0.1::8080 \
  --user 10001:10001 \
  -e GOCOVERDIR=/var/lib/caesium/coverage \
  -e CAESIUM_DATABASE_PATH=/var/lib/caesium/dqlite \
  -e CAESIUM_AUTH_MODE=none \
  -v "$RAW/browser:/var/lib/caesium/coverage" \
  "$IMAGE_ID" start >/dev/null

browser_healthy=0
for _ in $(seq 1 60); do
  if "$CONTAINER_CLI" run --rm --platform "$PLATFORM" \
      --network "$NETWORK" --user 0:0 --entrypoint wget \
      "$IMAGE_ID" -q -O - "http://$BROWSER_SERVER_NAME:8080/health" 2>/dev/null | grep -q healthy; then
    browser_healthy=1
    break
  fi
  sleep 1
done
browser_rc=125
if [[ "$browser_healthy" -eq 1 ]]; then
  browser_addr="$("$CONTAINER_CLI" port "$BROWSER_SERVER_NAME" 8080/tcp | head -n 1)"
  if [[ "$browser_addr" =~ ^127\.0\.0\.1:[0-9]+$ ]]; then
    browser_rc=0
    bash "$ROOT/scripts/coverage-browser-journey.sh" \
      "http://$browser_addr" "$ARTIFACTS/browser-playwright.json" \
      >"$ARTIFACTS/browser-journey.log" 2>&1 || browser_rc=$?
  else
    log "unexpected browser coverage port mapping: $browser_addr"
  fi
else
  "$CONTAINER_CLI" logs "$BROWSER_SERVER_NAME" >"$ARTIFACTS/browser-server.log" 2>&1 || true
fi

"$CONTAINER_CLI" kill --signal=SIGUSR2 "$BROWSER_SERVER_NAME" >/dev/null 2>&1 || true
sleep 1
browser_stop_rc=0
"$CONTAINER_CLI" stop -t 60 "$BROWSER_SERVER_NAME" >/dev/null || browser_stop_rc=$?
browser_inspect="$("$CONTAINER_CLI" inspect "$BROWSER_SERVER_NAME" 2>/dev/null || true)"
browser_exit="$(printf '%s' "$browser_inspect" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d[0]["State"].get("ExitCode", 1) if d else 1)' 2>/dev/null || echo 1)"
browser_oom="$(printf '%s' "$browser_inspect" | python3 -c 'import json,sys; d=json.load(sys.stdin); print("true" if d and d[0]["State"].get("OOMKilled") else "false")' 2>/dev/null || echo true)"
browser_complete=false
browser_missing=true
browser_killed=false
if [[ "$browser_oom" == true || "$browser_exit" == 137 ]]; then
  browser_killed=true
fi
if [[ "$browser_rc" -eq 0 && "$browser_stop_rc" -eq 0 && "$browser_killed" == false && ( "$browser_exit" == 0 || "$browser_exit" == 143 ) ]] \
    && gocoverdir_complete "$RAW/browser"; then
  browser_missing=false
  if textfmt_dir "$RAW/browser" "$PROFILES/browser.out"; then
    browser_complete=true
  fi
else
  browser_missing=$(gocoverdir_complete "$RAW/browser" && echo false || echo true)
fi
write_provenance "$PROFILES/browser.provenance.json" <<EOF
{"schema_version":1,"source":"browser","kind":"gocoverdir","module":"github.com/caesium-cloud/caesium","candidate_sha":"$CANDIDATE_SHA","image_id":"$IMAGE_ID","build_context":$BUILD_CONTEXT,"image_provenance":"$IMAGE_PROVENANCE","verified":$IMAGE_VERIFIED,"complete":$browser_complete,"missing":$browser_missing,"killed":$browser_killed,"test_exit_code":$browser_rc,"test_results":"$ARTIFACTS/browser-playwright.json","exit_code":$browser_exit,"stop_rc":$browser_stop_rc,"oom_killed":$browser_oom,"collection":"chromium-live-console","flush":"sigusr2+sigterm"}
EOF

log "checking labelled coverage"
run_checker
rc=$?
log "coverage checker exit $rc"
exit "$rc"
