#!/usr/bin/env bash
# C3 checker-strength gate: run the candidate, then require named tests to
# reject known-bad mutations of the actual model and B3 history/ordering code.
set -euo pipefail

root=$(git rev-parse --show-toplevel)
cd "$root"
if [[ -n $(git status --porcelain) ]]; then
  echo 'oracle validation needs a clean, committed candidate' >&2
  exit 2
fi
candidate=$(git rev-parse HEAD)
scratch=$(mktemp -d "${TMPDIR:-/tmp}/caesium-oracles.XXXXXX")
trap 'rm -rf -- "$scratch"' EXIT

# A local shared-object clone is a separate Git checkout. Mutants and probe
# files never touch the caller's worktree or its worktree registry.
git clone --quiet --shared --no-checkout "$root" "$scratch/checkout"
checkout="$scratch/checkout"
git -C "$checkout" checkout --quiet --detach "$candidate"
cp "$root/test/model/testdata/history_oracle_probe_test.go" "$checkout/test/robustness/history/c3_oracle_probe_test.go"
cp "$root/test/model/testdata/robustness_oracle_probe_test.go" "$checkout/test/robustness/c3_oracle_probe_test.go"

export GOCACHE="$scratch/go-cache"
export GOPROXY=off
test_timeout=${ORACLE_TEST_TIMEOUT_SECONDS:-300}
if [[ ! $test_timeout =~ ^[1-9][0-9]*$ ]]; then
  echo 'ORACLE_TEST_TIMEOUT_SECONDS must be a positive integer' >&2
  exit 2
fi

run_probe() {
  local mode=$1 package=$2 selector=$3 expected=$4 label=$5 marker=${6:-}
  local log="$scratch/${label}.jsonl"
  python3 - "$checkout" "$mode" "$package" "$selector" "$expected" "$label" "$log" "$test_timeout" "$marker" <<'PY'
import json
import re
import subprocess
import sys

checkout, mode, package, selector, expected_csv, label, log, deadline, marker = sys.argv[1:]
expected = set(expected_csv.split(','))
cmd = ['go', 'test', '-json', '-count=1', '-timeout=45s', '-run', selector, package]
try:
    result = subprocess.run(cmd, cwd=checkout, text=True, stdout=subprocess.PIPE,
                            stderr=subprocess.STDOUT, timeout=int(deadline), check=False)
except subprocess.TimeoutExpired as exc:
    print(f'{label}: checker exceeded {deadline}s; inconclusive, refusing green', file=sys.stderr)
    sys.exit(1)
with open(log, 'w', encoding='utf-8') as stream:
    stream.write(result.stdout)
actions = {}
for line in result.stdout.splitlines():
    try:
        event = json.loads(line)
    except json.JSONDecodeError:
        continue
    name = event.get('Test')
    if name in expected and event.get('Action') in ('pass', 'fail', 'skip'):
        actions[name] = event['Action']
want = 'pass' if mode == 'candidate' else 'fail'
bad = {name: actions.get(name, 'missing') for name in expected if actions.get(name) != want}
exit_ok = result.returncode == (0 if mode == 'candidate' else 1)
resource_failure = re.search(r'panic: test timed out|out of memory|signal: killed', result.stdout, re.IGNORECASE)
if bad or not exit_ok or resource_failure or (mode == 'mutant' and marker not in result.stdout):
    print(f'{label}: expected {want} for {sorted(expected)}, observed {actions}, go exit {result.returncode}; log={log}', file=sys.stderr)
    print(result.stdout[-6000:], file=sys.stderr)
    sys.exit(1)
print(f'{label}: {want} {sorted(expected)} (go exit {result.returncode})')
PY
}

echo "oracle candidate=$candidate"
run_probe candidate ./test/model '^TestOracleRegression' \
  'TestOracleRegressionLostAcknowledgedState,TestOracleRegressionStaleGeneration,TestOracleRegressionWholeGroupFanIn,TestOracleRegressionDurableCompletionReplay,TestOracleRegressionLegalDuplicateDelivery' model-candidate
run_probe candidate ./test/robustness/history '^TestC3' \
  'TestC3MissingReplay,TestC3UnaccountedExternalEffect,TestC3LegalDuplicateDelivery,TestC3MissingEvidenceFailsClosed' history-candidate
run_probe candidate ./test/robustness '^TestC3' \
  'TestC3FanInStartedTooEarly,TestC3AmbiguousTimeoutHistory' robustness-candidate

run_mutation() {
  local name=$1 patch=$2 path=$3 package=$4 test_name=$5 marker=$6
  git -C "$checkout" reset --quiet --hard "$candidate"
  git -C "$checkout" apply --unidiff-zero --check "$root/$patch"
  git -C "$checkout" apply --unidiff-zero "$root/$patch"
  git -C "$checkout" add -- "$path"
  git -C "$checkout" -c user.name='C3 Oracle Probe' -c user.email='oracle-probe@example.invalid' \
    commit --quiet -m "Intentional bad oracle: $name"
  local bad_sha patch_blob
  bad_sha=$(git -C "$checkout" rev-parse HEAD)
  patch_blob=$(git -C "$checkout" hash-object "$root/$patch")
  echo "mutation=$name bad_sha=$bad_sha patch=$patch patch_blob=$patch_blob"
  run_probe mutant "$package" "^${test_name}$" "$test_name" "$name" "$marker"
}

run_mutation lost-ack test/model/testdata/mutations/lost-ack.patch test/model/oracle.go ./test/model TestOracleRegressionLostAcknowledgedState 'lost acknowledged identity escaped'
run_mutation accept-stale-generation test/model/testdata/mutations/accept-stale-generation.patch test/model/run.go ./test/model TestOracleRegressionStaleGeneration 'old owner completion was accepted or persisted'
run_mutation drop-durable-replay test/model/testdata/mutations/drop-durable-replay.patch test/model/run.go ./test/model TestOracleRegressionDurableCompletionReplay 'duplicate delivery failed to replay'
run_mutation partial-fan-in test/model/testdata/mutations/partial-fan-in.patch test/robustness/corelogic.go ./test/robustness TestC3FanInStartedTooEarly 'join started before all predecessor partitions completed'
run_mutation omit-catch-up test/model/testdata/mutations/omit-catch-up.patch test/robustness/history/history.go ./test/robustness/history TestC3MissingReplay 'missing replay escaped checker'
run_mutation aggregate-effects test/model/testdata/mutations/aggregate-effects.patch test/robustness/history/history.go ./test/robustness/history TestC3UnaccountedExternalEffect 'unaccounted completion escaped per-step effect checker'

echo 'oracle validation PASS: candidate accepted; all six known-bad mutations rejected by named tests'
