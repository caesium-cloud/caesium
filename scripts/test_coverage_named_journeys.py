#!/usr/bin/env python3
"""Require exact, non-skipped top-level Go integration suite passes."""

from __future__ import annotations

import argparse
import datetime
import hashlib
import json
import pathlib
import re
import subprocess
import sys
import tempfile
import uuid
import unittest
from unittest import mock


SUITE = "TestIntegrationTestSuite"
NAME_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")


def _event_pattern(name: str) -> re.Pattern[str]:
    if not NAME_RE.fullmatch(name):
        raise ValueError(f"invalid integration test name: {name!r}")
    return re.compile(
        r"^[ \t]*--- (PASS|SKIP): "
        + re.escape(SUITE + "/" + name)
        + r"(?:[ \t]+\([^/\r\n]*\))?[ \t]*$"
    )


def check_output(output: str, required: list[str]) -> dict[str, object]:
    if len(required) != len(set(required)):
        raise ValueError("required integration test names must be unique")
    patterns = {name: _event_pattern(name) for name in required}
    counts: dict[str, dict[str, int]] = {
        name: {"PASS": 0, "SKIP": 0} for name in required
    }
    for line in output.splitlines():
        for name, pattern in patterns.items():
            match = pattern.fullmatch(line)
            if match:
                counts[name][match.group(1)] += 1

    passed = [name for name in required if counts[name]["PASS"] == 1 and counts[name]["SKIP"] == 0]
    missing = [name for name in required if counts[name]["PASS"] == 0 and counts[name]["SKIP"] == 0]
    skipped = [name for name in required if counts[name]["SKIP"] > 0]
    duplicate = [name for name in required if counts[name]["PASS"] > 1]
    return {
        "required": list(required),
        "passed": passed,
        "missing": missing,
        "skipped": skipped,
        "duplicate": duplicate,
        "valid": not (missing or skipped or duplicate),
    }


def check_git_receipt(receipt_path: pathlib.Path, state_path: pathlib.Path, finished_at: str) -> dict[str, object]:
    if receipt_path.is_symlink() or state_path.is_symlink():
        raise ValueError("Git-sync receipt and state must be regular non-symlink files")
    receipt_raw = receipt_path.read_bytes()
    state_raw = state_path.read_bytes()
    receipt = json.loads(receipt_raw)
    state = json.loads(state_raw)
    if not isinstance(receipt, dict) or not isinstance(state, dict):
        raise ValueError("Git-sync receipt and state must be JSON objects")
    expected = {
        "schema_version",
        "source",
        "initial_commit",
        "updated_commit",
        "deleted_commit",
        "job_id",
        "sentinel_id",
        "run_id",
        "run_status",
        "run_completed_at",
        "task_completed_at",
        "output_marker",
        "imported_job_absent",
        "sentinel_survived",
    }
    state_fields = {"schema_version", "alias", "source_id", "url", "ref", "path", "image", "initial_commit", "git_version"}
    if set(state) != state_fields or type(state.get("schema_version")) is not int or state.get("schema_version") != 1:
        raise ValueError("Git-sync initialized source state has an unexpected schema")
    if any(not isinstance(state.get(key), str) or not state[key] for key in state_fields - {"schema_version"}):
        raise ValueError("Git-sync initialized source state is incomplete")
    if not re.fullmatch(r"[0-9a-f]{40}", state["initial_commit"]) or not state["git_version"].startswith("git version "):
        raise ValueError("Git-sync initialized source state lacks native Git identity")
    if set(receipt) != expected or type(receipt.get("schema_version")) is not int or receipt.get("schema_version") != 1 or receipt.get("source") != state:
        raise ValueError("Git-sync receipt does not bind the complete initialized source state")
    commits = [receipt.get(key) for key in ("initial_commit", "updated_commit", "deleted_commit")]
    if any(not isinstance(commit, str) or not re.fullmatch(r"[0-9a-f]{40}", commit) for commit in commits):
        raise ValueError("Git-sync receipt contains an invalid native commit ID")
    if len(set(commits)) != 3:
        raise ValueError("Git-sync receipt commits are not distinct")
    if commits[0] != state.get("initial_commit"):
        raise ValueError("Git-sync receipt initial commit differs from initialized native HEAD")
    identifiers = [receipt.get(key) for key in ("job_id", "sentinel_id", "run_id")]
    try:
        parsed_ids = [uuid.UUID(value) for value in identifiers if isinstance(value, str)]
    except (ValueError, AttributeError) as exc:
        raise ValueError("Git-sync receipt contains a malformed durable identity") from exc
    if len(parsed_ids) != 3 or any(str(item) != value or item.int == 0 for item, value in zip(parsed_ids, identifiers)):
        raise ValueError("Git-sync receipt identities are not canonical nonzero UUIDs")
    if len({item.int for item in parsed_ids}) != 3:
        raise ValueError("Git-sync receipt reuses a durable identity")
    if receipt.get("run_status") != "succeeded" or receipt.get("output_marker") != "updated":
        raise ValueError("Git-sync receipt does not prove the updated job completed with its output")
    if receipt.get("imported_job_absent") is not True or receipt.get("sentinel_survived") is not True:
        raise ValueError("Git-sync receipt does not prove source-scoped prune and sentinel survival")

    def parse_timestamp(value: object, label: str) -> datetime.datetime:
        if not isinstance(value, str):
            raise ValueError(f"Git-sync {label} timestamp is missing")
        try:
            parsed = datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))
        except ValueError as exc:
            raise ValueError(f"Git-sync {label} timestamp is malformed") from exc
        if parsed.tzinfo is None or parsed.year <= 1:
            raise ValueError(f"Git-sync {label} timestamp is not a nonzero qualified instant")
        return parsed

    finished = parse_timestamp(finished_at, "server-finished")
    completed = [
        parse_timestamp(receipt.get("run_completed_at"), "run-completed"),
        parse_timestamp(receipt.get("task_completed_at"), "task-completed"),
    ]
    if any(value > finished for value in completed):
        raise ValueError("Git-sync run/task completion occurred after the main server finished")
    return {
        "valid": True,
        "receipt_sha256": hashlib.sha256(receipt_raw).hexdigest(),
        "state_sha256": hashlib.sha256(state_raw).hexdigest(),
        "initial_commit": commits[0],
        "updated_commit": commits[1],
        "deleted_commit": commits[2],
        "job_id": identifiers[0],
        "sentinel_id": identifiers[1],
        "run_id": identifiers[2],
    }
def _run_check(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--log", required=True, type=pathlib.Path)
    parser.add_argument("--required-name", action="append", default=[])
    args = parser.parse_args(argv)
    try:
        output = args.log.read_text(encoding="utf-8", errors="replace")
        result = check_output(output, args.required_name)
    except (OSError, ValueError) as exc:
        print(f"named integration pass check failed: {exc}", file=sys.stderr)
        return 2
    print(json.dumps(result, sort_keys=True))
    if not result["valid"]:
        print("required top-level integration tests are missing, skipped, or duplicated", file=sys.stderr)
        return 1
    return 0


def _run_git_receipt(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description="Validate the public Git-sync semantic receipt")
    parser.add_argument("--receipt", required=True, type=pathlib.Path)
    parser.add_argument("--state", required=True, type=pathlib.Path)
    parser.add_argument("--server-finished-at", required=True)
    args = parser.parse_args(argv)
    try:
        result = check_git_receipt(args.receipt, args.state, args.server_finished_at)
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        print(f"Git-sync semantic receipt rejected: {exc}", file=sys.stderr)
        return 1
    print(json.dumps(result, sort_keys=True))
    return 0


class NamedJourneyGuardTests(unittest.TestCase):
    def test_sourced_shell_builder_accepts_empty_named_list_with_nounset(self) -> None:
        script = pathlib.Path(__file__).with_name("coverage-journeys.sh")
        completed = subprocess.run(
            [
                "bash",
                "-c",
                'set -u; source "$1"; coverage_journey_build_named_args "/tmp/lane log"; printf "%s\\n" "${COVERAGE_JOURNEY_NAMED_ARGS[@]}"',
                "coverage-named-args-test",
                str(script),
            ],
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertEqual(completed.stdout.splitlines(), ["--log", "/tmp/lane log"])

    def _actual_lane_selection(self) -> dict[str, dict[str, object]]:
        # Capture the actual dispatch arguments; declaration placement and lane
        # order do not affect the selector/floor/required-name contract.
        shell_script = pathlib.Path(__file__).with_name("coverage-journeys.sh")
        script = r'''set -eu
source "$1"
ARTIFACTS="$2"
RAW="$ARTIFACTS/raw"
ID=selection-fixture
CONTAINER_CLI=docker
PLATFORM=linux/amd64
IMAGE_ID=sha256:fixture-image
IMAGE_PROVENANCE=built-by-this-run
IMAGE_VERIFIED=true
CANDIDATE_SHA=fixture-candidate
COVERAGE_SELECTION_CAPTURED=0
unset CAESIUM_DISTRIBUTED_INTEGRATION_MIN_PASS CAESIUM_OWNER_MEMORY_INTEGRATION_MIN_PASS
docker() {
  case "$1" in
    create) printf '%064d\n' 1 ;;
    cp) [[ "$#" -eq 3 ]] && printf 'fixture CLI\n' > "$3" ;;
    *) printf 'unexpected container command: %s\n' "$1" >&2; return 1 ;;
  esac
}
coverage_journey_track_id() { [[ "$1" == "$cli_ctr" ]]; }
coverage_journey_remove_owned() { [[ "$1" == "$cli_ctr" ]]; }
coverage_journey_run_lane() { printf 'unexpected native lane dispatch\n' >&2; return 1; }
python3() {
  [[ "$#" -eq 2 && "$1" == - && "$2" == "$cli_dir/caesium" ]] || {
    printf 'unexpected journey driver\n' >&2; return 1
  }
  command python3 "$@"
}
coverage_journey_run_parallel_pair() {
  shift # Pair identity does not choose the tests or floors.
  local lane argc name
  while (($#)); do
    [[ "$#" -ge 2 ]] || return 1
    lane="$1" argc="$2"
    shift 2
    [[ "$argc" =~ ^[0-9]+$ ]] && ((argc >= 6 && argc <= $#)) || return 1
    local -a command=("${@:1:argc}")
    [[ "${command[0]}" == coverage_journey_run_lane && "${command[1]}" == "$lane" ]] || return 1
    case "$lane" in
      local|auth)
        printf '%s\t%s\t%s\n' "$lane" "${command[3]}" "${command[4]}"
        for name in "${command[@]:6}"; do printf '%s-name\t%s\n' "$lane" "$name"; done
        COVERAGE_SELECTION_CAPTURED=$((COVERAGE_SELECTION_CAPTURED + 1))
        ;;
    esac
    shift "$argc"
  done
  # Stop before any native lane or later SSO/Git driver executes.
  ((COVERAGE_SELECTION_CAPTURED < 2))
}
if run_coverage_journeys; then
  printf 'collector did not stop at captured dispatch\n' >&2
  exit 1
fi
[[ "$COVERAGE_SELECTION_CAPTURED" -eq 2 ]]
'''
        with tempfile.TemporaryDirectory(prefix="coverage-lane-selection-") as directory:
            result = subprocess.run(
                ["bash", "-c", script, "coverage-selection-test", str(shell_script), directory],
                capture_output=True, text=True, check=False,
            )
        self.assertEqual(result.returncode, 0, result.stderr)
        lanes = {"local": {"names": []}, "auth": {"names": []}}
        for line in result.stdout.splitlines():
            fields = line.split("\t")
            if fields[0].endswith("-name"):
                lanes[fields[0].removesuffix("-name")]["names"].append(fields[1])
            else:
                lanes[fields[0]].update(pattern=fields[1], minimum=int(fields[2]))
        return lanes

    def test_floor_gap_journeys_preserve_original_selection_and_floors(self) -> None:
        lanes = self._actual_lane_selection()
        self.assertEqual(lanes["local"]["minimum"], 39)
        self.assertEqual(lanes["auth"]["minimum"], 21)
        original_local = {
            "TestCaesiumWhyExplainsHitAndMiss", "TestRunDiffAttributesChangedField",
            "TestRunDiffRESTEndpointCoversHTTPSurface", "TestReproducibilityReceiptRoundTrip",
            "TestContractGraphCLIJSONReportsInferredEdge", "TestContractsGraphEndpointReportsInferredEdgeAndFeatureFlag",
            "TestJobLintServerJSONReportsContractFinding", "TestContractCheckFailsOnBreakingLocalChange",
            "TestCheckImagesCLIGatesLocalDockerAvailability", "TestAtomSpecPersistence",
            "TestDatasetRESTAndCLIListSurfacesManualAdvance", "TestIncidentRoutesGatedOffByDefault",
            "TestDevOnceExecutesDAG", "TestDevOnceRunTimeoutStopsAndRemovesContainer",
            "TestDevOnceSIGINTStopsAndRemovesContainer", "TestTestCommandValidatesDefinitions",
            "TestTestCommandRunsHarnessScenarioWithObservabilityAssertions",
            "TestFreshnessCronTickSkipsFreshOutput", "TestAutomaticRetryDelayConstantAndBackoff",
        }
        original_auth = {
            "TestIncidentCLIListJSONStdout", "TestIncidentApprovalDecisionsCLI",
            "TestIncidentApprovalWhyExplains", "TestIncidentEscalationDeliversNotifiableEvent",
            "TestIncidentOpenedEventObservable", "TestIncidentApplyJobdefPatchCannotEditItsOwnPolicy",
            "TestIncidentApprovalSecondPendingRequestStaysDecidable", "TestIncidentPerClassNarrowingGatesTheMatchingClass",
            "TestIncidentPerClassNarrowingIgnoresNonMatchingClass", "TestScopedKeyWhoamiAllowed",
            "TestScopedKeyAllowDenyMatrix", "TestHoldDatasetCLIReleaseReopensTheGate", "TestIncidentBundleFromRealFailure",
        }
        added = {
            "local": {"TestCacheCLIListsInvalidatesAndPrunes", "TestNativeTaskSIGTERMResultClassification",
                      "TestJobSchemaNamespaceDocumentation", "TestCacheNamespaceMove"},
            "auth": {"TestPublicListingsOrderByAndRefuseInvalidTerms", "TestIncidentNamespaceLifecycle"},
        }
        for lane, original in (("local", original_local), ("auth", original_auth)):
            self.assertEqual(set(lanes[lane]["names"]), original | added[lane])
            self.assertEqual(len(lanes[lane]["names"]), len(original | added[lane]))
            for name in lanes[lane]["names"]:
                self.assertIsNotNone(re.fullmatch(lanes[lane]["pattern"], SUITE + "/" + name))
            other_lane = "auth" if lane == "local" else "local"
            for name in added[other_lane]:
                self.assertIsNone(re.fullmatch(lanes[lane]["pattern"], SUITE + "/" + name))

    def test_floor_gap_admission_refuses_skip_nested_duplicate_or_missing(self) -> None:
        lanes = self._actual_lane_selection()
        new = {"local": ["TestCacheCLIListsInvalidatesAndPrunes", "TestNativeTaskSIGTERMResultClassification",
                         "TestJobSchemaNamespaceDocumentation", "TestCacheNamespaceMove"],
               "auth": ["TestPublicListingsOrderByAndRefuseInvalidTerms", "TestIncidentNamespaceLifecycle"]}
        for lane, names in new.items():
            required = lanes[lane]["names"]
            lines = {name: "    --- PASS: " + SUITE + "/" + name + " (0.01s)\n" for name in required}
            complete = "".join(lines.values())
            self.assertTrue(check_output(complete, required)["valid"])
            for name in names:
                refusals = {
                    "missing": "", "skip": lines[name].replace("PASS", "SKIP"),
                    "nested": lines[name].replace(name + " (", name + "/case ("),
                    "lookalike": lines[name].replace(name + " (", name + "Extra ("),
                    "duplicate": lines[name] + lines[name],
                }
                for control, replacement in refusals.items():
                    with self.subTest(lane=lane, name=name, control=control):
                        result = check_output(complete.replace(lines[name], replacement), required)
                        self.assertFalse(result["valid"])

    def test_nested_pass_cannot_replace_missing_top_level_pass(self) -> None:
        result = check_output(
            "    --- PASS: TestIntegrationTestSuite/TestRequired/subtest (0.00s)\n",
            ["TestRequired"],
        )
        self.assertEqual(result["missing"], ["TestRequired"])
        self.assertFalse(result["valid"])

    def test_exact_top_level_pass_is_accepted(self) -> None:
        result = check_output(
            "    --- PASS: TestIntegrationTestSuite/TestRequired (0.01s)\n",
            ["TestRequired"],
        )
        self.assertEqual(result["passed"], ["TestRequired"])
        self.assertTrue(result["valid"])

    def test_skip_is_not_a_pass(self) -> None:
        result = check_output(
            "    --- SKIP: TestIntegrationTestSuite/TestRequired (0.00s)\n",
            ["TestRequired"],
        )
        self.assertEqual(result["skipped"], ["TestRequired"])
        self.assertFalse(result["valid"])

    def test_lookalike_name_does_not_match(self) -> None:
        result = check_output(
            "    --- PASS: TestIntegrationTestSuite/TestRequiredExtra (0.00s)\n",
            ["TestRequired"],
        )
        self.assertEqual(result["missing"], ["TestRequired"])
        self.assertFalse(result["valid"])

    def test_duplicate_top_level_pass_is_ambiguous(self) -> None:
        line = "--- PASS: TestIntegrationTestSuite/TestRequired (0.00s)\n"
        result = check_output(line + line, ["TestRequired"])
        self.assertEqual(result["duplicate"], ["TestRequired"])
        self.assertFalse(result["valid"])

    def test_git_receipt_requires_source_scoped_prune_and_terminal_times(self) -> None:
        state = {
            "schema_version": 1,
            "alias": "coverage-git-run-1",
            "source_id": "coverage-git-run-1",
            "url": "git://coverage-git-run-1:9418/coverage.git",
            "ref": "main",
            "path": "jobs/imported.job.yaml",
            "image": "alpine:3.23",
            "initial_commit": "1" * 40,
            "git_version": "git version 2.47.0",
        }
        receipt = {
            "schema_version": 1,
            "source": state,
            "initial_commit": "1" * 40,
            "updated_commit": "2" * 40,
            "deleted_commit": "3" * 40,
            "job_id": "00000000-0000-4000-8000-000000000001",
            "sentinel_id": "00000000-0000-4000-8000-000000000002",
            "run_id": "00000000-0000-4000-8000-000000000003",
            "run_status": "succeeded",
            "run_completed_at": "2026-10-04T16:00:00Z",
            "task_completed_at": "2026-10-04T16:00:00Z",
            "output_marker": "updated",
            "imported_job_absent": True,
            "sentinel_survived": True,
        }
        with mock.patch("pathlib.Path.read_bytes") as read_bytes:
            read_bytes.side_effect = [json.dumps(receipt).encode(), json.dumps(state).encode()]
            with mock.patch("pathlib.Path.is_symlink", return_value=False):
                result = check_git_receipt(pathlib.Path("receipt"), pathlib.Path("state"), "2026-10-04T16:00:01Z")
        self.assertTrue(result["valid"])

    def test_git_receipt_rejects_prune_without_sentinel_survival(self) -> None:
        state = {
            "schema_version": 1,
            "alias": "coverage-git-run-1",
            "source_id": "coverage-git-run-1",
            "url": "git://coverage-git-run-1:9418/coverage.git",
            "ref": "main",
            "path": "jobs/imported.job.yaml",
            "image": "alpine:3.23",
            "initial_commit": "1" * 40,
            "git_version": "git version 2.47.0",
        }
        receipt = {
            "schema_version": 1,
            "source": state,
            "initial_commit": "1" * 40,
            "updated_commit": "2" * 40,
            "deleted_commit": "3" * 40,
            "job_id": "00000000-0000-4000-8000-000000000001",
            "sentinel_id": "00000000-0000-4000-8000-000000000002",
            "run_id": "00000000-0000-4000-8000-000000000003",
            "run_status": "succeeded",
            "run_completed_at": "2026-10-04T16:00:00Z",
            "task_completed_at": "2026-10-04T16:00:00Z",
            "output_marker": "updated",
            "imported_job_absent": True,
            "sentinel_survived": False,
        }
        with mock.patch("pathlib.Path.read_bytes") as read_bytes:
            read_bytes.side_effect = [json.dumps(receipt).encode(), json.dumps(state).encode()]
            with mock.patch("pathlib.Path.is_symlink", return_value=False):
                with self.assertRaisesRegex(ValueError, "source-scoped prune"):
                    check_git_receipt(pathlib.Path("receipt"), pathlib.Path("state"), "2026-10-04T16:00:01Z")


PREP_LIFECYCLE_HARNESS = r"""set -u
source "$1"
ARTIFACTS="$2"
FAKE_FIXTURE="$3"
FAKE_MODE="$4"
CANDIDATE_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
ID=run-prep-lifecycle
BUILDER_RUN_IMAGE=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
PLATFORM=linux/arm64
CONTAINER_CLI=docker
COVERAGE_JOURNEY_PREP_POLL_INTERVAL=0
COVERAGE_JOURNEY_PREP_CLEANUP_POLL_LIMIT=2
COVERAGE_JOURNEY_GIT_TEMP_DIRS=("$FAKE_FIXTURE")
FAKE_PRESENT=false
FAKE_RUNNING=false
FAKE_STATUS=created
FAKE_EXIT=0
FAKE_NAME=
FAKE_LANE=
FAKE_ID=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
FAKE_REMOVE_FAIL=false
[[ "$FAKE_MODE" == remove-fail ]] && FAKE_REMOVE_FAIL=true
log() { :; }
coverage_journey_fail() { return 1; }
docker() {
  local joined=" $* "
  case "$1" in
    run)
      [[ "$joined" == *" --pull=never "* && "$joined" == *"--label caesium.coverage.owner=$CANDIDATE_SHA"* \
        && "$joined" == *"--label caesium.coverage.run=$ID"* && "$joined" == *" --name "* \
        && "$joined" != *" --rm "* && "$joined" == *" $BUILDER_RUN_IMAGE "* \
        && ${#COVERAGE_JOURNEY_PREP_PENDING_NAMES[@]} -eq 1 ]] || return 91
      shift
      while (($#)); do
        case "$1" in
          --name) FAKE_NAME="$2"; shift 2 ;;
          --label)
            case "$2" in
              caesium.coverage.lane=*) FAKE_LANE="$(printf '%s' "$2" | sed 's/^[^=]*=//')" ;;
            esac
            shift 2
            ;;
          *) shift ;;
        esac
      done
      FAKE_PRESENT=true
      FAKE_RUNNING=true
      FAKE_STATUS=running
      FAKE_EXIT=0
      if [[ "$FAKE_MODE" == complete || "$FAKE_MODE" == remove-fail ]]; then
        FAKE_RUNNING=false
        FAKE_STATUS=exited
      fi
      if [[ "$FAKE_MODE" == interrupt ]]; then kill -TERM "$$"; fi
      if [[ "$FAKE_MODE" == late-fail ]]; then return 47; fi
      printf '%s\n' "$FAKE_ID"
      ;;
    stop)
      [[ "$4" == "$FAKE_ID" ]] || return 92
      FAKE_RUNNING=false
      FAKE_STATUS=exited
      FAKE_EXIT=143
      ;;
    logs)
      [[ "$2" == "$FAKE_ID" ]] || return 93
      printf 'fixture preparation output\n'
      ;;
    *) return 94 ;;
  esac
}
coverage_journey_resource() {
  local action="$1" kind="$2" reference="$3" image="" lane=""
  if (($# >= 4)); then image="$4"; fi
  if (($# >= 5)); then lane="$5"; fi
  case "$action" in
    absent)
      [[ "$kind" == container && "$FAKE_PRESENT" == false ]] || return 1
      printf '{"absent":true}\n'
      ;;
    owned)
      [[ "$kind" == container && "$FAKE_PRESENT" == true && "$image" == "$BUILDER_RUN_IMAGE" \
        && "$lane" == "$FAKE_LANE" && ( "$reference" == "$FAKE_NAME" || "$reference" == "$FAKE_ID" ) ]] || return 1
      local running_json=false finished_at=2026-10-04T16:00:00Z
      if [[ "$FAKE_RUNNING" == true ]]; then running_json=true; finished_at=0001-01-01T00:00:00Z; fi
      printf '{"Id":"%s","Image":"%s","Config":{"Labels":{"caesium.coverage.owner":"%s","caesium.coverage.run":"%s","caesium.coverage.lane":"%s"}},"State":{"Running":%s,"Status":"%s","ExitCode":%s,"OOMKilled":false,"FinishedAt":"%s"},"RestartCount":0}\n' \
        "$FAKE_ID" "$BUILDER_RUN_IMAGE" "$CANDIDATE_SHA" "$ID" "$FAKE_LANE" "$running_json" "$FAKE_STATUS" "$FAKE_EXIT" "$finished_at"
      ;;
    remove)
      [[ "$kind" == container && "$reference" == "$FAKE_ID" && "$image" == "$BUILDER_RUN_IMAGE" \
        && "$lane" == "$FAKE_LANE" && "$FAKE_RUNNING" == false && "$FAKE_REMOVE_FAIL" == false ]] || return 1
      FAKE_PRESENT=false
      printf '{"absent":true,"Id":"%s"}\n' "$FAKE_ID"
      ;;
    *) return 95 ;;
  esac
}
mkdir -p "$ARTIFACTS/journeys"
if [[ "$FAKE_MODE" == complete ]]; then
  COVERAGE_JOURNEY_GIT_TEMP_DIRS=()
  coverage_journey_register_git_dir "$FAKE_FIXTURE" || exit 88
fi
log_path="$ARTIFACTS/journeys/preparation.log"
if coverage_journey_with_prep_signal_cleanup \
  coverage_journey_run_builder_prep run-prep-lifecycle git-prep-test "$log_path" 1 \
  --network none "$BUILDER_RUN_IMAGE" sh -c true; then
  prep_rc=0
else
  prep_rc=$?
fi
cleanup_coverage_journeys || true
if [[ "$FAKE_MODE" == complete ]]; then
  [[ "$prep_rc" -eq 0 && "$COVERAGE_JOURNEY_PREP_FAILED" == false \
    && "$FAKE_PRESENT" == false && ! -e "$FAKE_FIXTURE" \
    && -f "$ARTIFACTS/journeys/preparation.log" ]] || exit 80
  exit 0
fi
[[ "$prep_rc" -ne 0 && "$COVERAGE_JOURNEY_PREP_FAILED" == true ]] || exit 81
[[ -d "$FAKE_FIXTURE" && -f "$ARTIFACTS/retained-owned-git-fixture-paths.txt" \
  && -f "$ARTIFACTS/retained-owned-git-preparation.txt" ]] || exit 82
grep -Fxq "fixture_path=$FAKE_FIXTURE" "$ARTIFACTS/retained-owned-git-preparation.txt" || exit 83
if [[ "$FAKE_MODE" == remove-fail ]]; then
  [[ "$FAKE_PRESENT" == true ]] || exit 84
  grep -Fxq "$FAKE_ID"$'\t'git-prep-test "$ARTIFACTS/retained-owned-preparation-container-ids.txt" || exit 85
else
  [[ "$FAKE_PRESENT" == false ]] || exit 86
fi
if [[ "$FAKE_MODE" == interrupt ]]; then
  grep -Fxq 'signal=TERM' "$ARTIFACTS/retained-owned-git-preparation.txt" || exit 87
fi
"""


class BuilderPreparationLifecycleTests(unittest.TestCase):
    def test_successful_preparation_is_checked_removed_before_fixture_cleanup(self) -> None:
        with tempfile.TemporaryDirectory(prefix="coverage-preparation-") as temporary:
            root = pathlib.Path(temporary)
            artifacts = root / "artifacts"
            artifacts.mkdir()
            (artifacts / "journeys").mkdir()
            fixture = artifacts / "journeys" / "git-source-fixture-run-prep-lifecycle.abc123"
            fixture.mkdir()
            artifacts = artifacts.resolve()
            fixture = fixture.resolve()
            shell_script = pathlib.Path(__file__).with_name("coverage-journeys.sh")
            completed = subprocess.run(
                [
                    "bash",
                    "-c",
                    PREP_LIFECYCLE_HARNESS,
                    "coverage-preparation-test",
                    str(shell_script),
                    str(artifacts),
                    str(fixture),
                    "complete",
                ],
                check=False,
                capture_output=True,
                text=True,
                timeout=20,
            )
            self.assertEqual(completed.returncode, 0, completed.stderr + completed.stdout)

    def _exercise_failure(self, mode: str) -> None:
        with tempfile.TemporaryDirectory(prefix="coverage-preparation-") as temporary:
            root = pathlib.Path(temporary)
            artifacts = root / "artifacts"
            fixture = root / "fixture"
            artifacts.mkdir()
            fixture.mkdir()
            shell_script = pathlib.Path(__file__).with_name("coverage-journeys.sh")
            completed = subprocess.run(
                [
                    "bash",
                    "-c",
                    PREP_LIFECYCLE_HARNESS,
                    "coverage-preparation-test",
                    str(shell_script),
                    str(artifacts),
                    str(fixture),
                    mode,
                ],
                check=False,
                capture_output=True,
                text=True,
                timeout=20,
            )
            self.assertEqual(completed.returncode, 0, completed.stderr + completed.stdout)

    def test_interrupted_late_allocation_is_joined_before_fixture_retention(self) -> None:
        self._exercise_failure("interrupt")

    def test_failed_run_with_late_allocation_is_reconciled_by_name(self) -> None:
        self._exercise_failure("late-fail")

    def test_wait_timeout_joins_owned_preparation_before_retaining_paths(self) -> None:
        self._exercise_failure("wait")

    def test_remove_failure_cannot_pass_and_retains_identity_and_fixture(self) -> None:
        self._exercise_failure("remove-fail")


def main() -> int:
    if len(sys.argv) == 2 and sys.argv[1] == "--self-test":
        suite = unittest.defaultTestLoader.loadTestsFromModule(sys.modules[__name__])
        result = unittest.TextTestRunner(verbosity=2).run(suite)
        return 0 if result.wasSuccessful() else 1
    if len(sys.argv) >= 2 and sys.argv[1] == "validate-git-receipt":
        return _run_git_receipt(sys.argv[2:])
    return _run_check(sys.argv[1:])


if __name__ == "__main__":
    raise SystemExit(main())
