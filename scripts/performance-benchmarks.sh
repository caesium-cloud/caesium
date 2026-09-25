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
EXPECTED="$ARTIFACTS/observations/benchmark-names.json"
python3 - "$CANDIDATE_SRC" "$BASE_SRC" "$EXPECTED" <<'PY'
import hashlib,json,pathlib,re,sys
candidate,base,output=map(pathlib.Path,sys.argv[1:])
files=('internal/run/owner_benchmark_test.go','internal/run/recovery_benchmark_test.go')
pattern=re.compile(r'^func\s+(Benchmark(?:Owner|Recover)[A-Za-z0-9_]*)\s*\(\s*[A-Za-z_][A-Za-z_0-9]*\s+\*testing\.B\s*\)',re.M)
names=[];sources=[]
for path in files:
  candidate_bytes=(candidate/path).read_bytes()
  if (base/path).read_bytes()!=candidate_bytes:
    raise SystemExit(f'benchmark harness differs between sides: {path}')
  found=pattern.findall(candidate_bytes.decode())
  if not found:raise SystemExit(f'benchmark harness has no selected functions: {path}')
  names.extend(found)
  sources.append({'path':path,'sha256':hashlib.sha256(candidate_bytes).hexdigest()})
if len(names)!=len(set(names)):
  raise SystemExit('benchmark harness contains duplicate function names')
output.write_text(json.dumps({'source_files':sources,'benchmark_names':sorted(names)},indent=2)+'\n')
PY
# Compile the candidate benchmark overlay against the base before sampling.
# A failure here identifies harness incompatibility, distinct from a base
# benchmark that compiles and then fails while running. Keep the raw output and
# continue recording every paired sample so the comparison remains auditable.
BASE_COMPILE="$ARTIFACTS/observations/benchmark-base-compile.txt"
if docker run --rm --platform "$DOCKER_PLATFORM" \
  -v "$BASE_SRC:/bld/caesium" -w /bld/caesium \
  "$BASE_BUILDER" \
  sh -c 'mkdir -p ui/dist && touch ui/dist/index.html && go test -c -o /tmp/caesium-benchmark-base.test ./internal/run' \
  >"$BASE_COMPILE" 2>&1; then
  printf '0\n' >"$ARTIFACTS/observations/benchmark-base-compile.exit"
else
  rc=$?
  printf '%s\n' "$rc" >"$ARTIFACTS/observations/benchmark-base-compile.exit"
  printf 'benchmark harness incompatible with base: compile exited %s (recorded in %s)\n' "$rc" "$BASE_COMPILE" >&2
fi
: >"$ARTIFACTS/observations/benchmark-order.tsv"
for side in base candidate; do
  mkdir -p "$ARTIFACTS/$side"
  : >"$ARTIFACTS/$side/bench.txt"
  printf '0\n' >"$ARTIFACTS/$side/bench.txt.exit"
  : >"$ARTIFACTS/$side/bench.txt.repeats.tsv"
done

run_sample() {
  local side="$1" repeat="$2" src builder dest sample rc
  if [[ "$side" == base ]]; then
    src="$BASE_SRC"
    builder="$BASE_BUILDER"
  else
    src="$CANDIDATE_SRC"
    builder="$CANDIDATE_BUILDER"
  fi
  dest="$ARTIFACTS/$side/bench.txt"
  sample="$ARTIFACTS/$side/bench-repeat-$repeat.txt"
  if docker run --rm --platform "$DOCKER_PLATFORM" \
    -v "$src:/bld/caesium" -w /bld/caesium \
    "$builder" \
    sh -c "mkdir -p ui/dist && touch ui/dist/index.html && go test -bench='^Benchmark(Owner|Recover)' -benchmem -count=1 -run '^$' ./internal/run" \
    >"$sample" 2>&1; then
    rc=0
  else
    rc=$?
  fi
  cat "$sample" >>"$dest"
  if [[ "$rc" -eq 0 ]]; then
    if ! python3 - "$EXPECTED" "$sample" "$(dirname "$0")/compare-performance.py" \
        >"$sample.validation" 2>&1 <<'PY'; then
import json,pathlib,runpy,sys
expected_path,sample_path,comparator_path=map(pathlib.Path,sys.argv[1:])
expected=json.loads(expected_path.read_text())['benchmark_names']
benchmark_re=runpy.run_path(str(comparator_path))['GO_BENCH_RE']
rows=[]
for line in sample_path.read_text().splitlines():
  line=line.strip()
  if not line.startswith('Benchmark'):continue
  match=benchmark_re.fullmatch(line)
  if match is None:raise SystemExit(f'malformed benchmark row: {line}')
  if match.group(4) is None or match.group(5) is None:
    raise SystemExit(f'benchmark row lacks -benchmem metrics: {line}')
  rows.append(match.group(1))
if sorted(rows)!=expected:
  missing=sorted(set(expected)-set(rows))
  extra=sorted(set(rows)-set(expected))
  duplicate=sorted({name for name in rows if rows.count(name)>1})
  raise SystemExit(f'benchmark sample rows differ from shared harness: missing={missing} extra={extra} duplicate={duplicate}')
PY
      rc=65
      cat "$sample.validation" >>"$dest"
    fi
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
