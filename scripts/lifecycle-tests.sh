#!/usr/bin/env bash
# F4 single-node previous-release upgrade qualification: host controller.
#
# Owns every image, volume, network and container the qualification touches and
# drives the integration-tagged runner in test/lifecycle/standalone_test.go once
# per phase. The runner never starts or inspects a container; this script writes
# each container fact it needs into the artifacts directory, so a fact that was
# never observed is reported BLOCKED rather than passed.
#
# Shape mirrors scripts/robustness.sh (B1): host controller + a Go runner that
# is compiled explicitly with -tags=integration inside the builder image,
# because the precompiled ./test binary does not contain subpackage tests.
#
#   CANDIDATE_SHA=$(git rev-parse HEAD)
#   CAESIUM_LIFECYCLE_ID="lifecycle-$(uuidgen | tr '[:upper:]' '[:lower:]' | tr -d - | cut -c1-12)" \
#   CAESIUM_LIFECYCLE_ARTIFACTS="$(mktemp -d)" \
#   CAESIUM_LIFECYCLE_PREV_IMAGE="caesiumcloud/caesium:v0.1.0" \
#   CAESIUM_LIFECYCLE_CANDIDATE_IMAGE="caesiumcloud/caesium:$CANDIDATE_SHA" \
#     bash scripts/lifecycle-tests.sh
#
# Leave the candidate image unbuilt: the harness builds it itself (`just
# tag="$CANDIDATE_SHA" build-release`) from this checkout when
# CAESIUM_LIFECYCLE_CANDIDATE_IMAGE is absent, which is what binds
# candidate_sha to the image actually qualified (see candidate-image-provenance
# below). Pre-building it yourself makes the image "supplied", not
# "built-by-this-run", and the run is BLOCKED unless you also set
# CAESIUM_LIFECYCLE_ALLOW_UNVERIFIED_IMAGE=1.
#
# Every resource this script creates carries CAESIUM_LIFECYCLE_ID in its name,
# and teardown removes only those. It never touches a pre-existing container,
# volume or network, never publishes a host port, and never bind-mounts the
# dqlite data directory (a bind mount does not inherit the release image's
# 10001:10001 ownership; a fresh named volume does). Task containers the fixture
# jobs launch are matched by a per-invocation ownership token (see OWNER_TOKEN),
# never by a substring of the lifecycle id, and only once THIS invocation has
# established ownership of its named resources.
#
# The qualification record ($CAESIUM_LIFECYCLE_ARTIFACTS/qualification.json) says
# "pass" only when every expected case recorded a pass (or a recorded outcome)
# under THIS invocation's lifecycle id AND every phase returned 0. A run that
# aborts leaves the "incomplete" placeholder written at startup, never a stale
# pass from an earlier invocation.
#
# Optional:
#   CAESIUM_LIFECYCLE_ALLOW_UNVERIFIED_IMAGE=1
#       proceed (and record it) when the candidate image's provenance cannot be
#       established — a pre-existing/supplied image, or a build from a dirty
#       working tree. Without it such a run is BLOCKED, not qualified.
#   CAESIUM_LIFECYCLE_KEEP=1   leave owned resources in place for debugging.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

log() { printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }
die() { log "ERROR: $*"; exit 1; }

require_cmd() { command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"; }
require_env() { [[ -n "${!1:-}" ]] || die "$1 is required"; }

require_cmd docker
require_cmd python3

require_env CAESIUM_LIFECYCLE_ID
require_env CAESIUM_LIFECYCLE_ARTIFACTS
require_env CAESIUM_LIFECYCLE_PREV_IMAGE
require_env CAESIUM_LIFECYCLE_CANDIDATE_IMAGE

ID="$CAESIUM_LIFECYCLE_ID"
if [[ ! "$ID" =~ ^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$ ]]; then
  die "CAESIUM_LIFECYCLE_ID must be a lowercase DNS-1123 name of at most 40 characters, got '$ID'"
fi
# Must match suffixOf() in test/lifecycle/standalone_test.go: the fixture job
# aliases and the marker in every task container's command derive from it.
SUFFIX="$(printf '%s' "$ID" | tr '[:upper:]' '[:lower:]' | tr -cd 'a-z0-9' | tail -c 12)"
[[ -n "$SUFFIX" ]] || die "CAESIUM_LIFECYCLE_ID has no alphanumeric characters"

PREV_IMAGE="$CAESIUM_LIFECYCLE_PREV_IMAGE"
CANDIDATE_IMAGE="$CAESIUM_LIFECYCLE_CANDIDATE_IMAGE"
case "$PREV_IMAGE" in
  *:latest|*:latest@*) die "the 'latest' tag is not a release (last pushed 2021, amd64-only); pin a released tag" ;;
  *:*) ;;
  *) die "CAESIUM_LIFECYCLE_PREV_IMAGE must be tagged, got '$PREV_IMAGE'" ;;
esac

CANDIDATE_SHA="${CANDIDATE_SHA:-${CANDIDATE_IMAGE##*:}}"
[[ -n "$CANDIDATE_SHA" && "$CANDIDATE_SHA" != "$CANDIDATE_IMAGE" ]] \
  || die "CANDIDATE_SHA is required (export it, or use caesiumcloud/caesium:<sha> as CAESIUM_LIFECYCLE_CANDIDATE_IMAGE)"

PAIR="${CAESIUM_LIFECYCLE_PAIR:-v0.1.0-to-candidate}"
MANUAL_KEY="${CAESIUM_LIFECYCLE_MANUAL_KEY:-caesium-lifecycle-manual-key}"
SOCK="${CAESIUM_SOCK:-/var/run/docker.sock}"
[[ -S "$SOCK" ]] || die "docker socket $SOCK is not a socket; the fixture jobs cannot launch task containers"

case "$(uname -m)" in
  x86_64|amd64) HOST_ARCH=amd64 ;;
  aarch64|arm64) HOST_ARCH=arm64 ;;
  *) HOST_ARCH="$(uname -m)" ;;
esac
PLATFORM="${CAESIUM_PLATFORM:-linux/$HOST_ARCH}"
TASK_IMAGE="${CAESIUM_LIFECYCLE_TASK_IMAGE:-alpine:3.23}"

mkdir -p "$CAESIUM_LIFECYCLE_ARTIFACTS"
ARTIFACTS="$(cd "$CAESIUM_LIFECYCLE_ARTIFACTS" && pwd)"
export CAESIUM_LIFECYCLE_ARTIFACTS="$ARTIFACTS"

# Records from an earlier invocation are not evidence for this one. Purge the
# directories this script owns before writing anything, so a phase that never
# runs cannot inherit a passing case file. (Every surviving record is ALSO
# checked against this invocation's lifecycle id when the record is assembled.)
rm -rf "$ARTIFACTS/cases" "$ARTIFACTS/observations" "$ARTIFACTS/logs" "$ARTIFACTS/cli" \
       "$ARTIFACTS/qualification.json" "$ARTIFACTS/fixture.json" "$ARTIFACTS/lifecycle.test"
mkdir -p "$ARTIFACTS/cases" "$ARTIFACTS/observations" "$ARTIFACTS/logs" "$ARTIFACTS/cli"
cp "$ROOT/test/lifecycle/versions.json" "$ARTIFACTS/versions.json"

# An aborted run must leave a NON-passing record behind, never nothing (which a
# consumer could confuse with an older passing file) and never a stale pass.
PLACEHOLDER_ID="$ID" PLACEHOLDER_PAIR="$PAIR" PLACEHOLDER_SHA="$CANDIDATE_SHA" \
PLACEHOLDER_DEST="$ARTIFACTS/qualification.json" python3 - <<'PY'
import datetime, json, os, pathlib
pathlib.Path(os.environ["PLACEHOLDER_DEST"]).write_text(json.dumps({
    "schema_version": 1,
    "kind": "caesium-lifecycle-qualification",
    "pair": os.environ["PLACEHOLDER_PAIR"],
    "candidate_sha": os.environ["PLACEHOLDER_SHA"],
    "lifecycle_id": os.environ["PLACEHOLDER_ID"],
    "started_at": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
    "result": "incomplete",
    "detail": "scripts/lifecycle-tests.sh started but did not finish; this placeholder is "
              "overwritten only when the full record is assembled",
}, indent=2) + "\n")
PY

NET="${ID}-net"
ISO_NET="${ID}-iso-net"
VOL_DATA="${ID}-data"
VOL_ISO="${ID}-iso-data"
VOL_FAILTX="${ID}-failtx"
VOL_ROLLBACK="${ID}-rollback"
VOL_SHARDS="${ID}-shards"
CTR_PREV="${ID}-prev"
CTR_ISO="${ID}-iso"
CTR_CAND="${ID}-cand"
CTR_READDR="${ID}-readdr"
CTR_FAILTX="${ID}-failtx"
CTR_ROLLBACK="${ID}-rollback"
CTR_SHARDS="${ID}-shards"
SERVER_UID="${CAESIUM_LIFECYCLE_SERVER_USER:-10001:10001}"
START_EPOCH="$(date -u +%s)"

# Ownership token for THIS invocation.
#
# The fixture's task containers are launched by the server, so we cannot label
# them; the only handle is a marker in their command. The lifecycle id alone is
# not a safe handle: SUFFIX is the last 12 characters of the sanitized id, so
# two ids can share it, and a substring match would let "abc" claim "abcdef"'s
# live containers. The nonce makes the token unique per invocation, and cleanup
# matches it with a delimiter-anchored pattern (see TASK_MARKER_PATTERN) rather
# than `grep -F`, so neither a shared suffix nor a prefix can collide.
NONCE="$(python3 -c 'import secrets; print(secrets.token_hex(6))')"
[[ -n "$NONCE" ]] || die "could not mint an ownership nonce"
OWNER_TOKEN="${SUFFIX}-${NONCE}"
TASK_MARKER="caesium-lc-owner=${OWNER_TOKEN}"
TASK_MARKER_PATTERN="(^|[^A-Za-z0-9_=-])${TASK_MARKER}([^A-Za-z0-9_=-]|\$)"
# Set to 1 only once every name this run claims has been checked and created.
# Until then cleanup must not sweep ANY task container: a run that aborts in
# refuse_existing has claimed nothing, and the containers carrying a similar
# marker belong to the invocation that is still using them.
OWNERSHIP_ESTABLISHED=0

# --------------------------------------------------------------------------
# Version matrix
# --------------------------------------------------------------------------
matrix_field() {
  CAESIUM_LIFECYCLE_MATRIX_PAIR="$PAIR" python3 - "$ARTIFACTS/versions.json" "$1" <<'PY'
import json, os, sys
path, expr = sys.argv[1], sys.argv[2]
doc = json.load(open(path))
want = os.environ["CAESIUM_LIFECYCLE_MATRIX_PAIR"]
pair = next((p for p in doc["pairs"] if p["id"] == want), None)
if pair is None:
    raise SystemExit(f"versions.json has no pair {want!r}")
cur = pair
for part in expr.split("."):
    cur = cur[part]
if isinstance(cur, (dict, list)):
    print(json.dumps(cur))
else:
    print(cur)
PY
}

PINNED_PREV_IMAGE="$(matrix_field previous.image)"
[[ "$PREV_IMAGE" == "$PINNED_PREV_IMAGE" ]] \
  || die "CAESIUM_LIFECYCLE_PREV_IMAGE ($PREV_IMAGE) is not the pinned image for pair $PAIR ($PINNED_PREV_IMAGE)"
PREV_RELEASE="$(matrix_field previous.release)"
SHARDS="$(matrix_field standalone.database_shards)"
DATA_DIR="$(matrix_field standalone.data_directory)"
NODE_ADDRESS="$(matrix_field standalone.node_address)"
ALT_NODE_ADDRESS="$(matrix_field standalone.alternate_node_address)"
ALT_SHARDS="$(matrix_field 'standalone.transitions' | python3 -c 'import json,sys; print(next(t["shards"] for t in json.load(sys.stdin) if t["id"]=="shard-count-change"))')"

# --------------------------------------------------------------------------
# Teardown: only resources carrying $ID.
# --------------------------------------------------------------------------
OWNED_CONTAINERS=()
OWNED_VOLUMES=()
OWNED_NETWORKS=()

own_container() { OWNED_CONTAINERS+=("$1"); }
own_volume() { OWNED_VOLUMES+=("$1"); }
own_network() { OWNED_NETWORKS+=("$1"); }

cleanup() {
  local status=$?
  set +e
  if [[ "${CAESIUM_LIFECYCLE_KEEP:-}" == "1" ]]; then
    log "CAESIUM_LIFECYCLE_KEEP=1; leaving owned resources in place"
    exit "$status"
  fi
  # Task containers the fixture jobs launched carry THIS invocation's ownership
  # token in their command. Sweep them only once this invocation actually owns
  # its named resources — otherwise a refused start (an id that is already
  # active) would kill the live containers of the invocation that owns it.
  if [[ "$OWNERSHIP_ESTABLISHED" == "1" ]]; then
    local stragglers
    stragglers="$(docker ps -a --no-trunc --format '{{.ID}}\t{{.Command}}' 2>/dev/null \
      | grep -E "$TASK_MARKER_PATTERN" | cut -f1)"
    if [[ -n "$stragglers" ]]; then
      log "cleanup: removing $(printf '%s\n' "$stragglers" | wc -l | tr -d ' ') task container(s) owned by $OWNER_TOKEN"
      # shellcheck disable=SC2086
      docker rm -f $stragglers >/dev/null 2>&1
    fi
  else
    log "cleanup: this invocation never established ownership; leaving every task container alone"
  fi
  local name
  for name in "${OWNED_CONTAINERS[@]:-}"; do
    [[ -n "$name" ]] || continue
    # Defense in depth against the same race start_server's ordering guards
    # against: only remove a named server container if it still carries THIS
    # invocation's ownership label (or the label cannot be read at all,
    # meaning the container is already gone). A container that raced us for
    # the name belongs to whichever invocation's `docker run` actually
    # created it, and carries that invocation's own label instead.
    local owner_label
    owner_label="$(docker inspect "$name" --format '{{index .Config.Labels "caesium-lc-owner"}}' 2>/dev/null || true)"
    if [[ -z "$owner_label" || "$owner_label" == "$OWNER_TOKEN" ]]; then
      docker rm -f "$name" >/dev/null 2>&1
    else
      log "cleanup: $name is labeled for owner '$owner_label', not ours ($OWNER_TOKEN); leaving it alone"
    fi
  done
  for name in "${OWNED_VOLUMES[@]:-}"; do
    [[ -n "$name" ]] || continue
    # Same defense in depth as the container loop above: create_owned_volume
    # only ever puts a name in OWNED_VOLUMES after confirming the label, but
    # verify again here rather than trusting that held.
    local vol_owner_label
    vol_owner_label="$(docker volume inspect "$name" --format '{{index .Labels "caesium-lc-owner"}}' 2>/dev/null || true)"
    if [[ -z "$vol_owner_label" || "$vol_owner_label" == "$OWNER_TOKEN" ]]; then
      docker volume rm -f "$name" >/dev/null 2>&1
    else
      log "cleanup: volume $name is labeled for owner '$vol_owner_label', not ours ($OWNER_TOKEN); leaving it alone"
    fi
  done
  for name in "${OWNED_NETWORKS[@]:-}"; do
    [[ -n "$name" ]] && docker network rm "$name" >/dev/null 2>&1
  done
  exit "$status"
}
trap cleanup EXIT INT TERM

refuse_existing() {
  local kind="$1" name="$2" existing
  case "$kind" in
    container) existing="$(docker ps -a --format '{{.Names}}' | grep -Fx "$name" || true)" ;;
    volume) existing="$(docker volume ls --format '{{.Name}}' | grep -Fx "$name" || true)" ;;
    network) existing="$(docker network ls --format '{{.Name}}' | grep -Fx "$name" || true)" ;;
  esac
  [[ -z "$existing" ]] || die "$kind $name already exists; refusing to claim or delete it"
}

# create_owned_volume claims a named volume for THIS invocation, the same way
# start_server claims a named container: `docker volume create` is idempotent
# — it succeeds on an EXISTING volume without creating anything and without
# signaling that it adopted rather than created — so a name collision after
# the `refuse_existing` preflight (another invocation's `docker volume create`
# won the name first) would otherwise be silently adopted, mounted writable
# and later deleted out from under the invocation that actually owns it. The
# ownership label can only be set at creation time, so it is the one thing
# that tells the two cases apart: create with THIS invocation's label, then
# read it back and refuse (without registering ownership, mounting or
# removing anything) unless it matches exactly.
create_owned_volume() {
  local name="$1" label
  docker volume create --label "$TASK_MARKER" "$name" >/dev/null
  label="$(docker volume inspect "$name" --format '{{index .Labels "caesium-lc-owner"}}' 2>/dev/null || true)"
  [[ "$label" == "$OWNER_TOKEN" ]] \
    || die "volume $name already exists and is not labeled for this invocation (label='${label:-<none>}', expected '$OWNER_TOKEN'); refusing to adopt, mount or delete it"
  own_volume "$name"
}

# --------------------------------------------------------------------------
# Claim every name this run uses, BEFORE any image work, and create the
# resources that need no image. Until this block completes, cleanup sweeps no
# task container at all (OWNERSHIP_ESTABLISHED above).
# --------------------------------------------------------------------------
for n in "$NET" "$ISO_NET"; do refuse_existing network "$n"; done
for v in "$VOL_DATA" "$VOL_ISO" "$VOL_FAILTX" "$VOL_ROLLBACK" "$VOL_SHARDS"; do refuse_existing volume "$v"; done
for c in "$CTR_PREV" "$CTR_ISO" "$CTR_CAND" "$CTR_READDR" "$CTR_FAILTX" "$CTR_ROLLBACK" "$CTR_SHARDS"; do
  refuse_existing container "$c"
done

docker network create "$NET" >/dev/null; own_network "$NET"
docker network create "$ISO_NET" >/dev/null; own_network "$ISO_NET"
create_owned_volume "$VOL_DATA"
create_owned_volume "$VOL_ISO"
OWNERSHIP_ESTABLISHED=1
log "ownership established: token $OWNER_TOKEN, resources named $ID-*"

# --------------------------------------------------------------------------
# Case records the shell owns (the Go runner writes its own).
# --------------------------------------------------------------------------
shell_case() {
  local name="$1" status="$2" duration="$3" detail="$4"
  CASE_NAME="$name" CASE_STATUS="$status" CASE_DURATION="$duration" CASE_DETAIL="$detail" \
  CASE_ID="$ID" CASE_DIR="$ARTIFACTS/cases" python3 - <<'PY'
import json, os, pathlib, re
name = os.environ["CASE_NAME"]
rec = {
    "name": name,
    "phase": "host-controller",
    # Stamped so the finalizer can reject a record from another invocation.
    "lifecycle_id": os.environ["CASE_ID"],
    "status": os.environ["CASE_STATUS"],
    "duration_seconds": float(os.environ["CASE_DURATION"]),
    "detail": os.environ["CASE_DETAIL"],
}
path = pathlib.Path(os.environ["CASE_DIR"]) / (re.sub(r"[^A-Za-z0-9_-]", "-", name) + ".json")
path.write_text(json.dumps(rec, indent=2) + "\n")
PY
  log "case $name: $status ($detail)"
}

# --------------------------------------------------------------------------
# Images
# --------------------------------------------------------------------------
log "pair=$PAIR candidate_sha=$CANDIDATE_SHA lifecycle_id=$ID artifacts=$ARTIFACTS platform=$PLATFORM"

# --------------------------------------------------------------------------
# Candidate provenance.
#
# The record names a candidate_sha, so something has to bind that SHA to the
# image actually qualified. There is no product version surface yet (F1
# prerequisite 3: the binary reports no build SHA), so the binding is external:
# this run either BUILT the image from a clean checkout at that SHA — recorded
# with the resulting image id — or it did not, in which case the image is
# supplied/unverified and the qualification is BLOCKED unless the operator
# explicitly overrides, which is itself recorded.
# --------------------------------------------------------------------------
provenance_start="$(date -u +%s)"
GIT_HEAD=""
GIT_DIRTY=false
GIT_DIFFSTAT=""
if command -v git >/dev/null 2>&1 && git -C "$ROOT" rev-parse --git-dir >/dev/null 2>&1; then
  GIT_HEAD="$(git -C "$ROOT" rev-parse HEAD)"
  if [[ -n "$(git -C "$ROOT" status --porcelain)" ]]; then
    GIT_DIRTY=true
    GIT_DIFFSTAT="$(git -C "$ROOT" status --porcelain | head -60)"
  fi
fi

ALLOW_UNVERIFIED="${CAESIUM_LIFECYCLE_ALLOW_UNVERIFIED_IMAGE:-0}"
CANDIDATE_BUILT_HERE=false
PROVENANCE_REASONS=""
unverified_because() { PROVENANCE_REASONS="${PROVENANCE_REASONS}${1}"$'\n'; }
provenance_summary() { printf '%s' "$PROVENANCE_REASONS" | tr '\n' ';' | sed 's/;/; /g'; }

if docker image inspect "$CANDIDATE_IMAGE" >/dev/null 2>&1; then
  unverified_because "$CANDIDATE_IMAGE already existed on this host, so this run did not build it: nothing binds the image to $CANDIDATE_SHA"
else
  if [[ -z "$GIT_HEAD" ]]; then
    unverified_because "no git checkout is available at $ROOT, so a built image cannot be bound to a commit"
  else
    if [[ "$GIT_DIRTY" == true ]]; then
      if [[ "$ALLOW_UNVERIFIED" != "1" ]]; then
        log "dirty working tree:"
        printf '%s\n' "$GIT_DIFFSTAT"
        die "refusing to build $CANDIDATE_IMAGE from a dirty working tree: the image would be tagged with the clean SHA $GIT_HEAD it was not built from. Commit the changes, or set CAESIUM_LIFECYCLE_ALLOW_UNVERIFIED_IMAGE=1 to record an explicitly non-qualifying run."
      fi
      unverified_because "built from a DIRTY working tree at $GIT_HEAD"
    fi
    if [[ "$CANDIDATE_SHA" != "$GIT_HEAD" ]]; then
      unverified_because "the candidate tag names $CANDIDATE_SHA but this checkout's HEAD is $GIT_HEAD"
    fi
  fi
  command -v just >/dev/null 2>&1 \
    || die "candidate image $CANDIDATE_IMAGE is absent and 'just' is not on PATH; build it with: CAESIUM_SKIP_IMAGE_BUILD=false just tag=$CANDIDATE_SHA build-release"
  log "candidate image $CANDIDATE_IMAGE absent; building it from this checkout"
  CAESIUM_SKIP_IMAGE_BUILD=false just tag="$CANDIDATE_SHA" build-release
  CANDIDATE_BUILT_HERE=true
fi
docker image inspect "$CANDIDATE_IMAGE" >/dev/null || die "candidate image $CANDIDATE_IMAGE is not present"
CANDIDATE_IMAGE_ID="$(docker image inspect --format '{{.Id}}' "$CANDIDATE_IMAGE")"
CANDIDATE_ARCH="$(docker image inspect --format '{{.Architecture}}' "$CANDIDATE_IMAGE")"
CANDIDATE_CREATED="$(docker image inspect --format '{{.Created}}' "$CANDIDATE_IMAGE")"
docker image inspect "$CANDIDATE_IMAGE" >"$ARTIFACTS/candidate-image.json"

PROVENANCE_VERIFIED=true
[[ -z "$PROVENANCE_REASONS" ]] || PROVENANCE_VERIFIED=false
PROVENANCE_OVERRIDE=false
[[ "$PROVENANCE_VERIFIED" == true || "$ALLOW_UNVERIFIED" != "1" ]] || PROVENANCE_OVERRIDE=true

PROV_BUILT="$CANDIDATE_BUILT_HERE" PROV_VERIFIED="$PROVENANCE_VERIFIED" \
PROV_OVERRIDE="$PROVENANCE_OVERRIDE" PROV_SHA="$CANDIDATE_SHA" PROV_GIT_HEAD="$GIT_HEAD" \
PROV_DIRTY="$GIT_DIRTY" PROV_DIFFSTAT="$GIT_DIFFSTAT" PROV_IMAGE="$CANDIDATE_IMAGE" \
PROV_IMAGE_ID="$CANDIDATE_IMAGE_ID" PROV_CREATED="$CANDIDATE_CREATED" \
PROV_REASONS="$PROVENANCE_REASONS" \
PROV_DEST="$ARTIFACTS/observations/candidate-provenance.json" python3 - <<'PY'
import json, os, pathlib
reasons = [r for r in os.environ["PROV_REASONS"].splitlines() if r.strip()]
built = os.environ["PROV_BUILT"] == "true"
verified = os.environ["PROV_VERIFIED"] == "true"
pathlib.Path(os.environ["PROV_DEST"]).write_text(json.dumps({
    "provenance": "built-by-this-run" if (built and verified) else "supplied/unverified",
    "built_by_this_run": built,
    "verified": verified,
    "override": os.environ["PROV_OVERRIDE"] == "true",
    "override_env": "CAESIUM_LIFECYCLE_ALLOW_UNVERIFIED_IMAGE",
    "candidate_sha": os.environ["PROV_SHA"],
    "git_head": os.environ["PROV_GIT_HEAD"],
    "dirty": os.environ["PROV_DIRTY"] == "true",
    "dirty_status": [l for l in os.environ["PROV_DIFFSTAT"].splitlines() if l.strip()],
    "image_ref": os.environ["PROV_IMAGE"],
    "image_id": os.environ["PROV_IMAGE_ID"],
    "image_created": os.environ["PROV_CREATED"],
    "unverified_reasons": reasons,
}, indent=2) + "\n")
PY

if [[ "$PROVENANCE_VERIFIED" == true ]]; then
  shell_case "candidate-image-provenance" "pass" "$(( $(date -u +%s) - provenance_start ))" \
    "this run built $CANDIDATE_IMAGE ($CANDIDATE_IMAGE_ID) from the clean checkout at $GIT_HEAD, so candidate_sha binds to the image qualified"
elif [[ "$PROVENANCE_OVERRIDE" == true ]]; then
  shell_case "candidate-image-provenance" "pass" "$(( $(date -u +%s) - provenance_start ))" \
    "PROVENANCE UNVERIFIED, overridden by CAESIUM_LIFECYCLE_ALLOW_UNVERIFIED_IMAGE=1: $(provenance_summary) image $CANDIDATE_IMAGE_ID is NOT bound to $CANDIDATE_SHA"
  log "WARNING: candidate provenance is unverified and was explicitly overridden; the record says so"
else
  shell_case "candidate-image-provenance" "blocked" "$(( $(date -u +%s) - provenance_start ))" \
    "candidate provenance cannot be established: $(provenance_summary) nothing binds $CANDIDATE_IMAGE ($CANDIDATE_IMAGE_ID) to candidate_sha $CANDIDATE_SHA. Delete the image and let this script build it, or set CAESIUM_LIFECYCLE_ALLOW_UNVERIFIED_IMAGE=1 to record an explicitly non-qualifying run."
  die "candidate image provenance is unverified; refusing to qualify $CANDIDATE_SHA"
fi

log "pulling pinned previous release $PREV_IMAGE"
docker pull --platform "$PLATFORM" "$PREV_IMAGE" >/dev/null || die "docker pull $PREV_IMAGE failed"
docker image inspect "$PREV_IMAGE" >"$ARTIFACTS/previous-image.json"
PREV_IMAGE_ID="$(docker image inspect --format '{{.Id}}' "$PREV_IMAGE")"
PREV_ARCH="$(docker image inspect --format '{{.Architecture}}' "$PREV_IMAGE")"
PREV_REPO_DIGESTS="$(docker image inspect --format '{{join .RepoDigests ","}}' "$PREV_IMAGE")"

digest_start="$(date -u +%s)"
PREV_DIGEST="$(CAESIUM_LIFECYCLE_MATRIX_PAIR="$PAIR" \
  CAESIUM_LIFECYCLE_MATRIX="$ARTIFACTS/versions.json" \
  CAESIUM_LIFECYCLE_REPO_DIGESTS="$PREV_REPO_DIGESTS" \
  CAESIUM_LIFECYCLE_PLATFORM="$PLATFORM" python3 - <<'PY'
import json, os, sys
doc = json.load(open(os.environ["CAESIUM_LIFECYCLE_MATRIX"]))
pair = next(p for p in doc["pairs"] if p["id"] == os.environ["CAESIUM_LIFECYCLE_MATRIX_PAIR"])
pinned = pair["previous"]["digests"]
acceptable = {pinned["index"]}
per_arch = pinned.get(os.environ["CAESIUM_LIFECYCLE_PLATFORM"])
if per_arch:
    acceptable.add(per_arch)
raw = os.environ["CAESIUM_LIFECYCLE_REPO_DIGESTS"]
observed = {d.split("@", 1)[1] for d in raw.split(",") if "@" in d}
if not observed:
    print("the pulled previous-release image carries no repo digest", file=sys.stderr)
    raise SystemExit(1)
matched = observed & acceptable
if not matched:
    print(f"resolved digest(s) {sorted(observed)} match none of the pinned digests {sorted(acceptable)}",
          file=sys.stderr)
    raise SystemExit(1)
print(sorted(matched)[0])
PY
)" || die "the pulled $PREV_IMAGE is not the pinned release image"
[[ -n "$PREV_DIGEST" ]] || die "the pulled $PREV_IMAGE is not the pinned release image"
shell_case "previous-release-digest-pinned" "pass" "$(( $(date -u +%s) - digest_start ))" \
  "$PREV_IMAGE resolved to $PREV_REPO_DIGESTS, matching pinned $PREV_DIGEST, arch=$PREV_ARCH"

[[ "$PREV_ARCH" == "$HOST_ARCH" ]] \
  || die "previous-release image architecture $PREV_ARCH does not match host $HOST_ARCH"
[[ "$CANDIDATE_ARCH" == "$HOST_ARCH" ]] \
  || die "candidate image architecture $CANDIDATE_ARCH does not match host $HOST_ARCH"

docker pull --platform "$PLATFORM" "$TASK_IMAGE" >/dev/null || die "docker pull $TASK_IMAGE failed"

# --------------------------------------------------------------------------
# Runner: compile ./test/lifecycle explicitly with -tags=integration.
# --------------------------------------------------------------------------
BUILDER_IMAGE="${CAESIUM_LIFECYCLE_BUILDER_IMAGE:-caesiumcloud/caesium-builder:$CANDIDATE_SHA}"
if ! docker image inspect "$BUILDER_IMAGE" >/dev/null 2>&1; then
  BUILDER_IMAGE="caesiumcloud/caesium-builder:latest"
fi
docker image inspect "$BUILDER_IMAGE" >/dev/null \
  || die "no builder image (tried caesiumcloud/caesium-builder:$CANDIDATE_SHA and :latest); run 'just builder' first"

compile_start="$(date -u +%s)"
log "compiling ./test/lifecycle with -tags=integration in $BUILDER_IMAGE"
docker run --rm --platform "$PLATFORM" \
  -v "$ROOT":/bld/caesium \
  -v "$ARTIFACTS":/artifacts \
  -w /bld/caesium \
  -e CGO_ENABLED=0 \
  -e GOFLAGS=-buildvcs=false \
  "$BUILDER_IMAGE" \
  go test -tags=integration -c ./test/lifecycle -o /artifacts/lifecycle.test \
  || die "compiling ./test/lifecycle failed"
[[ -x "$ARTIFACTS/lifecycle.test" ]] || die "the compiled runner is missing at $ARTIFACTS/lifecycle.test"
shell_case "runner-compiled-with-integration-tag" "pass" "$(( $(date -u +%s) - compile_start ))" \
  "go test -tags=integration -c ./test/lifecycle in $BUILDER_IMAGE"

# --------------------------------------------------------------------------
# Docker socket access for the fixture jobs' task containers.
# --------------------------------------------------------------------------
SOCK_GID="$(docker run --rm -v "$SOCK":/var/run/docker.sock "$TASK_IMAGE" stat -c '%g' /var/run/docker.sock 2>/dev/null || true)"
if [[ -z "$SOCK_GID" ]]; then
  die "could not determine the docker socket's group inside a container; refusing to guess"
fi
SOCKET_MODE="group-add:$SOCK_GID"
log "docker socket access: --user $SERVER_UID --group-add $SOCK_GID (identical on both sides)"

# --------------------------------------------------------------------------
# The shared env block. Both sides of every supported transition get exactly
# this; the candidate-only variables are inert on the previous release and no
# fixture depends on them.
#
# ONE setting differs by side, deliberately and recorded: the run-queue DEQUEUER
# is off while the previous release seeds. cmd/start/start.go ORs
# CAESIUM_RUN_QUEUE_ENABLED and CAESIUM_RUN_QUEUE_DEQUEUER_ENABLED when it
# decides whether to launch the dequeuer, so both have to be false to stop it.
# The queued-work fixture only exists if the row survives the transition, and a
# row can only survive it if nothing drains it — while a run that is still in
# flight when the server stops is never finalized again (local execution mode
# has no restart recovery), so holding the slot open with a live run would leave
# the queue permanently blocked and `job apply` permanently refused on that job.
# Admission to the run queue does not consult this flag (internal/run/store.go's
# ConcurrencyStrategyQueue branch), so the fixture is created exactly as F1
# describes: a trigger admitted to the queue while its predecessor is running.
# Draining it is the candidate's job, which is what assertion 5 measures.
server_env_args() {
  local shards="${1:-$SHARDS}" node_address="${2:-$NODE_ADDRESS}" dequeuer="${3:-true}"
  printf '%s\n' \
    "-e" "CAESIUM_LOG_LEVEL=debug" \
    "-e" "CAESIUM_DATABASE_PATH=$DATA_DIR" \
    "-e" "CAESIUM_DATABASE_SHARDS=$shards" \
    "-e" "CAESIUM_DATABASE_CONSOLE_ENABLED=true" \
    "-e" "CAESIUM_NODE_ADDRESS=$node_address" \
    "-e" "CAESIUM_MANUAL_TRIGGER_API_KEY=$MANUAL_KEY" \
    "-e" "CAESIUM_RUN_QUEUE_ENABLED=$dequeuer" \
    "-e" "CAESIUM_RUN_QUEUE_DEQUEUER_ENABLED=$dequeuer" \
    "-e" "CAESIUM_RUN_QUEUE_DEQUEUE_INTERVAL=500ms" \
    "-e" "DOCKER_HOST=unix:///var/run/docker.sock"
}

start_server() {
  local name="$1" image="$2" volume="$3" network="$4" shards="$5" node_address="$6" dequeuer="${7:-true}"
  local env_args=()
  while IFS= read -r line; do env_args+=("$line"); done < <(server_env_args "$shards" "$node_address" "$dequeuer")
  # Register ownership only once `docker run` actually names the container: if
  # another invocation's container has already claimed $name (a TOCTOU race
  # against the initial refuse_existing preflight, which ran once for every
  # name this invocation will ever use, long before some of them are actually
  # started), `docker run --name` fails and must NOT cause cleanup to
  # `rm -f` the container that won the race. The --label is a second,
  # independent guard: even a container that IS in OWNED_CONTAINERS is only
  # ever removed by cleanup if it still carries THIS invocation's ownership
  # token (see cleanup()).
  docker run -d --name "$name" \
    --platform "$PLATFORM" \
    --network "$network" \
    --user "$SERVER_UID" \
    --group-add "$SOCK_GID" \
    --label "$TASK_MARKER" \
    -v "$volume":"$DATA_DIR" \
    -v "$SOCK":/var/run/docker.sock \
    "${env_args[@]}" \
    "$image" start >/dev/null
  own_container "$name"
}

capture_state() {
  local name="$1" dest="$2"
  docker inspect "$name" --format \
    '{"status":"{{.State.Status}}","exit_code":{{.State.ExitCode}},"restart_count":{{.RestartCount}},"oom_killed":{{.State.OOMKilled}},"started_at":"{{.State.StartedAt}}","finished_at":"{{.State.FinishedAt}}","image_id":"{{.Image}}"}' \
    >"$dest"
}

# evaluate_graceful_stop reads a capture_state record and says whether the
# container actually shut down on its own within the `docker stop -t
# <timeout>` grace period, rather than being SIGKILLed when the timeout
# expired. `docker stop` exits 0 in BOTH cases, so its return code alone
# proves nothing: a container that ignores SIGTERM still reports exit 0 from
# `docker stop`, then shows up here with exit_code 137 (128+SIGKILL) and a
# stop duration at (or past) the timeout. Prints exactly one of
# pass|fail|blocked; the caller never derives the verdict any other way, so a
# synthetic capture_state record exercises the identical logic a real run
# uses.
evaluate_graceful_stop() {
  local state_file="$1" stop_seconds="$2" timeout="$3"
  STATE_FILE="$state_file" STOP_SECONDS="$stop_seconds" STOP_TIMEOUT="$timeout" python3 - <<'PY'
import json, os, sys

try:
    with open(os.environ["STATE_FILE"]) as f:
        state = json.load(f)
    stop_seconds = float(os.environ["STOP_SECONDS"])
    timeout = float(os.environ["STOP_TIMEOUT"])
except Exception as exc:
    print(f"could not read stop state: {exc}", file=sys.stderr)
    print("blocked")
    sys.exit(0)

reasons = []
if state.get("status") == "running":
    reasons.append("container is still running")
if state.get("exit_code") != 0:
    reasons.append(f"exit_code={state.get('exit_code')} (137 means SIGKILLed by the docker stop timeout)")
if state.get("oom_killed"):
    reasons.append("oom_killed=true")
if stop_seconds >= timeout:
    reasons.append(f"docker stop took {stop_seconds}s, which reached its {timeout}s timeout")

if reasons:
    print("; ".join(reasons), file=sys.stderr)
    print("fail")
else:
    print("pass")
PY
}

capture_logs() {
  local name="$1" dest="$2"
  docker logs "$name" >"$dest" 2>&1 || true
}

# wait_for_log blocks until a line appears in a container's log, so a snapshot
# taken afterwards cannot race the line the assertions look for.
#
# The match is a bash pattern on a captured string, never `docker logs | grep -q`:
# under `set -o pipefail` a matching `grep -q` exits as soon as it finds the
# needle, `docker logs` then dies of SIGPIPE, and the pipeline reports 141 — so
# the needle being present made the check report ABSENT. That cost 120s and
# emitted "the candidate did not log ..." for a line the log demonstrably had.
wait_for_log() {
  local name="$1" needle="$2" timeout="${3:-120}" i logs
  for ((i = 0; i < timeout; i++)); do
    logs="$(docker logs "$name" 2>&1 || true)"
    if [[ "$logs" == *"$needle"* ]]; then
      return 0
    fi
    if [[ "$(docker inspect "$name" --format '{{.State.Status}}' 2>/dev/null || echo gone)" != "running" ]]; then
      return 1
    fi
    sleep 1
  done
  return 1
}

isolation_start="$(date -u +%s)"
log "isolation: starting a second, independently named instance on its own network and volume"
start_server "$CTR_ISO" "$PREV_IMAGE" "$VOL_ISO" "$ISO_NET" "$SHARDS" "$NODE_ADDRESS" false
start_server "$CTR_PREV" "$PREV_IMAGE" "$VOL_DATA" "$NET" "$SHARDS" "$NODE_ADDRESS" false

run_phase() {
  # run_phase <phase> <test name> <image> <base url> <network> [extra docker args...]
  local phase="$1" test_name="$2" image="$3" base_url="$4" network="$5"
  shift 5
  local cli_dir
  case "$image" in
    "$PREV_IMAGE") cli_dir="$ARTIFACTS/cli/previous" ;;
    *) cli_dir="$ARTIFACTS/cli/candidate" ;;
  esac
  docker run --rm \
    --platform "$PLATFORM" \
    --network "$network" \
    --user 0:0 \
    --entrypoint /artifacts/lifecycle.test \
    -v "$ARTIFACTS":/artifacts \
    -v "$cli_dir":/cli:ro \
    -e CAESIUM_LIFECYCLE_PHASE="$phase" \
    -e CAESIUM_LIFECYCLE_ID="$ID" \
    -e CAESIUM_LIFECYCLE_PAIR="$PAIR" \
    -e CAESIUM_LIFECYCLE_ARTIFACTS=/artifacts \
    -e CAESIUM_LIFECYCLE_BASE_URL="$base_url" \
    -e CAESIUM_LIFECYCLE_TASK_MARKER="$TASK_MARKER" \
    -e CAESIUM_CLI_PATH=/cli/caesium \
    -e CAESIUM_MANUAL_TRIGGER_API_KEY="$MANUAL_KEY" \
    "$@" \
    "$image" -test.v -test.count=1 -test.timeout 20m -test.run "^${test_name}\$" \
    2>&1 | tee -a "$ARTIFACTS/logs/runner-${phase}.log"
  return "${PIPESTATUS[0]}"
}

extract_cli() {
  local image="$1" dest="$2" ctr
  mkdir -p "$dest"
  ctr="$(docker create --platform "$PLATFORM" "$image" true)"
  docker cp "$ctr":/bin/caesium "$dest/caesium" >/dev/null
  docker rm -f "$ctr" >/dev/null 2>&1 || true
  chmod +x "$dest/caesium"
}

log "extracting each side's own CLI with docker cp"
extract_cli "$PREV_IMAGE" "$ARTIFACTS/cli/previous"
extract_cli "$CANDIDATE_IMAGE" "$ARTIFACTS/cli/candidate"

# Phase rc ledger. Every phase's exit status is folded into the qualification
# record: a phase that failed or never ran can never leave a passing record
# behind, whatever the case files happen to contain.
PHASE_RCS=""
phase_rc() { PHASE_RCS="${PHASE_RCS}${1}=${2}"$'\n'; }

# Self-check: the runner's own guard against an unobservable recorded outcome.
# It touches no server, so it runs before anything can be observed.
SELFCHECK_RC=0
run_phase "selfcheck" "TestLifecycleObservationValidation" "$CANDIDATE_IMAGE" "http://unused.invalid:8080" "$NET" \
  || SELFCHECK_RC=$?
phase_rc selfcheck "$SELFCHECK_RC"
[[ "$SELFCHECK_RC" -eq 0 ]] || die "the runner's observation-completeness self-check failed (rc=$SELFCHECK_RC)"

# The isolation proof: both instances run at once and neither network can
# resolve the other's container name.
if ! run_phase "probe" "TestLifecycleProbe" "$PREV_IMAGE" "http://${CTR_ISO}:8080" "$ISO_NET" \
  -e CAESIUM_LIFECYCLE_PROBE_NAME=isolation-second-instance; then
  phase_rc probe-isolation-second-instance 1
  die "the isolation instance could not be probed"
fi
phase_rc probe-isolation-second-instance 0
if ! run_phase "probe" "TestLifecycleProbe" "$PREV_IMAGE" "http://${CTR_PREV}:8080" "$NET" \
  -e CAESIUM_LIFECYCLE_PROBE_NAME=isolation-primary-instance; then
  phase_rc probe-isolation-primary-instance 1
  die "the primary instance could not be probed"
fi
phase_rc probe-isolation-primary-instance 0
if docker run --rm --network "$NET" "$TASK_IMAGE" sh -c "nc -z -w 3 $CTR_ISO 8080" >/dev/null 2>&1; then
  die "isolation failed: a container on $NET reached $CTR_ISO"
fi
ISOLATION_OK="$(python3 - "$ARTIFACTS/observations/probe-isolation-primary-instance.json" \
                         "$ARTIFACTS/observations/probe-isolation-second-instance.json" <<'PY'
import json, sys
ok = all(json.load(open(p)).get("healthy") for p in sys.argv[1:])
print("yes" if ok else "no")
PY
)"
[[ "$ISOLATION_OK" == "yes" ]] || die "isolation failed: the two concurrent instances were not both healthy"
shell_case "two-instances-coexist" "pass" "$(( $(date -u +%s) - isolation_start ))" \
  "$CTR_PREV on $NET/$VOL_DATA and $CTR_ISO on $ISO_NET/$VOL_ISO were healthy at the same time; $NET cannot resolve $CTR_ISO"
log "isolation proven; removing the second instance"
docker rm -f "$CTR_ISO" >/dev/null 2>&1 || true
docker volume rm -f "$VOL_ISO" >/dev/null 2>&1 || true
docker network rm "$ISO_NET" >/dev/null 2>&1 || true

# --------------------------------------------------------------------------
# Phase: seed the previous release.
# --------------------------------------------------------------------------
seed_start="$(date -u +%s)"
log "seeding the retained-state fixture through $PREV_RELEASE's own public surface"
if ! run_phase "seed" "TestLifecycleSeedPreviousRelease" "$PREV_IMAGE" "http://${CTR_PREV}:8080" "$NET"; then
  phase_rc seed 1
  capture_logs "$CTR_PREV" "$ARTIFACTS/logs/previous.log"
  die "the seed phase failed; see $ARTIFACTS/logs/runner-seed.log and $ARTIFACTS/logs/previous.log"
fi
phase_rc seed 0
capture_logs "$CTR_PREV" "$ARTIFACTS/logs/previous.log"
capture_state "$CTR_PREV" "$ARTIFACTS/observations/previous-state.json"
log "seed complete in $(( $(date -u +%s) - seed_start ))s"

# --------------------------------------------------------------------------
# Stop the previous release with its full grace period, then branch the volume.
# --------------------------------------------------------------------------
STOP_TIMEOUT=60
stop_start="$(date -u +%s)"
log "docker stop -t $STOP_TIMEOUT $CTR_PREV (30s shutdown grace period must be able to run)"
docker stop -t "$STOP_TIMEOUT" "$CTR_PREV" >/dev/null
STOP_SECONDS="$(( $(date -u +%s) - stop_start ))"
capture_logs "$CTR_PREV" "$ARTIFACTS/logs/previous-after-stop.log"
STOPPED_STATE_FILE="$ARTIFACTS/observations/previous-stopped-state.json"
capture_state "$CTR_PREV" "$STOPPED_STATE_FILE"
PREVIOUS_STOPPED_AT="$(docker inspect "$CTR_PREV" --format '{{.State.FinishedAt}}')"
# `docker stop` returns 0 whether the process exited on its own or Docker had
# to SIGKILL it once the timeout elapsed — its exit status alone cannot tell
# these apart, so the verdict is read back from the state docker actually
# observed (see evaluate_graceful_stop).
STOP_VERDICT="$(evaluate_graceful_stop "$STOPPED_STATE_FILE" "$STOP_SECONDS" "$STOP_TIMEOUT" 2>"$ARTIFACTS/logs/previous-stop-verdict.log")"
shell_case "previous-release-stopped-gracefully" "$STOP_VERDICT" "$STOP_SECONDS" \
  "docker stop -t ${STOP_TIMEOUT} returned after ${STOP_SECONDS}s; container finished at $PREVIOUS_STOPPED_AT; state=$(cat "$STOPPED_STATE_FILE" 2>/dev/null || echo unavailable); $(cat "$ARTIFACTS/logs/previous-stop-verdict.log" 2>/dev/null || true)"

copy_volume() {
  local src="$1" dst="$2"
  refuse_existing volume "$dst"
  create_owned_volume "$dst"
  docker run --rm --user 0:0 \
    -v "$src":/from:ro -v "$dst":/to \
    -e OWNER="$SERVER_UID" \
    "$TASK_IMAGE" sh -ec 'cp -a /from/. /to/ && chown "$OWNER" /to'
}

log "branching the stopped volume for the transitions that must not contaminate the main case"
copy_volume "$VOL_DATA" "$VOL_FAILTX"

# --------------------------------------------------------------------------
# Phase: upgrade — the candidate on the SAME volume.
# --------------------------------------------------------------------------
upgrade_start="$(date -u +%s)"
log "starting the candidate on the retained volume $VOL_DATA"
start_server "$CTR_CAND" "$CANDIDATE_IMAGE" "$VOL_DATA" "$NET" "$SHARDS" "$NODE_ADDRESS"
wait_for_log "$CTR_CAND" "migrating database" 120 \
  || log "WARNING: the candidate did not log 'migrating database' within 120s; the upgrade case will report it"
capture_logs "$CTR_CAND" "$ARTIFACTS/logs/candidate.log"
capture_state "$CTR_CAND" "$ARTIFACTS/observations/candidate-state.json"
OBSERVED_IMAGE_ID="$(docker inspect "$CTR_CAND" --format '{{.Image}}')"
CAESIUM_LIFECYCLE_OBS_DIR="$ARTIFACTS/observations" \
CAESIUM_LIFECYCLE_EXPECTED_IMAGE_ID="$CANDIDATE_IMAGE_ID" \
CAESIUM_LIFECYCLE_OBSERVED_IMAGE_ID="$OBSERVED_IMAGE_ID" \
CAESIUM_LIFECYCLE_IMAGE_REF="$CANDIDATE_IMAGE" python3 - <<'PY'
import json, os, pathlib
out = pathlib.Path(os.environ["CAESIUM_LIFECYCLE_OBS_DIR"]) / "candidate-identity.json"
out.write_text(json.dumps({
    "expected_image_id": os.environ["CAESIUM_LIFECYCLE_EXPECTED_IMAGE_ID"],
    "observed_image_id": os.environ["CAESIUM_LIFECYCLE_OBSERVED_IMAGE_ID"],
    "image_ref": os.environ["CAESIUM_LIFECYCLE_IMAGE_REF"],
}, indent=2) + "\n")
PY

UPGRADE_RC=0
run_phase "upgrade" "TestLifecycleUpgradeToCandidate" "$CANDIDATE_IMAGE" "http://${CTR_CAND}:8080" "$NET" \
  -e CAESIUM_LIFECYCLE_PREVIOUS_STOPPED_AT="$PREVIOUS_STOPPED_AT" || UPGRADE_RC=$?
phase_rc upgrade "$UPGRADE_RC"
capture_logs "$CTR_CAND" "$ARTIFACTS/logs/candidate.log"
capture_state "$CTR_CAND" "$ARTIFACTS/observations/candidate-state-after.json"
CAND_STATUS_AFTER="$(docker inspect "$CTR_CAND" --format '{{.State.Status}}')"
CAND_EXIT_AFTER="$(docker inspect "$CTR_CAND" --format '{{.State.ExitCode}}')"
CAND_RESTARTS_AFTER="$(docker inspect "$CTR_CAND" --format '{{.RestartCount}}')"
if [[ "$CAND_STATUS_AFTER" == "running" && "$CAND_EXIT_AFTER" == "0" && "$CAND_RESTARTS_AFTER" == "0" ]]; then
  shell_case "candidate-never-exited-nonzero" "pass" "$(( $(date -u +%s) - upgrade_start ))" \
    "after the upgrade phase the candidate container is $CAND_STATUS_AFTER, exit $CAND_EXIT_AFTER, $CAND_RESTARTS_AFTER restarts"
else
  shell_case "candidate-never-exited-nonzero" "fail" "$(( $(date -u +%s) - upgrade_start ))" \
    "after the upgrade phase the candidate container is $CAND_STATUS_AFTER, exit $CAND_EXIT_AFTER, $CAND_RESTARTS_AFTER restarts"
fi
log "upgrade phase finished rc=$UPGRADE_RC in $(( $(date -u +%s) - upgrade_start ))s"

# --------------------------------------------------------------------------
# Phase: supported re-address (PR #536), on the SAME volume.
# --------------------------------------------------------------------------
log "stopping the candidate before the re-address transition"
docker stop -t 60 "$CTR_CAND" >/dev/null
capture_logs "$CTR_CAND" "$ARTIFACTS/logs/candidate-after-stop.log"

log "branching the migrated volume for the recorded-outcome cases"
copy_volume "$VOL_DATA" "$VOL_ROLLBACK"
copy_volume "$VOL_DATA" "$VOL_SHARDS"

readdress_start="$(date -u +%s)"
log "restarting the candidate on $VOL_DATA at $ALT_NODE_ADDRESS (PR #536's supported re-address)"
start_server "$CTR_READDR" "$CANDIDATE_IMAGE" "$VOL_DATA" "$NET" "$SHARDS" "$ALT_NODE_ADDRESS"
READDRESS_RC=0
run_phase "readdress" "TestLifecycleCandidateAddressChange" "$CANDIDATE_IMAGE" "http://${CTR_READDR}:8080" "$NET" \
  -e CAESIUM_LIFECYCLE_EXPECT_NODE_ADDRESS="$ALT_NODE_ADDRESS" \
  -e CAESIUM_LIFECYCLE_DATA_DIR=/volume \
  -v "$VOL_DATA":/volume:ro || READDRESS_RC=$?
phase_rc readdress "$READDRESS_RC"
capture_logs "$CTR_READDR" "$ARTIFACTS/logs/candidate-readdress.log"
capture_state "$CTR_READDR" "$ARTIFACTS/observations/candidate-readdress-state.json"
log "readdress phase finished rc=$READDRESS_RC in $(( $(date -u +%s) - readdress_start ))s"
docker stop -t 60 "$CTR_READDR" >/dev/null 2>&1 || true

# --------------------------------------------------------------------------
# Required failing transition: the pinned previous release, re-addressed,
# on its OWN copy of the volume so it cannot disturb the main case.
# --------------------------------------------------------------------------
# record_container_outcome writes what was ACTUALLY observed, and records every
# observation that failed. A swallowed `docker inspect`/`docker logs` error used
# to look identical to a real "exited 0 with no output": the runner now rejects
# any record whose observation is incomplete (validateContainerOutcome), so a
# recorded-outcome case built on one is blocked, never reported.
record_container_outcome() {
  local name="$1" image="$2" volume="$3" envdesc="$4" dest="$5"
  local status exit_code log_tail errors="" log_captured=true complete=true
  local note
  if ! status="$(docker inspect "$name" --format '{{.State.Status}}' 2>&1)" || [[ -z "$status" ]]; then
    errors="${errors}docker inspect .State.Status failed for $name: ${status:-no output}"$'\n'
    status="unknown"
  fi
  if ! exit_code="$(docker inspect "$name" --format '{{.State.ExitCode}}' 2>&1)" \
     || [[ ! "$exit_code" =~ ^-?[0-9]+$ ]]; then
    errors="${errors}docker inspect .State.ExitCode failed for $name: ${exit_code:-no output}"$'\n'
    exit_code="-1"
  fi
  if ! log_tail="$(docker logs --tail 200 "$name" 2>&1)"; then
    errors="${errors}docker logs failed for $name: ${log_tail:-no output}"$'\n'
    log_tail=""
    log_captured=false
  fi
  [[ -z "$errors" ]] || complete=false
  if [[ "$complete" != true ]]; then
    note="$(printf '%s' "$errors" | tr '\n' ';')"
    log "WARNING: the outcome of $name could not be fully observed: $note"
  fi
  CO_NAME="$name" CO_IMAGE="$image" CO_VOLUME="$volume" CO_ENV="$envdesc" \
  CO_STATUS="$status" CO_EXIT="$exit_code" CO_LOG="$log_tail" CO_DEST="$dest" \
  CO_ERRORS="$errors" CO_COMPLETE="$complete" CO_LOG_CAPTURED="$log_captured" python3 - <<'PY'
import datetime, json, os, pathlib
rec = {
    "name": os.environ["CO_NAME"],
    "image": os.environ["CO_IMAGE"],
    "volume": os.environ["CO_VOLUME"],
    "env": os.environ["CO_ENV"],
    "exit_code": int(os.environ["CO_EXIT"]),
    "status": os.environ["CO_STATUS"],
    "started": os.environ["CO_STATUS"] == "running",
    "log_tail": os.environ["CO_LOG"],
    "observed_at": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
    "observation_complete": os.environ["CO_COMPLETE"] == "true",
    "observation_errors": [e for e in os.environ["CO_ERRORS"].splitlines() if e.strip()],
    "log_captured": os.environ["CO_LOG_CAPTURED"] == "true",
}
pathlib.Path(os.environ["CO_DEST"]).write_text(json.dumps(rec, indent=2) + "\n")
PY
}

PROBE_FAILTX_RC=0
PROBE_ROLLBACK_RC=0
PROBE_SHARDS_RC=0
log "unsupported transition: $PREV_RELEASE on its own copy of the volume at $ALT_NODE_ADDRESS"
start_server "$CTR_FAILTX" "$PREV_IMAGE" "$VOL_FAILTX" "$NET" "$SHARDS" "$ALT_NODE_ADDRESS"
# The previous release has no address reconciliation: it is expected to exit.
# Give it a bounded window and record whatever it actually did.
for _ in $(seq 1 30); do
  [[ "$(docker inspect "$CTR_FAILTX" --format '{{.State.Status}}')" == "running" ]] || break
  sleep 1
done
capture_logs "$CTR_FAILTX" "$ARTIFACTS/logs/previous-readdress.log"
record_container_outcome "$CTR_FAILTX" "$PREV_IMAGE" "$VOL_FAILTX" \
  "CAESIUM_NODE_ADDRESS=$ALT_NODE_ADDRESS" "$ARTIFACTS/observations/previous-readdress.json"
run_phase "probe" "TestLifecycleProbe" "$CANDIDATE_IMAGE" "http://${CTR_FAILTX}:8080" "$NET" \
  -e CAESIUM_LIFECYCLE_PROBE_NAME=previous-readdress \
  -e CAESIUM_LIFECYCLE_PROBE_DEADLINE_SECONDS=20 || PROBE_FAILTX_RC=$?
phase_rc probe-previous-readdress "$PROBE_FAILTX_RC"
docker rm -f "$CTR_FAILTX" >/dev/null 2>&1 || true

# --------------------------------------------------------------------------
# Recorded-outcome cases, each on its own copy of the volume so ordering
# cannot contaminate the main case. No expectation is set for either.
# --------------------------------------------------------------------------
log "recorded outcome: $PREV_RELEASE restarted on a COPY of the candidate-migrated volume (rollback)"
start_server "$CTR_ROLLBACK" "$PREV_IMAGE" "$VOL_ROLLBACK" "$NET" "$SHARDS" "$NODE_ADDRESS"
run_phase "probe" "TestLifecycleProbe" "$CANDIDATE_IMAGE" "http://${CTR_ROLLBACK}:8080" "$NET" \
  -e CAESIUM_LIFECYCLE_PROBE_NAME=rollback || PROBE_ROLLBACK_RC=$?
phase_rc probe-rollback "$PROBE_ROLLBACK_RC"
capture_logs "$CTR_ROLLBACK" "$ARTIFACTS/logs/rollback.log"
record_container_outcome "$CTR_ROLLBACK" "$PREV_IMAGE" "$VOL_ROLLBACK" \
  "CAESIUM_DATABASE_SHARDS=$SHARDS" "$ARTIFACTS/observations/rollback.json"
docker rm -f "$CTR_ROLLBACK" >/dev/null 2>&1 || true

log "recorded outcome: the candidate on a COPY of the volume with CAESIUM_DATABASE_SHARDS=$ALT_SHARDS"
start_server "$CTR_SHARDS" "$CANDIDATE_IMAGE" "$VOL_SHARDS" "$NET" "$ALT_SHARDS" "$NODE_ADDRESS"
run_phase "probe" "TestLifecycleProbe" "$CANDIDATE_IMAGE" "http://${CTR_SHARDS}:8080" "$NET" \
  -e CAESIUM_LIFECYCLE_PROBE_NAME=shards || PROBE_SHARDS_RC=$?
phase_rc probe-shards "$PROBE_SHARDS_RC"
capture_logs "$CTR_SHARDS" "$ARTIFACTS/logs/shards.log"
record_container_outcome "$CTR_SHARDS" "$CANDIDATE_IMAGE" "$VOL_SHARDS" \
  "CAESIUM_DATABASE_SHARDS=$ALT_SHARDS" "$ARTIFACTS/observations/shards.json"
docker rm -f "$CTR_SHARDS" >/dev/null 2>&1 || true

# --------------------------------------------------------------------------
# Phase: judge the transitions.
# --------------------------------------------------------------------------
OUTCOMES_RC=0
run_phase "outcomes" "TestLifecycleTransitionOutcomes" "$CANDIDATE_IMAGE" "http://${CTR_CAND}:8080" "$NET" \
  || OUTCOMES_RC=$?
phase_rc outcomes "$OUTCOMES_RC"

# --------------------------------------------------------------------------
# Qualification record.
#
# The complete expected case set. A qualification is a claim about ALL of
# these: a case with no record means the phase that owns it never got far
# enough to record it, which is blocked, not absent. Adding a case here without
# producing it — or producing one without listing it — fails the run, which is
# the point: the record can no longer say "pass" while a whole phase is missing.
# --------------------------------------------------------------------------
EXPECTED_CASES="previous-release-digest-pinned
candidate-image-provenance
runner-compiled-with-integration-tag
observation-validation-self-check
two-instances-coexist
seed-previous-release
previous-release-stopped-gracefully
assert1-candidate-healthy-and-migrated
assert2-schema-migrated-additively
assert3-recorded-identities-readable
assert4-event-replay-from-explicit-cursor
assert5-queued-row-reaches-a-started-run
assert6-export-relints-and-diffs-clean
assert7-serving-build-is-the-candidate
recorded-in-flight-run-after-upgrade
candidate-never-exited-nonzero
supported-candidate-readdress
unsupported-previous-release-readdress
recorded-rollback-previous-release-on-migrated-volume
recorded-shard-count-change"

FINISH_EPOCH="$(date -u +%s)"
CLI_PREV_SHA="$(python3 -c '
import hashlib, sys
print(hashlib.sha256(open(sys.argv[1], "rb").read()).hexdigest())
' "$ARTIFACTS/cli/previous/caesium")"
CLI_CAND_SHA="$(python3 -c '
import hashlib, sys
print(hashlib.sha256(open(sys.argv[1], "rb").read()).hexdigest())
' "$ARTIFACTS/cli/candidate/caesium")"

set +e
CAESIUM_LIFECYCLE_QUAL_ENV="$(server_env_args | paste -sd' ' -)" \
QUAL_ARTIFACTS="$ARTIFACTS" \
QUAL_PAIR="$PAIR" \
QUAL_SHA="$CANDIDATE_SHA" \
QUAL_ID="$ID" \
QUAL_START="$START_EPOCH" \
QUAL_FINISH="$FINISH_EPOCH" \
QUAL_HOST_ARCH="$HOST_ARCH" \
QUAL_PREV_IMAGE="$PREV_IMAGE" \
QUAL_PREV_IMAGE_ID="$PREV_IMAGE_ID" \
QUAL_PREV_DIGESTS="$PREV_REPO_DIGESTS" \
QUAL_PREV_ARCH="$PREV_ARCH" \
QUAL_CAND_IMAGE="$CANDIDATE_IMAGE" \
QUAL_CAND_IMAGE_ID="$CANDIDATE_IMAGE_ID" \
QUAL_CAND_ARCH="$CANDIDATE_ARCH" \
QUAL_BUILDER_IMAGE="$BUILDER_IMAGE" \
QUAL_CLI_PREV_SHA="$CLI_PREV_SHA" \
QUAL_CLI_CAND_SHA="$CLI_CAND_SHA" \
QUAL_SOCKET_MODE="$SOCKET_MODE" \
QUAL_SERVER_UID="$SERVER_UID" \
QUAL_SHARDS="$SHARDS" \
QUAL_EXPECTED_CASES="$EXPECTED_CASES" \
QUAL_PHASES="$PHASE_RCS" \
QUAL_OWNER_TOKEN="$OWNER_TOKEN" \
python3 - <<'PY'
import datetime, json, os, pathlib

art = pathlib.Path(os.environ["QUAL_ARTIFACTS"])
matrix = json.loads((art / "versions.json").read_text())
pair = next(p for p in matrix["pairs"] if p["id"] == os.environ["QUAL_PAIR"])
lifecycle_id = os.environ["QUAL_ID"]

# Every phase's exit status. A phase that failed, or that never ran and so
# never reported one, fails the qualification on its own — independently of
# what the case files happen to say.
phases, malformed_phase_lines = {}, []
for line in os.environ["QUAL_PHASES"].splitlines():
    line = line.strip()
    if not line:
        continue
    name, _, rc = line.partition("=")
    try:
        phases[name] = int(rc)
    except ValueError:
        malformed_phase_lines.append(line)
failed_phases = sorted(n for n, rc in phases.items() if rc != 0)

expected = [n for n in (l.strip() for l in os.environ["QUAL_EXPECTED_CASES"].splitlines()) if n]

# Case records, keyed by name, each validated against THIS invocation's
# lifecycle id so a record written by an earlier run can never be counted.
by_name, stale = {}, []
case_dir = art / "cases"
for path in sorted(case_dir.glob("*.json")):
    try:
        rec = json.loads(path.read_text())
    except Exception as exc:  # a case record that cannot be read is not a pass
        rec = {"name": path.stem, "status": "blocked", "duration_seconds": 0.0,
               "detail": f"case record unreadable: {exc}"}
    name = rec.get("name") or path.stem
    got_id = rec.get("lifecycle_id")
    if got_id != lifecycle_id:
        stale.append(f"{name} (lifecycle_id={got_id!r})")
        rec = dict(rec, status="blocked", duration_seconds=rec.get("duration_seconds", 0.0),
                   detail=f"case record carries lifecycle_id {got_id!r}, not this run's "
                          f"{lifecycle_id!r}: it is not evidence for this qualification")
    by_name[name] = rec

missing = [n for n in expected if n not in by_name]
for name in missing:
    by_name[name] = {
        "name": name, "phase": "never-recorded", "lifecycle_id": lifecycle_id,
        "status": "blocked", "duration_seconds": 0.0,
        "detail": "expected case produced no record: the phase that owns it did not run, "
                  "or did not reach the point of recording it",
    }
unexpected = sorted(n for n in by_name if n not in expected)
cases = [by_name[n] for n in expected] + [by_name[n] for n in unexpected]

# recorded-outcome cases carry no pre-judged expectation, so their OUTCOME does
# not decide the qualification — but they must exist and be conclusive: a
# missing or blocked one is synthesized/marked above and fails here.
failed = [c["name"] for c in cases if c.get("status") not in ("pass", "recorded-outcome")]

delta_path = art / "observations" / "schema-delta.json"
delta = json.loads(delta_path.read_text()) if delta_path.exists() else None

prov_path = art / "observations" / "candidate-provenance.json"
provenance = json.loads(prov_path.read_text()) if prov_path.exists() else None

integrity = []
if provenance is None:
    integrity.append("observations/candidate-provenance.json is missing: nothing binds the qualified "
                     "image to candidate_sha")
if malformed_phase_lines:
    integrity.append(f"unparseable phase return codes: {malformed_phase_lines}")

ok = not failed and not failed_phases and not integrity

record = {
    "schema_version": 1,
    "kind": "caesium-lifecycle-qualification",
    "pair": os.environ["QUAL_PAIR"],
    "candidate_sha": os.environ["QUAL_SHA"],
    "lifecycle_id": os.environ["QUAL_ID"],
    "started_at": datetime.datetime.fromtimestamp(
        int(os.environ["QUAL_START"]), datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
    "finished_at": datetime.datetime.fromtimestamp(
        int(os.environ["QUAL_FINISH"]), datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
    "duration_seconds": int(os.environ["QUAL_FINISH"]) - int(os.environ["QUAL_START"]),
    "host": {"arch": os.environ["QUAL_HOST_ARCH"]},
    "topology": {"nodes": 1, "database_shards": int(os.environ["QUAL_SHARDS"])},
    "images": {
        "previous": {
            "ref": os.environ["QUAL_PREV_IMAGE"],
            "release": pair["previous"]["release"],
            "image_id": os.environ["QUAL_PREV_IMAGE_ID"],
            "repo_digests": os.environ["QUAL_PREV_DIGESTS"].split(",") if os.environ["QUAL_PREV_DIGESTS"] else [],
            "pinned_digests": pair["previous"]["digests"],
            "architecture": os.environ["QUAL_PREV_ARCH"],
        },
        "candidate": {
            "ref": os.environ["QUAL_CAND_IMAGE"],
            "image_id": os.environ["QUAL_CAND_IMAGE_ID"],
            "architecture": os.environ["QUAL_CAND_ARCH"],
        },
        "runner_builder": os.environ["QUAL_BUILDER_IMAGE"],
    },
    "cli": {
        "previous_sha256": os.environ["QUAL_CLI_PREV_SHA"],
        "candidate_sha256": os.environ["QUAL_CLI_CAND_SHA"],
        "extraction": "docker cp <container>:/bin/caesium, one per side",
        "published_release_assets_sha256": pair["previous"]["release_assets_sha256"],
        "published_asset_smoke": pair["previous"]["cli_smoke"],
    },
    "server_env": os.environ["CAESIUM_LIFECYCLE_QUAL_ENV"],
    "server_user": os.environ["QUAL_SERVER_UID"],
    "docker_socket_access": os.environ["QUAL_SOCKET_MODE"],
    "schema_delta": delta,
    "ownership_token": os.environ["QUAL_OWNER_TOKEN"],
    "candidate_provenance": provenance,
    "expected_cases": expected,
    "phases": phases,
    "cases": cases,
    "result": "pass" if ok else "fail",
    "failed_cases": failed,
    "failed_phases": failed_phases,
    "missing_cases": missing,
    "unexpected_cases": unexpected,
    "stale_case_records": stale,
    "integrity_problems": integrity,
}
if provenance and not provenance.get("verified", False):
    record["result_qualifier"] = (
        "candidate provenance UNVERIFIED and explicitly overridden: the image qualified is "
        "not bound to candidate_sha"
    )
(art / "qualification.json").write_text(json.dumps(record, indent=2) + "\n")

print()
print(f"lifecycle qualification: {record['result']}  ({record['duration_seconds']}s)")
for case in cases:
    print(f"  {case.get('status','?'):16s} {case.get('name','?')}"
          f"  ({case.get('duration_seconds', 0.0):.1f}s)")
if delta:
    def _n(key):
        return len(delta.get(key) or [])
    print(f"  schema: {delta['previous_table_count']} -> {delta['candidate_table_count']} tables; "
          f"pinned additions: tables={_n('pinned_table_additions')} columns={_n('pinned_column_additions')}; "
          f"unpinned additions recorded: tables={_n('unpinned_table_additions')} "
          f"columns={_n('unpinned_column_additions')}; "
          f"dropped: tables={_n('dropped_tables')} columns={_n('dropped_columns')}")
print(f"  phases: {json.dumps(phases, sort_keys=True)}")
if missing:
    print(f"  MISSING expected cases (blocked): {missing}")
if stale:
    print(f"  STALE case records rejected: {stale}")
if unexpected:
    print(f"  unexpected case records: {unexpected}")
if failed_phases:
    print(f"  FAILED phases: {[f'{n}={phases[n]}' for n in failed_phases]}")
for problem in integrity:
    print(f"  INTEGRITY: {problem}")
if record.get("result_qualifier"):
    print(f"  NOTE: {record['result_qualifier']}")
if not ok:
    raise SystemExit(1)
PY
QUAL_RC=$?
set -e

log "qualification record written to $ARTIFACTS/qualification.json"
# Belt and braces: the record itself already folds every phase rc in (QUAL_RC is
# nonzero whenever it does not say "pass"), so the shell's exit status and the
# record can no longer disagree. This check would catch a phase rc that never
# reached the ledger.
RECORD_RESULT="$(python3 -c '
import json, sys
print(json.load(open(sys.argv[1])).get("result", "missing"))
' "$ARTIFACTS/qualification.json" 2>/dev/null || echo unreadable)"
if [[ "$UPGRADE_RC" -ne 0 || "$READDRESS_RC" -ne 0 || "$OUTCOMES_RC" -ne 0 || "$QUAL_RC" -ne 0 \
      || "$RECORD_RESULT" != "pass" ]]; then
  die "lifecycle qualification FAILED (upgrade=$UPGRADE_RC readdress=$READDRESS_RC outcomes=$OUTCOMES_RC record=$QUAL_RC result=$RECORD_RESULT)"
fi
log "lifecycle qualification PASSED for $CANDIDATE_SHA against $PREV_RELEASE (record result=$RECORD_RESULT)"
