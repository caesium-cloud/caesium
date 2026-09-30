#!/bin/sh
# Discover, run, and grade this repo's native Go fuzz targets, plus repeat a
# selected set of concurrency-sensitive tests under -race with varied
# scheduling. Runs inside the SAME caesium-builder image `just unit-test` and
# `just lint` use, with the same repo bind mount and ui/dist/index.html
# placeholder — see the `unit-test`, `lint`, and `builder-full` recipes in
# ../justfile (read, never edited: the justfile is G6-owned for this plan).
#
# POSIX sh only, deliberately: the builder-full image (build/Dockerfile.build)
# installs no bash, only Alpine's busybox ash — see scripts/integration-test.sh
# for the same convention.
#
# Usage:
#   scripts/fuzz-tests.sh                  # default small bounded budgets
#   CAESIUM_FUZZ_SECONDS=120s scripts/fuzz-tests.sh   # G4's longer campaigns
#   CAESIUM_FUZZ_CONCURRENCY_COUNT=10 scripts/fuzz-tests.sh
#
# Env overrides (all optional):
#   CAESIUM_FUZZ_SECONDS             per-target -fuzztime (default: 25s)
#   CAESIUM_FUZZ_CONCURRENCY_COUNT   -count for the concurrency repeat matrix (default: 3)
#   CAESIUM_FUZZ_ARTIFACT_DIR        host directory for corpus/logs/gocache (default: <repo>/.fuzz-artifacts)
#   CAESIUM_CONTAINER_CLI            docker (default) or podman
#   CAESIUM_PODMAN                   "true" to use podman + localhost-prefixed image refs
#   CAESIUM_PLATFORM                 container platform (default: linux/<host arch>)
set -eu

if [ "${CAESIUM_FUZZ_IN_CONTAINER:-}" != "1" ]; then
  # ---------------------------------------------------------------------
  # HOST SIDE: build the builder-full image (reusing the justfile's own
  # recipe as the single source of truth for the Dockerfile build) and
  # re-exec this SAME script inside it, mounting the repo exactly the way
  # `just unit-test` does.
  # ---------------------------------------------------------------------
  # shellcheck disable=SC1007
  REPO_DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
  cd "$REPO_DIR"

  REPO="caesiumcloud"
  IMAGE="caesium"
  BUILDER_IMAGE="${IMAGE}-builder"
  TAG="latest"
  BLD_DIR="/bld/caesium"

  PODMAN="${CAESIUM_PODMAN:-false}"
  if [ "$PODMAN" = "true" ]; then
    CONTAINER_CLI="${CAESIUM_CONTAINER_CLI:-podman}"
    LOCAL_BUILDER_REF="localhost/${REPO}/${BUILDER_IMAGE}"
  else
    CONTAINER_CLI="${CAESIUM_CONTAINER_CLI:-docker}"
    LOCAL_BUILDER_REF="${REPO}/${BUILDER_IMAGE}"
  fi

  ARCH="$(uname -m)"
  case "$ARCH" in
    aarch64|arm64) DOCKER_ARCH="arm64" ;;
    x86_64|amd64) DOCKER_ARCH="amd64" ;;
    *) DOCKER_ARCH="$ARCH" ;;
  esac
  PLATFORM="${CAESIUM_PLATFORM:-linux/${DOCKER_ARCH}}"
  BUILDER_REF="${LOCAL_BUILDER_REF}:${TAG}-full"

  # Same image `unit-test`/`lint` depend on; building it here (instead of
  # duplicating the Dockerfile invocation) keeps the build itself owned by
  # the justfile, per this stream's "no justfile edit" scope — this script
  # only adds its OWN new recipe-shaped entry point, it does not fork the
  # image build logic.
  just builder-full

  ARTIFACT_DIR="${CAESIUM_FUZZ_ARTIFACT_DIR:-$REPO_DIR/.fuzz-artifacts}"
  mkdir -p "$ARTIFACT_DIR/corpus" "$ARTIFACT_DIR/logs" "$ARTIFACT_DIR/gocache"

  echo "fuzz-tests.sh: artifacts -> $ARTIFACT_DIR"

  exec "$CONTAINER_CLI" run --rm --platform "$PLATFORM" \
    -v "$REPO_DIR:$BLD_DIR" \
    -v "$ARTIFACT_DIR:/fuzz-artifacts" \
    -w "$BLD_DIR" \
    -e CAESIUM_FUZZ_IN_CONTAINER=1 \
    -e CAESIUM_FUZZ_SECONDS="${CAESIUM_FUZZ_SECONDS:-}" \
    -e CAESIUM_FUZZ_CONCURRENCY_COUNT="${CAESIUM_FUZZ_CONCURRENCY_COUNT:-}" \
    -e GOCACHE=/fuzz-artifacts/gocache \
    "$BUILDER_REF" \
    sh -c 'mkdir -p ui/dist && touch ui/dist/index.html && sh scripts/fuzz-tests.sh'
fi

# ---------------------------------------------------------------------
# CONTAINER SIDE (CAESIUM_FUZZ_IN_CONTAINER=1): discovery, bounded fuzzing,
# artifact export, and the concurrency repeat matrix.
# ---------------------------------------------------------------------

FUZZ_TIME="${CAESIUM_FUZZ_SECONDS:-25s}"
CONCURRENCY_COUNT="${CAESIUM_FUZZ_CONCURRENCY_COUNT:-3}"
ARTIFACT_DIR="/fuzz-artifacts"
mkdir -p "$ARTIFACT_DIR/corpus" "$ARTIFACT_DIR/logs"
SUMMARY="$ARTIFACT_DIR/summary.txt"
: > "$SUMMARY"

FAIL=0

note() {
  echo "$1" | tee -a "$SUMMARY"
}

# A zero or non-numeric -count would make `go test -count=$CONCURRENCY_COUNT`
# build the binary and run NOTHING (exit 0, zero subtests) — silently turning
# the whole concurrency matrix into a no-op "pass". Validate it up front,
# before starting anything Docker-heavy.
case "$CONCURRENCY_COUNT" in
  ''|*[!0-9]*)
    note "FAIL: CAESIUM_FUZZ_CONCURRENCY_COUNT/CONCURRENCY_COUNT must be a positive integer, got '$CONCURRENCY_COUNT'"
    exit 1
    ;;
esac
if [ "$CONCURRENCY_COUNT" -le 0 ]; then
  note "FAIL: CAESIUM_FUZZ_CONCURRENCY_COUNT/CONCURRENCY_COUNT must be a positive integer, got '$CONCURRENCY_COUNT'"
  exit 1
fi

# extract_field LINE MARKER: prints the whitespace-delimited token immediately
# following the first occurrence of MARKER in LINE (e.g. "execs: 4200 (…"
# with MARKER="execs: " yields "4200"). Plain awk -F, no regex metacharacter
# escaping required for any marker used below.
extract_field() {
  printf '%s\n' "$1" | awk -F"$2" '{print $2}' | awk '{print $1}'
}

# --- Declared fuzz target manifest -----------------------------------------
# The single source of truth this script checks the real `go test -list`
# output against, in both directions: a declared target that vanished (a
# rename, a deleted file) and an undeclared target that appeared (a new fuzz
# func nobody added here) both fail the run, so this list cannot rot silently.
PACKAGES="pkg/jobdef/schemacompat internal/jobdef/diff internal/trigger/cron internal/run"

targets_for_pkg() {
  case "$1" in
    pkg/jobdef/schemacompat)
      echo "FuzzCompare" ;;
    internal/jobdef/diff)
      echo "FuzzDecodeDefinitions" ;;
    internal/trigger/cron)
      echo "FuzzExtractExpression FuzzExtractLocation" ;;
    internal/run)
      echo "FuzzTaskExecutionDescriptorRoundTrip FuzzMergeDescriptorSecretRefs FuzzValidateCheckpointBlob FuzzRecoverRunStateTerminalRows" ;;
    *)
      echo "" ;;
  esac
}

# --- Step 1: discovery — fail on drift before running anything -------------
note "=== discovering fuzz targets ==="
for pkg in $PACKAGES; do
  declared="$(targets_for_pkg "$pkg")"
  discovered="$(go test -list '^Fuzz' "./$pkg" | grep '^Fuzz' || true)"
  note "./$pkg: declared=[$declared] discovered=[$(echo "$discovered" | tr '\n' ' ')]"

  for want in $declared; do
    found=0
    for have in $discovered; do
      [ "$have" = "$want" ] && found=1
    done
    if [ "$found" -eq 0 ]; then
      note "FAIL: declared fuzz target $want not found by 'go test -list' in ./$pkg"
      FAIL=1
    fi
  done
  for have in $discovered; do
    known=0
    for want in $declared; do
      [ "$have" = "$want" ] && known=1
    done
    if [ "$known" -eq 0 ]; then
      note "FAIL: undeclared fuzz target $have discovered in ./$pkg — add it to targets_for_pkg() in this script, or remove it"
      FAIL=1
    fi
  done
done

# --- Step 1b: repo-wide sweep ------------------------------------------------
# The per-package loop above only ever looks inside $PACKAGES, so a new Fuzz
# func added to a package nobody listed here is invisible to it and the run
# would still exit 0. Sweep every *_test.go in the repo (busybox grep has no
# --include/--exclude-dir, hence the `find -prune` instead) for ANY top-level
# `func FuzzX(` in a test file, whatever its signature looks like. Matching the
# parameter list would tie discovery to one spelling: Go accepts any parameter
# name, an aliased import (`import test "testing"` -> `f *test.F`), and a
# parameter list broken across lines, and each of those would hide a target
# from a stricter pattern. Over-matching is the safe direction here — a helper
# that merely happens to be named FuzzX is flagged and must be declared or
# renamed, whereas an under-match lets a real target go unexplored while the
# script exits 0. Require each hit to be an exact (package, target) member of
# the declared manifest above — in either direction: a target in an unlisted
# package, or an extra target in a listed one this loop didn't already catch.
note ""
note "=== repo-wide fuzz-target sweep (catches a new Fuzz func in ANY package) ==="
SWEEP_FILE="$ARTIFACT_DIR/logs/repo-fuzz-sweep.txt"
find . \( -name .git -o -name vendor -o -name node_modules \) -prune -o \
  -type f -name '*_test.go' -print >"$ARTIFACT_DIR/logs/repo-test-files.txt"
: > "$SWEEP_FILE"
# Word-splitting the file list below is intended (one file argument per
# whitespace-free path from find, POSIX sh has no arrays).
# shellcheck disable=SC2046
grep -H -n '^func Fuzz[A-Za-z0-9_]*(' $(cat "$ARTIFACT_DIR/logs/repo-test-files.txt") \
  >"$SWEEP_FILE" 2>/dev/null || true

while IFS= read -r hit; do
  [ -z "$hit" ] && continue
  hit_file="${hit%%:*}"
  hit_rest="${hit#*:}"
  hit_rest="${hit_rest#*:}"
  hit_func="$(printf '%s\n' "$hit_rest" | sed -n 's/^func \(Fuzz[A-Za-z0-9_]*\).*/\1/p')"
  hit_pkg="$(dirname "$hit_file")"
  hit_pkg="${hit_pkg#./}"

  member=0
  for pkg in $PACKAGES; do
    [ "$pkg" = "$hit_pkg" ] || continue
    for want in $(targets_for_pkg "$pkg"); do
      [ "$want" = "$hit_func" ] && member=1
    done
  done
  if [ "$member" -eq 0 ]; then
    note "FAIL: repo-wide sweep found $hit_func in ./$hit_pkg ($hit_file), not declared in this script's manifest (targets_for_pkg / PACKAGES) — add it there, or remove it"
    FAIL=1
  fi
done <"$SWEEP_FILE"

if [ "$FAIL" -ne 0 ]; then
  note "Target list drift detected — see FAIL lines above. Not running any fuzz target."
  exit 1
fi

# --- Step 2: bounded fuzzing, one target per `go test` invocation ----------
# -fuzz requires exactly one matching target, hence the per-target loop and
# the anchored -run/-fuzz regex.
note ""
note "=== bounded fuzzing (fuzztime=$FUZZ_TIME per target) ==="
for pkg in $PACKAGES; do
  for target in $(targets_for_pkg "$pkg"); do
    logfile="$ARTIFACT_DIR/logs/${target}.log"
    note "--- $target (./$pkg) ---"
    start=$(date +%s)
    set +e
    go test -run "^${target}\$" -fuzz "^${target}\$" -fuzztime "$FUZZ_TIME" -v "./$pkg" >"$logfile" 2>&1
    rc=$?
    set -e
    end=$(date +%s)
    duration=$((end - start))

    execs_line="$(grep 'execs:' "$logfile" | tail -1 || true)"
    execs="$(extract_field "$execs_line" 'execs: ')"
    new_interesting_total="$(extract_field "$execs_line" 'total: ')"
    new_interesting_total="$(printf '%s' "$new_interesting_total" | tr -d ')')"

    # The seed/baseline corpus is replayed before any generated input is ever
    # tried, and Go's own execs counter includes those baseline replays: with
    # a small enough budget, the run can finish gathering baseline coverage
    # (replaying every f.Add() seed) and exit 0 with an "execs:" line whose
    # count is nothing but the seed corpus size — zero mutation, zero real
    # exploration, yet execs>0. "fuzz: elapsed: Ns, gathering baseline
    # coverage: N/N completed[, now fuzzing with W workers]" always reports
    # the FULL intended baseline size as the denominator, whether or not
    # baseline gathering ever finished, so that is what execs must exceed.
    baseline_line="$(grep 'gathering baseline coverage:' "$logfile" | tail -1 || true)"
    baseline="$(printf '%s\n' "$baseline_line" | sed -n 's#.*coverage: [0-9]*/\([0-9]*\) completed.*#\1#p')"
    [ -z "$baseline" ] && baseline=0

    # Preserve corpus artifacts UNCONDITIONALLY (success or crash), before any
    # `continue`: testdata/fuzz/<target> inside the PACKAGE directory is where
    # Go durably persists every newly-found interesting AND crashing input as
    # it runs — the same directory `just unit-test` replays as ordinary
    # seed-only regressions on every future run, and the only place a fresh
    # crasher actually lands. Copy it out so the artifacts survive this --rm
    # container regardless of the exploratory build-cache corpus below, and
    # regardless of whether this target crashed.
    if [ -d "$pkg/testdata/fuzz/$target" ]; then
      mkdir -p "$ARTIFACT_DIR/corpus/${target}"
      cp -r "$pkg/testdata/fuzz/$target/." "$ARTIFACT_DIR/corpus/${target}/" 2>/dev/null || true
    fi
    # Best-effort: also copy whatever this run added to the fuzz build cache
    # (exploratory corpus entries that increased coverage but never became a
    # committed regression), so a later long campaign (G4) can seed from the
    # full explored corpus, not just the committed subset.
    cache_dir="$(find "$GOCACHE/fuzz" -type d -name "$target" 2>/dev/null | head -1 || true)"
    if [ -n "$cache_dir" ]; then
      mkdir -p "$ARTIFACT_DIR/corpus/${target}-cache"
      cp -r "$cache_dir/." "$ARTIFACT_DIR/corpus/${target}-cache/" 2>/dev/null || true
    fi

    if [ "$rc" -ne 0 ]; then
      note "CRASH: $target failed (exit $rc) after ${duration}s"
      # Go prints this path RELATIVE TO THE PACKAGE DIRECTORY: `go test`
      # always runs the test binary with its cwd set to the package's source
      # directory, not the directory `go test` itself was invoked from (this
      # script's cwd, the repo root). Resolve it against $pkg, not bare.
      crasher_rel="$(grep 'Failing input written to' "$logfile" | sed 's/.*Failing input written to //' | tail -1 || true)"
      note "  reproduce with: go test -run=${target} ./${pkg}"
      if [ -n "$crasher_rel" ]; then
        crasher_path="$pkg/$crasher_rel"
        note "  minimized failing input: $crasher_path"
        note "  or precisely: go test -run=${target}/$(basename "$crasher_rel") ./${pkg}"
        mkdir -p "$ARTIFACT_DIR/corpus/${target}"
        [ -f "$crasher_path" ] && cp "$crasher_path" "$ARTIFACT_DIR/corpus/${target}/" 2>/dev/null || true
      fi
      note "  --- last 40 log lines ---"
      tail -40 "$logfile" | tee -a "$SUMMARY"
      FAIL=1
      continue
    fi

    if [ -z "$execs_line" ]; then
      note "FAIL: $target produced no 'execs:' progress line in ${duration}s — the fuzz engine did not actually run (seed-only, not bounded exploration)"
      FAIL=1
      continue
    fi
    if [ -z "$execs" ] || [ "$execs" -eq 0 ] 2>/dev/null; then
      note "FAIL: $target ran 0 executions in ${duration}s — bounded exploration did not happen"
      FAIL=1
      continue
    fi
    if [ "$execs" -le "$baseline" ] 2>/dev/null; then
      note "FAIL: $target ran $execs executions in ${duration}s, at or below its baseline/seed count of $baseline — the budget expired during baseline replay before any generated input was tried (not bounded exploration; raise CAESIUM_FUZZ_SECONDS)"
      FAIL=1
      continue
    fi

    note "OK: $target — ${duration}s, execs=$execs (baseline=$baseline), new_interesting_total=${new_interesting_total:-0}"
  done
done

# --- Step 3: concurrency repeat matrix --------------------------------------
# The renewal goroutines this stream added/exercises (internal/worker) plus
# their existing neighbours, repeated under -race with varied -count/-cpu.
# Scope is deliberately narrow ("selected") rather than the whole repo: these
# are the tests this stream's C2 work is actually about, and they are all
# hermetic (fakes, no DB, no socket) so the matrix stays fast.
note ""
note "=== concurrency repeat matrix ==="
CONCURRENCY_PKG="./internal/worker"
CONCURRENCY_RUN='^Test(TaskClaimRenewal|RunLeaseRenewal|BatchedRenewal|ClaimLiveness|RunRunLeaseRenewal|WithRunLeaseRenewal)'
for cpu in 1 2 4; do
  logfile="$ARTIFACT_DIR/logs/concurrency-cpu${cpu}.log"
  note "--- -race -count=$CONCURRENCY_COUNT -cpu=$cpu ---"
  set +e
  go test -race -count="$CONCURRENCY_COUNT" -cpu="$cpu" -run "$CONCURRENCY_RUN" -v "$CONCURRENCY_PKG" >"$logfile" 2>&1
  rc=$?
  set -e
  passed=$(grep -c -- '--- PASS' "$logfile" || true)
  failed=$(grep -c -- '--- FAIL' "$logfile" || true)
  if [ "$rc" -ne 0 ]; then
    note "FAIL: concurrency repeat at -cpu=$cpu exited $rc (pass=$passed fail=$failed) — see $logfile"
    tail -40 "$logfile" | tee -a "$SUMMARY"
    FAIL=1
  elif [ "$failed" -ne 0 ]; then
    note "FAIL: concurrency repeat at -cpu=$cpu exited 0 but reported $failed failing subtest(s) (pass=$passed) — see $logfile"
    tail -40 "$logfile" | tee -a "$SUMMARY"
    FAIL=1
  elif [ "$passed" -le 0 ]; then
    # rc==0 and failed==0 with passed==0 means nothing actually ran: a bad
    # -count, a -run pattern matching nothing, or an all-skipped selection
    # all report exit 0 with zero subtests — a silent no-op "pass" that
    # proved nothing about concurrency at all.
    note "FAIL: concurrency repeat at -cpu=$cpu exited 0 but ran ZERO subtests (pass=$passed fail=$failed) — the -run pattern or -count matched nothing, see $logfile"
    tail -40 "$logfile" | tee -a "$SUMMARY"
    FAIL=1
  else
    note "OK: -cpu=$cpu pass=$passed fail=$failed"
  fi
done

# --- Summary -----------------------------------------------------------------
note ""
note "=== fuzz-tests.sh summary ==="
note "artifacts: $ARTIFACT_DIR (logs/, corpus/)"

if [ "$FAIL" -ne 0 ]; then
  note "RESULT: FAILED"
  exit 1
fi
note "RESULT: all declared targets explored with genuine execution, all concurrency configurations passed"
exit 0
