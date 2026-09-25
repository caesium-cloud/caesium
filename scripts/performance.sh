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

log() { printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }
require_cmd() { command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"; }
require_env() { [[ -n "${!1:-}" ]] || die "$1 is required"; }

# Overlay only E3's measurement code on the temporary base checkout. The
# release images are built before this is called; changing their source would
# invalidate the image identity. The two exact candidate-commit blobs define
# one common harness, while each side still measures its own implementation.
prepare_benchmark_harness() {
  python3 - "$@" <<'PY'
import hashlib
import json
import pathlib
import re
import subprocess
import sys

candidate_dir, base_dir, candidate_sha, base_sha, dest, base_image_id, candidate_image_id = sys.argv[1:]
files = (
    "internal/run/owner_benchmark_test.go",
    "internal/run/recovery_benchmark_test.go",
)

def git(root, *args, check=True):
    result = subprocess.run(["git", "-C", root, *args], capture_output=True, check=False)
    if check and result.returncode:
        raise SystemExit(f"benchmark harness: git {' '.join(args)} failed in {root}: {result.stderr.decode(errors='replace').strip()}")
    return result

def sha256(data):
    return hashlib.sha256(data).hexdigest()

def status_paths(root):
    raw = git(root, "status", "--porcelain", "--untracked-files=all", "-z").stdout
    return {entry[3:].decode() for entry in raw.split(b"\0") if entry}

for label, root, expected in (("candidate", candidate_dir, candidate_sha), ("base", base_dir, base_sha)):
    head = git(root, "rev-parse", "HEAD").stdout.decode().strip()
    if head != expected:
        raise SystemExit(f"benchmark harness: {label} checkout HEAD {head} != declared SHA {expected}")
    dirty = status_paths(root)
    if dirty:
        raise SystemExit(f"benchmark harness: {label} checkout is dirty before overlay: {sorted(dirty)}")

overlaid = []
manifest = []
benchmark_names = []
benchmark_function = re.compile(r'^func\s+(Benchmark(?:Owner|Recover)[A-Za-z0-9_]*)\s*\(\s*[A-Za-z_][A-Za-z_0-9]*\s+\*testing\.B\s*\)', re.M)

# Check helper identity before creating any overlay. Benchmarks call helpers
# from other internal/run test files, including owner_state_test.go.
def other_test_files(root, sha):
    paths = git(root, "ls-tree", "-r", "--name-only", sha, "--", "internal/run").stdout.decode().splitlines()
    return sorted(path for path in paths if path.endswith("_test.go") and path not in files)

candidate_helpers = other_test_files(candidate_dir, candidate_sha)
base_helpers = other_test_files(base_dir, base_sha)
if candidate_helpers != base_helpers:
    raise SystemExit("benchmark harness: internal/run test helper path sets differ between base and candidate")
helper_manifest = []
for path in candidate_helpers:
    candidate_blob = git(candidate_dir, "show", f"{candidate_sha}:{path}").stdout
    base_blob = git(base_dir, "show", f"{base_sha}:{path}").stdout
    if candidate_blob != base_blob:
        raise SystemExit(f"benchmark harness: test helper differs between base and candidate: {path}")
    helper_manifest.append({"path": path, "sha256": sha256(candidate_blob)})

for path in files:
    candidate_blob = git(candidate_dir, "show", f"{candidate_sha}:{path}").stdout
    candidate_file = pathlib.Path(candidate_dir, path)
    if not candidate_file.is_file() or candidate_file.read_bytes() != candidate_blob:
        raise SystemExit(f"benchmark harness: candidate file {path} differs from {candidate_sha}")
    found = benchmark_function.findall(candidate_blob.decode())
    if not found:
        raise SystemExit(f"benchmark harness: {path} has no selected benchmark functions")
    benchmark_names.extend(found)
    base_result = git(base_dir, "show", f"{base_sha}:{path}", check=False)
    base_blob = base_result.stdout if base_result.returncode == 0 else None
    changed = base_blob != candidate_blob
    if changed:
        target = pathlib.Path(base_dir, path)
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(candidate_blob)
        overlaid.append(path)
    if pathlib.Path(base_dir, path).read_bytes() != candidate_blob:
        raise SystemExit(f"benchmark harness: base did not receive the exact {candidate_sha}:{path} blob")
    manifest.append({
        "path": path,
        "sha256": sha256(candidate_blob),
        "base_original_sha256": sha256(base_blob) if base_blob is not None else None,
        "overlaid": changed,
    })

# Keep the digest order fixed: benchmark files first, then sorted helper files.
digest = hashlib.sha256()
for entry in manifest + helper_manifest:
    digest.update(entry["path"].encode() + b"\0" + entry["sha256"].encode() + b"\0")

dirty = status_paths(base_dir)
if dirty != set(overlaid):
    raise SystemExit(f"benchmark harness: base changed outside the declared overlay: {sorted(dirty ^ set(overlaid))}")
if len(benchmark_names) != len(set(benchmark_names)):
    raise SystemExit("benchmark harness: duplicate benchmark function names")
doc = {
    "schema_version": 1,
    "base_source_sha": base_sha,
    "candidate_source_sha": candidate_sha,
    "harness_source_sha": candidate_sha,
    "harness_sha256": digest.hexdigest(),
    "benchmark_names": sorted(benchmark_names),
    "base_overlay_paths": overlaid,
    "base_release_image_id": base_image_id,
    "candidate_release_image_id": candidate_image_id,
    "files": manifest,
    "helper_files": helper_manifest,
}
output = pathlib.Path(dest)
output.parent.mkdir(parents=True, exist_ok=True)
output.write_text(json.dumps(doc, indent=2) + "\n")
print(f"benchmark harness: {len(files)} benchmark files and {len(helper_manifest)} matched test helpers from {candidate_sha}; base overlay {overlaid}; manifest {dest}", file=sys.stderr)
PY
}

cleanup_benchmark_harness() {
  python3 - "$@" <<'PY'
import pathlib
import subprocess
import sys

candidate_arg, base_arg, expected_base_sha, expected_candidate_sha = sys.argv[1:]
if not all((candidate_arg, base_arg, expected_base_sha, expected_candidate_sha)):
    raise SystemExit("benchmark cleanup: candidate, base, and both source SHAs must be nonempty")
candidate = pathlib.Path(candidate_arg).resolve(strict=True)
base = pathlib.Path(base_arg).resolve(strict=True)
if candidate == base:
    raise SystemExit("benchmark cleanup: base path resolves to the candidate checkout")
if base == pathlib.Path.cwd().resolve():
    raise SystemExit("benchmark cleanup: base path resolves to the script checkout root")

def git(root, *args, check=True):
    result = subprocess.run(["git", "-C", str(root), *args], capture_output=True, check=False)
    if check and result.returncode:
        raise SystemExit(f"benchmark cleanup: git {' '.join(args)} failed in {root}: {result.stderr.decode(errors='replace').strip()}")
    return result

for label, root, expected in (("candidate", candidate, expected_candidate_sha), ("base", base, expected_base_sha)):
    top = pathlib.Path(git(root, "rev-parse", "--show-toplevel").stdout.decode().strip()).resolve()
    if top != root:
        raise SystemExit(f"benchmark cleanup: {label} path is not its Git worktree root")
    head = git(root, "rev-parse", "HEAD").stdout.decode().strip()
    if head != expected:
        raise SystemExit(f"benchmark cleanup: {label} checkout HEAD {head} != declared SHA {expected}")

candidate_common = pathlib.Path(git(candidate, "rev-parse", "--path-format=absolute", "--git-common-dir").stdout.decode().strip()).resolve()
base_common = pathlib.Path(git(base, "rev-parse", "--path-format=absolute", "--git-common-dir").stdout.decode().strip()).resolve()
base_git_dir = pathlib.Path(git(base, "rev-parse", "--path-format=absolute", "--git-dir").stdout.decode().strip()).resolve()
if candidate_common != base_common or base_git_dir == base_common:
    raise SystemExit("benchmark cleanup: base is not a linked worktree of the candidate repository")
registered = {
    pathlib.Path(line.removeprefix("worktree ")).resolve()
    for line in git(candidate, "worktree", "list", "--porcelain").stdout.decode().splitlines()
    if line.startswith("worktree ")
}
if base not in registered:
    raise SystemExit("benchmark cleanup: base is not a registered linked worktree")

paths = ("internal/run/owner_benchmark_test.go", "internal/run/recovery_benchmark_test.go")
status = git(base, "status", "--porcelain", "--untracked-files=all", "-z").stdout
dirty = {entry[3:].decode() for entry in status.split(b"\0") if entry}
if dirty - set(paths):
    raise SystemExit(f"benchmark cleanup: base has changes outside the overlay: {sorted(dirty - set(paths))}")
if git(base, "diff", "--cached", "--quiet", check=False).returncode != 0:
    raise SystemExit("benchmark cleanup: base has staged changes")
for path in paths:
    target = base / path
    expected = git(candidate, "show", f"{expected_candidate_sha}:{path}").stdout
    if not target.is_file() or target.read_bytes() != expected:
        raise SystemExit(f"benchmark cleanup: overlay {path} differs from the candidate commit")

for path in paths:
    tracked = git(base, "ls-files", "--error-unmatch", "--", path, check=False).returncode == 0
    if tracked:
        git(base, "restore", "--", path)
    else:
        (base / path).unlink()
if git(base, "status", "--porcelain", "--untracked-files=all").stdout:
    raise SystemExit("benchmark cleanup: base checkout is dirty after overlay cleanup")
PY
}

if [[ "${1:-}" == "prepare-bench-harness" ]]; then
  shift
  [[ "$#" -eq 7 ]] || die "prepare-bench-harness needs candidate dir, base dir, candidate SHA, base SHA, manifest path, base image ID, candidate image ID"
  require_cmd git
  require_cmd python3
  prepare_benchmark_harness "$@"
  exit $?
fi

if [[ "${1:-}" == "cleanup-bench-harness" ]]; then
  shift
  [[ "$#" -eq 4 ]] || die "cleanup-bench-harness needs candidate dir, base dir, base SHA, candidate SHA"
  require_cmd git
  require_cmd python3
  cleanup_benchmark_harness "$@"
  exit $?
fi

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

CANDIDATE_REF="${CAESIUM_PERF_CANDIDATE_SHA:-HEAD}"
BASE_REF="${CAESIUM_PERF_BASE_SHA:-}"
[[ -n "$BASE_REF" ]] || die "CAESIUM_PERF_BASE_SHA is required for a live comparison"
# Resolve references before image tags, builds, and provenance are created.
CANDIDATE_SHA="$(git -C "$ROOT" rev-parse --verify --end-of-options "${CANDIDATE_REF}^{commit}" 2>/dev/null)" \
  || die "CAESIUM_PERF_CANDIDATE_SHA is not a commit: $CANDIDATE_REF"
BASE_SHA="$(git -C "$ROOT" rev-parse --verify --end-of-options "${BASE_REF}^{commit}" 2>/dev/null)" \
  || die "CAESIUM_PERF_BASE_SHA is not a commit: $BASE_REF"

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
# Per-SHA builders. just tag=$sha build-release depends on builder with that
# tag, so each release is built by caesium-builder:$sha, not :latest.
# ---------------------------------------------------------------------------
BUILDER_IMAGE="caesiumcloud/caesium-builder:latest"

record_builder() {
  local sha="$1" dest="$2"
  local img="caesiumcloud/caesium-builder:${sha}"
  if ! docker image inspect "$img" >/dev/null 2>&1; then
    log "WARNING: $img is not present; cannot record the builder that produced this SHA"
    printf '%s\n' '{"image_ref":"'"$img"'","image_id":"","go_version":""}' >"$dest"
    return 1
  fi
  local id go
  id="$(docker image inspect --format '{{.Id}}' "$img")"
  go="$(docker run --rm --platform "$DOCKER_PLATFORM" "$img" go version | awk '{print $3}')"
  printf '{"image_ref":"%s","image_id":"%s","go_version":"%s","toolchain_id":"%s@%s"}\n' \
    "$img" "$id" "$go" "$img" "$id" >"$dest"
}

# ---------------------------------------------------------------------------
# Build release-equivalent images (uninstrumented)
# ---------------------------------------------------------------------------
build_release() {
  local sha="$1" dest="$2" src="$3"
  log "building release image $dest from $src at $sha (just tag=$sha build-release)"
  (
    cd "$src"
    CAESIUM_SKIP_IMAGE_BUILD=false just tag="$sha" build-release
  ) >&2 || return 1
  return 0
}

CANDIDATE_BUILT="supplied"
if docker image inspect "$CANDIDATE_IMAGE" >/dev/null 2>&1; then
  log "image $CANDIDATE_IMAGE already present; recording as supplied"
else
  GIT_HEAD="$(git -C "$ROOT" rev-parse HEAD)"
  if [[ -n "$(git -C "$ROOT" status --porcelain)" && "$ALLOW_UNVERIFIED" != "1" ]]; then
    die "refusing to build $CANDIDATE_IMAGE from a dirty working tree; commit or set CAESIUM_PERF_ALLOW_UNVERIFIED_IMAGE=1"
  fi
  if [[ "$CANDIDATE_SHA" != "$GIT_HEAD" && "$ALLOW_UNVERIFIED" != "1" ]]; then
    die "CAESIUM_PERF_CANDIDATE_SHA=$CANDIDATE_SHA but HEAD is $GIT_HEAD"
  fi
  if build_release "$CANDIDATE_SHA" "$CANDIDATE_IMAGE" "$ROOT"; then
    CANDIDATE_BUILT=built
  else
    die "candidate build-release failed for $CANDIDATE_SHA"
  fi
fi

BASE_BUILT="supplied"
if docker image inspect "$BASE_IMAGE" >/dev/null 2>&1; then
  log "image $BASE_IMAGE already present; recording as supplied"
else
  BASE_WORKTREE="$(mktemp -d "${TMPDIR:-/tmp}/caesium-perf-base.XXXXXX")"
  git -C "$ROOT" worktree add --detach "$BASE_WORKTREE" "$BASE_SHA"
  if build_release "$BASE_SHA" "$BASE_IMAGE" "$BASE_WORKTREE"; then
    BASE_BUILT=built
  else
    die "base build-release failed for $BASE_SHA"
  fi
fi

if [[ -z "$BASE_WORKTREE" ]]; then
  BASE_WORKTREE="$(mktemp -d "${TMPDIR:-/tmp}/caesium-perf-base.XXXXXX")"
  git -C "$ROOT" worktree add --detach "$BASE_WORKTREE" "$BASE_SHA"
fi

if [[ "$CANDIDATE_BUILT" != "built" || "$BASE_BUILT" != "built" ]]; then
  if [[ "$ALLOW_UNVERIFIED" != "1" ]]; then
    if [[ "$CANDIDATE_BUILT" != "built" ]]; then
      die "candidate image $CANDIDATE_IMAGE was not built by this run; delete it and rerun, or set CAESIUM_PERF_ALLOW_UNVERIFIED_IMAGE=1"
    fi
    if [[ "$BASE_BUILT" != "built" ]]; then
      die "base image $BASE_IMAGE was not built by this run; delete it and rerun, or set CAESIUM_PERF_ALLOW_UNVERIFIED_IMAGE=1"
    fi
  fi
  log "WARNING: one or both images were supplied rather than built by this run"
fi

record_builder "$CANDIDATE_SHA" "$ARTIFACTS/candidate/builder.json" || true
record_builder "$BASE_SHA" "$ARTIFACTS/base/builder.json" || true
CANDIDATE_GO_VERSION="$(python3 -c "import json,pathlib; print(json.loads(pathlib.Path('$ARTIFACTS/candidate/builder.json').read_text()).get('go_version',''))")"
BASE_GO_VERSION="$(python3 -c "import json,pathlib; print(json.loads(pathlib.Path('$ARTIFACTS/base/builder.json').read_text()).get('go_version',''))")"
CANDIDATE_BUILDER_ID="$(python3 -c "import json,pathlib; print(json.loads(pathlib.Path('$ARTIFACTS/candidate/builder.json').read_text()).get('image_id',''))")"
BASE_BUILDER_ID="$(python3 -c "import json,pathlib; print(json.loads(pathlib.Path('$ARTIFACTS/base/builder.json').read_text()).get('image_id',''))")"
CANDIDATE_TOOLCHAIN="$(python3 -c "import json,pathlib; print(json.loads(pathlib.Path('$ARTIFACTS/candidate/builder.json').read_text()).get('toolchain_id',''))")"
BASE_TOOLCHAIN="$(python3 -c "import json,pathlib; print(json.loads(pathlib.Path('$ARTIFACTS/base/builder.json').read_text()).get('toolchain_id',''))")"

if docker image inspect "caesiumcloud/caesium-builder:${CANDIDATE_SHA}" >/dev/null 2>&1; then
  BUILDER_IMAGE="caesiumcloud/caesium-builder:${CANDIDATE_SHA}"
fi

if [[ "$RUN_BENCH" == "1" || "$RUN_BUNDLE" == "1" ]]; then
  log "ensuring builder-full for hermetic Go benchmarks / UI build"
  just tag="$CANDIDATE_SHA" builder-full >&2
  (cd "$BASE_WORKTREE" && just tag="$BASE_SHA" builder-full) >&2 || die "base builder-full failed"
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
BENCH_HARNESS_MANIFEST="$ARTIFACTS/observations/benchmark-harness.json"
if [[ "$RUN_BENCH" == "1" ]]; then
  # Build/inspect both release images before the temporary base checkout is
  # overlaid. The benchmark output then measures the two source SHAs with the
  # same test code; the overlay is recorded separately from image provenance.
  prepare_benchmark_harness "$ROOT" "$BASE_WORKTREE" "$CANDIDATE_SHA" "$BASE_SHA" \
    "$BENCH_HARNESS_MANIFEST" "$BASE_IMAGE_ID" "$CANDIDATE_IMAGE_ID" \
    || die "benchmark harness preparation failed"
  bash "$ROOT/scripts/performance-benchmarks.sh" "$REPEATS" "$DOCKER_PLATFORM" \
    "$ROOT" "$BASE_WORKTREE" \
    "caesiumcloud/caesium-builder:${CANDIDATE_SHA}-full" \
    "caesiumcloud/caesium-builder:${BASE_SHA}-full" "$ARTIFACTS" \
    || die "benchmark scheduling failed"
  # Remove only the measurement overlay before bundle/browser work uses the
  # base checkout. The manifest retains the exact benchmark provenance.
  cleanup_benchmark_harness "$ROOT" "$BASE_WORKTREE" "$BASE_SHA" "$CANDIDATE_SHA"
fi

# ---------------------------------------------------------------------------
# Bundle size: build UI at each SHA, then the node check (same gzip as ui-ci).
# ---------------------------------------------------------------------------
build_ui_bundle() {
  local src="$1" dest="$2" builder="$3" sha="$4"
  log "building UI at $sha through $builder"
  set +e
  docker run --rm --platform "$DOCKER_PLATFORM" \
    -v "$src:/bld/caesium" -w /bld/caesium/ui \
    "$builder" \
    sh -c 'npm ci --prefer-offline && npm run build' >&2
  local rc=$?
  set -e
  if [[ "$rc" -ne 0 || ! -d "$src/ui/dist/assets" ]]; then
    printf '%s\n' "{\"ok\":false,\"errors\":[\"ui/dist/assets missing after UI build at ${sha} (exit ${rc})\"]}" >"$dest"
    return 0
  fi
  if ! command -v node >/dev/null 2>&1; then
    printf '%s\n' '{"ok":false,"errors":["node is required to run check-bundle-size.mjs"]}' >"$dest"
    return 0
  fi
  set +e
  node "$ROOT/ui/scripts/check-bundle-size.mjs" --dist "$src/ui/dist/assets" --json >"$dest"
  set -e
}

if [[ "$RUN_BUNDLE" == "1" ]]; then
  build_ui_bundle "$ROOT" "$ARTIFACTS/candidate/bundle.json" "caesiumcloud/caesium-builder:${CANDIDATE_SHA}-full" "$CANDIDATE_SHA"
  build_ui_bundle "$BASE_WORKTREE" "$ARTIFACTS/base/bundle.json" "caesiumcloud/caesium-builder:${BASE_SHA}-full" "$BASE_SHA"
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

  # Warm repetitions are interleaved the same way as cold: neither side
  # runs all of its warm samples before the other starts. Each iteration
  # starts a server, discards a warmup, takes one warm sample, optionally
  # runs Playwright, then stops — so host drift is shared.
  r=1
  while [[ "$r" -le "$REPEATS" ]]; do
    if (( r % 2 == 1 )); then order=(base candidate); else order=(candidate base); fi
    for side in "${order[@]}"; do
      image="$BASE_IMAGE"
      [[ "$side" == "candidate" ]] && image="$CANDIDATE_IMAGE"
      start_server "$side" "$image"
      for workload in "${WORKLOAD_LIST[@]}"; do
        workload="${workload// /}"
        log "warm-up (discarded) $side $workload #$r"
        run_workload "$side" "warmup" "$workload" "$r"
      done
      for workload in "${WORKLOAD_LIST[@]}"; do
        workload="${workload// /}"
        log "warm $side $workload #$r"
        run_workload "$side" "warm" "$workload" "$r"
      done
      if [[ "$RUN_BROWSER" == "1" ]]; then
        log "browser performance.spec.ts against $side repeat #$r"
        mkdir -p "$ARTIFACTS/$side"
        set +e
        (
          cd "$ROOT/ui"
          PLAYWRIGHT_BASE_URL="http://127.0.0.1:${PERF_PORT}" \
          CAESIUM_MANUAL_TRIGGER_API_KEY="$API_KEY" \
          CAESIUM_PERF_BROWSER_OUT="$ARTIFACTS/$side/browser.jsonl" \
          npx playwright test e2e/performance.spec.ts --project=default
        )
        brc=$?
        set -e
        printf '%s\n' "$brc" >>"$ARTIFACTS/$side/browser.exit"
        if [[ "$brc" -ne 0 ]]; then
          log "browser spec failed for $side repeat #$r exit=$brc (recorded)"
        fi
      fi
      stop_server "$side"
    done
    r=$((r + 1))
  done
fi

if [[ "$RUN_LOAD" != "1" && "$RUN_BROWSER" == "1" ]]; then
  r=1
  while [[ "$r" -le "$REPEATS" ]]; do
    if (( r % 2 == 1 )); then order=(base candidate); else order=(candidate base); fi
    for side in "${order[@]}"; do
      image="$BASE_IMAGE"
      [[ "$side" == "candidate" ]] && image="$CANDIDATE_IMAGE"
      start_server "$side" "$image"
      log "browser performance.spec.ts against $side repeat #$r"
      mkdir -p "$ARTIFACTS/$side"
      set +e
      (
        cd "$ROOT/ui"
        PLAYWRIGHT_BASE_URL="http://127.0.0.1:${PERF_PORT}" \
        CAESIUM_MANUAL_TRIGGER_API_KEY="$API_KEY" \
        CAESIUM_PERF_BROWSER_OUT="$ARTIFACTS/$side/browser.jsonl" \
        npx playwright test e2e/performance.spec.ts --project=default
      )
      brc=$?
      set -e
      printf '%s\n' "$brc" >>"$ARTIFACTS/$side/browser.exit"
      stop_server "$side"
    done
    r=$((r + 1))
  done
fi

# ---------------------------------------------------------------------------
# Assemble comparison document and run the fail-closed comparator
# ---------------------------------------------------------------------------
export BASE_SHA CANDIDATE_SHA BASE_IMAGE CANDIDATE_IMAGE
export BASE_IMAGE_ID CANDIDATE_IMAGE_ID BASE_CLI_DIGEST CANDIDATE_CLI_DIGEST
export CATALOG_SHA SETTINGS_SHA BASE_BUILT CANDIDATE_BUILT
export BASE_GO_VERSION CANDIDATE_GO_VERSION BASE_BUILDER_ID CANDIDATE_BUILDER_ID
export BASE_TOOLCHAIN CANDIDATE_TOOLCHAIN
export RUN_LOAD RUN_BENCH RUN_BROWSER RUN_BUNDLE
export BENCH_HARNESS_MANIFEST REPEATS
python3 - <<'PY'
import json, os, pathlib, re

art = pathlib.Path(os.environ["ARTIFACTS"])

def load_json(path):
    try:
        return json.loads(pathlib.Path(path).read_text())
    except Exception:
        return None

bench_harness = None
bench_sampling = None
base_compile_exit = None
if os.environ.get("RUN_BENCH") == "1":
    bench_harness = load_json(os.environ["BENCH_HARNESS_MANIFEST"])
    if not isinstance(bench_harness, dict):
        raise SystemExit("benchmark harness manifest is missing or malformed")
    for field, expected in (
        ("base_source_sha", os.environ["BASE_SHA"]),
        ("candidate_source_sha", os.environ["CANDIDATE_SHA"]),
        ("harness_source_sha", os.environ["CANDIDATE_SHA"]),
        ("base_release_image_id", os.environ["BASE_IMAGE_ID"]),
        ("candidate_release_image_id", os.environ["CANDIDATE_IMAGE_ID"]),
    ):
        if bench_harness.get(field) != expected:
            raise SystemExit(f"benchmark harness {field} does not match the release comparison")
    if not bench_harness.get("harness_sha256") or len(bench_harness.get("files") or []) != 2:
        raise SystemExit("benchmark harness manifest lacks the two measured source files")
    names_doc = load_json(art / "observations" / "benchmark-names.json")
    source_files = [
        {"path": entry["path"], "sha256": entry["sha256"]}
        for entry in bench_harness["files"]
    ]
    if not isinstance(names_doc, dict) or names_doc.get("source_files") != source_files or \
            names_doc.get("benchmark_names") != bench_harness.get("benchmark_names"):
        raise SystemExit("benchmark names do not match the exact shared harness source files")
    repeats = int(os.environ["REPEATS"])
    if repeats < 1:
        raise SystemExit("benchmark repeats must be positive")
    compile_output = art / "observations" / "benchmark-base-compile.txt"
    compile_exit = art / "observations" / "benchmark-base-compile.exit"
    if not compile_output.is_file() or not compile_exit.is_file():
        raise SystemExit("base benchmark harness compile preflight evidence is missing")
    raw_exit = compile_exit.read_text().strip()
    if not raw_exit.isdecimal():
        raise SystemExit(f"base benchmark harness compile exit is malformed: {raw_exit!r}")
    base_compile_exit = int(raw_exit)
    repeat_exits = {}
    aggregate_exits = {}
    for label in ("base", "candidate"):
        exit_path = art / label / "bench.txt.exit"
        repeats_path = art / label / "bench.txt.repeats.tsv"
        if not exit_path.is_file() or not repeats_path.is_file():
            raise SystemExit(f"{label} benchmark exit or repeat evidence is missing")
        aggregate = exit_path.read_text().strip()
        if not aggregate.isdecimal():
            raise SystemExit(f"{label} benchmark aggregate exit is malformed: {aggregate!r}")
        aggregate_exits[label] = int(aggregate)
        rows = [line.split("\t") for line in repeats_path.read_text().splitlines()]
        if len(rows) != repeats:
            raise SystemExit(f"{label} benchmark has {len(rows)} repeat exits, want {repeats}")
        statuses = []
        for repeat, row in enumerate(rows, 1):
            if len(row) != 2 or row[0] != str(repeat) or not row[1].isdecimal():
                raise SystemExit(f"{label} benchmark repeat {repeat} evidence is malformed: {row!r}")
            if not (art / label / f"bench-repeat-{repeat}.txt").is_file():
                raise SystemExit(f"{label} benchmark repeat {repeat} raw output is missing")
            statuses.append(int(row[1]))
        first_failure = next((status for status in statuses if status != 0), 0)
        if aggregate_exits[label] != first_failure:
            raise SystemExit(f"{label} aggregate benchmark exit disagrees with repeat exits")
        if first_failure == 0:
            raw = "".join((art / label / f"bench-repeat-{repeat}.txt").read_text()
                          for repeat in range(1, repeats + 1))
            aggregate_path = art / label / "bench.txt"
            if not aggregate_path.is_file() or aggregate_path.read_text() != raw:
                raise SystemExit(f"{label} aggregate benchmark text differs from its raw repeats")
        repeat_exits[label] = statuses
    order_path = art / "observations" / "benchmark-order.tsv"
    if not order_path.is_file():
        raise SystemExit("benchmark order evidence is missing")
    order = [line.split("\t") for line in order_path.read_text().splitlines()]
    expected_order = [
        [str(repeat), label, str(repeat_exits[label][repeat - 1])]
        for repeat in range(1, repeats + 1)
        for label in (("base", "candidate") if repeat % 2 else ("candidate", "base"))
    ]
    if order != expected_order:
        raise SystemExit("benchmark order differs from paired repeat exits")
    bench_sampling = {
        "schema_version": 1,
        "repeats": repeats,
        "base_compile": {
            "exit_code": base_compile_exit,
            "output_path": "observations/benchmark-base-compile.txt",
        },
        "expected_names": names_doc["benchmark_names"],
        "source_files": source_files,
        "settings_sha256": os.environ["SETTINGS_SHA"],
        "order": [
            {"repeat": int(repeat), "side": label, "exit_code": int(status)}
            for repeat, label, status in order
        ],
        "aggregate_exit": aggregate_exits,
    }

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
    return load_json(art / side / "bundle.json") or {}

def browser_series(side):
    browser = {}
    browser_path = art / side / "browser.jsonl"
    if not browser_path.is_file():
        return browser
    for line in browser_path.read_text().splitlines():
        if not line.strip():
            continue
        row = json.loads(line)
        metric = row.get("metric")
        if not metric:
            continue
        route = row.get("route") or ""
        kind = row.get("kind") or "live"
        series_key = f"{route}|{kind}" if route else kind
        browser.setdefault(metric, {}).setdefault(series_key, []).append(row.get("value"))
    return browser

def correctness(label, workloads):
    failures = []
    for name, body in workloads.items():
        for i, sample in enumerate(body.get("samples") or []):
            if sample.get("outcome") != "passed":
                failures.append(f"{name}[{i}] outcome={sample.get('outcome')}")
    bench_exit = art / label / "bench.txt.exit"
    if os.environ.get("RUN_BENCH") == "1" and not bench_exit.is_file():
        failures.append("benchmark exit marker missing")
    elif bench_exit.is_file():
        rc = bench_exit.read_text().strip()
        if rc != "0" and not (label == "base" and base_compile_exit not in (None, 0)):
            failures.append(f"benchmarks exited {rc}")
    browser_exit = art / label / "browser.exit"
    if browser_exit.is_file():
        codes = [c.strip() for c in browser_exit.read_text().splitlines() if c.strip()]
        bad = [c for c in codes if c != "0"]
        if bad:
            failures.append(f"playwright exited {','.join(bad)}")
    bundle_doc = bundle(label)
    if bundle_doc.get("ok") is False:
        failures.extend(str(x) for x in (bundle_doc.get("errors") or ["bundle.ok is false"]))
    return {"ok": not failures, "failures": failures}

def side_doc(label, sha, image, image_id, cli_digest, built, go_version, builder_id, toolchain):
    workloads = {}
    workloads.update(workload_samples(label, "cold"))
    workloads.update(workload_samples(label, "warm"))
    provenance = {
        "git_sha": sha,
        "image_id": image_id,
        "image_ref": image,
        "platform": os.environ["DOCKER_PLATFORM"],
        "go_version": go_version,
        "builder_image_id": builder_id,
        "host_id": os.environ["HOST_ID"],
        "catalog_sha256": os.environ["CATALOG_SHA"],
        "settings_sha256": os.environ["SETTINGS_SHA"],
        "instrumented": False,
        "cli_digest": cli_digest,
        "toolchain_id": toolchain,
        "built_by_this_run": built == "built",
    }
    if bench_harness is not None:
        provenance.update({
            "benchmark_source_git_sha": sha,
            "benchmark_harness_git_sha": bench_harness["harness_source_sha"],
            "benchmark_harness_sha256": bench_harness["harness_sha256"],
            "benchmark_overlay_paths": bench_harness["base_overlay_paths"] if label == "base" else [],
            "benchmark_repeats": bench_sampling["repeats"],
        })
    return {
        "label": label,
        "provenance": provenance,
        "correctness": correctness(label, workloads),
        "workloads": workloads,
        "benchmarks": bench_text(art / label / "bench.txt"),
        "browser": browser_series(label),
        "bundle": bundle(label),
        "system": {},
    }

families = []
if os.environ.get("RUN_LOAD") == "1":
    families.append("workload")
if os.environ.get("RUN_BENCH") == "1":
    families.append("benchmark")
if os.environ.get("RUN_BROWSER") == "1":
    families.append("browser")
if os.environ.get("RUN_BUNDLE") == "1":
    families.append("bundle")

doc = {
    "schema_version": 1,
    "required_families": families,
    "benchmark_harness": bench_harness,
    "benchmark_sampling": bench_sampling,
    "required_browser_series": [
        "browser.route_readiness_ms./jobs.live",
        "browser.route_readiness_ms./triggers.live",
        "browser.route_readiness_ms./system.live",
        "browser.route_readiness_ms./jobdefs.live",
        "browser.action_to_render_ms.live",
        "browser.long_session_heap_bytes.live",
    ] if os.environ.get("RUN_BROWSER") == "1" else [],
    "base": side_doc("base", os.environ["BASE_SHA"], os.environ["BASE_IMAGE"],
                     os.environ["BASE_IMAGE_ID"], os.environ["BASE_CLI_DIGEST"],
                     os.environ["BASE_BUILT"], os.environ["BASE_GO_VERSION"],
                     os.environ["BASE_BUILDER_ID"], os.environ["BASE_TOOLCHAIN"]),
    "candidate": side_doc("candidate", os.environ["CANDIDATE_SHA"], os.environ["CANDIDATE_IMAGE"],
                          os.environ["CANDIDATE_IMAGE_ID"], os.environ["CANDIDATE_CLI_DIGEST"],
                          os.environ["CANDIDATE_BUILT"], os.environ["CANDIDATE_GO_VERSION"],
                          os.environ["CANDIDATE_BUILDER_ID"], os.environ["CANDIDATE_TOOLCHAIN"]),
}
(art / "comparison.json").write_text(json.dumps(doc, indent=2) + "\n")
PY

python3 "$ROOT/scripts/compare-performance.py" \
  --input "$ARTIFACTS/comparison.json" \
  --output "$ARTIFACTS/report.json"
exit $?
