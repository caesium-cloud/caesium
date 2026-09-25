#!/usr/bin/env bash
# Run one hermetic Go benchmark sample per side in each repeat. The caller
# prepares the shared benchmark harness and release-image provenance first.
set -euo pipefail

if [[ "$#" -ne 7 ]]; then
  printf 'usage: %s REPEATS PLATFORM CANDIDATE_SRC BASE_SRC CANDIDATE_BUILDER BASE_BUILDER ARTIFACTS\n' "$0" >&2
  exit 2
fi

REPEATS="$1"
DOCKER_PLATFORM="$2"
CANDIDATE_SRC="$3"
BASE_SRC="$4"
CANDIDATE_BUILDER="$5"
BASE_BUILDER="$6"
ARTIFACTS="$7"
[[ "$REPEATS" =~ ^[1-9][0-9]*$ ]] || { printf 'benchmark repeats must be positive: %s\n' "$REPEATS" >&2; exit 2; }

mkdir -p "$ARTIFACTS/observations"
: >"$ARTIFACTS/observations/benchmark-order.tsv"
for side in base candidate; do
  mkdir -p "$ARTIFACTS/$side"
  : >"$ARTIFACTS/$side/bench.txt"
  printf '0\n' >"$ARTIFACTS/$side/bench.txt.exit"
  : >"$ARTIFACTS/$side/bench.txt.repeats.tsv"
done

run_sample() {
  local side="$1" repeat="$2" src builder dest rc
  if [[ "$side" == base ]]; then
    src="$BASE_SRC"
    builder="$BASE_BUILDER"
  else
    src="$CANDIDATE_SRC"
    builder="$CANDIDATE_BUILDER"
  fi
  dest="$ARTIFACTS/$side/bench.txt"
  if docker run --rm --platform "$DOCKER_PLATFORM" \
    -v "$src:/bld/caesium" -w /bld/caesium \
    "$builder" \
    sh -c "mkdir -p ui/dist && touch ui/dist/index.html && go test -bench='^Benchmark(Owner|Recover)' -benchmem -count=1 -run '^$' ./internal/run" \
    >>"$dest" 2>&1; then
    rc=0
  else
    rc=$?
  fi
  printf '%s\t%s\n' "$repeat" "$rc" >>"$dest.repeats.tsv"
  printf '%s\t%s\t%s\n' "$repeat" "$side" "$rc" >>"$ARTIFACTS/observations/benchmark-order.tsv"
  if [[ "$rc" -ne 0 ]]; then
    if [[ "$(cat "$dest.exit")" == 0 ]]; then
      printf '%s\n' "$rc" >"$dest.exit"
    fi
    printf 'benchmark %s repeat %s exited %s (recorded in %s.exit)\n' "$side" "$repeat" "$rc" "$dest" >&2
  fi
}

for ((repeat = 1; repeat <= REPEATS; repeat++)); do
  if ((repeat % 2 == 1)); then
    order=(base candidate)
  else
    order=(candidate base)
  fi
  for side in "${order[@]}"; do
    run_sample "$side" "$repeat"
  done
done
