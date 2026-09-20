#!/usr/bin/env bash
# E3 host controller: compare a base SHA/image against a candidate across
# backend (E2 Go load driver + hermetic Go benchmarks) and browser/bundle
# surfaces. Release-equivalent UNINSTRUMENTED images only.
#
# This script does not edit the justfile (G6 owns later recipes). It calls
# existing `just builder` / `just tag=<sha> build-release` / `just builder-full`.
# It never starts the shared integration-up server, never sets GOCOVERDIR, and
# never deploys a testfault-instrumented image.
#
# Compare-only (no Docker; used by scripts/test_compare_performance.py):
#   CAESIUM_PERF_ARTIFACTS=/tmp/perf bash scripts/performance.sh compare comparison.json
#
# Live interleaved comparison (orchestrator-owned; serializes Docker so base
# and candidate never compete for the daemon):
#   CAESIUM_PERF_ID=perf-<id> \
#   CAESIUM_PERF_ARTIFACTS="$(mktemp -d)" \
#   CAESIUM_PERF_BASE_SHA=<sha> \
#   CAESIUM_PERF_CANDIDATE_SHA=<sha> \
#     bash scripts/performance.sh
#
# Optional:
#   CAESIUM_PERF_BASE_IMAGE / CAESIUM_PERF_CANDIDATE_IMAGE
#       skip the build and use these release images (recorded as supplied
#       unless this run built them).
#   CAESIUM_PERF_ALLOW_UNVERIFIED_IMAGE=1
#       proceed when an image was not built by this run.
#   CAESIUM_PERF_WORKLOADS=closed-baseline   catalog names (comma list)
#   CAESIUM_PERF_REPEATS=5                   samples per side per phase
#   CAESIUM_PERF_LOAD=1                      run the Go load driver (default 1)
#   CAESIUM_PERF_BENCH=1                     run hermetic Go benchmarks (default 1)
#   CAESIUM_PERF_BROWSER=0                   run ui/e2e/performance.spec.ts (default 0)
#   CAESIUM_PERF_BUNDLE=1                    run check-bundle-size.mjs (default 1)
#   CAESIUM_PERF_KEEP=1                      leave owned containers in place
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

log() { printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }
die() { log "ERROR: $*"; exit 1; }
require_cmd() { command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"; }
require_env() { [[ -n "${!1:-}" ]] || die "$1 is required"; }

# ---------------------------------------------------------------------------
# compare-only: no Docker, feed an existing document to the comparator.
# ---------------------------------------------------------------------------
if [[ "${1:-}" == "compare" ]]; then
  shift
  ARTIFACTS="${CAESIUM_PERF_ARTIFACTS:-}"
  [[ -n "$ARTIFACTS" ]] || die "CAESIUM_PERF_ARTIFACTS is required for compare"
  mkdir -p "$ARTIFACTS"
  INPUT="${1:-$ARTIFACTS/comparison.json}"
  [[ -f "$INPUT" ]] || die "comparison document not found: $INPUT"
  require_cmd python3
  python3 "$ROOT/scripts/compare-performance.py" \
    --input "$INPUT" \
    --output "$ARTIFACTS/report.json"
  exit $?
fi

# ---------------------------------------------------------------------------
# Live interleaved comparison
# ---------------------------------------------------------------------------
require_cmd docker
require_cmd python3
require_cmd git
require_cmd just
require_env CAESIUM_PERF_ID
require_env CAESIUM_PERF_ARTIFACTS

ID="$CAESIUM_PERF_ID"
if [[ ! "$ID" =~ ^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$ ]]; then
  die "CAESIUM_PERF_ID must be a lowercase DNS-1123 name of at most 40 characters, got '$ID'"
fi

ARTIFACTS="$(mkdir -p "$CAESIUM_PERF_ARTIFACTS" && cd "$CAESIUM_PERF_ARTIFACTS" && pwd)"
CAESIUM_PERF_ARTIFACTS="$ARTIFACTS"

CANDIDATE_SHA="${CAESIUM_PERF_CANDIDATE_SHA:-$(git -C "$ROOT" rev-parse HEAD)}"
BASE_SHA="${CAESIUM_PERF_BASE_SHA:-}"
[[ -n "$BASE_SHA" ]] || die "CAESIUM_PERF_BASE_SHA is required for a live comparison"

IMAGE_REPO="caesiumcloud/caesium"
BASE_IMAGE="${CAESIUM_PERF_BASE_IMAGE:-$IMAGE_REPO:$BASE_SHA}"
CANDIDATE_IMAGE="${CAESIUM_PERF_CANDIDATE_IMAGE:-$IMAGE_REPO:$CANDIDATE_SHA}"

WORKLOADS="${CAESIUM_PERF_WORKLOADS:-closed-baseline}"
REPEATS="${CAESIUM_PERF_REPEATS:-5}"
RUN_LOAD="${CAESIUM_PERF_LOAD:-1}"
RUN_BENCH="${CAESIUM_PERF_BENCH:-1}"
RUN_BROWSER="${CAESIUM_PERF_BROWSER:-0}"
RUN_BUNDLE="${CAESIUM_PERF_BUNDLE:-1}"
ALLOW_UNVERIFIED="${CAESIUM_PERF_ALLOW_UNVERIFIED_IMAGE:-0}"
KEEP="${CAESIUM_PERF_KEEP:-0}"
API_KEY="${CAESIUM_MANUAL_TRIGGER_API_KEY:-perf-test-key}"
TASK_IMAGE="${CAESIUM_PERF_TASK_IMAGE:-alpine:3.23}"
PERF_PORT="${CAESIUM_PERF_PORT:-18080}"

NETWORK="caesium-perf-$ID"
OWNED=()
BASE_WORKTREE=""
PERF_LOCK="${CAESIUM_PERF_LOCK:-/tmp/caesium-perf.lock}"

if ! mkdir "$PERF_LOCK" 2>/dev/null; then
  die "another performance comparison holds $PERF_LOCK; isolate competing load"
fi

cleanup() {
  local rc=$?
  rmdir "$PERF_LOCK" 2>/dev/null || true
  if [[ "$KEEP" == "1" ]]; then
    log "CAESIUM_PERF_KEEP=1; leaving owned resources in place"
    return "$rc"
  fi
  local name
  for name in "${OWNED[@]+"${OWNED[@]}"}"; do
    docker rm -f "$name" >/dev/null 2>&1 || true
  done
  docker network rm "$NETWORK" >/dev/null 2>&1 || true
  if [[ -n "$BASE_WORKTREE" && -d "$BASE_WORKTREE" ]]; then
    git -C "$ROOT" worktree remove --force "$BASE_WORKTREE" >/dev/null 2>&1 || rm -rf "$BASE_WORKTREE"
  fi
  rm -f "$ARTIFACTS/.binary-under-check" "$ROOT/.tmp/caesium-load-driver-perf"
  return "$rc"
}
trap cleanup EXIT

mkdir -p "$ARTIFACTS/base" "$ARTIFACTS/candidate" "$ARTIFACTS/observations" "$ROOT/.tmp"
: >"$ARTIFACTS/owned-containers.txt"

# Competing load isolation: refuse a shared host that already has a Caesium
# server we do not own, unless the operator opts in.
if docker ps --format '{{.Names}}' | grep -Eq '^caesium-server'; then
  if [[ "${CAESIUM_PERF_ALLOW_SHARED_HOST:-0}" != "1" ]]; then
    die "a caesium-server* container is already running; isolate competing load or set CAESIUM_PERF_ALLOW_SHARED_HOST=1"
  fi
  log "WARNING: shared host allowed; competing load is not isolated"
fi

HOST_ID="$(printf '%s|%s|%s' "$(hostname)" "$(uname -s)" "$(uname -m)")"
case "$(uname -m)" in
  aarch64|arm64) DOCKER_ARCH="arm64" ;;
  x86_64|amd64) DOCKER_ARCH="amd64" ;;
  *) DOCKER_ARCH="$(uname -m)" ;;
esac
DOCKER_PLATFORM="${CAESIUM_PLATFORM:-linux/${DOCKER_ARCH}}"

export ARTIFACTS HOST_ID DOCKER_PLATFORM
python3 - <<'PY'
import json, os, platform, pathlib, socket, subprocess
art = os.environ["ARTIFACTS"]
try:
    loadavg = os.getloadavg()
except OSError:
    loadavg = None
try:
    docker = subprocess.check_output(["docker", "version", "--format", "{{.Server.Version}}"], text=True).strip()
except Exception as err:
    docker = f"unavailable: {err}"
pathlib.Path(art, "observations", "host.json").write_text(json.dumps({
    "hostname": socket.gethostname(),
    "platform": platform.platform(),
    "machine": platform.machine(),
    "processor": platform.processor(),
    "python": platform.python_version(),
    "loadavg": loadavg,
    "docker": docker,
    "host_id": os.environ["HOST_ID"],
    "docker_platform": os.environ["DOCKER_PLATFORM"],
}, indent=2) + "\n")
PY

CATALOG="$ROOT/test/performance/workloads.json"
[[ -f "$CATALOG" ]] || die "missing $CATALOG"
CATALOG_SHA="$(python3 -c "import hashlib,pathlib; print(hashlib.sha256(pathlib.Path(r'''$CATALOG''').read_bytes()).hexdigest())")"
SETTINGS_SHA="$(printf '%s\n' \
  "workloads=$WORKLOADS" \
  "repeats=$REPEATS" \
  "task_image=$TASK_IMAGE" \
  "log_level=info" \
  "cache=true" \
  "queue_dequeuer=true" \
  "instrumented=false" \
  | python3 -c "import hashlib,sys; print(hashlib.sha256(sys.stdin.buffer.read()).hexdigest())")"

log "id=$ID base=$BASE_SHA candidate=$CANDIDATE_SHA artifacts=$ARTIFACTS workloads=$WORKLOADS repeats=$REPEATS"

# ---------------------------------------------------------------------------
# Toolchain: same builder for both images
# ---------------------------------------------------------------------------
log "ensuring containerized builder (just builder)"
just builder
BUILDER_IMAGE="caesiumcloud/caesium-builder:latest"
BUILDER_ID="$(docker image inspect --format '{{.Id}}' "$BUILDER_IMAGE")"
GO_VERSION="$(docker run --rm --platform "$DOCKER_PLATFORM" "$BUILDER_IMAGE" go version | awk '{print $3}')"
TOOLCHAIN_ID="${BUILDER_IMAGE}@${BUILDER_ID}"

if [[ "$RUN_BENCH" == "1" ]]; then
  log "ensuring builder-full for hermetic Go benchmarks"
  just builder-full
fi

# ---------------------------------------------------------------------------
# Build release-equivalent images (uninstrumented)
# ---------------------------------------------------------------------------
build_release() {
  local sha="$1" dest="$2" src="$3"
  if docker image inspect "$dest" >/dev/null 2>&1; then
    log "image $dest already present; recording as supplied"
    echo "supplied"
    return 0
  fi
  log "building release image $dest from $src at $sha (just tag=$sha build-release)"
  (
    cd "$src"
    CAESIUM_SKIP_IMAGE_BUILD=false just tag="$sha" build-release
  )
  echo "built"
}

CANDIDATE_BUILT="supplied"
if ! docker image inspect "$CANDIDATE_IMAGE" >/dev/null 2>&1; then
  GIT_HEAD="$(git -C "$ROOT" rev-parse HEAD)"
  if [[ -n "$(git -C "$ROOT" status --porcelain)" && "$ALLOW_UNVERIFIED" != "1" ]]; then
    die "refusing to build $CANDIDATE_IMAGE from a dirty working tree; commit or set CAESIUM_PERF_ALLOW_UNVERIFIED_IMAGE=1"
  fi
  if [[ "$CANDIDATE_SHA" != "$GIT_HEAD" && "$ALLOW_UNVERIFIED" != "1" ]]; then
    die "CAESIUM_PERF_CANDIDATE_SHA=$CANDIDATE_SHA but HEAD is $GIT_HEAD"
  fi
  CANDIDATE_BUILT="$(build_release "$CANDIDATE_SHA" "$CANDIDATE_IMAGE" "$ROOT")"
fi

BASE_BUILT="supplied"
if ! docker image inspect "$BASE_IMAGE" >/dev/null 2>&1; then
  BASE_WORKTREE="$(mktemp -d "${TMPDIR:-/tmp}/caesium-perf-base.XXXXXX")"
  git -C "$ROOT" worktree add --detach "$BASE_WORKTREE" "$BASE_SHA"
  BASE_BUILT="$(build_release "$BASE_SHA" "$BASE_IMAGE" "$BASE_WORKTREE")"
fi

if [[ "$CANDIDATE_BUILT" != "built" || "$BASE_BUILT" != "built" ]]; then
  if [[ "$ALLOW_UNVERIFIED" != "1" && ( "$CANDIDATE_BUILT" != "built" && "$BASE_BUILT" != "built" ) ]]; then
    log "WARNING: one or both images were supplied rather than built by this run"
  fi
  if [[ "$ALLOW_UNVERIFIED" != "1" ]]; then
    if [[ "$CANDIDATE_BUILT" != "built" ]]; then
      die "candidate image $CANDIDATE_IMAGE was not built by this run; delete it and rerun, or set CAESIUM_PERF_ALLOW_UNVERIFIED_IMAGE=1"
    fi
    if [[ "$BASE_BUILT" != "built" ]]; then
      die "base image $BASE_IMAGE was not built by this run; delete it and rerun, or set CAESIUM_PERF_ALLOW_UNVERIFIED_IMAGE=1"
    fi
  fi
fi

docker image inspect "$BASE_IMAGE" >/dev/null || die "base image $BASE_IMAGE is not present"
docker image inspect "$CANDIDATE_IMAGE" >/dev/null || die "candidate image $CANDIDATE_IMAGE is not present"
BASE_IMAGE_ID="$(docker image inspect --format '{{.Id}}' "$BASE_IMAGE")"
CANDIDATE_IMAGE_ID="$(docker image inspect --format '{{.Id}}' "$CANDIDATE_IMAGE")"
docker image inspect "$BASE_IMAGE" >"$ARTIFACTS/base/image.json"
docker image inspect "$CANDIDATE_IMAGE" >"$ARTIFACTS/candidate/image.json"

# ---------------------------------------------------------------------------
# Uninstrumented scan: GOCOVERDIR / testfault / coverage must not leak
# ---------------------------------------------------------------------------
TESTFAULT_MARKERS=(caesium-testfault-control CAESIUM_TESTFAULT_DIR bus-publish-pause.json testfault GOCOVERDIR)
extract_binary() {
  local image="$1" dest="$2" cid
  cid="$(docker create --platform "$DOCKER_PLATFORM" "$image" true)" || return 1
  docker cp "$cid":/bin/caesium "$dest" >/dev/null 2>&1
  local rc=$?
  docker rm -f "$cid" >/dev/null 2>&1 || true
  return $rc
}

scan_uninstrumented() {
  local image="$1" report="$2" dest="$ARTIFACTS/.binary-under-check"
  extract_binary "$image" "$dest" || die "could not extract /bin/caesium from $image"
  : >"$report"
  local marker hits
  for marker in "${TESTFAULT_MARKERS[@]}"; do
    hits="$(grep -ac -- "$marker" "$dest" 2>/dev/null || true)"
    printf '%s marker=%s hits=%s\n' "$image" "$marker" "${hits:-0}" >>"$report"
    [[ "${hits:-0}" == "0" ]] || die "image $image contains instrumentation marker '$marker' ($hits hits); performance images must be release-equivalent and uninstrumented"
  done
  python3 -c "import hashlib,pathlib; print('sha256:'+hashlib.sha256(pathlib.Path(r'''$dest''').read_bytes()).hexdigest())"
}

BASE_CLI_DIGEST="$(scan_uninstrumented "$BASE_IMAGE" "$ARTIFACTS/base/uninstrumented.txt")"
CANDIDATE_CLI_DIGEST="$(scan_uninstrumented "$CANDIDATE_IMAGE" "$ARTIFACTS/candidate/uninstrumented.txt")"
log "uninstrumented scan passed for both images"

# ---------------------------------------------------------------------------
# Compile the E2 load driver once (candidate tree, same builder)
# ---------------------------------------------------------------------------
DRIVER="$ARTIFACTS/caesium-load-driver"
if [[ "$RUN_LOAD" == "1" ]]; then
  log "compiling E2 load driver through $BUILDER_IMAGE"
  docker run --rm --platform "$DOCKER_PLATFORM" \
    -v "$ROOT:/bld/caesium" -w /bld/caesium \
    "$BUILDER_IMAGE" \
    go build -trimpath -o /bld/caesium/.tmp/caesium-load-driver-perf ./test/load
  cp "$ROOT/.tmp/caesium-load-driver-perf" "$DRIVER"
  chmod +x "$DRIVER"
fi

# ---------------------------------------------------------------------------
# Hermetic Go benchmarks (no server)
# ---------------------------------------------------------------------------
run_benches() {
  local src="$1" dest="$2"
  mkdir -p "$(dirname "$dest")"
  docker run --rm --platform "$DOCKER_PLATFORM" \
    -v "$src:/bld/caesium" -w /bld/caesium \
    "caesiumcloud/caesium-builder:latest-full" \
    sh -c "mkdir -p ui/dist && touch ui/dist/index.html && go test -bench='^Benchmark(Owner|Recover)' -benchmem -count=${REPEATS} -run '^$' ./internal/run" \
    >"$dest" 2>&1 || {
      log "WARNING: benchmarks in $src failed; recording the log, not treating it as a speed pass"
      return 1
    }
}

if [[ "$RUN_BENCH" == "1" ]]; then
  run_benches "$ROOT" "$ARTIFACTS/candidate/bench.txt" || true
  if [[ -n "$BASE_WORKTREE" && -d "$BASE_WORKTREE" ]]; then
    run_benches "$BASE_WORKTREE" "$ARTIFACTS/base/bench.txt" || true
  else
    log "base worktree absent; skipping base benches (comparator fails closed if only one side has them)"
  fi
fi

# ---------------------------------------------------------------------------
# Bundle size
# ---------------------------------------------------------------------------
if [[ "$RUN_BUNDLE" == "1" ]]; then
  if [[ -d "$ROOT/ui/dist/assets" ]]; then
    python3 "$ROOT/scripts/compare-performance.py" --bundle-dir "$ROOT/ui/dist/assets" \
      >"$ARTIFACTS/candidate/bundle.json" || true
  else
    printf '%s\n' '{"ok":false,"errors":["ui/dist/assets missing; run npm run build"],"note":"bundle not collected"}' \
      >"$ARTIFACTS/candidate/bundle.json"
  fi
  if [[ -n "$BASE_WORKTREE" && -d "$BASE_WORKTREE/ui/dist/assets" ]]; then
    python3 "$ROOT/scripts/compare-performance.py" --bundle-dir "$BASE_WORKTREE/ui/dist/assets" \
      >"$ARTIFACTS/base/bundle.json" || true
  fi
fi

# ---------------------------------------------------------------------------
# Isolated network + interleaved cold/warm load runs
# ---------------------------------------------------------------------------
docker network create "$NETWORK" >/dev/null
log "created isolated network $NETWORK"

start_server() {
  local side="$1" image="$2"
  local name="caesium-perf-$ID-$side"
  local publish=()
  if [[ "$RUN_BROWSER" == "1" ]]; then
    publish=(-p "${PERF_PORT}:8080")
  fi
  docker rm -f "$name" >/dev/null 2>&1 || true
  # Pin settings. info-level logs, cache+queue on so catalog entries that
  # require them are runnable; NEVER GOCOVERDIR, NEVER testfault.
  docker run -d --platform "$DOCKER_PLATFORM" \
    --name "$name" \
    --network "$NETWORK" \
    ${publish[@]+"${publish[@]}"} \
    --privileged \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -e DOCKER_HOST=unix:///var/run/docker.sock \
    -e CAESIUM_MANUAL_TRIGGER_API_KEY="$API_KEY" \
    -e CAESIUM_LOG_LEVEL=info \
    -e CAESIUM_CACHE_ENABLED=true \
    -e CAESIUM_CACHE_PIN_DIGESTS=true \
    -e CAESIUM_RUN_QUEUE_ENABLED=true \
    -e CAESIUM_RUN_QUEUE_DEQUEUER_ENABLED=true \
    -e CAESIUM_RUN_QUEUE_DEQUEUE_INTERVAL=500ms \
    --user 0:0 \
    "$image" start >/dev/null
  OWNED+=("$name")
  printf '%s\n' "$name" >>"$ARTIFACTS/owned-containers.txt"
  local tries=0
  until docker run --rm --network "container:$name" "$BUILDER_IMAGE" \
      wget -q -O /dev/null http://127.0.0.1:8080/health; do
    tries=$((tries + 1))
    if [[ "$tries" -gt 40 ]]; then
      docker logs "$name" >&2 || true
      die "$name did not become ready"
    fi
    sleep 1
  done
  local cover
  cover="$(docker exec "$name" printenv GOCOVERDIR 2>/dev/null || true)"
  [[ -z "$cover" ]] || die "$name has GOCOVERDIR=$cover; uninstrumented contract violated"
  cover="$(docker exec "$name" printenv CAESIUM_TESTFAULT_DIR 2>/dev/null || true)"
  [[ -z "$cover" ]] || die "$name has CAESIUM_TESTFAULT_DIR set; uninstrumented contract violated"
}

stop_server() {
  local side="$1"
  local name="caesium-perf-$ID-$side"
  docker rm -f "$name" >/dev/null 2>&1 || true
}

run_workload() {
  local side="$1" phase="$2" workload="$3" idx="$4"
  local name="caesium-perf-$ID-$side"
  local outdir="$ARTIFACTS/$side/runs"
  mkdir -p "$outdir"
  local json_out="$outdir/${workload}-${phase}-${idx}.json"
  local err_out="$outdir/${workload}-${phase}-${idx}.stderr"
  set +e
  docker run --rm --platform "$DOCKER_PLATFORM" \
    --network "container:$name" \
    -v "$ROOT:/bld/caesium" \
    -v "$DRIVER:/driver:ro" \
    -e CAESIUM_MANUAL_TRIGGER_API_KEY="$API_KEY" \
    -e CAESIUM_LOAD_SERVER=http://127.0.0.1:8080 \
    "$BUILDER_IMAGE" \
    /driver \
      -catalog /bld/caesium/test/performance/workloads.json \
      -catalog-workload "$workload" \
      -server http://127.0.0.1:8080 \
      -json-output - \
      -image "$TASK_IMAGE" \
      -resource-container "$name" \
    >"$json_out" 2>"$err_out"
  local rc=$?
  set -e
  printf '%s\n' "$rc" >"$outdir/${workload}-${phase}-${idx}.exit"
  return 0
}

IFS=',' read -r -a WORKLOAD_LIST <<<"$WORKLOADS"

if [[ "$RUN_LOAD" == "1" ]]; then
  [[ -x "$DRIVER" ]] || die "load driver was not compiled"
  log "interleaving $REPEATS cold runs per side, then $REPEATS warm runs per side"

  r=1
  while [[ "$r" -le "$REPEATS" ]]; do
    if (( r % 2 == 1 )); then order=(base candidate); else order=(candidate base); fi
    for side in "${order[@]}"; do
      image="$BASE_IMAGE"
      [[ "$side" == "candidate" ]] && image="$CANDIDATE_IMAGE"
      start_server "$side" "$image"
      for workload in "${WORKLOAD_LIST[@]}"; do
        workload="${workload// /}"
        log "cold $side $workload #$r"
        run_workload "$side" "cold" "$workload" "$r"
      done
      stop_server "$side"
    done
    r=$((r + 1))
  done

  if (( REPEATS % 2 == 1 )); then order=(base candidate); else order=(candidate base); fi
  for side in "${order[@]}"; do
    image="$BASE_IMAGE"
    [[ "$side" == "candidate" ]] && image="$CANDIDATE_IMAGE"
    start_server "$side" "$image"
    for workload in "${WORKLOAD_LIST[@]}"; do
      workload="${workload// /}"
      log "warm-up (discarded) $side $workload"
      run_workload "$side" "warmup" "$workload" "0" || true
    done
    r=1
    while [[ "$r" -le "$REPEATS" ]]; do
      for workload in "${WORKLOAD_LIST[@]}"; do
        workload="${workload// /}"
        log "warm $side $workload #$r"
        run_workload "$side" "warm" "$workload" "$r"
      done
      r=$((r + 1))
    done
    if [[ "$RUN_BROWSER" == "1" ]]; then
      log "browser performance.spec.ts against $side (live)"
      (
        cd "$ROOT/ui"
        PLAYWRIGHT_BASE_URL="http://127.0.0.1:${PERF_PORT}" \
        CAESIUM_MANUAL_TRIGGER_API_KEY="$API_KEY" \
        CAESIUM_PERF_BROWSER_OUT="$ARTIFACTS/$side/browser.jsonl" \
        npx playwright test e2e/performance.spec.ts --project=default
      ) || log "WARNING: browser spec failed for $side; correctness will fail closed"
    fi
    stop_server "$side"
  done
fi

# ---------------------------------------------------------------------------
# Assemble comparison document and run the fail-closed comparator
# ---------------------------------------------------------------------------
export BASE_SHA CANDIDATE_SHA BASE_IMAGE CANDIDATE_IMAGE
export BASE_IMAGE_ID CANDIDATE_IMAGE_ID BASE_CLI_DIGEST CANDIDATE_CLI_DIGEST
export GO_VERSION TOOLCHAIN_ID CATALOG_SHA SETTINGS_SHA
export BASE_BUILT CANDIDATE_BUILT BUILDER_ID
python3 - <<'PY'
import json, os, pathlib, re

art = pathlib.Path(os.environ["ARTIFACTS"])

def load_json(path):
    try:
        return json.loads(pathlib.Path(path).read_text())
    except Exception:
        return None

def bench_text(path):
    p = pathlib.Path(path)
    return p.read_text() if p.is_file() else ""

def workload_samples(side, phase):
    runs = art / side / "runs"
    out = {}
    if not runs.is_dir():
        return out
    for path in sorted(runs.glob(f"*-{phase}-*.json")):
        m = re.match(r"^(.*)-" + re.escape(phase) + r"-(\d+)\.json$", path.name)
        if not m:
            continue
        workload = m.group(1)
        report = load_json(path)
        exit_path = path.with_suffix(".exit")
        exit_code = int(exit_path.read_text().strip()) if exit_path.is_file() else None
        outcome = "failed"
        duration = None
        if isinstance(report, dict):
            outcome = report.get("outcome") or "failed"
            duration = report.get("duration_seconds")
            if exit_code not in (None, 0) and outcome == "passed":
                outcome = "failed"
        key = f"{workload}.{phase}"
        out.setdefault(key, {"phase": phase, "samples": []})
        out[key]["samples"].append({
            "value": duration if duration is not None else 0,
            "outcome": "passed" if outcome == "passed" else "failed",
            "duration_seconds": duration,
            "exit_code": exit_code,
        })
    return out

def bundle(side):
    doc = load_json(art / side / "bundle.json") or {}
    keep = {}
    for key in ("largest_js_raw_bytes", "largest_js_gzip_bytes", "total_raw_bytes", "total_gzip_bytes"):
        if key in doc:
            keep[key] = doc[key]
    return keep

def correctness(workloads):
    failures = []
    for name, body in workloads.items():
        for i, sample in enumerate(body.get("samples") or []):
            if sample.get("outcome") != "passed":
                failures.append(f"{name}[{i}] outcome={sample.get('outcome')}")
    return {"ok": not failures, "failures": failures}

def side_doc(label, sha, image, image_id, cli_digest, built):
    workloads = {}
    workloads.update(workload_samples(label, "cold"))
    workloads.update(workload_samples(label, "warm"))
    benches = bench_text(art / label / "bench.txt")
    browser = {}
    browser_path = art / label / "browser.jsonl"
    if browser_path.is_file():
        for line in browser_path.read_text().splitlines():
            if not line.strip():
                continue
            row = json.loads(line)
            metric = row.get("metric")
            if not metric:
                continue
            browser.setdefault(metric, []).append(row.get("value"))
    return {
        "label": label,
        "provenance": {
            "git_sha": sha,
            "image_id": image_id,
            "image_ref": image,
            "platform": os.environ["DOCKER_PLATFORM"],
            "go_version": os.environ["GO_VERSION"],
            "builder_image_id": os.environ["BUILDER_ID"],
            "host_id": os.environ["HOST_ID"],
            "catalog_sha256": os.environ["CATALOG_SHA"],
            "settings_sha256": os.environ["SETTINGS_SHA"],
            "instrumented": False,
            "cli_digest": cli_digest,
            "toolchain_id": os.environ["TOOLCHAIN_ID"],
            "built_by_this_run": built == "built",
        },
        "correctness": correctness(workloads),
        "workloads": workloads,
        "benchmarks": benches,
        "browser": browser,
        "bundle": bundle(label),
        "system": {},
    }

doc = {
    "schema_version": 1,
    "base": side_doc("base", os.environ["BASE_SHA"], os.environ["BASE_IMAGE"],
                     os.environ["BASE_IMAGE_ID"], os.environ["BASE_CLI_DIGEST"],
                     os.environ["BASE_BUILT"]),
    "candidate": side_doc("candidate", os.environ["CANDIDATE_SHA"], os.environ["CANDIDATE_IMAGE"],
                          os.environ["CANDIDATE_IMAGE_ID"], os.environ["CANDIDATE_CLI_DIGEST"],
                          os.environ["CANDIDATE_BUILT"]),
}
(art / "comparison.json").write_text(json.dumps(doc, indent=2) + "\n")
PY

python3 "$ROOT/scripts/compare-performance.py" \
  --input "$ARTIFACTS/comparison.json" \
  --output "$ARTIFACTS/report.json"
exit $?
