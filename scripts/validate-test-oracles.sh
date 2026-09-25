#!/usr/bin/env bash
# C3 checker-strength gate: run the candidate, then require named tests to
# reject known-bad mutations of the actual model and B3 history/ordering code.
# Ordinary C3 Go/Python probes run in CI's existing unit/config lanes; G6 owns
# promotion of this whole isolated mutation gate into the CI workflow.
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

export GOCACHE="$scratch/go-cache"
export GOPROXY=off
test_timeout=${ORACLE_TEST_TIMEOUT_SECONDS:-300}
if [[ ! $test_timeout =~ ^[1-9][0-9]*$ ]]; then
  echo 'ORACLE_TEST_TIMEOUT_SECONDS must be a positive integer' >&2
  exit 2
fi
python3 "$checkout/scripts/test_oracle_probe_result.py" -v

run_probe() {
  local mode=$1 package=$2 selector=$3 expected=$4 label=$5 marker=${6:-}
  local log="$scratch/${label}.jsonl"
  python3 "$checkout/scripts/oracle_probe_result.py" \
    "$checkout" "$mode" "$package" "$selector" "$expected" "$label" "$log" "$test_timeout" "$marker"
}

echo "oracle candidate=$candidate"
run_probe candidate ./test/model '^TestOracleRegression' \
  'TestOracleRegressionLostAcknowledgedState,TestOracleRegressionCrossEpochAcknowledgedState,TestOracleRegressionRetryRetainsAcknowledgedState,TestOracleRegressionStaleGeneration,TestOracleRegressionAcceptedStaleGenerationStateDiff,TestOracleRegressionStaleGenerationSequenceDiff,TestOracleRegressionWholeGroupFanIn,TestOracleRegressionDurableCompletionReplay,TestOracleRegressionLegalDuplicateDelivery' model-candidate
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
  GIT_AUTHOR_NAME='C3 Oracle Probe' GIT_AUTHOR_EMAIL='oracle-probe@example.invalid' \
    GIT_COMMITTER_NAME='C3 Oracle Probe' GIT_COMMITTER_EMAIL='oracle-probe@example.invalid' \
    GIT_AUTHOR_DATE='2000-01-01T00:00:00 +0000' GIT_COMMITTER_DATE='2000-01-01T00:00:00 +0000' \
    git -C "$checkout" -c commit.gpgsign=false commit --quiet -m "Intentional bad oracle: $name"
  local bad_sha patch_blob
  bad_sha=$(git -C "$checkout" rev-parse HEAD)
  patch_blob=$(git -C "$checkout" hash-object "$root/$patch")
  echo "mutation=$name bad_sha=$bad_sha patch=$patch patch_blob=$patch_blob"
  run_probe mutant "$package" "^${test_name}$" "$test_name" "$name" "$marker"
}

run_mutation lost-ack test/model/testdata/mutations/lost-ack.patch test/model/oracle.go ./test/model TestOracleRegressionCrossEpochAcknowledgedState 'cross-epoch acknowledged identity change escaped'
run_mutation retry-drops-ack test/model/testdata/mutations/retry-drops-ack.patch test/model/run.go ./test/model TestOracleRegressionRetryRetainsAcknowledgedState 'retry lost acknowledged identity'
run_mutation accept-stale-generation test/model/testdata/mutations/accept-stale-generation.patch test/model/run.go ./test/model TestOracleRegressionStaleGeneration 'old owner completion was accepted or persisted'
run_mutation weaken-refusal-inert test/model/testdata/mutations/weaken-refusal-inert.patch test/model/oracle.go ./test/model TestOracleRegressionAcceptedStaleGenerationStateDiff 'accepted stale completion escaped refusal checker'
run_mutation drop-durable-replay test/model/testdata/mutations/drop-durable-replay.patch test/model/run.go ./test/model TestOracleRegressionDurableCompletionReplay 'duplicate delivery failed to replay'
run_mutation partial-fan-in test/model/testdata/mutations/partial-fan-in.patch test/robustness/corelogic.go ./test/robustness TestC3FanInStartedTooEarly 'join started before all predecessor partitions completed'
run_mutation omit-catch-up test/model/testdata/mutations/omit-catch-up.patch test/robustness/history/history.go ./test/robustness/history TestC3MissingReplay 'missing replay escaped checker'
run_mutation aggregate-effects test/model/testdata/mutations/aggregate-effects.patch test/robustness/history/history.go ./test/robustness/history TestC3UnaccountedExternalEffect 'unaccounted completion escaped per-step effect checker'

echo 'oracle validation PASS: candidate accepted; all eight known-bad mutations rejected by named tests'
