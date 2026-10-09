"""Regression checks for the CI gate, path selection and full-suite wiring."""

import argparse
import base64
import copy
import io
import fnmatch
import hashlib
import itertools
import json
import os
from pathlib import Path
import re
import runpy
import shlex
import shutil
import subprocess
import sys
import tempfile
import unittest
import zipfile

import yaml


ROOT = Path(__file__).resolve().parents[1]
WORKFLOW = yaml.safe_load((ROOT / ".github/workflows/ci.yml").read_text())
JOBS = WORKFLOW["jobs"]
# PyYAML parses the unquoted `on:` key as the boolean True (YAML 1.1), not the
# string "on" -- this is the workflow's declared trigger map.
TRIGGERS = WORKFLOW[True]
# distributed-testing G4: the qualification lanes live once in a reusable
# workflow, called by CI's release chain and by the nightly schedule.
LANES_WORKFLOW = yaml.safe_load((ROOT / ".github/workflows/qualification-lanes.yml").read_text())
LANE_JOBS = LANES_WORKFLOW["jobs"]
NIGHTLY_WORKFLOW = yaml.safe_load((ROOT / ".github/workflows/testing-qualification.yml").read_text())
NIGHTLY_JOBS = NIGHTLY_WORKFLOW["jobs"]
QUALIFICATION = runpy.run_path(str(ROOT / "scripts/qualification-gate.py"))
QUAL_LANES = QUALIFICATION["LANES"]
CI_OK = runpy.run_path(str(ROOT / "scripts/ci-ok.py"))
SELECTORS = CI_OK["SELECTORS"]
EVIDENCE_JOB = CI_OK["EVIDENCE_JOB"]
EVIDENCE_GATE = CI_OK["EVIDENCE_GATE"]
EVIDENCE_LANES = CI_OK["EVIDENCE_LANES"]
UNPROMOTED_LANES = CI_OK["UNPROMOTED_LANES"]
# distributed-testing G6 promoted lanes (every evidence lane but G5's).
G6_LANES = {job: gate for job, gate in EVIDENCE_LANES.items() if job != EVIDENCE_JOB}
RECOGNIZED_EVENTS = CI_OK["RECOGNIZED_EVENTS"]
VERIFY_CANDIDATE_IDENTITY = CI_OK["verify_candidate_identity"]
VERIFY_BASE_FRESHNESS = CI_OK["verify_base_freshness"]
VERIFY_PULL_REQUEST_CANDIDATE_PARENTS = CI_OK["verify_pull_request_candidate_parents"]
FLAGS = ("go", "ui", "helm", "reagents", "ci")
MANIFEST_PATH = ROOT / "test/contracts/scenarios.json"
MANIFEST = json.loads(MANIFEST_PATH.read_text())
CANDIDATE_SHA = "b3f1c0de" + "0" * 32
CANDIDATE_DIGEST = "sha256:" + "cd" * 32
CANDIDATE_BASE_SHA = "base0000" + "0" * 32
CANDIDATE_HEAD_SHA = "head0000" + "0" * 32


def gated_scenarios(manifest=None, gate=EVIDENCE_GATE):
    return [item for item in (manifest or MANIFEST)["scenarios"]
            if gate in (item.get("gates") or [])]


def evidence_report(sha=CANDIDATE_SHA, digest=CANDIDATE_DIGEST, gate=EVIDENCE_GATE):
    """A report of the shape the lane uploads, rebuilt from the real manifest.

    Every observed value is echoed from the committed manifest row, so this
    fixture cannot drift away from what the gate demands.
    """
    scenarios = []
    for item in gated_scenarios(gate=gate):
        evidence = item.get("evidence") or {}
        entry = {
            "id": item["id"],
            "status": "pass",
            "topology": dict(item["topology"]),
            "mode": dict(item["mode"]),
            "feature_flags": dict(item["feature_flags"]),
            "artifact_identity": evidence.get("artifact_identity", ""),
            "observations": [obs["id"] for obs in item["expected_observations"]],
            "recorder": {"present": True, "sample_count": max(evidence.get("min_samples", 0), 1)},
            "checker": {"timed_out": False},
        }
        if evidence.get("require_candidate_sha"):
            entry["candidate_sha"] = sha
        if evidence.get("require_candidate_digest"):
            entry["candidate_digest"] = digest
        fault = item.get("fault_activation") or {}
        if fault.get("required"):
            entry["fault_activation"] = {
                "activated": True,
                "kind": fault["kind"],
                "observations": list(fault["required_observations"]),
            }
        scenarios.append(entry)
    return {
        "candidate_sha": sha,
        "candidate_digest": digest,
        "gate_enabled": True,
        "disabled_gates": [],
        "scenarios": scenarios,
    }


_LANE_REPORT_DIR = tempfile.TemporaryDirectory()
_LANE_REPORTS = {}


def honest_lane_reports(sha=CANDIDATE_SHA):
    """One honest report per promoted G6 lane, shaped like its upload."""
    if sha not in _LANE_REPORTS:
        root = Path(_LANE_REPORT_DIR.name) / sha
        reports = {}
        for job, gate in G6_LANES.items():
            path = root / job / "evidence.json"
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(json.dumps(evidence_report(sha=sha, gate=gate)))
            reports[job] = path
        _LANE_REPORTS[sha] = reports
    return dict(_LANE_REPORTS[sha])


def gate_command(report, manifest=MANIFEST_PATH, sha=CANDIDATE_SHA, jobs=None,
                  event_name="pull_request", base_sha=CANDIDATE_BASE_SHA,
                  head_sha=CANDIDATE_HEAD_SHA, current_base_sha="",
                  candidate_parents=f"{CANDIDATE_BASE_SHA} {CANDIDATE_HEAD_SHA}",
                  lane_reports=None):
    # Defaults describe a well-formed pull_request candidate identity --
    # including two matching git parents, [base, head] -- so every existing
    # evidence-focused test keeps exercising exactly the evidence wiring it
    # did before G7 added these flags; tests that care about identity/
    # freshness/parents override the relevant keyword. G6's promoted lanes
    # get honest reports for the tested SHA unless a test passes its own.
    lanes = honest_lane_reports(sha) if lane_reports is None else lane_reports
    lane_args = []
    for job, path in lanes.items():
        lane_args += ["--lane-evidence", f"{job}={path}"]
    return [
        sys.executable, str(ROOT / "scripts/ci-ok.py"),
        "--evidence-report", str(report),
        "--evidence-manifest", str(manifest),
        *lane_args,
        "--candidate-sha", sha,
        "--event-name", event_name,
        "--base-sha", base_sha,
        "--head-sha", head_sha,
        "--current-base-sha", current_base_sha,
        "--candidate-parents", candidate_parents,
        *(JOBS["ci-ok"]["needs"] if jobs is None else jobs),
    ]


def selected_outputs(enabled=()):
    outputs = {name: str(name in enabled).lower() for name in FLAGS}
    outputs["images"] = str(bool(enabled)).lower()
    return outputs


def results(outputs):
    needs = {}
    for name in JOBS["ci-ok"]["needs"]:
        selectors = SELECTORS[name]
        selected = not selectors or any(outputs[key] == "true" for key in selectors)
        needs[name] = {"result": "success" if selected else "skipped"}
    needs["changes"]["outputs"] = outputs
    return needs


def browser_nsenter_invocation(script):
    """Parse the parent nsenter options, stopping before its child command."""
    tokens = shlex.split(script.replace("\\\n", " "))
    # The executable also appears as the argument to `test -x`; an invocation
    # must begin with options so it selects the intended network namespace.
    calls = [i for i, token in enumerate(tokens[:-1])
             if token == "$nsenter_bin" and tokens[i + 1].startswith("-")]
    if len(calls) != 1:
        raise AssertionError("expected one nsenter invocation")
    index = calls[0] + 1
    namespaces = []
    targets = []
    while index < len(tokens) and tokens[index].startswith("-"):
        option = tokens[index]
        index += 1
        if option == "--":
            break
        if option in ("--net", "-n"):
            namespaces.append("net")
        elif option in ("--target", "-t"):
            if index >= len(tokens):
                raise AssertionError("nsenter target is missing")
            targets.append(tokens[index])
            index += 1
        elif option.startswith("--target="):
            targets.append(option.split("=", 1)[1])
        else:
            raise AssertionError(f"unexpected nsenter option: {option}")
    if index >= len(tokens):
        raise AssertionError("nsenter child command is missing")
    return namespaces, targets, tokens[index]


class GateTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.evidence = Path(tmp.name) / "evidence.json"
        self.evidence.write_text(json.dumps(evidence_report()))

    def gate(self, needs, expected):
        result = subprocess.run(
            gate_command(self.evidence),
            env={**os.environ, "NEEDS_JSON": json.dumps(needs)},
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.returncode, expected, result.stdout + result.stderr)

    def test_every_change_combination(self):
        for bits in itertools.product((False, True), repeat=len(FLAGS)):
            enabled = [name for name, bit in zip(FLAGS, bits) if bit]
            with self.subTest(enabled=enabled):
                self.gate(results(selected_outputs(enabled)), 0)

    def test_failed_cancelled_missing_and_unexpectedly_skipped_jobs(self):
        baseline = results(selected_outputs(FLAGS))
        for name in baseline:
            for status in ("failure", "cancelled", "skipped", "unknown", None):
                with self.subTest(job=name, status=status):
                    needs = copy.deepcopy(baseline)
                    if status is None:
                        del needs[name]
                    else:
                        needs[name]["result"] = status
                    self.gate(needs, 1)

    def test_failed_producer_cannot_hide_behind_skipped_consumers(self):
        needs = results(selected_outputs(("ui",)))
        needs["images"]["result"] = "failure"
        needs["ui-e2e"]["result"] = "skipped"
        needs["ui-e2e-auth"]["result"] = "skipped"
        self.gate(needs, 1)

    def test_invalid_inputs(self):
        for needs in (None, [], {}, {"changes": "success"}):
            self.gate(needs, 1)
        for outputs in ({}, selected_outputs() | {"images": "true"}, selected_outputs() | {"go": None}):
            needs = results(selected_outputs())
            needs["changes"]["outputs"] = outputs
            self.gate(needs, 1)


class WorkflowTests(unittest.TestCase):
    def test_ci_config_runs_replacement_membership_regression(self):
        commands = [line.strip() for step in JOBS["ci-config"]["steps"]
                    for line in step.get("run", "").splitlines()]
        self.assertIn("bash scripts/test_helm_pod_replacement_membership.sh", commands)

    def test_ci_config_discovers_all_validator_tests(self):
        # The complete hosted suite already took 294s before wrapper/action
        # overhead; retain every test while providing a bounded ten-minute job.
        self.assertEqual(JOBS["ci-config"]["timeout-minutes"], 10)
        commands = [line.strip() for step in JOBS["ci-config"]["steps"]
                    for line in step.get("run", "").splitlines()
                    if "unittest discover" in line]
        self.assertEqual(commands, ["python3 -m unittest discover -s scripts -p 'test_*.py' -v"])
        # Execute the workflow's selector on an additional validator module. A
        # narrowed pattern would silently miss its failure and return success.
        with tempfile.TemporaryDirectory() as tmp:
            (Path(tmp) / "test_extra_validator.py").write_text(
                "import unittest\n"
                "class ExtraValidator(unittest.TestCase):\n"
                "    def test_failure(self):\n"
                "        self.fail('extra validator was discovered')\n"
            )
            command = shlex.split(commands[0])
            command[0] = sys.executable
            command[command.index("-s") + 1] = tmp
            result = subprocess.run(command, capture_output=True, text=True)
            self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
            self.assertIn("extra validator was discovered", result.stderr)
            self.assertIn("Ran 1 test", result.stderr)

    def test_browser_diagnostics_survive_setup_and_test_failures(self):
        for name, container in (("ui-e2e", "caesium-server"),
                                ("ui-e2e-auth", "caesium-server-auth")):
            with self.subTest(job=name):
                steps = JOBS[name]["steps"]
                collect = next(step for step in steps if step.get("name") == "Collect browser diagnostics")
                upload = next(step for step in steps if step.get("name") == "Upload browser diagnostics")
                cleanup = steps[-1]
                browser = next(step for step in steps if step.get("id") == "playwright")
                self.assertLess(steps.index(browser), steps.index(collect))
                self.assertLess(steps.index(collect), steps.index(upload))
                self.assertLess(steps.index(upload), steps.index(cleanup))
                for step in (collect, upload, cleanup):
                    self.assertEqual(step["if"], "always()")
                for step in (browser, collect, upload):
                    self.assertFalse(step.get("continue-on-error", False))
                self.assertEqual(upload["uses"], "actions/upload-artifact@v7")
                self.assertEqual(upload["with"]["if-no-files-found"], "error")
                self.assertEqual(upload["with"]["path"], "ui/ci-artifacts/")
                self.assertIn(name, upload["with"]["name"])
                # Run the real collection script with a failed setup and a
                # missing server, then with a failed test and retained logs.
                for setup_failed in (False, True):
                    with self.subTest(setup_failed=setup_failed), tempfile.TemporaryDirectory() as tmp:
                        root = Path(tmp)
                        docker = root / "docker"
                        docker.write_text(
                            '#!/bin/sh\n'
                            'if [ "$1" = logs ]; then\n'
                            '  echo "server diagnostic for $2 bootstrap csk_fixture-secret_123"\n'
                            f'  exit {1 if setup_failed else 0}\n'
                            'fi\necho "container status"\n'
                        )
                        docker.chmod(0o755)
                        trace = io.BytesIO()
                        with zipfile.ZipFile(trace, "w", zipfile.ZIP_DEFLATED) as archive:
                            archive.writestr("0-trace.network", json.dumps({
                                "headers": {"Authorization": "Bearer csk_trace-secret_456"},
                            }))
                            archive.writestr("0-trace.trace", '{"type":"context-options"}\n')
                            archive.writestr("resources/page.html", "<p>csk_snapshot-secret_789</p>")
                            archive.writestr("resources/image.png", b"\x89PNG\r\n\x1a\n")
                        (root / "ui/test-results").mkdir(parents=True)
                        (root / "ui/test-results/trace.zip").write_bytes(trace.getvalue())
                        (root / "ui/playwright-report").mkdir()
                        (root / "ui/playwright-report/index.html").write_text(
                            '<template id="playwrightReportBase64">data:application/zip;base64,'
                            + base64.b64encode(trace.getvalue()).decode() + '</template>'
                        )
                        (root / "ui/playwright-results.json").write_text(
                            '{"error":"csk_json-secret_123","status":"flaky"}'
                        )
                        outcomes = {
                            "server": {"outcome": "failure" if setup_failed else "success",
                                       "conclusion": "failure" if setup_failed else "success",
                                       "outputs": {"sensitive": "must not appear"}},
                            "playwright": {"outcome": "skipped" if setup_failed else "failure",
                                           "conclusion": "skipped" if setup_failed else "failure"},
                        }
                        result = subprocess.run(
                            ["bash", "-e", "-o", "pipefail", "-c", collect["run"]], cwd=tmp,
                            env={**os.environ, "PATH": tmp + os.pathsep + os.environ["PATH"],
                                 "STEP_RESULTS": json.dumps(outcomes), "CANDIDATE_SHA": "candidate",
                                 "GITHUB_RUN_ID": "123", "GITHUB_RUN_ATTEMPT": "2", "GITHUB_JOB": name},
                            capture_output=True, text=True,
                        )
                        self.assertEqual(result.returncode, int(setup_failed), result.stderr)
                        artifacts = root / "ui/ci-artifacts"
                        reports = artifacts / "ci-diagnostics"
                        report = json.loads((reports / "outcomes.json").read_text())
                        self.assertEqual(report["candidate_sha"], "candidate")
                        self.assertEqual(report["steps"]["playwright"]["outcome"],
                                         "skipped" if setup_failed else "failure")
                        self.assertNotIn("must not appear", json.dumps(report))
                        server_log = (reports / "server.log").read_text()
                        self.assertIn(container, server_log)
                        self.assertIn("[REDACTED_API_KEY]", server_log)
                        self.assertNotIn("csk_fixture-secret_123", server_log)
                        self.assertIn("container status", (reports / "containers.log").read_text())
                        if setup_failed:
                            self.assertIn("::error::Could not collect", result.stdout)
                        # Both the standalone trace and HTML's embedded ZIP
                        # retain readable entries while removing credentials.
                        html = (artifacts / "playwright-report/index.html").read_text()
                        embedded = re.search(r"base64,([A-Za-z0-9+/=]+)", html)[1]
                        for data in ((artifacts / "test-results/trace.zip").read_bytes(),
                                     base64.b64decode(embedded)):
                            with zipfile.ZipFile(io.BytesIO(data)) as archive:
                                self.assertIsNone(archive.testzip())
                                self.assertEqual(archive.read("resources/image.png"), b"\x89PNG\r\n\x1a\n")
                                for entry in archive.infolist():
                                    self.assertNotIn(b"csk_", archive.read(entry))
                                headers = json.loads(archive.read("0-trace.network"))["headers"]
                                self.assertEqual(headers["Authorization"], "Bearer [REDACTED_API_KEY]")
                        structured = json.loads((artifacts / "playwright-results.json").read_text())
                        self.assertEqual(structured, {"error": "[REDACTED_API_KEY]", "status": "flaky"})

    def test_default_browser_lane_selects_network_recovery_with_dependencies(self):
        setup_node = next(step for step in JOBS["ui-e2e"]["steps"]
                          if step.get("uses") == "actions/setup-node@v6")
        dependencies = next(step for step in JOBS["ui-e2e"]["steps"]
                            if step.get("id") == "dependencies")
        browser = next(step for step in JOBS["ui-e2e"]["steps"]
                       if step.get("id") == "playwright")
        self.assertEqual(setup_node["with"]["node-version"], "22")
        self.assertEqual(setup_node["with"]["cache"], "npm")
        self.assertEqual(setup_node["with"]["cache-dependency-path"], "ui/package-lock.json")
        self.assertEqual(dependencies["working-directory"], "ui")
        self.assertIn("npm ci", dependencies["run"])
        self.assertIn("npx playwright install --with-deps chromium", dependencies["run"])

        self.assertEqual(browser["working-directory"], "ui")
        self.assertEqual(browser["env"]["CI"], "true")
        self.assertEqual(browser["env"]["PLAYWRIGHT_BASE_URL"], "http://127.0.0.1:8080")
        self.assertEqual(browser["env"]["CAESIUM_MANUAL_TRIGGER_API_KEY"], "e2e-test-key")
        self.assertIn("docker inspect --format '{{.State.Pid}}' caesium-server", browser["run"])
        self.assertIn("case \"$server_pid\"", browser["run"])
        self.assertIn('sudo test -e "/proc/$server_pid/ns/net"', browser["run"])
        self.assertIn("runner_uid=\"$(id -u)\"", browser["run"])
        self.assertIn("runner_gid=\"$(id -g)\"", browser["run"])
        self.assertIn("runner_groups=\"$(id -G | tr ' ' ',')\"", browser["run"])
        self.assertIn("runner_home=\"${HOME:?runner HOME is required}\"", browser["run"])
        self.assertIn('runner_path="$PATH"', browser["run"])
        self.assertIn('runner_pwd="$PWD"', browser["run"])
        self.assertIn('host_mount_ns="$(readlink /proc/self/ns/mnt)"', browser["run"])
        self.assertIn("docker inspect --format '{{.ResolvConfPath}}' caesium-server", browser["run"])
        self.assertIn('sudo test -f "$server_resolv_conf"', browser["run"])
        self.assertIn("node_bin=\"$(command -v node)\"", browser["run"])
        self.assertIn("npm_bin=\"$(command -v npm)\"", browser["run"])
        self.assertIn("nsenter_bin=\"$(command -v nsenter)\"", browser["run"])
        self.assertIn("unshare_bin=\"$(command -v unshare)\"", browser["run"])
        self.assertIn("setpriv_bin=\"$(command -v setpriv)\"", browser["run"])
        self.assertIn('require("playwright")', browser["run"])
        self.assertIn('test -x "$browser_bin"', browser["run"])
        self.assertIn("sudo env", browser["run"])
        self.assertIn('HOME="$runner_home"', browser["run"])
        self.assertIn('PATH="$runner_path"', browser["run"])
        self.assertIn('PWD="$runner_pwd"', browser["run"])
        self.assertEqual(browser_nsenter_invocation(browser["run"]),
                         (["net"], ["$server_pid"], "$unshare_bin"))
        self.assertIn('"$unshare_bin" --mount --propagation private', browser["run"])
        self.assertIn('mount --make-rprivate /', browser["run"])
        self.assertIn('mount --bind "$CAESIUM_E2E_SERVER_RESOLV_CONF" /etc/resolv.conf', browser["run"])
        self.assertIn('CAESIUM_E2E_HOST_MOUNT_NS="$host_mount_ns"', browser["run"])
        self.assertIn('test /etc/resolv.conf -ef "$CAESIUM_E2E_SERVER_RESOLV_CONF"', browser["run"])
        self.assertIn('"$CAESIUM_E2E_SETPRIV_BIN" --reuid="$CAESIUM_E2E_RUNNER_UID"', browser["run"])
        self.assertIn('--groups="$CAESIUM_E2E_RUNNER_GROUPS" --inh-caps=-all --ambient-caps=-all', browser["run"])
        self.assertIn('--bounding-set=-all --no-new-privs', browser["run"])
        self.assertIn("PLAYWRIGHT_BASE_URL=\"$PLAYWRIGHT_BASE_URL\"", browser["run"])
        self.assertIn("CAESIUM_MANUAL_TRIGGER_API_KEY=\"$CAESIUM_MANUAL_TRIGGER_API_KEY\"", browser["run"])
        self.assertIn('curl -sf "$PLAYWRIGHT_BASE_URL/health"', browser["run"])
        self.assertIn('for font_host in fonts.googleapis.com fonts.gstatic.com', browser["run"])
        self.assertIn('--input-type=module', browser["run"])
        self.assertIn('const browser = await chromium.launch()', browser["run"])
        self.assertIn('exec "$CAESIUM_E2E_NPM_BIN" run test:e2e -- --project=network-recovery', browser["run"])
        self.assertIn("Host resolver nameservers:", browser["run"])
        self.assertIn("Server resolver nameservers:", browser["run"])
        self.assertNotIn("--network=host", browser["run"])
        self.assertNotIn("mcr.microsoft.com/playwright", browser["run"])

        justfile = (ROOT / "justfile").read_text()
        recipe_start = justfile.index("\nui-e2e:")
        recipe_end = justfile.index("\nui-e2e-auth:", recipe_start)
        recipe = justfile[recipe_start:recipe_end]
        self.assertIn("npm run test:e2e -- --project=network-recovery", recipe)
        self.assertNotIn("--no-deps", recipe)

    def test_browser_nsenter_scope_is_independent_of_option_order(self):
        for arguments in ('--target "$server_pid" --net',
                          '--net --target "$server_pid"',
                          '--net --target="$server_pid"',
                          '-n -t "$server_pid"'):
            with self.subTest(arguments=arguments):
                command = f'test -x "$nsenter_bin"\n"$nsenter_bin" {arguments} "$unshare_bin" --mount --propagation private'
                self.assertEqual(browser_nsenter_invocation(command),
                                 (["net"], ["$server_pid"], "$unshare_bin"))

    def test_browser_nsenter_scope_rejects_extra_namespaces_and_context(self):
        for option in ('--mount', '-m', '--pid', '-p', '--user', '-U',
                       '--root', '-r', '--wd', '-w', '--wdns', '-W',
                       '--all', '-a', '--uts', '-u', '--ipc', '-i',
                       '--cgroup', '-C', '--time', '-T', '--mount=/proc/1/ns/mnt'):
            for arguments in (f'{option} --target "$server_pid" --net',
                              f'--target "$server_pid" --net {option}'):
                with self.subTest(arguments=arguments), self.assertRaises(AssertionError):
                    browser_nsenter_invocation(f'"$nsenter_bin" {arguments} "$unshare_bin" --mount')

    def test_external_font_dns_failure_warns_and_allows_browser_assertions(self):
        browser = next(step for step in JOBS["ui-e2e"]["steps"]
                       if step.get("id") == "playwright")
        probe = re.search(r'^for font_host in .*?^done$', browser["run"], re.MULTILINE | re.DOTALL)
        self.assertIsNotNone(probe)
        with tempfile.TemporaryDirectory() as tmp:
            getent = Path(tmp) / "getent"
            getent.write_text('#!/bin/sh\nexit 2\n')
            getent.chmod(0o755)
            result = subprocess.run(
                ["bash", "-e", "-o", "pipefail", "-c", probe.group() + '\nprintf "browser assertions reached\\n"'],
                env={**os.environ, "PATH": tmp + os.pathsep + os.environ["PATH"]},
                capture_output=True, text=True,
            )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("browser assertions reached", result.stdout)
        for hostname in ("fonts.googleapis.com", "fonts.gstatic.com"):
            self.assertIn(f"::warning::preflight: cannot resolve {hostname} (external DNS)", result.stderr)

    def test_malformed_browser_archive_never_uploads_raw_credentials(self):
        for name in ("ui-e2e", "ui-e2e-auth"):
            collect = next(step for step in JOBS[name]["steps"]
                           if step.get("name") == "Collect browser diagnostics")
            with self.subTest(job=name), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                docker = root / "docker"
                docker.write_text('#!/bin/sh\necho "diagnostic csk_server_secret"\n')
                docker.chmod(0o755)
                (root / "ui/test-results").mkdir(parents=True)
                (root / "ui/test-results/trace.zip").write_bytes(b"invalid zip csk_trace_secret")
                result = subprocess.run(
                    ["bash", "-e", "-o", "pipefail", "-c", collect["run"]], cwd=tmp,
                    env={**os.environ, "PATH": tmp + os.pathsep + os.environ["PATH"],
                         "STEP_RESULTS": "{}", "CANDIDATE_SHA": "candidate",
                         "GITHUB_RUN_ID": "123", "GITHUB_RUN_ATTEMPT": "2", "GITHUB_JOB": name},
                    capture_output=True, text=True,
                )
                self.assertNotEqual(result.returncode, 0)
                artifacts = root / "ui/ci-artifacts"
                self.assertTrue((artifacts / "ci-diagnostics/outcomes.json").is_file())
                self.assertFalse((artifacts / "test-results/trace.zip").exists())
                for path in artifacts.rglob("*"):
                    if path.is_file():
                        self.assertNotIn(b"csk_", path.read_bytes())

    def test_gate_selectors_match_actual_job_conditions(self):
        for name, selectors in SELECTORS.items():
            with self.subTest(job=name):
                condition = JOBS[name].get("if", "")
                actual = re.findall(r"needs.changes.outputs.(\w+) == 'true'", condition)
                self.assertEqual(set(actual), set(selectors))
                if selectors:
                    self.assertEqual(condition, " || ".join(
                        f"needs.changes.outputs.{key} == 'true'" for key in selectors
                    ))
        gate = JOBS["ci-ok"]
        command = shlex.split(gate["steps"][-1]["run"].replace("\\\n", " "))
        self.assertEqual(command[:2], ["python3", "scripts/ci-ok.py"])
        # Named options carry the evidence wiring; the positionals are the
        # required job names and must be exactly this job's `needs`.
        positional, rest = [], iter(command[2:])
        for item in rest:
            if item.startswith("--"):
                next(rest)
            else:
                positional.append(item)
        self.assertEqual(positional, gate["needs"])
        for name in gate["needs"]:
            self.assertTrue(set(JOBS[name].get("needs", [])) <= set(gate["needs"]))

    def test_required_legacy_checks_always_report(self):
        for name in ("build-and-integration-test", "build-and-integration-test-agent-auth"):
            self.assertIn(name, JOBS)
            self.assertEqual(JOBS[name]["if"], "always()")
            self.assertEqual(JOBS[name]["needs"], ["changes", "images", "integration"])

    def test_full_suite_shards_cover_all_indexes(self):
        for name in ("integration", "integration-arm64"):
            job = JOBS[name]
            self.assertFalse(job["strategy"]["fail-fast"])
            entries = job["strategy"]["matrix"]["include"]
            shards = [entry for entry in entries if entry["id"] == "docker"]
            self.assertEqual(sorted(entry["shard"] for entry in shards), [1, 2, 3])
            self.assertTrue(all(entry["recipe"] == "integration-test" for entry in shards))
            inputs = job["steps"][-1]["with"]
            self.assertEqual(inputs["shard-index"], "${{ matrix.shard }}")
            self.assertEqual(inputs["shard-count"], "${{ matrix.id == 'docker' && '3' || '' }}")
        for name in ("helm-integration-test", "podman-integration-test"):
            job = JOBS[name]
            self.assertFalse(job["strategy"]["fail-fast"])
            self.assertEqual(job["strategy"]["matrix"]["shard"], [1, 2, 3])
            runs = [step["run"] for step in job["steps"] if "sh scripts/integration-test.sh" in step.get("run", "")]
            self.assertEqual(len(runs), 2 if name == "helm-integration-test" else 1)
            full_suite = [run for run in runs if "CAESIUM_TEST_SHARD_INDEX=" in run]
            self.assertEqual(len(full_suite), 1)
            self.assertNotIn("-run", full_suite[0])
            self.assertNotIn("-testify.m", full_suite[0])
            self.assertIn("caesiumcloud/caesium-integration:", full_suite[0])
            self.assertIn("CAESIUM_TEST_SHARD_INDEX=${{ matrix.shard }}", full_suite[0])
            self.assertIn("CAESIUM_TEST_SHARD_COUNT=3", full_suite[0])

    def test_distributed_kubernetes_deadlines_follow_the_full_local_suite(self):
        steps = JOBS["helm-integration-test"]["steps"]
        phase_index, phase = next((i, step) for i, step in enumerate(steps)
                                  if step.get("name") == "Distributed Kubernetes deadline regression")
        full_index = next(i for i, step in enumerate(steps)
                          if "CAESIUM_TEST_SHARD_INDEX=" in step.get("run", ""))
        self.assertLess(full_index, phase_index)
        self.assertEqual(phase["if"], "matrix.shard == 1")
        self.assertFalse(phase.get("continue-on-error", False))
        command = phase["run"]
        self.assertTrue(command.startswith("set -euo pipefail\n"))
        switch = command.index("kubectl set env statefulset/caesium --containers=caesium")
        rollout = command.index("kubectl rollout status statefulset/caesium --timeout=240s")
        invocation = command.index("docker run --rm")
        self.assertLess(switch, rollout)
        self.assertLess(rollout, invocation)
        server = command[switch:rollout]
        runner = command[invocation:]
        for setting in ("CAESIUM_EXECUTION_MODE=distributed", "CAESIUM_RUN_OWNER_ENABLED=true", "CAESIUM_WORKER_ENABLED=true"):
            self.assertIn(setting, server)
        self.assertNotIn("CAESIUM_NODE_ADDRESS=", server)
        for setting in ("CAESIUM_EXECUTION_MODE=distributed", "CAESIUM_TEST_ENGINE=kubernetes", "KUBECONFIG=/tmp/deadline-kubeconfig", "CAESIUM_TEST_WITNESS_HOST="):
            self.assertIn(setting, runner)
        self.assertNotIn("CAESIUM_TEST_SHARD_", runner)
        self.assertIn("-test.run '^TestIntegrationTestSuite$'", runner)
        self.assertIn("-testify.m '^TestDistributedKubernetesDeadlines$'", runner)
        self.assertIn(":/tmp/deadline-kubeconfig:ro", runner)
        self.assertIn('tee "$deadline_log"', runner)

    def test_distributed_deadline_witness_selects_an_ipv4_gateway(self):
        phase = next(step for step in JOBS["helm-integration-test"]["steps"]
                     if step.get("name") == "Distributed Kubernetes deadline regression")
        command = phase["run"]
        selector = command[command.index("witness_host=$(python3"):command.index("deadline_log=")]
        ipv4 = {"Subnet": "172.18.0.0/16", "Gateway": "172.18.0.1"}
        ipv6 = {"Subnet": "fc00:f853:ccd:e793::/64", "Gateway": "fc00:f853:ccd:e793::1"}
        for configs, expected in (
            ([ipv4], "172.18.0.1"),
            ([ipv6, ipv4], "172.18.0.1"),
            ([{"Subnet": "fc00:f853:ccd:e793::/64"}, ipv4], "172.18.0.1"),
            ([{"Gateway": ""}, ipv6, ipv4], "172.18.0.1"),
            ([ipv6], None),
            ([], None),
        ):
            with self.subTest(configs=configs):
                result = subprocess.run(
                    ["bash", "-e", "-o", "pipefail", "-c", selector],
                    env={**os.environ, "network_config": json.dumps(configs)},
                    capture_output=True, text=True,
                )
                if expected is None:
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("kind network has no IPv4 gateway", result.stderr)
                else:
                    self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                    self.assertIn(f"Kubernetes deadline witness gateway: {expected}", result.stdout)

    def test_distributed_kubernetes_deadline_gate_rejects_missing_or_skipped_coverage(self):
        phase = next(step for step in JOBS["helm-integration-test"]["steps"]
                     if step.get("name") == "Distributed Kubernetes deadline regression")
        command = phase["run"]
        verifier = command[command.index("if ! grep -Eq --"):]
        passed = "    --- PASS: TestIntegrationTestSuite/TestDistributedKubernetesDeadlines (1.00s)\n"
        for output, expected in (
            (passed, 0),
            ("PASS\n", 1),
            ("", 1),
            ("    --- SKIP: TestIntegrationTestSuite/TestDistributedKubernetesDeadlines (0.00s)\n", 1),
            (passed + "        --- SKIP: TestIntegrationTestSuite/TestDistributedKubernetesDeadlines/run_timeout (0.00s)\n", 1),
            (passed + "    --- SKIP: TestIntegrationTestSuite/Unrelated (0.00s)\n", 0),
        ):
            with self.subTest(output=output), tempfile.TemporaryDirectory() as tmp:
                log = Path(tmp) / "deadline.log"
                log.write_text(output)
                result = subprocess.run(["bash", "-c", verifier],
                                        env={**os.environ, "deadline_log": str(log)},
                                        capture_output=True, text=True)
                self.assertEqual(result.returncode, expected, result.stdout + result.stderr)

    def test_fixture_and_build_paths_select_tests(self):
        filters = yaml.safe_load(next(
            step["with"]["filters"] for step in JOBS["changes"]["steps"] if step.get("id") == "filter"
        ))
        # These filters use only literals and ** globs, for which fnmatch also
        # recognizes the concrete (non-root) fixture paths below.
        for path, group in (
            ("test/definitions/job_one.yaml", "go"),
            ("test/kubernetes_cache_identity.py", "go"),
            ("pkg/jobdef/testdata/schema.json", "go"),
            ("docs/examples/minimal.job.yaml", "go"),
            ("docs/examples-k8s/minimal.job.yaml", "go"),
            (".dockerignore", "go"),
            ("reagents/go.mod", "reagents"),
            ("scripts/test_ci.py", "ci"),
        ):
            self.assertTrue(any(fnmatch.fnmatchcase(path, rule) for rule in filters[group]), path)
        for path in ("docs/ci.md", "README.md"):
            self.assertFalse(any(fnmatch.fnmatchcase(path, rule) for rules in filters.values() for rule in rules))

    def test_kubernetes_cache_identity_runs_both_modes_and_retains_evidence(self):
        steps = JOBS["helm-integration-test"]["steps"]
        scenario = next(step for step in steps if "test/kubernetes_cache_identity.py" in step.get("run", ""))
        self.assertEqual(scenario["if"], "matrix.shard == 1")
        commands = scenario["run"]
        self.assertIn("set -euo pipefail", commands)
        self.assertEqual(commands.count("python3 test/kubernetes_cache_identity.py"), 2)
        download = next(step for step in steps if step.get("with", {}).get("name") == "release-cli-amd64")
        self.assertEqual(download["if"], "matrix.shard == 1")
        self.assertEqual(download["with"]["path"], ".tmp/cache-identity-cli")
        self.assertEqual(commands.count("--cli .tmp/cache-identity-cli/caesium-linux-amd64"), 2)
        self.assertIn("chmod +x .tmp/cache-identity-cli/caesium-linux-amd64", commands)
        self.assertIn("--mode local", commands)
        self.assertIn("--mode distributed", commands)
        self.assertIn("CAESIUM_EXECUTION_MODE=distributed", commands)
        self.assertIn("kubectl rollout status", commands)
        not_run = next(step for step in steps if '"result":"not_run"' in step.get("run", ""))
        self.assertEqual(not_run["if"], "always() && matrix.shard == 1")
        self.assertIn("for mode in local distributed", not_run["run"])
        self.assertIn('[ ! -f "$evidence" ]', not_run["run"])
        evidence = next(step for step in steps if step.get("with", {}).get("name") == "kubernetes-cache-identity")
        self.assertLess(steps.index(not_run), steps.index(evidence))
        with tempfile.TemporaryDirectory() as directory:
            subprocess.run(["bash", "-e", "-c", not_run["run"]], cwd=directory, check=True)
            root = Path(directory) / ".tmp/kubernetes-cache-identity"
            for mode in ("local", "distributed"):
                record = json.loads((root / f"{mode}.log").read_text())
                self.assertEqual(record["result"], "not_run")
                self.assertEqual(record["mode"], mode)
            (root / "local.log").write_text("retained scenario failure\n")
            subprocess.run(["bash", "-e", "-c", not_run["run"]], cwd=directory, check=True)
            self.assertEqual((root / "local.log").read_text(), "retained scenario failure\n")

        self.assertEqual(evidence["if"], "always() && matrix.shard == 1")
        self.assertEqual(evidence["with"]["if-no-files-found"], "error")
        for mode in ("local", "distributed"):
            self.assertIn(f'tee {evidence["with"]["path"]}{mode}.log', commands)

    def test_downloaded_artifacts_have_producers(self):
        saved = set()
        downloaded = set()
        for job in JOBS.values():
            for step in job.get("steps", []):
                if step.get("uses") == "./.github/actions/save-docker-images":
                    saved.add(step["with"]["name"])
                if step.get("uses") == "./.github/actions/load-docker-images":
                    downloaded.add(step["with"]["name"])
        self.assertTrue(downloaded <= saved, downloaded - saved)
        for arch in ("amd64", "arm64"):
            self.assertIn(f"product-{arch}", saved)
            self.assertIn(f"builder-{arch}", saved)
            self.assertIn(f"reagents-{arch}", saved)
            self.assertIn(f"integration-runner-{arch}", saved)

    def test_integration_consumers_do_not_download_compilers(self):
        action = yaml.safe_load((ROOT / ".github/actions/run-integration/action.yml").read_text())
        steps = action["runs"]["steps"]
        self.assertIn("CAESIUM_INTEGRATION_RUNNER_IMAGE", steps[-1]["env"])
        groups = [steps, JOBS["helm-integration-test"]["steps"], JOBS["podman-integration-test"]["steps"]]
        for steps in groups:
            loads = [step["with"]["name"] for step in steps if step.get("uses") == "./.github/actions/load-docker-images"]
            self.assertTrue(any(name.startswith("integration-runner-") for name in loads))
            self.assertFalse(any(name.startswith("builder-") for name in loads))


def recipe_body(name):
    """The body of one justfile recipe (header line through the next blank line)."""
    text = (ROOT / "justfile").read_text()
    match = re.search(rf"^{re.escape(name)}(?:\s[^\n]*)?:[^\n]*\n(.*?)(?=\n\S|\Z)", text, re.S | re.M)
    if match is None:
        raise AssertionError(f"justfile has no recipe {name!r}")
    return match.group(1)


def change_filters():
    return yaml.safe_load(next(
        step["with"]["filters"] for step in JOBS["changes"]["steps"] if step.get("id") == "filter"
    ))


def selected_groups(path):
    return {group for group, rules in change_filters().items()
            if any(fnmatch.fnmatchcase(path, rule) for rule in rules)}


def job_selectors(name):
    return set(re.findall(r"needs\.changes\.outputs\.(\w+) == 'true'", JOBS[name].get("if", "")))


class EarlyEvidenceLaneTests(unittest.TestCase):
    """The first persistent three-node lane: selection, wiring and fail-closed."""

    LANE = "early-evidence"
    MANIFEST = json.loads((ROOT / "test/contracts/scenarios.json").read_text())
    EARLY = [item for item in MANIFEST["scenarios"] if "early" in item["gates"]]

    def test_lane_selects_code_fixture_chart_dependency_build_and_workflow_changes(self):
        selectors = job_selectors(self.LANE)
        self.assertEqual(selectors, {"go", "helm", "ci"})
        for path, reason in (
            ("internal/run/store.go", "code"),
            ("test/robustness/owner_crash_test.go", "lane test code"),
            ("test/contracts/scenarios.json", "scenario manifest"),
            ("test/definitions/job_one.yaml", "fixtures"),
            ("pkg/jobdef/testdata/schema.json", "fixtures"),
            ("helm/caesium/ci/test-values-robustness.yaml", "chart values"),
            ("helm/caesium/templates/statefulset.yaml", "chart"),
            ("go.mod", "dependency"),
            ("go.sum", "dependency"),
            ("build/Dockerfile.robustness", "image"),
            ("build/ci.docker-bake.hcl", "build graph"),
            ("justfile", "recipe"),
            ("scripts/robustness.sh", "lane runner"),
            ("scripts/collect-evidence.py", "artifact consumer"),
            (".github/workflows/ci.yml", "workflow"),
            (".github/actions/run-integration/action.yml", "workflow"),
        ):
            with self.subTest(path=path, reason=reason):
                self.assertTrue(selected_groups(path) & selectors, path)
        # Documentation-only changes must not spend a kind cluster.
        for path in ("docs/ci.md", "README.md", "docs/exec-plans/active/distributed-testing.md"):
            with self.subTest(path=path):
                self.assertFalse(selected_groups(path) & selectors, path)

    def test_lane_is_promoted_into_the_merge_gate_only(self):
        # G5 promoted the lane into the fail-closed aggregate. The two wrapper
        # jobs that ARE required contexts keep their existing contract; changing
        # what they mean is a repository-settings decision, not a workflow one.
        self.assertIn(self.LANE, JOBS)
        self.assertIn(self.LANE, JOBS["ci-ok"]["needs"])
        self.assertIn(self.LANE, SELECTORS)
        for name in ("build-and-integration-test", "build-and-integration-test-agent-auth"):
            self.assertNotIn(self.LANE, JOBS[name]["needs"])
            self.assertEqual(JOBS[name]["needs"], ["changes", "images", "integration"])

    def test_required_check_jobs_are_preserved(self):
        for name in ("lint", "unit-test", "unit-test-arm64", "ui-test", "ui-e2e",
                     "ui-e2e-auth", "build-and-integration-test",
                     "build-and-integration-test-agent-auth"):
            self.assertIn(name, JOBS, name)

    def test_existing_engine_and_architecture_suites_are_preserved(self):
        recipes = {entry["recipe"] for name in ("integration", "integration-extra", "integration-arm64")
                   for entry in JOBS[name]["strategy"]["matrix"]["include"]}
        self.assertTrue({
            "integration-test",
            "integration-test-agent",
            "integration-test-distributed",
            "integration-test-owner-memory",
            "integration-test-infra",
        } <= recipes, recipes)
        for name in ("helm-integration-test", "podman-integration-test"):
            self.assertEqual(JOBS[name]["strategy"]["matrix"]["shard"], [1, 2, 3])

    def test_runner_image_compiles_the_subpackage_with_integration_tags(self):
        dockerfile = (ROOT / "build/Dockerfile.robustness").read_text()
        self.assertIn("go test -tags=integration -c ./test/robustness", dockerfile)
        bake = (ROOT / "build/ci.docker-bake.hcl").read_text()
        self.assertIn('dockerfile = "build/Dockerfile.robustness"', bake)
        self.assertIn('caesium-release = "target:release"', bake)
        images = JOBS["images"]["steps"]
        bake_step = next(step for step in images if step.get("uses") == "./.github/actions/bake-images")
        self.assertIn("robustness", bake_step["with"]["targets"].split())
        saved = [step["with"]["name"] for step in images
                 if step.get("uses") == "./.github/actions/save-docker-images"]
        self.assertIn("robustness-amd64", saved)

    def test_lane_consumes_prebuilt_images_and_never_a_compiler(self):
        action = yaml.safe_load((ROOT / ".github/actions/run-integration/action.yml").read_text())
        loads = [step["with"]["name"] for step in action["runs"]["steps"]
                 if step.get("uses") == "./.github/actions/load-docker-images"]
        self.assertIn("robustness-${{ inputs.arch }}", loads)
        steps = JOBS[self.LANE]["steps"]
        runner = next(step for step in steps if step.get("uses") == "./.github/actions/run-integration")
        self.assertEqual(runner["with"]["load-robustness"], "true")
        self.assertEqual(runner["with"]["recipe"], "integration-test-sql-budget")
        self.assertEqual(JOBS[self.LANE]["env"]["CAESIUM_SKIP_IMAGE_BUILD"], "true")
        # A `-amd64` image tag can never stand in for a real commit SHA.
        self.assertEqual(JOBS[self.LANE]["env"]["CANDIDATE_SHA"], "${{ github.sha }}")
        self.assertFalse([step for step in steps
                          if step.get("uses") == "./.github/actions/load-docker-images"
                          and step["with"]["name"].startswith("builder-")])

    def test_lane_runs_both_scenarios_then_validates_and_uploads_evidence(self):
        steps = JOBS[self.LANE]["steps"]
        recipes = [step["run"].split()[-1] for step in steps
                   if step.get("run", "").startswith("just ")]
        self.assertEqual(recipes, ["robustness-test", "check-evidence"])
        self.assertTrue(any(step.get("uses", "").startswith("azure/setup-helm") for step in steps))
        self.assertTrue(any("kind-linux-amd64" in step.get("run", "") for step in steps))
        upload = steps[-1]
        self.assertEqual(upload["uses"], "actions/upload-artifact@v7")
        self.assertEqual(upload["if"], "always() && steps.redact_robustness_evidence.outcome == 'success'")
        self.assertEqual(upload["with"]["if-no-files-found"], "error")

    def test_early_upload_requires_always_run_redaction_even_after_lane_failure(self):
        steps = JOBS[self.LANE]["steps"]
        redact = next(step for step in steps if step.get("id") == "redact_robustness_evidence")
        self.assertEqual(redact["if"], "always()")
        self.assertNotIn("continue-on-error", redact)
        self.assertLess(steps.index(redact), len(steps) - 1)
        self.assertIn("--artifacts .tmp/evidence/robustness", redact["run"])
        self.assertNotIn("|| true", redact["run"])
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "scripts").mkdir()
            shutil.copyfile(ROOT / "scripts/collect-evidence.py", root / "scripts/collect-evidence.py")
            art = root / ".tmp/evidence/robustness"
            result = subprocess.run(["bash", "-e", "-c", redact["run"]], cwd=root,
                                    capture_output=True, text=True, timeout=10)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("No robustness artifact directory was produced", result.stdout)
            art.parent.mkdir(parents=True)
            art.write_text("not a directory; must refuse upload")
            result = subprocess.run(["bash", "-e", "-c", redact["run"]], cwd=root,
                                    capture_output=True, text=True, timeout=10)
            self.assertNotEqual(result.returncode, 0)
            art.unlink()
            (art / "member-logs").mkdir(parents=True)
            (art / "internal-token.txt").write_text("fixture-generated-token\n")
            (art / "member-logs/crash.log").write_text("failed startup fixture-generated-token")
            result = subprocess.run(["bash", "-e", "-c", redact["run"]], cwd=root,
                                    capture_output=True, text=True, timeout=10)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual((art / "member-logs/crash.log").read_text(), "failed startup [REDACTED_INTERNAL_TOKEN]")
            self.assertFalse((art / "internal-token.txt").exists())

    def test_registered_selectors_name_real_tests(self):
        by_id = {item["id"]: item for item in self.EARLY}
        self.assertEqual(sorted(by_id), [
            "b1-owner-crash-leader", "b1-owner-crash-nonleader", "e5-sql-work-budget",
        ])
        owner_crash = (ROOT / "test/robustness/owner_crash_test.go").read_text()
        runner = (ROOT / "scripts/robustness.sh").read_text()
        # B2 made the runner selection configurable (CAESIUM_ROBUSTNESS_RUN) so the
        # targeted-fault tests can share the harness. The early-evidence lane must
        # still run EXACTLY the registered selector: the script's default is the
        # owner-crash pattern, that pattern is what reaches the runner, and
        # nothing on the early-evidence path overrides it. G6's core-robustness
        # recipe is the ONE place that sets it, to exactly '^TestCore$'.
        self.assertIn('RUN_PATTERN="${CAESIUM_ROBUSTNESS_RUN:-^TestOwnerCrash\\$}"', runner)
        self.assertIn('"-test.run", "${RUN_PATTERN}"', runner)
        self.assertNotIn("CAESIUM_ROBUSTNESS_RUN", (ROOT / ".github/workflows/ci.yml").read_text())
        justfile = (ROOT / "justfile").read_text()
        for recipe in ("robustness-test", "robustness-runner", "early-evidence", "check-evidence",
                       "integration-test-sql-budget"):
            self.assertNotIn("CAESIUM_ROBUSTNESS_RUN", recipe_body(recipe), recipe)
        self.assertEqual(justfile.count("CAESIUM_ROBUSTNESS_RUN"), 1)
        self.assertIn("CAESIUM_ROBUSTNESS_RUN='^TestCore$'", recipe_body("core-robustness"))
        # Required subtests are declared per selection and enforced by one loop; a
        # registered selector missing from the owner-crash branch, or a loop that no
        # longer dies on a missing PASS line, would let the lane go green hollow.
        branch = re.search(r"\*TestOwnerCrash\*\)\n\s+REQUIRED_SUBTESTS=\(([^)]*)\)", runner)
        self.assertIsNotNone(branch, "owner-crash selection has no REQUIRED_SUBTESTS")
        required = branch.group(1).split()
        self.assertIn('pass_line "$name" || die "required subtest $name did not PASS"', runner)
        for sid in ("b1-owner-crash-leader", "b1-owner-crash-nonleader"):
            subtest = by_id[sid]["selector"].split("/", 1)[1]
            self.assertIn(f't.Run("{subtest}"', owner_crash)
            self.assertIn(f"TestOwnerCrash/{subtest}", required)
        # Bare method names match nothing: the suite prefix is load-bearing.
        selector = by_id["e5-sql-work-budget"]["selector"]
        self.assertTrue(selector.startswith("TestIntegrationTestSuite/"), selector)
        method = selector.split("/", 1)[1]
        self.assertIn(f") {method}()", (ROOT / "test/statement_budget_test.go").read_text())
        justfile = (ROOT / "justfile").read_text()
        self.assertIn(f'env("CAESIUM_SQL_BUDGET_RUN", "{selector}")', justfile)

    def test_evidence_check_fails_on_a_missing_artifact_or_scenario(self):
        recipe = re.search(r"\ncheck-evidence:\n(.*?)\n\n", (ROOT / "justfile").read_text(), re.S)
        self.assertIsNotNone(recipe)
        body = recipe.group(1)
        self.assertIn("scripts/collect-evidence.py report", body)
        self.assertIn("--require early", body)
        self.assertIn("--strict", body)
        manifest = ROOT / "test/contracts/scenarios.json"
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            report = root / "evidence.json"
            # 1. A lane that never wrote its fragment fails the merge step.
            missing = subprocess.run(
                [sys.executable, str(ROOT / "scripts/collect-evidence.py"), "report",
                 "--out", str(report), str(root / "sql-budget.json")],
                capture_output=True, text=True,
            )
            self.assertEqual(missing.returncode, 1, missing.stdout + missing.stderr)
            self.assertIn("missing evidence fragment", missing.stderr)
            # 2. A report that omits a registered scenario fails the checker.
            report.write_text(json.dumps({
                "candidate_sha": "a" * 40,
                "candidate_digest": "sha256:" + "ab" * 32,
                "gate_enabled": True,
                "disabled_gates": [],
                "scenarios": [],
            }))
            checked = subprocess.run(
                [sys.executable, str(ROOT / "scripts/check-test-evidence.py"),
                 "--manifest", str(manifest), "--report", str(report),
                 "--require", "early", "--strict"],
                capture_output=True, text=True,
            )
            self.assertEqual(checked.returncode, 1, checked.stdout + checked.stderr)
            for scenario in self.EARLY:
                self.assertIn(f"missing-scenario={scenario['id']}",
                              checked.stdout + checked.stderr)


class EarlyEvidencePromotionTests(unittest.TestCase):
    """G5: the early lane is required by the aggregate, and a green job is not enough."""

    LANE = EVIDENCE_JOB

    def gate_step(self):
        return next(step for step in JOBS["ci-ok"]["steps"] if step.get("name") == "Evaluate merge gate")

    def gate_argv(self):
        return shlex.split(self.gate_step()["run"].replace("\\\n", " "))

    def run_gate(self, expected, report=evidence_report, manifest=None, sha=CANDIDATE_SHA,
                 enabled=FLAGS, needs=None):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "evidence.json"
            if report is not None:
                path.write_text(json.dumps(report() if callable(report) else report))
            manifest_path = MANIFEST_PATH
            if manifest is not None:
                manifest_path = Path(tmp) / "scenarios.json"
                manifest_path.write_text(json.dumps(manifest))
            result = subprocess.run(
                gate_command(path, manifest=manifest_path, sha=sha),
                env={**os.environ,
                     "NEEDS_JSON": json.dumps(results(selected_outputs(enabled)) if needs is None else needs)},
                capture_output=True, text=True,
            )
            self.assertEqual(result.returncode, expected, result.stdout + result.stderr)
            return result.stdout + result.stderr

    def test_lane_is_a_required_dependency_of_the_aggregate(self):
        self.assertIn(self.LANE, JOBS["ci-ok"]["needs"])
        # The gate's skip policy must be exactly the lane's own path selection,
        # or a selected lane could vanish and read as an allowed skip.
        self.assertEqual(set(SELECTORS[self.LANE]), job_selectors(self.LANE))
        self.assertEqual(SELECTORS[self.LANE], ("go", "helm", "ci"))
        argv = self.gate_argv()
        self.assertIn(self.LANE, argv)
        for name in JOBS["ci-ok"]["needs"]:
            self.assertIn(name, argv, name)

    def test_gate_step_is_wired_to_the_uploaded_evidence(self):
        steps = JOBS["ci-ok"]["steps"]
        download = next(step for step in steps if step.get("uses", "").startswith("actions/download-artifact"))
        upload = JOBS[self.LANE]["steps"][-1]
        self.assertEqual(download["uses"], "actions/download-artifact@v8")
        self.assertEqual(download["with"]["name"], upload["with"]["name"])
        self.assertEqual(download["if"], f"needs.{self.LANE}.result == 'success'")
        self.assertLess(steps.index(download), steps.index(self.gate_step()))
        argv = self.gate_argv()
        report = argv[argv.index("--evidence-report") + 1]
        # The gate must read the artifact where the download step puts it.
        self.assertEqual(str(Path(report).parent), download["with"]["path"])
        self.assertEqual(argv[argv.index("--evidence-manifest") + 1], "test/contracts/scenarios.json")
        # Evidence from another commit cannot satisfy this run.
        self.assertEqual(argv[argv.index("--candidate-sha") + 1], "${{ github.sha }}")
        self.assertEqual(JOBS[self.LANE]["env"]["CANDIDATE_SHA"], "${{ github.sha }}")

    def test_new_dependency_fails_closed_on_every_bad_result(self):
        for status in ("failure", "cancelled", "skipped", "timed_out", "unknown", None):
            with self.subTest(status=status):
                needs = results(selected_outputs(FLAGS))
                if status is None:
                    del needs[self.LANE]
                else:
                    needs[self.LANE]["result"] = status
                self.assertIn(f"{self.LANE}={status or 'missing'}", self.run_gate(1, needs=needs))

    def test_deselected_lane_needs_no_evidence(self):
        # A docs-only change spends no kind cluster and owes no report.
        needs = results(selected_outputs(()))
        self.assertEqual(needs[self.LANE]["result"], "skipped")
        output = self.run_gate(0, report=None, enabled=(), needs=needs)
        self.assertIn("deselected by the path filters", output)

    def test_absent_or_foreign_evidence_fails_closed(self):
        self.assertIn("produced no evidence", self.run_gate(1, report=None))
        self.assertIn("must be an object", self.run_gate(1, report=[]))
        self.assertIn("not the tested candidate", self.run_gate(1, sha="a" * 40))
        report = evidence_report()
        report["candidate_sha"] = "e" * 40
        self.assertIn("not the tested candidate", self.run_gate(1, report=report))

    def test_hollow_evidence_fails_closed(self):
        ids = [item["id"] for item in gated_scenarios()]
        self.assertEqual(sorted(ids), [
            "b1-owner-crash-leader", "b1-owner-crash-nonleader", "e5-sql-work-budget",
        ])
        for sid in ids:
            with self.subTest(scenario=sid, mutation="missing"):
                report = evidence_report()
                report["scenarios"] = [item for item in report["scenarios"] if item["id"] != sid]
                self.assertIn(f"scenario {sid!r} is missing", self.run_gate(1, report=report))
            for status in ("fail", "inconclusive", "timeout", "skip"):
                with self.subTest(scenario=sid, status=status):
                    report = evidence_report()
                    entry = next(item for item in report["scenarios"] if item["id"] == sid)
                    entry["status"] = status
                    self.assertIn(f"scenario {sid!r} status {status!r}", self.run_gate(1, report=report))
        # No report at all, and a report with no scenarios, are both failures.
        report = evidence_report()
        report["scenarios"] = []
        self.assertEqual(len(gated_scenarios()), self.run_gate(1, report=report).count("is missing from the report"))

    def test_absent_fault_activation_fails_closed(self):
        faulted = [item for item in gated_scenarios() if (item.get("fault_activation") or {}).get("required")]
        self.assertTrue(faulted, "the early gate must require fault evidence from at least one scenario")
        for scenario in faulted:
            sid = scenario["id"]
            fault = scenario["fault_activation"]
            for mutation, patch in (
                ("not activated", {"activated": False}),
                ("wrong kind", {"kind": "graceful-shutdown"}),
                ("dropped observation", {"observations": list(fault["required_observations"])[1:]}),
                ("no fault evidence", None),
            ):
                with self.subTest(scenario=sid, mutation=mutation):
                    report = evidence_report()
                    entry = next(item for item in report["scenarios"] if item["id"] == sid)
                    if patch is None:
                        del entry["fault_activation"]
                    else:
                        entry["fault_activation"].update(patch)
                    output = self.run_gate(1, report=report)
                    self.assertIn(sid, output)
                    self.assertIn("fault", output)

    def test_gate_delegates_deep_validation_to_the_scenario_checker(self):
        # Failures the gate's own exact-scenario pass cannot see must still fail:
        # they come back from scripts/check-test-evidence.py.
        sid = gated_scenarios()[0]["id"]
        mutations = {
            "wrong-topology": lambda entry, report: entry["topology"].update({"replicas": 1}),
            "wrong-feature-flags": lambda entry, report: entry["feature_flags"].update(
                {next(iter(entry["feature_flags"])): "tampered"}),
            "missing-observation": lambda entry, report: entry["observations"].pop(),
            "wrong-artifact-identity": lambda entry, report: entry.update({"artifact_identity": "something-else"}),
            "missing-recorder": lambda entry, report: entry["recorder"].update({"present": False}),
            "disabled-gate": lambda entry, report: report.update({"gate_enabled": False}),
            "disabled-gates-list": lambda entry, report: report.update({"disabled_gates": ["early"]}),
        }
        for name, mutate in mutations.items():
            with self.subTest(mutation=name):
                report = evidence_report()
                mutate(next(item for item in report["scenarios"] if item["id"] == sid), report)
                output = self.run_gate(1, report=report)
                self.assertIn("check-test-evidence.py exited 1", output)

    def test_an_ungated_manifest_cannot_vacuously_pass(self):
        manifest = copy.deepcopy(MANIFEST)
        for item in manifest["scenarios"]:
            item["gates"] = []
        output = self.run_gate(1, manifest=manifest)
        self.assertIn("has no registered scenarios", output)

    def test_honest_evidence_passes(self):
        output = self.run_gate(0)
        self.assertIn("ci-ok passed", output)
        self.assertIn("test-evidence passed", output)

    def test_existing_optional_lanes_keep_their_current_status(self):
        # helm-pod-replacement-test (issue #493, PR #536) landed after G5 with
        # the same unpromoted shape as its siblings here; G7 keeps that honest
        # by covering it with the same assertion rather than leaving it
        # unchecked.
        for name in ("helm-integration-test", "podman-integration-test",
                     "integration-extra", "integration-arm64",
                     "helm-pod-replacement-test"):
            self.assertIn(name, JOBS, name)
            self.assertNotIn(name, JOBS["ci-ok"]["needs"], name)
            self.assertNotIn(name, SELECTORS, name)
            self.assertNotIn(name, self.gate_argv(), name)


class CandidateIdentityUnitTests(unittest.TestCase):
    """Pure-function coverage for G7's identity/staleness checks (no subprocess)."""

    def test_recognized_events_match_the_workflows_declared_triggers(self):
        self.assertEqual(set(RECOGNIZED_EVENTS),
                         {"pull_request", "merge_group", "push", "workflow_dispatch"})
        self.assertEqual(set(TRIGGERS), set(RECOGNIZED_EVENTS))

    def test_identity_requires_event_name_and_candidate_sha(self):
        self.assertEqual(
            VERIFY_CANDIDATE_IDENTITY("", "sha", "base", "head"),
            ["candidate identity: no --event-name was passed to the gate"],
        )
        self.assertEqual(
            VERIFY_CANDIDATE_IDENTITY("pull_request", "", "base", "head"),
            ["candidate identity: no --candidate-sha was passed to the gate"],
        )

    def test_identity_rejects_an_unrecognized_event(self):
        self.assertEqual(
            VERIFY_CANDIDATE_IDENTITY("schedule", "sha", "", ""),
            ["candidate identity: unrecognized event 'schedule'"],
        )
        # G6: a dispatch is push-like -- no base/head pair to require.
        self.assertEqual(VERIFY_CANDIDATE_IDENTITY("workflow_dispatch", "sha", "", ""), [])

    def test_identity_requires_base_and_head_for_pull_request_and_merge_group(self):
        for event in ("pull_request", "merge_group"):
            with self.subTest(event=event):
                problems = VERIFY_CANDIDATE_IDENTITY(event, "sha", "", "")
                self.assertEqual(len(problems), 2, problems)
                self.assertTrue(any("--base-sha" in p for p in problems), problems)
                self.assertTrue(any("--head-sha" in p for p in problems), problems)
                # merge_group additionally requires candidate == head (see
                # test_identity_merge_group_candidate_must_equal_head);
                # pull_request's candidate is a synthetic merge commit and is
                # never expected to equal its own head.
                head = "sha" if event == "merge_group" else "head"
                self.assertEqual(VERIFY_CANDIDATE_IDENTITY(event, "sha", "base", head), [])

    def test_identity_push_needs_no_base_or_head(self):
        self.assertEqual(VERIFY_CANDIDATE_IDENTITY("push", "sha", "", ""), [])

    def test_identity_merge_group_candidate_must_equal_head(self):
        # GitHub's contract: on merge_group, the tested commit IS the queue
        # entry's head commit -- unlike pull_request, whose tested commit is
        # a separate synthetic merge (covered by the parent-derivation tests
        # below instead).
        self.assertEqual(
            VERIFY_CANDIDATE_IDENTITY("merge_group", "sha1", "base", "sha1"), [],
        )
        problems = VERIFY_CANDIDATE_IDENTITY("merge_group", "sha1", "base", "sha2")
        self.assertEqual(len(problems), 1, problems)
        self.assertIn("does not match head sha", problems[0])
        # pull_request has no such constraint: candidate != head is normal.
        self.assertEqual(
            VERIFY_CANDIDATE_IDENTITY("pull_request", "merge-sha", "base", "head"), [],
        )

    def test_freshness_is_a_noop_outside_pull_request_and_merge_group(self):
        self.assertEqual(VERIFY_BASE_FRESHNESS("push", "", ""), ([], None))
        self.assertEqual(VERIFY_BASE_FRESHNESS("workflow_dispatch", "a", "a"), ([], None))

    def test_freshness_on_pull_request_reports_but_never_fails(self):
        for base, current, contains in (
            ("a", "b", "does not match"),
            ("a", "a", "matches current master tip"),
            ("", "a", "no tested base sha"),
            ("a", "", "no --current-base-sha"),
        ):
            with self.subTest(base=base, current=current):
                problems, message = VERIFY_BASE_FRESHNESS("pull_request", base, current)
                self.assertEqual(problems, [])
                self.assertIn(contains, message)

    def test_freshness_on_merge_group_fails_closed_on_stale_or_missing(self):
        for base, current, contains in (
            ("a", "b", "does not match"),
            ("", "b", "no tested base sha"),
            ("a", "", "no --current-base-sha"),
        ):
            with self.subTest(base=base, current=current):
                problems, message = VERIFY_BASE_FRESHNESS("merge_group", base, current)
                self.assertEqual(len(problems), 1, problems)
                self.assertIn(contains, problems[0])
                self.assertEqual(problems[0], message)

    def test_freshness_on_merge_group_passes_when_fresh(self):
        problems, message = VERIFY_BASE_FRESHNESS("merge_group", "a", "a")
        self.assertEqual(problems, [])
        self.assertIn("matches current master tip", message)


class PullRequestCandidateParentsUnitTests(unittest.TestCase):
    """Pure-function coverage for verifying a pull_request candidate's real
    git parents against the event payload (no subprocess, no git)."""

    # staticmethod: a plain function stored as a class attribute is a
    # descriptor, so `self.VERIFY(...)` would otherwise implicitly bind
    # `self` as its first positional argument.
    VERIFY = staticmethod(VERIFY_PULL_REQUEST_CANDIDATE_PARENTS)

    def test_exactly_two_matching_parents_passes_and_resolves_the_base(self):
        problems, base, disagreement = self.VERIFY(["base-sha", "head-sha"], "base-sha", "head-sha")
        self.assertEqual(problems, [])
        self.assertEqual(base, "base-sha")
        self.assertIsNone(disagreement)

    def test_wrong_parent_count_refuses(self):
        for parents in ([], ["only-one"], ["a", "b", "c"]):
            with self.subTest(parents=parents):
                problems, base, disagreement = self.VERIFY(parents, "base-sha", "head-sha")
                self.assertEqual(len(problems), 1, problems)
                self.assertIn(f"got {len(parents)}", problems[0])
                # Falls back to the payload base since there is no resolved
                # parent to use; the run fails regardless, from `problems`.
                self.assertEqual(base, "base-sha")
                self.assertIsNone(disagreement)

    def test_second_parent_mismatch_refuses(self):
        problems, base, disagreement = self.VERIFY(["base-sha", "wrong-head"], "base-sha", "head-sha")
        self.assertEqual(len(problems), 1, problems)
        self.assertIn("second parent", problems[0])
        self.assertIn("'wrong-head'", problems[0])
        self.assertIn("'head-sha'", problems[0])

    def test_first_parent_disagreement_is_reported_not_refused(self):
        problems, base, disagreement = self.VERIFY(["actual-base", "head-sha"], "stale-payload-base", "head-sha")
        self.assertEqual(problems, [])
        self.assertEqual(base, "actual-base")
        self.assertIsNotNone(disagreement)
        self.assertIn("actual-base", disagreement)
        self.assertIn("stale-payload-base", disagreement)

    def test_empty_payload_fields_are_not_compared(self):
        # Missing payload values were already caught by
        # verify_candidate_identity; this function must not pile on a
        # second, confusing complaint about the same root cause.
        problems, base, disagreement = self.VERIFY(["actual-base", "actual-head"], "", "")
        self.assertEqual(problems, [])
        self.assertEqual(base, "actual-base")
        self.assertIsNone(disagreement)


class CandidateIdentityGateTests(unittest.TestCase):
    """End-to-end CLI wiring: scripts/ci-ok.py's --event-name/--base-sha/
    --head-sha/--current-base-sha flags, as the `ci-ok` job actually passes
    them (see gate_command's pull_request-shaped defaults)."""

    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.evidence = Path(tmp.name) / "evidence.json"
        self.evidence.write_text(json.dumps(evidence_report()))

    def gate(self, expected, needs=None, **kwargs):
        needs = needs if needs is not None else results(selected_outputs(FLAGS))
        result = subprocess.run(
            gate_command(self.evidence, **kwargs),
            env={**os.environ, "NEEDS_JSON": json.dumps(needs)},
            capture_output=True, text=True,
        )
        self.assertEqual(result.returncode, expected, result.stdout + result.stderr)
        return result.stdout + result.stderr

    def test_default_pull_request_identity_passes_and_is_logged(self):
        output = self.gate(0)
        self.assertIn("ci-ok passed", output)
        self.assertIn(
            "ci-ok candidate identity: event='pull_request' "
            f"sha={CANDIDATE_SHA!r} base={CANDIDATE_BASE_SHA!r} head={CANDIDATE_HEAD_SHA!r}",
            output,
        )

    def test_missing_event_name_fails_closed(self):
        self.assertIn("no --event-name", self.gate(1, event_name=""))

    def test_unrecognized_event_fails_closed(self):
        self.assertIn("unrecognized event", self.gate(1, event_name="schedule"))

    def test_workflow_dispatch_needs_no_base_or_head(self):
        # G6: a dispatched nightly/push-equivalent run is push-like.
        output = self.gate(0, event_name="workflow_dispatch", base_sha="", head_sha="",
                           candidate_parents="")
        self.assertIn("ci-ok passed", output)

    def test_pull_request_missing_base_or_head_fails_closed(self):
        self.assertIn("missing --base-sha", self.gate(1, base_sha=""))
        self.assertIn("missing --head-sha", self.gate(1, head_sha=""))

    def test_merge_group_missing_base_or_head_fails_closed(self):
        self.assertIn("missing --base-sha",
                       self.gate(1, event_name="merge_group", base_sha=""))
        self.assertIn("missing --head-sha",
                       self.gate(1, event_name="merge_group", head_sha=""))

    def test_merge_group_candidate_sha_must_equal_head_sha(self):
        # gate_command's default sha (CANDIDATE_SHA) represents a
        # pull_request-shaped merge commit and deliberately does NOT equal
        # CANDIDATE_HEAD_SHA -- on merge_group that mismatch must itself
        # refuse, per GitHub's contract that the tested commit IS the queue
        # entry's head.
        output = self.gate(1, event_name="merge_group")
        self.assertIn("does not match head sha", output)
        # Passing a sha that actually matches head fixes it. Deselect
        # early-evidence (its fixture report is bound to CANDIDATE_SHA, not
        # CANDIDATE_HEAD_SHA) so this isolates the identity check.
        output = self.gate(0, needs=results(selected_outputs(())),
                            event_name="merge_group", sha=CANDIDATE_HEAD_SHA,
                            current_base_sha=CANDIDATE_BASE_SHA)
        self.assertIn("ci-ok passed", output)

    def test_push_needs_no_base_or_head(self):
        output = self.gate(0, event_name="push", base_sha="", head_sha="")
        self.assertIn("ci-ok passed", output)

    def test_pull_request_stale_base_is_reported_not_failed(self):
        output = self.gate(0, current_base_sha="c" * 40)
        self.assertIn("ci-ok passed", output)
        self.assertIn("base freshness", output)
        self.assertIn("does not match", output)

    def test_pull_request_missing_current_base_sha_is_not_failed(self):
        output = self.gate(0, current_base_sha="")
        self.assertIn("ci-ok passed", output)
        self.assertIn("no --current-base-sha", output)

    def test_merge_group_fresh_base_passes(self):
        output = self.gate(0, needs=results(selected_outputs(())),
                            event_name="merge_group", sha=CANDIDATE_HEAD_SHA,
                            current_base_sha=CANDIDATE_BASE_SHA)
        self.assertIn("ci-ok passed", output)
        self.assertIn("matches current master tip", output)

    def test_merge_group_stale_base_fails_closed(self):
        output = self.gate(1, event_name="merge_group", sha=CANDIDATE_HEAD_SHA,
                            current_base_sha="c" * 40)
        self.assertIn("base freshness", output)
        self.assertIn("does not match", output)

    def test_merge_group_missing_current_base_sha_fails_closed(self):
        self.assertIn("no --current-base-sha",
                       self.gate(1, event_name="merge_group", sha=CANDIDATE_HEAD_SHA,
                                 current_base_sha=""))

    def test_identity_check_ignores_callers_that_pass_no_candidate_sha(self):
        # The two legacy build-and-integration-test* wrapper jobs never pass
        # --candidate-sha (or any identity flag); they must keep passing.
        result = subprocess.run(
            [sys.executable, str(ROOT / "scripts/ci-ok.py"), "changes", "images", "integration"],
            env={**os.environ, "NEEDS_JSON": json.dumps(results(selected_outputs(FLAGS)))},
            capture_output=True, text=True,
        )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertNotIn("candidate identity", result.stdout)

    def test_explicit_empty_candidate_sha_still_triggers_validation(self):
        # A broken template expression could hand the gate `--candidate-sha
        # ''` -- as opposed to omitting the flag entirely, which is the ONLY
        # thing that exempts the two legacy wrapper jobs above. This must
        # refuse, not silently skip -- even with early-evidence itself
        # deselected, so there is no evidence-report reason to fail instead
        # (isolating that this specific refusal comes from identity, not
        # evidence).
        needs = results(selected_outputs(()))
        self.assertIn("early-evidence", needs)
        self.assertEqual(needs["early-evidence"]["result"], "skipped")
        output = self.gate(1, needs=needs, sha="")
        self.assertIn("no --candidate-sha", output)

    def test_explicit_empty_identity_flags_still_trigger_validation(self):
        # Same concern, but every identity flag (not just --candidate-sha)
        # came out empty -- e.g. a fully broken template block. Any ONE of
        # the four being explicitly passed (even empty) is enough to turn
        # validation on; here all four are.
        needs = results(selected_outputs(()))
        output = self.gate(1, needs=needs, sha="", event_name="", base_sha="", head_sha="")
        self.assertIn("no --event-name", output)

    def test_pull_request_parent_count_mismatch_fails_closed(self):
        output = self.gate(1, candidate_parents=CANDIDATE_BASE_SHA)  # only one parent
        self.assertIn("candidate parents", output)
        self.assertIn("got 1", output)

    def test_pull_request_second_parent_mismatch_fails_closed(self):
        output = self.gate(1, candidate_parents=f"{CANDIDATE_BASE_SHA} not-the-head-sha")
        self.assertIn("second parent", output)
        self.assertIn("does not match the payload head sha", output)

    def test_pull_request_first_parent_disagreement_is_reported_not_failed(self):
        actual_base = "c" * 40
        output = self.gate(0, candidate_parents=f"{actual_base} {CANDIDATE_HEAD_SHA}",
                            current_base_sha=actual_base)
        self.assertIn("ci-ok passed", output)
        self.assertIn("candidate parents", output)
        self.assertIn("disagrees with the payload base sha", output)
        # Freshness must have compared against the resolved (parent) base,
        # not the stale payload base -- which is exactly why this passes
        # with current_base_sha=actual_base rather than CANDIDATE_BASE_SHA.
        self.assertIn("matches current master tip", output)

    def test_pull_request_no_candidate_parents_input_fails_closed(self):
        # Mirrors a real transient failure of the workflow's `git cat-file`
        # step (continue-on-error: true leaves the output empty).
        output = self.gate(1, candidate_parents="")
        self.assertIn("candidate parents", output)
        self.assertIn("got 0", output)


class MergeGroupWorkflowWiringTests(unittest.TestCase):
    """G7: merge_group is wired end-to-end so a queue is activatable by a
    repository-settings change alone. No ruleset exists today (2026-09-16
    `gh api repos/caesium-cloud/caesium/rulesets` returned `[]`), so this
    trigger has never fired; everything here is proven statically."""

    def test_merge_group_trigger_is_declared(self):
        self.assertIn("merge_group", TRIGGERS)
        self.assertEqual(TRIGGERS["merge_group"], {"types": ["checks_requested"]})

    def test_concurrency_never_mixes_pull_request_and_merge_group(self):
        group = WORKFLOW["concurrency"]["group"]
        self.assertIn("github.event.pull_request.number", group)
        self.assertIn("github.event.merge_group.head_sha", group)
        self.assertIn("github.event_name == 'merge_group'", group)
        # G6: every dispatch gets its own group, so a nightly or calibration
        # dispatch never queues behind (or cancels) another run.
        self.assertIn("github.event_name == 'workflow_dispatch' && format('dispatch-{0}', github.run_id)", group)
        # cancel-in-progress must never apply to a merge_group run.
        self.assertEqual(WORKFLOW["concurrency"]["cancel-in-progress"],
                          "${{ github.event_name == 'pull_request' }}")

    def test_image_tag_is_already_event_agnostic(self):
        self.assertEqual(WORKFLOW["env"]["IMAGE_TAG"],
                          "${{ github.ref_type == 'tag' && github.ref_name || github.sha }}")

    def test_publish_writes_only_on_a_tag(self):
        # G4: a publish dry run walks the chain too, but every step that writes
        # outside the run is skipped on it, and it binds no environment.
        self.assertEqual(JOBS["publish"]["if"],
                         "startsWith(github.ref, 'refs/tags/v') || inputs.publish-dry-run")
        self.assertEqual(JOBS["publish"]["environment"], "${{ !inputs.publish-dry-run && 'Docker' || '' }}")
        writers = [step for step in JOBS["publish"]["steps"]
                   if re.search(r"docker (login|push|manifest)|gh release|secrets\.", step.get("run", ""))]
        # Pin the writer set by name: a new writer must be added here
        # deliberately (and inherit the dry-run guard), never slip in.
        self.assertEqual([step.get("name") for step in writers], [
            "Login to Docker Hub",
            "Push multi-arch manifest",
            "Push stress fixture multi-arch manifest",
            "Push reagent multi-arch manifests",
            "Create GitHub Release",
        ])
        for step in writers + [s for s in JOBS["publish"]["steps"] if "GITHUB_REF_NAME" in s.get("run", "")]:
            self.assertEqual(step.get("if"), "${{ !inputs.publish-dry-run }}", step.get("name"))

    def test_event_name_appears_only_in_audited_locations(self):
        """Every job-level `if:` must be event-agnostic (drive purely off
        `changes` outputs) so merge_group support required no per-job edits.
        Exactly four step-level `if:` conditions may reference event_name:
        the `changes` job's checkout and filter steps, and `ci-ok`'s
        base-freshness and candidate-parents steps. Any other appearance is
        unaudited and must fail this test rather than land silently."""
        for job_name, job in JOBS.items():
            self.assertNotIn("event_name", job.get("if") or "", job_name)
            for step in job.get("steps", []) or []:
                condition = step.get("if") or ""
                if "event_name" not in condition:
                    continue
                allowed = (
                    (job_name == "changes"
                     and (step.get("uses") == "actions/checkout@v6" or step.get("id") == "filter"))
                    or (job_name == "ci-ok" and step.get("id") in ("base-freshness", "candidate-parents"))
                )
                self.assertTrue(allowed, f"{job_name}: unaudited event_name if: {condition!r}")

    def test_changes_job_filters_on_both_pull_request_and_merge_group(self):
        steps = JOBS["changes"]["steps"]
        checkout = next(step for step in steps if step.get("uses") == "actions/checkout@v6")
        filter_step = next(step for step in steps if step.get("id") == "filter")
        both = "github.event_name == 'pull_request' || github.event_name == 'merge_group'"
        self.assertEqual(checkout["if"], both)
        self.assertEqual(filter_step["if"], both)
        self.assertEqual(checkout["with"]["fetch-depth"],
                          "${{ github.event_name == 'merge_group' && '0' || '1' }}")
        self.assertEqual(filter_step["with"]["base"],
                          "${{ github.event_name == 'merge_group' && "
                          "github.event.merge_group.base_sha || '' }}")

    @staticmethod
    def _gh_ternary(condition, true_branch, false_branch):
        """Mimic a GitHub Actions `cond && a || b` expression's actual
        short-circuit truthiness (JS-like: only `""`, `0`, `false`, `null`
        are falsy -- notably the non-empty STRING "0" is truthy). Used to
        prove the fetch-depth fix by evaluating behavior, not by re-matching
        the same literal text the workflow already contains."""
        def truthy(value):
            if isinstance(value, str):
                return value != ""
            if isinstance(value, (int, float)):
                return value != 0
            return bool(value)
        if condition and truthy(true_branch):
            return true_branch
        return false_branch

    def test_fetch_depth_ternary_uses_truthy_string_operands_not_falsy_zero(self):
        checkout = next(step for step in JOBS["changes"]["steps"]
                        if step.get("uses") == "actions/checkout@v6")
        expr = checkout["with"]["fetch-depth"]
        self.assertIn("'0'", expr)
        self.assertIn("'1'", expr)
        self.assertNotIn("&& 0 ", expr, "a bare numeric 0 is falsy in GitHub expressions")
        # The historic bug shape: `cond && 0 || 1` always evaluates to 1,
        # even when cond is true, because a true `cond && 0` immediately
        # falls through the `|| 1` (0 is falsy).
        self.assertEqual(self._gh_ternary(True, 0, 1), 1)
        self.assertEqual(self._gh_ternary(False, 0, 1), 1)
        # The fix: quoted string operands are truthy regardless of their
        # digits, so the merge_group branch ('0') is actually reachable.
        self.assertEqual(self._gh_ternary(True, "0", "1"), "0")
        self.assertEqual(self._gh_ternary(False, "0", "1"), "1")

    def test_pull_request_candidate_parents_step_is_wired(self):
        steps = JOBS["ci-ok"]["steps"]
        parents_step = next(step for step in steps if step.get("id") == "candidate-parents")
        self.assertEqual(parents_step["if"], "github.event_name == 'pull_request'")
        self.assertTrue(parents_step.get("continue-on-error"))
        self.assertIn('git cat-file -p "${{ github.sha }}"', parents_step["run"])
        self.assertIn("parent", parents_step["run"])
        gate = next(step for step in steps if step.get("name") == "Evaluate merge gate")
        self.assertLess(steps.index(parents_step), steps.index(gate))
        argv = shlex.split(gate["run"].replace("\\\n", " "))
        self.assertEqual(argv[argv.index("--candidate-parents") + 1],
                          "${{ steps.candidate-parents.outputs.parents }}")

    def test_no_checkout_overrides_ref_anywhere_in_the_workflow(self):
        """Every job that produces evidence or a required context must test
        exactly the commit `actions/checkout@v6` gives it by default -- on
        pull_request that is GitHub's prospective merge commit
        (refs/pull/N/merge). An explicit `ref:` could silently swap in the PR
        head (or anything else) instead."""
        checked = 0
        for job_name, job in JOBS.items():
            for step in job.get("steps", []) or []:
                if str(step.get("uses", "")).startswith("actions/checkout@"):
                    checked += 1
                    with self.subTest(job=job_name):
                        self.assertNotIn("ref", step.get("with") or {}, job_name)
        self.assertGreater(checked, 20, "expected checkout to appear in most jobs")

    def test_set_step_fails_closed_instead_of_treating_a_bad_filter_as_skip(self):
        step = next(step for step in JOBS["changes"]["steps"] if step.get("id") == "set")
        self.assertIn("merge_group", step["run"])
        self.assertIn('"${!name:-}"', step["run"])

        def run(event, outputs):
            # Every one of these five names must get an EXPLICIT value on
            # every call -- exactly like the real step's own `env:` block,
            # which always assigns `${{ steps.filter.outputs.X }}` (empty
            # string when that output doesn't exist, never "absent"). Do not
            # rely on omission-from-`outputs` to simulate "missing": GitHub
            # Actions runners set CI=true ambiently for every job by
            # convention, so building env from `{**os.environ, ...}` without
            # this masks exactly the "unset filter output" case this test
            # exists to catch -- the real regression the runner caught here.
            values = {name: outputs.get(name, "") for name in ("GO", "UI", "HELM", "REAGENTS", "CI")}
            env = {**os.environ, "EVENT": event, **values}
            with tempfile.TemporaryDirectory() as tmp:
                output_file = Path(tmp) / "output"
                env["GITHUB_OUTPUT"] = str(output_file)
                script_file = Path(tmp) / "step.sh"
                script_file.write_text(step["run"])
                # Match the runner's actual invocation, not just its text:
                # ci.yml's `defaults.run.shell: bash` maps to GitHub Actions'
                # documented default `bash --noprofile --norc -eo pipefail
                # {0}` (a script FILE), not an inline `bash -c` string.
                result = subprocess.run(
                    ["bash", "--noprofile", "--norc", "-eo", "pipefail", str(script_file)],
                    env=env, capture_output=True, text=True,
                )
                parsed = {}
                if output_file.exists():
                    for line in output_file.read_text().splitlines():
                        key, _, value = line.partition("=")
                        parsed[key] = value
                return result.returncode, result.stderr, parsed

        # push (and any other non-PR/merge_group event) never filters.
        code, _, parsed = run("push", {})
        self.assertEqual(code, 0)
        self.assertEqual(parsed, {k: "true" for k in ("go", "ui", "helm", "reagents", "ci", "images")})

        for event in ("pull_request", "merge_group"):
            with self.subTest(event=event):
                code, _, parsed = run(event, {"GO": "true", "UI": "false", "HELM": "false",
                                              "REAGENTS": "false", "CI": "false"})
                self.assertEqual(code, 0)
                self.assertEqual(parsed["go"], "true")
                self.assertEqual(parsed["ui"], "false")
                self.assertEqual(parsed["images"], "true")

                # A filter output that never ran (unset) must fail closed.
                code, stderr, _ = run(event, {"GO": "true", "UI": "false", "HELM": "false",
                                              "REAGENTS": "false"})
                self.assertNotEqual(code, 0)
                self.assertIn("not true/false", stderr)
                self.assertIn(event, stderr)

                # A malformed (non-boolean-string) filter output must also
                # fail closed rather than being coerced to false.
                code, stderr, _ = run(event, {"GO": "true", "UI": "false", "HELM": "false",
                                              "REAGENTS": "false", "CI": "maybe"})
                self.assertNotEqual(code, 0)
                self.assertIn("not true/false", stderr)

    def test_ci_ok_wires_identity_and_base_freshness(self):
        steps = JOBS["ci-ok"]["steps"]
        freshness = next(step for step in steps if step.get("id") == "base-freshness")
        self.assertEqual(freshness["if"],
                          "github.event_name == 'pull_request' || github.event_name == 'merge_group'")
        self.assertTrue(freshness.get("continue-on-error"))
        self.assertIn("git ls-remote origin refs/heads/master", freshness["run"])
        gate = next(step for step in steps if step.get("name") == "Evaluate merge gate")
        self.assertLess(steps.index(freshness), steps.index(gate))

        argv = shlex.split(gate["run"].replace("\\\n", " "))
        self.assertEqual(argv[argv.index("--event-name") + 1], "${{ github.event_name }}")
        self.assertEqual(argv[argv.index("--current-base-sha") + 1],
                          "${{ steps.base-freshness.outputs.sha }}")
        base = argv[argv.index("--base-sha") + 1]
        self.assertIn("github.event.pull_request.base.sha", base)
        self.assertIn("github.event.merge_group.base_sha", base)
        head = argv[argv.index("--head-sha") + 1]
        self.assertIn("github.event.pull_request.head.sha", head)
        self.assertIn("github.event.merge_group.head_sha", head)


class SystemSuiteLaneWiringTests(unittest.TestCase):
    """distributed-testing G6: each promoted system-suite lane is its own job,
    off the existing image artifacts, with its own manifest gate and report."""

    # job -> (recipe invocation, image artifacts it loads)
    LANES = {
        "lifecycle-standalone": ("lifecycle-standalone", {"builder-amd64", "product-amd64"}),
        "generated-fuzz": ("generated-fuzz", {"builder-amd64"}),
        "generated-oracles": ("generated-oracles", {"builder-amd64"}),
        "coverage-ratchets": ("coverage-ratchets", {"builder-amd64"}),
    }

    def gate_argv(self):
        step = next(step for step in JOBS["ci-ok"]["steps"] if step.get("name") == "Evaluate merge gate")
        return shlex.split(step["run"].replace("\\\n", " "))

    def test_every_promoted_lane_is_wired_and_classified(self):
        self.assertEqual(set(G6_LANES), set(self.LANES))
        needs = JOBS["ci-ok"]["needs"]
        for job in G6_LANES:
            with self.subTest(job=job):
                self.assertIn(job, JOBS)
                self.assertIn(job, needs)
                self.assertIn(job, SELECTORS)
                self.assertNotIn(job, UNPROMOTED_LANES)
                # Runs in parallel with early-evidence: it never waits on it.
                self.assertNotIn("early-evidence", JOBS[job]["needs"])
                self.assertIn("changes", JOBS[job]["needs"])
                # The required wrapper contexts keep their G5 meaning.
                for wrapper in ("build-and-integration-test", "build-and-integration-test-agent-auth"):
                    self.assertNotIn(job, JOBS[wrapper]["needs"])

    def test_lane_gates_have_registered_manifest_rows(self):
        for job, gate in G6_LANES.items():
            with self.subTest(job=job, gate=gate):
                rows = gated_scenarios(gate=gate)
                self.assertTrue(rows, f"gate {gate!r} has no manifest rows")
                for row in rows:
                    self.assertEqual(row["status"], "proven", row["id"])
                # The recipe validates the SAME gate ci-ok re-validates.
                body = recipe_body(self.LANES[job][0].split()[0])
                self.assertIn(f"--require {gate} --strict", body)
                self.assertIn(f"--gate {gate}", body)

    def test_lane_runs_its_recipe_off_loaded_images_and_never_a_compile_job(self):
        for job, (invocation, artifacts) in self.LANES.items():
            with self.subTest(job=job):
                steps = JOBS[job]["steps"]
                loads = {step["with"]["name"] for step in steps
                         if step.get("uses") == "./.github/actions/load-docker-images"}
                self.assertEqual(loads, artifacts)
                runs = [step["run"] for step in steps if f"just tag=${{{{ env.IMAGE_TAG }}}}-amd64 {invocation}" in step.get("run", "")]
                self.assertEqual(len(runs), 1, job)
                self.assertEqual(JOBS[job]["env"]["CANDIDATE_SHA"], "${{ github.sha }}")
                self.assertFalse([step for step in steps if "docker/build-push-action" in str(step.get("uses", ""))])

    def test_lane_uploads_its_report_even_on_failure(self):
        for job in G6_LANES:
            with self.subTest(job=job):
                steps = JOBS[job]["steps"]
                report = next(step for step in steps if step.get("with", {}).get("name") == f"{job}-evidence")
                self.assertEqual(report["uses"], "actions/upload-artifact@v7")
                self.assertEqual(report["if"], "always()")
                self.assertEqual(report["with"]["if-no-files-found"], "error")
                self.assertEqual(report["with"]["path"], f".tmp/lane-evidence/{job}/*.json")

    def test_ci_ok_downloads_and_revalidates_every_lane_report(self):
        steps = JOBS["ci-ok"]["steps"]
        gate = next(step for step in steps if step.get("name") == "Evaluate merge gate")
        argv = self.gate_argv()
        passed = {}
        for index, item in enumerate(argv):
            if item == "--lane-evidence":
                job, _, path = argv[index + 1].partition("=")
                passed[job] = path
        self.assertEqual(set(passed), set(G6_LANES))
        for job, path in passed.items():
            with self.subTest(job=job):
                download = next(step for step in steps
                                if step.get("uses", "").startswith("actions/download-artifact")
                                and step.get("with", {}).get("name") == f"{job}-evidence")
                self.assertEqual(download["if"], f"needs.{job}.result == 'success'")
                self.assertLess(steps.index(download), steps.index(gate))
                self.assertEqual(str(Path(path).parent), download["with"]["path"])
                self.assertEqual(Path(path).name, "evidence.json")
                self.assertIn(job, argv)

    def test_lanes_select_their_code_fixture_build_and_workflow_changes(self):
        common = [
            ("internal/run/store.go", "code"),
            ("test/contracts/scenarios.json", "scenario manifest"),
            ("go.mod", "dependency"),
            ("build/Dockerfile.robustness", "image"),
            ("justfile", "recipe"),
            ("scripts/collect-lane-evidence.py", "artifact consumer"),
            (".github/workflows/ci.yml", "workflow"),
        ]
        specific = {
            "lifecycle-standalone": [("test/lifecycle/standalone_test.go", "lane test code"),
                                     ("test/lifecycle/versions.json", "version matrix"),
                                     ("scripts/lifecycle-tests.sh", "lane runner")],
            "generated-fuzz": [("internal/run/descriptor_fuzz_test.go", "fuzz target"),
                               ("scripts/fuzz-tests.sh", "lane runner")],
            "generated-oracles": [("test/model/oracle_regression_test.go", "oracle"),
                                  ("test/model/testdata/mutations/lost-ack.patch", "mutation"),
                                  ("scripts/validate-test-oracles.sh", "lane runner")],
            "coverage-ratchets": [("ui/src/main.tsx", "browser journey"),
                                  ("scripts/coverage-ratchet.json", "ratchet"),
                                  ("build/Dockerfile.coverage", "coverage image")],
        }
        for job in G6_LANES:
            selectors = job_selectors(job)
            self.assertEqual(selectors, set(SELECTORS[job]), job)
            for path, reason in common + specific[job]:
                with self.subTest(job=job, path=path, reason=reason):
                    self.assertTrue(selected_groups(path) & selectors, path)
            for path in ("docs/ci.md", "README.md", "docs/exec-plans/active/distributed-testing.md"):
                with self.subTest(job=job, path=path):
                    self.assertFalse(selected_groups(path) & selectors, path)

    def test_integration_subpackages_are_compiled_explicitly_with_the_tag(self):
        # The precompiled ./test runner does not contain subpackage tests; each
        # subpackage lane compiles its own runner with -tags=integration.
        robustness = (ROOT / "build/Dockerfile.robustness").read_text()
        self.assertIn("go test -tags=integration -c ./test/robustness", robustness)
        lifecycle = (ROOT / "scripts/lifecycle-tests.sh").read_text()
        self.assertIn("go test -tags=integration -c ./test/lifecycle", lifecycle)
        self.assertIn("CAESIUM_LIFECYCLE_BUILDER_IMAGE", JOBS["lifecycle-standalone"]["steps"][-3]["run"])

    def test_standalone_lifecycle_binds_the_supplied_image_to_its_producer(self):
        images = JOBS["images"]["steps"]
        record = next(step for step in images if step.get("name") == "Record release image identity")
        upload = next(step for step in images if step.get("with", {}).get("name") == "image-ids-amd64")
        self.assertLess(images.index(record), images.index(upload))
        self.assertIn("caesiumcloud/caesium:${{ env.IMAGE_TAG }}-amd64", record["run"])
        steps = JOBS["lifecycle-standalone"]["steps"]
        download = next(step for step in steps if step.get("with", {}).get("name") == "image-ids-amd64")
        run = next(step for step in steps if "lifecycle-standalone" in step.get("run", ""))
        self.assertLess(steps.index(download), steps.index(run))
        self.assertIn("CAESIUM_LIFECYCLE_EXPECTED_IMAGE_ID=\"$expected\"", run["run"])
        self.assertIn("CAESIUM_LIFECYCLE_ALLOW_UNVERIFIED_IMAGE=1", run["run"])
        body = recipe_body("lifecycle-standalone")
        # An override without its producer's id is refused by the recipe.
        self.assertIn("needs CAESIUM_LIFECYCLE_EXPECTED_IMAGE_ID", body)
        self.assertIn('--expected-image-id "$expected"', body)

    def _podman_metadata_fixture(self, fault=None):
        """Quay transport seam for the actual embedded workflow resolver."""
        import time
        import urllib.error

        index_type = "application/vnd.oci.image.index.v1+json"
        manifest_type = "application/vnd.oci.image.manifest.v1+json"
        config_type = "application/vnd.oci.image.config.v1+json"
        repository = "quay.io/podman/stable"
        secret = "bearer-secret-never-retained"
        config = json.dumps({"os": "linux", "architecture": "arm64" if fault == "config-platform" else "amd64",
                             "config": {"Labels": {"org.opencontainers.image.version":
                                 "5.8.8" if fault == "version" else "5.8.7"}}}).encode()
        config_id = "sha256:" + hashlib.sha256(config).hexdigest()
        config_descriptor = {"mediaType": config_type, "digest": config_id, "size": len(config)}
        if fault in {"config-digest", "config-media", "config-size"}:
            config_descriptor[{"config-digest": "digest", "config-media": "mediaType", "config-size": "size"}[fault]] = {
                "config-digest": "sha256:" + "aa" * 32, "config-media": manifest_type, "config-size": len(config) + 1}[fault]
        child = json.dumps({"schemaVersion": 1 if fault == "child-schema" else 2,
                            "mediaType": config_type if fault == "child-media" else manifest_type,
                            "config": config_descriptor, "layers": []}).encode()
        child_id = "sha256:" + hashlib.sha256(child).hexdigest()
        descriptor = {"mediaType": manifest_type, "digest": child_id, "size": len(child),
                      "platform": {"os": "linux", "architecture": "amd64"}}
        changes = {"descriptor-digest": ("digest", "sha256:bad"),
                   "descriptor-size": ("size", len(child) + 1),
                   "descriptor-bool-size": ("size", True),
                   "descriptor-media": ("mediaType", config_type)}
        if fault in changes:
            key, value = changes[fault]
            descriptor[key] = value
        if fault == "missing-platform":
            del descriptor["platform"]
        if fault == "foreign-platform":
            descriptor["platform"]["architecture"] = "arm64"
        if fault == "variant":
            descriptor["platform"]["variant"] = "v1"
        manifests = [] if fault == "missing-child" else [descriptor] * (2 if fault == "duplicate-child" else 1)
        index = json.dumps({"schemaVersion": 1 if fault == "index-schema" else 2,
                            "mediaType": manifest_type if fault == "index-media" else index_type,
                            "manifests": manifests}).encode()
        index_id = "sha256:" + hashlib.sha256(index).hexdigest()
        log = []

        class Response:
            def __init__(self, body, media="application/json", header_digest=None, read_error=False, read_stall=False):
                self.body, self.offset, self.read_error = body, 0, read_error
                self.read_stall = read_stall
                self.code = 200
                self.headers = {"Content-Type": media, "Content-Length": str(len(body))}
                if header_digest:
                    self.headers["Docker-Content-Digest"] = header_digest

            def __enter__(self):
                return self

            def __exit__(self, *args):
                pass

            def read(self, size):
                if self.read_stall:
                    if not hasattr(self, "buffered"):
                        body = self.body

                        class Trickle(io.RawIOBase):
                            offset = 0

                            def readable(self):
                                return True

                            def readinto(self, buffer):
                                # Every receive is below the socket inactivity
                                # timeout, while BufferedReader.read stays blocked.
                                time.sleep(0.04)
                                chunk = body[self.offset:self.offset + 64]
                                buffer[:len(chunk)] = chunk
                                self.offset += len(chunk)
                                return len(chunk)

                        self.buffered = io.BufferedReader(Trickle())
                    return self.buffered.read(size)
                if self.read_error and self.offset:
                    raise OSError(secret + " https://cdn01.quay.io/config?signature=private")
                chunk = self.body[self.offset:self.offset + size]
                self.offset += len(chunk)
                return chunk

        class Opener:
            def open(opener_self, request, timeout):
                log.append(("http", request.full_url, dict(request.header_items())))
                self.assertEqual(timeout, 10)
                url = request.full_url
                if "/v2/auth?" in url:
                    if fault == "wall-auth-stall":
                        time.sleep(0.4)
                    self.assertNotIn("Authorization", dict(request.header_items()))
                    return Response(json.dumps({"token": secret}).encode())
                if url.startswith("https://cdn01.quay.io/"):
                    if fault == "wall-redirect-stall":
                        time.sleep(0.08)
                    self.assertNotIn("Authorization", dict(request.header_items()))
                    self.assertNotIn("Cookie", dict(request.header_items()))
                    return Response(config, "application/octet-stream")
                self.assertEqual(request.get_header("Authorization"), "Bearer " + secret)
                if "/blobs/" in url:
                    if fault in {"redirect", "foreign-redirect", "credential-redirect", "wall-redirect-stall"}:
                        if fault == "wall-redirect-stall":
                            time.sleep(0.08)
                        location = {"foreign-redirect": "https://evil.example/config?signature=private",
                                    "credential-redirect": "https://user:password@cdn01.quay.io/config?signature=private"}.get(
                                        fault, "https://cdn01.quay.io/config?signature=private")
                        raise urllib.error.HTTPError(url, 302, "signed " + location, {"Location": location}, io.BytesIO())
                    return Response(config + (b"corruption" if fault == "config-body" else b""), config_type, config_id)
                if url.endswith("manifests/v5.8.7"):
                    if fault == "wall-open-stall":
                        # opener.open includes SSL negotiation and header parsing.
                        time.sleep(0.4)
                    # A changing tag would return unrelated bytes if rediscovered.
                    self.assertEqual(sum(entry[0] == "http" and entry[1].endswith("manifests/v5.8.7") for entry in log), 1)
                    response = Response(index, index_type, "sha256:" + "ff" * 32 if fault == "header" else index_id,
                                        read_error=fault == "partial-read-error", read_stall=fault == "wall-body-stall")
                    if fault == "overflow":
                        response.body = index + b" " * (256 * 1024)
                        del response.headers["Content-Length"]
                    if fault == "truncated":
                        response.headers["Content-Length"] = str(len(index) + 1)
                    if fault == "response-media":
                        response.headers["Content-Type"] = manifest_type
                    return response
                if url.endswith("manifests/" + index_id):
                    return Response(index + (b" " if fault == "immutable-change" else b""), index_type,
                                    index_id)
                self.assertTrue("/manifests/" in url)
                return Response(child + (b"corruption" if fault == "child-body" else b""), manifest_type, child_id)

        return {"opener": Opener(), "log": log, "index": index, "child": child, "config": config,
                "index_id": index_id, "child_id": child_id, "config_id": config_id,
                "child_ref": repository + "@" + child_id, "index_ref": repository + "@" + index_id,
                "secret": secret}

    def _exercise_podman_resolution(self, fault=None, loaded_kind="config", wall_budget=None):
        import signal
        import urllib.request
        from unittest.mock import patch

        step = next(item for item in JOBS["coverage-ratchets"]["steps"]
                    if item.get("name") == "Pin isolated backend prerequisites")
        code = step["run"].split("<<'PY'\n", 1)[1].rsplit("\nPY", 1)[0]
        # Execute the exact resolver/acquisition portion; the adjacent harness
        # executes this same code through task export and input publication.
        code = code.split("podman_receipt['prerequisite_phase'] = 'task-pulls'", 1)[0]
        fixture = self._podman_metadata_fixture(fault)
        log = fixture["log"]
        identity = fixture[loaded_kind + "_id"]
        setitimer = signal.setitimer

        def timer(which, seconds, interval=0):
            log.append(("timer", seconds, interval))
            return setitimer(which, wall_budget if wall_budget is not None and seconds == 30 else seconds, interval)

        def call(args, timeout):
            log.append(("pull", args))
            self.assertEqual(timeout, 180)
            self.assertEqual(args, ["docker", "pull", "--platform", "linux/amd64", fixture["child_ref"]])
            if fault == "disappeared":
                raise subprocess.CalledProcessError(1, args, stderr=fixture["secret"])

        def output(args, text):
            log.append(("inspect", args))
            self.assertEqual(args, ["docker", "image", "inspect", fixture["child_ref"]])
            if fault == "inspect-error":
                raise OSError(fixture["secret"])
            value = {"Id": "sha256:" + "cc" * 32 if fault == "foreign-id" else identity,
                     "Os": "linux", "Architecture": "arm64" if fault == "loaded-platform" else "amd64",
                     "RepoDigests": [fixture["index_ref"]]}
            if fault == "missing-id":
                del value["Id"]
            if fault == "foreign-repodigest":
                value["RepoDigests"] = ["foreign.example/image@" + fixture["child_id"]]
            if fault == "ambiguous-repodigest":
                value["RepoDigests"].append(fixture["child_ref"])
            return json.dumps([value] * (2 if fault == "ambiguous-id" else 1))

        with tempfile.TemporaryDirectory() as directory:
            with patch.object(sys, "argv", ["-", directory]), \
                    patch.object(urllib.request, "build_opener", return_value=fixture["opener"]), \
                    patch.object(signal, "setitimer", side_effect=timer), \
                    patch.object(subprocess, "check_call", side_effect=call), \
                    patch.object(subprocess, "check_output", side_effect=output):
                error = None
                try:
                    exec(compile(code, "coverage-prerequisites", "exec"), {})
                except AssertionError as exc:
                    error = exc
            proof = Path(directory) / "proof"
            receipt = json.loads((proof / "podman-service-receipt.json").read_text())
            retained = b"".join(path.read_bytes() for path in proof.iterdir())
            self.assertNotIn(fixture["secret"].encode(), retained)
            self.assertNotIn(b"signature=private", retained)
            self.assertNotIn(b"user:password", retained)
            self.assertEqual(sum(entry[0] == "http" and entry[1].endswith("manifests/v5.8.7") for entry in log),
                             0 if fault == "wall-auth-stall" else 1)
            self.assertFalse((Path(directory) / "backend-producer-inputs.json").exists())
            if fault not in {None, "redirect", "retarget"}:
                self.assertIsNotNone(error, fault)
                self.assertNotIn(fixture["secret"], str(error))
                self.assertEqual(receipt["outcome"], "refused")
            else:
                self.assertIsNone(error)
                self.assertEqual(receipt["outcome"], "acquired")
                self.assertEqual(receipt["docker_image_id"], identity)
                self.assertEqual(receipt["docker_repo_digests"], [fixture["index_ref"]])
                self.assertEqual(receipt["discovery_ref"], "quay.io/podman/stable:v5.8.7")
                self.assertEqual(receipt["index_id"], fixture["index_id"])
                self.assertEqual(receipt["manifest_id"], fixture["child_id"])
                self.assertEqual(receipt["config_id"], fixture["config_id"])
                self.assertEqual(len({receipt[key] for key in ("index_id", "manifest_id", "config_id")}), 3)
            if any(entry[0] == "pull" for entry in log):
                for key, data in (("index_metadata", fixture["index"]), ("immutable_index_metadata", fixture["index"]),
                                  ("manifest_metadata", fixture["child"]), ("config_metadata", fixture["config"])):
                    self.assertEqual((proof / receipt[key]).read_bytes(), data)
                    self.assertEqual(receipt["metadata_sha256"][receipt[key]], hashlib.sha256(data).hexdigest())
                self.assertEqual(sum(entry[0] == "pull" for entry in log), 1)
                self.assertLess(max(i for i, entry in enumerate(log) if entry[0] == "http"),
                                next(i for i, entry in enumerate(log) if entry[0] == "pull"))
            if fault == "missing-id":
                self.assertFalse(receipt["loaded_image_observed"])
                self.assertIsNone(receipt["docker_image_id"])
            if fault == "disappeared":
                self.assertFalse(receipt["loaded_image_observed"])
                self.assertNotIn("docker_image_id", receipt)
                self.assertEqual(receipt["phase"], "immutable-pull")
            return receipt, log

    def test_podman_metadata_wall_deadline_interrupts_open_and_buffered_read(self):
        import signal
        import time

        for fault, phase in (("wall-auth-stall", "auth"), ("wall-open-stall", "discovery-index"),
                             ("wall-body-stall", "discovery-index")):
            with self.subTest(fault=fault):
                before_handler = signal.getsignal(signal.SIGALRM)
                before_timer = signal.getitimer(signal.ITIMER_REAL)
                started = time.monotonic()
                receipt, log = self._exercise_podman_resolution(fault, wall_budget=0.12)
                self.assertLess(time.monotonic() - started, 0.35)
                self.assertEqual(receipt["error_type"], "TimeoutError")
                self.assertEqual(receipt["phase"], phase)
                self.assertFalse(receipt["loaded_image_observed"])
                self.assertFalse(any(entry[0] == "pull" for entry in log))
                self.assertEqual(signal.getsignal(signal.SIGALRM), before_handler)
                self.assertEqual(signal.getitimer(signal.ITIMER_REAL), before_timer)

    def test_podman_metadata_redirect_shares_original_wall_deadline(self):
        import time

        started = time.monotonic()
        receipt, log = self._exercise_podman_resolution("wall-redirect-stall", wall_budget=0.12)
        self.assertLess(time.monotonic() - started, 0.35)
        self.assertEqual(receipt["error_type"], "TimeoutError")
        self.assertEqual(receipt["phase"], "selected-config")
        self.assertEqual(receipt["request_count"], 6)
        self.assertFalse(any(entry[0] == "pull" for entry in log))
        # Auth/index/immutable index/manifest/config have five wall timers;
        # the cross-origin config request must not allocate a sixth budget.
        self.assertEqual(sum(entry[0] == "timer" and entry[1] == 30 for entry in log), 5)

    def test_podman_metadata_wall_timer_restores_prior_handler_and_timer(self):
        import signal
        import time

        def prior_handler(signum, frame):
            self.fail("the prior long-lived timer must not expire")

        saved_handler = signal.getsignal(signal.SIGALRM)
        saved_timer = signal.setitimer(signal.ITIMER_REAL, 0)
        try:
            signal.signal(signal.SIGALRM, prior_handler)
            signal.setitimer(signal.ITIMER_REAL, 60, 7)
            for fault in (None, "wall-body-stall"):
                with self.subTest(fault=fault):
                    before, interval = signal.getitimer(signal.ITIMER_REAL)
                    started = time.monotonic()
                    self._exercise_podman_resolution(fault, wall_budget=0.12)
                    after, restored_interval = signal.getitimer(signal.ITIMER_REAL)
                    self.assertIs(signal.getsignal(signal.SIGALRM), prior_handler)
                    self.assertEqual(restored_interval, interval)
                    self.assertAlmostEqual(after, before - (time.monotonic() - started), delta=0.02)
        finally:
            signal.setitimer(signal.ITIMER_REAL, 0)
            signal.signal(signal.SIGALRM, saved_handler)
            signal.setitimer(signal.ITIMER_REAL, *saved_timer)

    def test_podman_v587_resolution_chain_and_actual_id(self):
        for kind in ("index", "child", "config"):
            with self.subTest(loaded_kind=kind):
                self._exercise_podman_resolution(loaded_kind=kind)

    def test_podman_v587_resolution_refuses_partial_or_ambiguous_index(self):
        controls = ("header", "partial-read-error", "overflow", "truncated", "response-media", "immutable-change",
                    "index-schema", "index-media", "missing-platform", "missing-child", "duplicate-child",
                    "foreign-platform", "variant", "descriptor-digest", "descriptor-size", "descriptor-bool-size",
                    "descriptor-media", "child-schema", "child-media", "child-body", "config-digest",
                    "config-media", "config-size", "config-body", "config-platform", "version",
                    "foreign-id", "missing-id", "ambiguous-id", "foreign-repodigest", "ambiguous-repodigest", "loaded-platform")
        for fault in controls:
            with self.subTest(fault=fault):
                self._exercise_podman_resolution(fault)

    def test_podman_v587_digest_pull_refuses_retarget_and_disappearance(self):
        self._exercise_podman_resolution("retarget")
        self._exercise_podman_resolution("disappeared")
        self._exercise_podman_resolution("inspect-error")

    def test_podman_resolution_redacts_credentials_and_redirects(self):
        receipt, log = self._exercise_podman_resolution("redirect")
        self.assertEqual(receipt["request_count"], 6)
        self.assertTrue(any(entry[0] == "http" and entry[1].startswith("https://cdn01.quay.io/") for entry in log))
        for fault in ("foreign-redirect", "credential-redirect", "partial-read-error"):
            with self.subTest(fault=fault):
                self._exercise_podman_resolution(fault)

    def _exercise_backend_task_export(self, fault=None, task_alias_loaded=False):
        """Execute the actual workflow prerequisite code against a hermetic daemon."""
        import hashlib
        import tarfile
        import types
        import urllib.request
        from unittest.mock import patch

        step = next(item for item in JOBS["coverage-ratchets"]["steps"]
                    if item.get("name") == "Pin isolated backend prerequisites")
        code = step["run"].split("<<'PY'\n", 1)[1].rsplit("\nPY", 1)[0]
        config = json.dumps({"os": "linux", "architecture": "arm64" if fault == "config-platform" else "amd64"}).encode()
        config_id = "sha256:" + hashlib.sha256(config).hexdigest()
        child = json.dumps({"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json", "config": {
            "mediaType": "application/vnd.oci.image.config.v1+json", "digest": config_id,
            "size": len(config) + (1 if fault == "config-size" else 0)}, "layers": []}).encode()
        child_id = "sha256:" + hashlib.sha256(child).hexdigest()
        descriptor = {"mediaType": "application/vnd.oci.image.manifest.v1+json", "size": len(child),
                      "digest": child_id, "platform": {"os": "linux", "architecture": "amd64"}}
        index = json.dumps({"schemaVersion": 2, "manifests": [descriptor] * (2 if fault == "ambiguous" else 1)}).encode()
        index_ref = "docker.io/library/alpine@sha256:" + hashlib.sha256(index).hexdigest()
        task_supplier_ref = "docker.io/library/alpine:3.23@" + index_ref.split("@")[1]
        production_task_supplier_ref = "docker.io/library/alpine:3.23@sha256:85fe1e81d6758c208f3e1eed4338a1997e19d4be002d4dd32d3100c9a8c010a0"
        kind_supplier_ref = "kindest/node:v1.36.1@sha256:3489c7674813ba5d8b1a9977baea8a6e553784dab7b84759d1014dbd78f7ebd5"
        self.assertIn("kind_image_supplier_ref = " + repr(kind_supplier_ref), code)
        self.assertEqual(code.count("task_image_supplier_ref = " + repr(production_task_supplier_ref)), 1)
        # Registry bytes are synthetic in this hermetic daemon. Rebind only
        # their external pin; execute the actual repository/digest validator.
        code = code.replace(repr(production_task_supplier_ref), repr(task_supplier_ref))
        child_ref = "alpine@" + child_id
        loaded_id = "sha256:" + "ab" * 32 if fault == "daemon" else child_id
        kind_id = "sha256:" + "cd" * 32
        calls = []
        task_pulls = []
        alias_inspections = 0
        tags = []
        backend = runpy.run_path(str(ROOT / "scripts/coverage-backends.py"))
        self.assertEqual(backend["PINNED_TASK_SUPPLIER_REF"], production_task_supplier_ref)
        self.assertEqual(backend["PINNED_KIND_SUPPLIER_REF"], kind_supplier_ref)
        validator = backend["validate_task_supplier_index_ref"]
        backend["validate_task_supplier_index_ref"] = types.FunctionType(
            validator.__code__, {**validator.__globals__, "PINNED_TASK_SUPPLIER_REF": task_supplier_ref},
            validator.__name__, validator.__defaults__, validator.__closure__)
        podman = self._podman_metadata_fixture()

        def output(args, text=False):
            nonlocal alias_inspections
            podman["log"].append(("command-output", args))
            if args[:3] == ["docker", "image", "inspect"]:
                ref = args[3]
                if ref == podman["child_ref"]:
                    return json.dumps([{"Id": podman["config_id"], "Os": "linux", "Architecture": "amd64",
                                        "RepoDigests": [podman["index_ref"]]}])
                self.assertIn(ref, (task_supplier_ref, "alpine:3.23", child_ref, kind_supplier_ref))
                identity = kind_id if ref == kind_supplier_ref else loaded_id if ref == child_ref else index_ref.split("@")[1]
                if fault == "pulled-daemon" and ref in (task_supplier_ref, "alpine:3.23"):
                    identity = "sha256:" + "ef" * 32
                if ref == "alpine:3.23":
                    alias_inspections += 1
                    if (fault == "retag" and alias_inspections == 3
                            or fault == "retag-before" and alias_inspections == 2
                            or fault == "alias-ownership" and alias_inspections == 1):
                        identity = "sha256:" + "ef" * 32
                value = [{"Id": identity, "Os": "linux", "Architecture": "arm64" if fault == "platform" else "amd64",
                          "RepoDigests": [index_ref],
                          "RepoTags": ["alpine:3.23"] if task_alias_loaded or ref == "alpine:3.23" else []}]
                if ref == task_supplier_ref:
                    if fault == "repo-digest":
                        value[0]["RepoDigests"] = ["alpine@sha256:" + "12" * 32]
                    elif fault == "foreign-repo-digest":
                        value[0]["RepoDigests"] = ["foreign.example/alpine@" + index_ref.split("@")[1]]
                    elif fault == "ambiguous-repo-digest":
                        value[0]["RepoDigests"].append("alpine@sha256:" + "12" * 32)
                return json.dumps(value) if text else json.dumps(value).encode()
            self.assertEqual(args[:4], ["docker", "buildx", "imagetools", "inspect"])
            self.assertEqual(args[-1], "--raw")
            self.assertIn(args[4], (index_ref, child_ref))
            raw = index if args[4] == index_ref else child
            return raw + (b"corruption" if fault == "metadata" or fault == "child-metadata" and args[4] == child_ref else b"\n")

        def call(args, **kwargs):
            podman["log"].append(("command-call", args))
            if args == ["docker", "pull", "--platform", "linux/amd64", podman["child_ref"]]:
                return 0
            if args == ["docker", "tag", task_supplier_ref, "alpine:3.23"]:
                self.assertFalse(task_alias_loaded)
                tags.append(args)
                return 0
            if args == ["docker", "pull", "--platform", "linux/amd64", kind_supplier_ref]:
                self.assertEqual(task_pulls, [])
                task_pulls.append(args)
                return 0
            if args == ["docker", "pull", "--platform", "linux/amd64", task_supplier_ref]:
                self.assertEqual(len(task_pulls), 1)
                task_pulls.append(args)
                return 0
            calls.append(args)
            self.assertEqual(args, ["docker", "pull", "--platform", "linux/amd64", child_ref])
            return 0

        def export(args, input, text, check, timeout):
            podman["log"].append(("command-run", args))
            calls.append(args)
            if fault == "export-command":
                raise RuntimeError("export failed")
            self.assertEqual(args[:3], ["docker", "buildx", "build"])
            self.assertEqual(args[3:11], ["--platform", "linux/amd64", "--network", "none",
                                        "--provenance=false", "--sbom=false", "--output", args[10]])
            self.assertEqual(args[11:], ["-t", "alpine:3.23", "-"])
            self.assertTrue(args[10].startswith("type=docker,dest="))
            self.assertEqual(input, "FROM docker.io/library/" + child_ref + "\n")
            self.assertTrue(text and check)
            self.assertEqual(timeout, 180)
            self.assertNotIn("--load", args)
            path = args[10].removeprefix("type=docker,dest=")
            saved = json.dumps({"os": "linux", "architecture": "arm64"}).encode() if fault == "config" else config
            config_path = "blobs/sha256/" + config_id.split(":")[1]
            child_path = "blobs/sha256/" + child_id.split(":")[1]
            archive_descriptor = dict(descriptor)
            if fault == "media":
                archive_descriptor["mediaType"] = "application/foreign.manifest"
            entries = {config_path: saved, child_path: child,
                       "oci-layout": b'{"imageLayoutVersion":"1.0.0"}',
                       "index.json": json.dumps({"schemaVersion": 2, "manifests": [archive_descriptor]}).encode(),
                       "manifest.json": json.dumps([{"Config": config_path,
                            "RepoTags": ["foreign:3.23" if fault == "tag" else "alpine:3.23"], "Layers": []}]).encode()}
            if fault == "closure":
                del entries[child_path]
            if fault == "missing-config":
                del entries[config_path]
            with tarfile.open(path, "w") as archive:
                for name, data in entries.items():
                    member = tarfile.TarInfo(name)
                    if fault == "config-link" and name == config_path:
                        member.type, member.linkname = tarfile.SYMTYPE, "/foreign"
                        archive.addfile(member)
                    else:
                        member.size = len(data)
                        archive.addfile(member, io.BytesIO(data))
                if fault == "duplicate":
                    member = tarfile.TarInfo(config_path)
                    member.size = len(config)
                    archive.addfile(member, io.BytesIO(config))
            return subprocess.CompletedProcess(args, 0)

        write_text = Path.write_text

        def write(path, *args, **kwargs):
            if fault == "input-write" and path.name == "backend-producer-inputs.json.pending":
                raise OSError("input write failed")
            return write_text(path, *args, **kwargs)

        with tempfile.TemporaryDirectory() as directory:
            with patch.object(sys, "argv", ["-", directory]), \
                    patch.object(urllib.request, "build_opener", return_value=podman["opener"]), \
                    patch.object(Path, "write_text", new=write), \
                    patch.object(subprocess, "check_output", side_effect=output), \
                    patch.object(subprocess, "check_call", side_effect=call), \
                    patch.object(subprocess, "run", side_effect=export), \
                    patch.object(runpy, "run_path", return_value=backend):
                try:
                    exec(compile(code, "coverage-prerequisites", "exec"), {})
                except (AssertionError, RuntimeError, OSError):
                    proof = Path(directory) / "proof"
                    podman_receipt = json.loads((proof / "podman-service-receipt.json").read_text())
                    self.assertEqual(podman_receipt["outcome"], "acquired")
                    self.assertEqual(podman_receipt["docker_image_id"], podman["config_id"])
                    self.assertEqual(podman_receipt["prerequisite_outcome"], "refused")
                    for name, data in (("podman-discovery-index.json", podman["index"]),
                                       ("podman-immutable-index.json", podman["index"]),
                                       ("podman-amd64-manifest.json", podman["child"]), ("podman-config.json", podman["config"])):
                        self.assertEqual((proof / name).read_bytes(), data)
                    if fault != "input-write":
                        self.assertFalse((proof / "task-export-receipt.json").exists())
                    self.assertFalse((Path(directory) / "backend-producer-inputs.json").exists())
                    if fault in {"repo-digest", "foreign-repo-digest", "ambiguous-repo-digest", "alias-ownership"}:
                        self.assertEqual(podman_receipt["prerequisite_phase"], "task-metadata")
                        self.assertFalse((proof / "task-index.json").exists())
                        self.assertFalse((proof / "task-amd64-manifest.json").exists())
                        self.assertEqual(calls, [], "supplier/alias refusal must precede child pull and export")
                        self.assertFalse(any(entry[0] == "command-output" and entry[1][:3] == ["docker", "buildx", "imagetools"]
                                             for entry in podman["log"]))
                    # A failed immutable digest check must never publish those bytes.
                    if fault == "metadata":
                        self.assertFalse((proof / "task-index.json").exists())
                    if fault == "child-metadata":
                        self.assertFalse((proof / "task-amd64-manifest.json").exists())
                    if fault in {"missing-config", "media", "closure", "duplicate", "config-link", "config-size", "config", "tag", "retag"}:
                        original = proof / "task-export-original.tar"
                        self.assertEqual(original.read_bytes(), (Path(directory) / "task.tar").read_bytes())
                        inventory = json.loads((proof / "task-export-members.json").read_text())
                        self.assertEqual(inventory["archive_sha256"], hashlib.sha256(original.read_bytes()).hexdigest())
                        manifest = json.loads((proof / "task-export-manifest.json").read_bytes())
                        if fault == "missing-config":
                            self.assertEqual(manifest[0]["Config"], "blobs/sha256/" + config_id.split(":")[1])
                            self.assertNotIn(manifest[0]["Config"], {m["name"] for m in inventory["members"]})
                    raise
            inputs = json.loads((Path(directory) / "backend-producer-inputs.json").read_text())
            proof = Path(directory) / "proof"
            receipt = json.loads((proof / "task-export-receipt.json").read_text())
            self.assertEqual((proof / receipt["index_metadata"]).read_bytes(), index)
            self.assertEqual((proof / receipt["manifest_metadata"]).read_bytes(), child)
            self.assertEqual("sha256:" + hashlib.sha256((proof / receipt["index_metadata"]).read_bytes()).hexdigest(),
                             receipt["index_ref"].split("@")[1])
            self.assertEqual("sha256:" + hashlib.sha256((proof / receipt["manifest_metadata"]).read_bytes()).hexdigest(),
                             receipt["manifest_id"])
            saved_manifest = json.loads((proof / receipt["manifest_metadata"]).read_bytes())
            self.assertEqual(saved_manifest["config"]["digest"], receipt["config_id"])
            self.assertEqual({entry.name for entry in proof.iterdir()},
                             {"task-index.json", "task-amd64-manifest.json", "task-export-receipt.json",
                              "task-export-original.tar", "task-export-members.json", "task-export-manifest.json",
                              "podman-service-receipt.json", "podman-discovery-index.json", "podman-immutable-index.json",
                              "podman-amd64-manifest.json", "podman-config.json"})
            original = proof / "task-export-original.tar"
            self.assertEqual(original.read_bytes(), Path(inputs["task_archive"]).read_bytes())
            inventory = json.loads((proof / "task-export-members.json").read_text())
            self.assertEqual(inventory["archive_sha256"], receipt["archive_sha256"])
            self.assertIn("blobs/sha256/" + config_id.split(":")[1], {m["name"] for m in inventory["members"]})
            self.assertEqual(receipt["exporter"], "buildkit-docker")
            self.assertEqual(receipt["mode"], "from-only-no-load")
            self.assertEqual(receipt["archive_file"], original.name)
            self.assertEqual(receipt["archive_members"], "task-export-members.json")
            self.assertEqual(receipt["archive_manifest"], "task-export-manifest.json")
            self.assertEqual(receipt["source_ref"], "docker.io/library/" + child_ref)
            self.assertEqual(receipt["dockerfile_sha256"], hashlib.sha256(("FROM " + receipt["source_ref"] + "\n").encode()).hexdigest())
            self.assertEqual(receipt["loaded_child_image_id"], child_id)
            self.assertEqual(inputs["task_image_id"], config_id)
            self.assertEqual(inputs["task_docker_image_id"], index_ref.split("@")[1])
            self.assertEqual(receipt["docker_image_id"], inputs["task_docker_image_id"])
            self.assertEqual(inputs["task_image_supplier_ref"], task_supplier_ref)
            self.assertEqual(inputs["kind_image_supplier_ref"], kind_supplier_ref)
            self.assertEqual(inputs["task_image_ref"], "alpine:3.23")
            self.assertEqual(inputs["kind_image_ref"], "kindest/node:v1.36.1")
            self.assertEqual(inputs["kind_image_id"], kind_id)
            self.assertEqual(receipt["supplier_ref"], task_supplier_ref)
            self.assertEqual(receipt["preserved_tag_alias"], "alpine:3.23")
            self.assertEqual(receipt["index_ref"], index_ref)
            self.assertEqual(receipt["manifest_id"], child_id)
            self.assertEqual(receipt["config_id"], config_id)
            self.assertEqual(receipt["archive_sha256"], inputs["task_archive_sha256"])
            provenance = inputs["podman_service_provenance"]
            self.assertEqual(provenance, json.loads((proof / "podman-service-receipt.json").read_text()))
            self.assertEqual(inputs["podman_service_proof_sha256"],
                             hashlib.sha256((proof / "podman-service-receipt.json").read_bytes()).hexdigest())
            self.assertEqual(inputs["podman_service_image_ref"], podman["child_ref"])
            self.assertEqual(inputs["podman_service_image_id"], podman["config_id"])
            self.assertEqual(provenance["index_id"], podman["index_id"])
            self.assertEqual(provenance["manifest_id"], podman["child_id"])
            self.assertEqual(provenance["config_id"], podman["config_id"])
            self.assertEqual(provenance["docker_image_id"], podman["config_id"])
            self.assertEqual(provenance["docker_repo_digests"], [podman["index_ref"]])
            self.assertEqual(sum(entry[0] == "http" and entry[1].endswith("manifests/v5.8.7")
                                 for entry in podman["log"]), 1)
            self.assertEqual(len(calls), 2)
            self.assertEqual(task_pulls, [["docker", "pull", "--platform", "linux/amd64", ref]
                                         for ref in (kind_supplier_ref, task_supplier_ref)])
            self.assertEqual(tags, [] if task_alias_loaded else [["docker", "tag", task_supplier_ref, "alpine:3.23"]])
            image_inspections = [entry[1][3] for entry in podman["log"]
                                 if entry[0] == "command-output" and entry[1][:3] == ["docker", "image", "inspect"]]
            self.assertEqual(image_inspections, [podman["child_ref"], task_supplier_ref, "alpine:3.23",
                                                child_ref, "alpine:3.23", "alpine:3.23", kind_supplier_ref])

    def test_podman_resolution_proof_survives_later_failure(self):
        for fault in ("metadata", "export-command", "input-write"):
            with self.subTest(fault=fault), self.assertRaises((AssertionError, RuntimeError, OSError)):
                self._exercise_backend_task_export(fault)

    def test_coverage_exports_verified_pulled_child_without_index_platform_selection(self):
        for task_alias_loaded in (False, True):
            with self.subTest(task_alias_loaded=task_alias_loaded):
                self._exercise_backend_task_export(task_alias_loaded=task_alias_loaded)

    def test_coverage_task_export_refuses_a_different_valid_supplier_repodigest(self):
        with self.assertRaisesRegex(RuntimeError, "RepoDigest differs from the committed immutable"):
            self._exercise_backend_task_export("repo-digest")

    def test_coverage_task_export_refuses_foreign_or_ambiguous_content(self):
        controls = {"ambiguous": "unique exact", "metadata": "metadata digest",
                    "daemon": "daemon task identity", "platform": "platform mismatch",
                    "config": "exported daemon config", "tag": "unexpected task archive",
                    "closure": "OCI", "child-metadata": "metadata digest",
                    "config-platform": "exported task platform", "retag": "alias changed during export",
                    "retag-before": "alias changed before export", "missing-config": "config member unavailable",
                    "config-size": "config size mismatch", "config-link": "config member unavailable",
                    "duplicate": "duplicate image archive", "media": "OCI index does not bind",
                    "pulled-daemon": "pulled task identity", "alias-ownership": "does not own the preserved archive alias",
                    "foreign-repo-digest": "RepoDigest differs from the committed immutable",
                    "ambiguous-repo-digest": "ambiguous pulled task index"}
        for fault, reason in controls.items():
            with self.subTest(fault=fault), self.assertRaisesRegex((AssertionError, RuntimeError), reason):
                self._exercise_backend_task_export(fault)

    def test_coverage_prerequisite_metadata_has_a_separate_failure_retained_artifact(self):
        steps = JOBS["coverage-ratchets"]["steps"]
        upload = next(step for step in steps if step.get("name") == "Upload backend prerequisite proof")
        self.assertEqual(upload["uses"], "actions/upload-artifact@v7")
        self.assertEqual(upload["if"], "always() && hashFiles('.tmp/coverage-backends/proof/*.json') != ''")
        self.assertEqual(upload["with"]["name"], "coverage-backend-prerequisites")
        self.assertEqual(upload["with"]["path"], ".tmp/coverage-backends/proof/")
        self.assertEqual(upload["with"]["if-no-files-found"], "error")
        original = next(step for step in steps if step.get("name") == "Upload lane artifacts")
        self.assertEqual(original["with"]["path"], ".tmp/lane-evidence/coverage-ratchets/")
        self.assertLess(steps.index(upload), next(i for i, step in enumerate(steps)
                       if step.get("name") == "Coverage collection against the ratchets"))

    def test_coverage_diff_uses_the_events_own_base(self):
        steps = JOBS["coverage-ratchets"]["steps"]
        diff = next(step for step in steps if step.get("name") == "Changed Go paths of the tested change")
        self.assertEqual(
            diff["env"]["DIFF_BASE_SHA"],
            "${{ github.event.pull_request.base.sha || github.event.merge_group.base_sha || github.event.before }}",
        )
        self.assertIn("CAESIUM_COVERAGE_CHANGED_PATHS=", diff["run"])
        self.assertIn("CAESIUM_COVERAGE_DIFF_BASE=", diff["run"])
        self.assertIn("-- '*.go'", diff["run"])
        self.assertNotIn("if", diff)

    def test_every_lane_recipe_refuses_a_foreign_or_dirty_candidate(self):
        check = recipe_body("lane-candidate-check")
        self.assertIn("git status --porcelain", check)
        self.assertIn('!= "$head"', check)
        text = (ROOT / "justfile").read_text()
        for recipe in ("core-robustness", "lifecycle-standalone", "generated-fuzz", "generated-oracles",
                       "coverage-ratchets", "lifecycle-cluster", "console-recovery", "performance-gate"):
            with self.subTest(recipe=recipe):
                header = re.search(rf"^{re.escape(recipe)}(?:\s[^\n]*)?:([^\n]*)$", text, re.M)
                self.assertIsNotNone(header, recipe)
                self.assertIn("lane-candidate-check", header.group(1))


class SystemSuiteLaneGateTests(unittest.TestCase):
    """G6 promoted lanes fail closed in ci-ok exactly like early-evidence."""

    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.dir = Path(tmp.name)
        self.early = self.dir / "early.json"
        self.early.write_text(json.dumps(evidence_report()))

    def lanes(self, **overrides):
        """Honest reports for every lane, with per-lane replacements."""
        reports = {}
        for job, gate in G6_LANES.items():
            path = self.dir / job / "evidence.json"
            path.parent.mkdir(parents=True, exist_ok=True)
            value = overrides.get(job, evidence_report(gate=gate))
            if value is None:
                path.unlink(missing_ok=True)
            else:
                path.write_text(json.dumps(value))
            reports[job] = path
        return reports

    def run_gate(self, expected, needs=None, lane_reports=None, enabled=FLAGS, argv_patch=None):
        command = gate_command(self.early, lane_reports=self.lanes() if lane_reports is None else lane_reports)
        if argv_patch:
            command = argv_patch(command)
        result = subprocess.run(
            command,
            env={**os.environ,
                 "NEEDS_JSON": json.dumps(results(selected_outputs(enabled)) if needs is None else needs)},
            capture_output=True, text=True,
        )
        self.assertEqual(result.returncode, expected, result.stdout + result.stderr)
        return result.stdout + result.stderr

    def test_honest_lane_reports_pass_and_unpromoted_lanes_are_listed(self):
        output = self.run_gate(0)
        self.assertIn("ci-ok passed", output)
        self.assertIn("not merge-blocking", output)
        for name in UNPROMOTED_LANES:
            self.assertIn(f"  {name}: ", output)

    def test_bad_job_results_fail_closed(self):
        for job in G6_LANES:
            for status in ("failure", "cancelled", "skipped", "timed_out", None):
                with self.subTest(job=job, status=status):
                    needs = results(selected_outputs(FLAGS))
                    if status is None:
                        del needs[job]
                    else:
                        needs[job]["result"] = status
                    self.assertIn(f"{job}={status or 'missing'}", self.run_gate(1, needs=needs))

    def test_deselected_lane_owes_no_report(self):
        needs = results(selected_outputs(("ui",)))
        reports = self.lanes(**{job: None for job in G6_LANES if job != "coverage-ratchets"})
        output = self.run_gate(0, needs=needs, lane_reports=reports, enabled=("ui",))
        for job in G6_LANES:
            if job == "coverage-ratchets":
                continue
            self.assertIn(f"ci-ok: {job} was deselected by the path filters", output)

    def test_absent_foreign_or_hollow_report_fails_closed(self):
        for job, gate in G6_LANES.items():
            with self.subTest(job=job, case="absent"):
                self.assertIn(f"{job} evidence:", self.run_gate(1, lane_reports=self.lanes(**{job: None})))
                self.assertIn("produced no evidence", self.run_gate(1, lane_reports=self.lanes(**{job: None})))
            with self.subTest(job=job, case="foreign"):
                self.assertIn("not the tested candidate",
                              self.run_gate(1, lane_reports=self.lanes(**{job: evidence_report(sha="e" * 40, gate=gate)})))
            with self.subTest(job=job, case="hollow"):
                report = evidence_report(gate=gate)
                missing = report["scenarios"].pop()["id"]
                self.assertIn(f"scenario {missing!r} is missing",
                              self.run_gate(1, lane_reports=self.lanes(**{job: report})))
            with self.subTest(job=job, case="failed"):
                report = evidence_report(gate=gate)
                report["scenarios"][0]["status"] = "fail"
                self.assertIn("status 'fail'", self.run_gate(1, lane_reports=self.lanes(**{job: report})))
            with self.subTest(job=job, case="another lane's report"):
                other = next(g for j, g in G6_LANES.items() if j != job)
                self.assertIn(f"{job} evidence",
                              self.run_gate(1, lane_reports=self.lanes(**{job: evidence_report(gate=other)})))

    def test_merge_group_candidate_gets_the_same_lane_verdicts(self):
        """The merge queue runs the promoted lanes on the queue's own head: the
        selectors, evidence binding and fail-closed rules are event-agnostic,
        so what passes on pull_request passes on merge_group and a foreign or
        missing lane report fails it too. (merge_group has never fired on this
        repository; this is the static proof, like G7's.)"""
        head = CANDIDATE_SHA
        def gate(lanes, expected):
            result = subprocess.run(
                gate_command(self.early, lane_reports=lanes, event_name="merge_group",
                             base_sha=CANDIDATE_BASE_SHA, head_sha=head,
                             current_base_sha=CANDIDATE_BASE_SHA, candidate_parents=""),
                env={**os.environ, "NEEDS_JSON": json.dumps(results(selected_outputs(FLAGS)))},
                capture_output=True, text=True,
            )
            self.assertEqual(result.returncode, expected, result.stdout + result.stderr)
            return result.stdout + result.stderr
        self.assertIn("ci-ok passed", gate(self.lanes(), 0))
        for job, lane_gate in G6_LANES.items():
            with self.subTest(job=job):
                foreign = self.lanes(**{job: evidence_report(sha="e" * 40, gate=lane_gate)})
                self.assertIn("not the tested candidate", gate(foreign, 1))
                self.assertIn("produced no evidence", gate(self.lanes(**{job: None}), 1))
        # The lane jobs themselves carry no event-specific condition or input.
        for job in G6_LANES:
            self.assertNotIn("event_name", JOBS[job]["if"])
            self.assertNotIn("inputs.", JOBS[job]["if"])
            self.assertEqual(JOBS[job]["env"]["CANDIDATE_SHA"], "${{ github.sha }}")
            for step in JOBS[job]["steps"]:
                self.assertNotIn("event_name", step.get("if", "") or "")

    def test_missing_or_malformed_lane_flags_fail_closed(self):
        reports = self.lanes()
        for job in G6_LANES:
            with self.subTest(job=job, case="flag omitted"):
                partial = {name: path for name, path in reports.items() if name != job}
                self.assertIn(f"no --lane-evidence {job}=PATH", self.run_gate(1, lane_reports=partial))
        self.assertIn("malformed --lane-evidence",
                      self.run_gate(1, argv_patch=lambda argv: argv + ["--lane-evidence", "no-equals-sign"]))
        self.assertIn("not a promoted G6 evidence lane",
                      self.run_gate(1, argv_patch=lambda argv: argv + ["--lane-evidence", "lifecycle-cluster=x.json"]))
        self.assertIn("not a promoted G6 evidence lane",
                      self.run_gate(1, argv_patch=lambda argv: argv + ["--lane-evidence", f"{EVIDENCE_JOB}=x.json"]))
        job, path = next(iter(reports.items()))
        self.assertIn("more than once",
                      self.run_gate(1, argv_patch=lambda argv: argv + ["--lane-evidence", f"{job}={path}"]))


def run_changes_set_step(event, nightly, **extra):
    """Execute the `changes` job's set step with the given dispatch inputs."""
    step = next(step for step in JOBS["changes"]["steps"] if step.get("id") == "set")
    values = {name: "" for name in ("GO", "UI", "HELM", "REAGENTS", "CI")}
    env = {**os.environ, "EVENT": event, "NIGHTLY": nightly, **values, **extra}
    with tempfile.TemporaryDirectory() as tmp:
        output_file = Path(tmp) / "output"
        env["GITHUB_OUTPUT"] = str(output_file)
        script = Path(tmp) / "step.sh"
        script.write_text(step["run"])
        result = subprocess.run(["bash", "--noprofile", "--norc", "-eo", "pipefail", str(script)],
                                env=env, capture_output=True, text=True)
        parsed = {}
        if output_file.exists():
            for line in output_file.read_text().splitlines():
                key, _, value = line.partition("=")
                parsed[key] = value
        return result.returncode, result.stderr, parsed


def lane_tokens(condition):
    """The lane tokens a qualification-lanes.yml job `if:` selects on."""
    return set(re.findall(r"',(\w[\w-]*),'\)", condition))


class NightlyLaneTests(unittest.TestCase):
    """G6's nightly set, moved by G4 into the reusable qualification-lanes.yml:
    dispatchable from CI, scheduled by testing-qualification.yml, never merge
    evidence."""

    # job -> (recipe invocation substring, recipe)
    INVOCATIONS = {
        "lifecycle-cluster": ("just lifecycle-cluster", "lifecycle-cluster"),
        "console-recovery": ("console-recovery", "console-recovery"),
        "performance-gate": ("just performance-gate", "performance-gate"),
        "core-robustness-nightly": ("core-robustness nightly-core", "core-robustness"),
        "fuzz-campaign": ("generated-fuzz", "generated-fuzz"),
        "soak": ("just soak-tests", "soak-tests"),
    }

    def test_dispatch_trigger_and_inputs(self):
        self.assertIn("workflow_dispatch", TRIGGERS)
        inputs = TRIGGERS["workflow_dispatch"]["inputs"]
        self.assertEqual(inputs["nightly"]["default"], "")
        self.assertEqual(inputs["performance-mode"]["options"], ["advisory", "enforcing"])
        self.assertEqual(inputs["performance-mode"]["default"], "advisory")
        self.assertEqual(inputs["performance-runs"]["default"], "[1]")
        for name in ("publish-dry-run", "simulate-failed-qualification"):
            self.assertEqual((inputs[name]["type"], inputs[name]["default"]), ("boolean", False), name)
        # CI itself is never scheduled; testing-qualification.yml is.
        self.assertNotIn("schedule", TRIGGERS)
        self.assertNotIn("workflow_call", TRIGGERS)

    def test_registry_matches_the_reusable_workflow(self):
        lanes = set(LANE_JOBS) - {"select", "results"}
        self.assertEqual(lanes, set(QUAL_LANES))
        self.assertEqual(lanes, set(self.INVOCATIONS))
        for job, lane in QUAL_LANES.items():
            with self.subTest(job=job):
                self.assertEqual(lane_tokens(LANE_JOBS[job]["if"]), {lane.token, "all"})

    def test_nightly_jobs_run_only_when_named_and_never_gate_a_merge(self):
        for job, (invocation, recipe) in self.INVOCATIONS.items():
            lane = QUAL_LANES[job]
            with self.subTest(job=job):
                condition = LANE_JOBS[job]["if"]
                self.assertEqual(condition,
                                 f"contains(format(',{{0}},', inputs.lanes), ',{lane.token},') || "
                                 "contains(format(',{0},', inputs.lanes), ',all,')")
                self.assertEqual(LANE_JOBS[job]["needs"], ["select"])
                self.assertNotIn(job, JOBS)
                self.assertNotIn(job, SELECTORS)
                self.assertIn(job, UNPROMOTED_LANES)
                self.assertEqual(LANE_JOBS[job]["env"]["CANDIDATE_SHA"], "${{ github.sha }}")
                runs = [step["run"] for step in LANE_JOBS[job]["steps"] if invocation in step.get("run", "")]
                self.assertEqual(len(runs), 1)
                gate = lane.gate
                if gate is None:
                    # Advisory: nothing in the manifest can report it passed.
                    self.assertEqual(lane.role, QUALIFICATION["ADVISORY"])
                    if job == "performance-gate":
                        rows = [row for row in MANIFEST["scenarios"] if "nightly-performance" in row["gates"]]
                        self.assertEqual(rows, [])
                        e4 = next(row for row in MANIFEST["scenarios"] if row["id"] == "e4-performance-gate")
                        self.assertEqual((e4["gates"], e4["status"]), ([], "unproven"))
                else:
                    rows = [row for row in MANIFEST["scenarios"] if gate in row["gates"]]
                    self.assertTrue(rows, gate)
                    self.assertTrue(all(row["status"] == "proven" for row in rows))
                    body = recipe_body(recipe)
                    self.assertTrue(f"--require {gate} --strict" in body
                                    or ('--require "{{ gate }}" --strict' in body and gate in invocation), gate)
                report = next(step for step in LANE_JOBS[job]["steps"]
                              if step.get("with", {}).get("name", "").startswith(lane.artifact))
                self.assertEqual(report["if"], "always()")
                self.assertEqual(report["with"]["if-no-files-found"], "error")
                self.assertEqual(report["with"]["retention-days"], "${{ inputs.retention-days }}")
        # No caller retains a lane's evidence for under 30 days.
        self.assertGreaterEqual(LANES_WORKFLOW[True]["workflow_call"]["inputs"]["retention-days"]["default"], 30)
        self.assertEqual(NIGHTLY_JOBS["qualification"]["with"]["retention-days"], 30)
        self.assertEqual(JOBS["qualification"]["with"]["retention-days"],
                         "${{ startsWith(github.ref, 'refs/tags/v') && 90 || 30 }}")

    def test_nightly_dispatch_deselects_the_pr_matrix_and_rejects_unknown_lanes(self):
        code, _, parsed = run_changes_set_step("workflow_dispatch", "cluster-lifecycle,performance")
        self.assertEqual(code, 0)
        self.assertEqual(parsed, {k: "false" for k in ("go", "ui", "helm", "reagents", "ci", "images")})
        code, _, parsed = run_changes_set_step("workflow_dispatch", "all")
        self.assertEqual(code, 0)
        self.assertEqual(parsed["images"], "false")
        code, stderr, _ = run_changes_set_step("workflow_dispatch", "cluster-lifecycle,typo")
        self.assertNotEqual(code, 0)
        self.assertIn("unknown nightly lane 'typo'", stderr)
        # An empty selection is a push-equivalent full run.
        code, _, parsed = run_changes_set_step("workflow_dispatch", "")
        self.assertEqual(code, 0)
        self.assertEqual(parsed, {k: "true" for k in ("go", "ui", "helm", "reagents", "ci", "images")})
        # The token list in the set step is exactly the registry's tokens.
        step = next(step for step in JOBS["changes"]["steps"] if step.get("id") == "set")
        tokens = re.search(r"\n\s+(all\|[^)]*)\)", step["run"]).group(1).split("|")
        self.assertEqual(set(tokens), {"all"} | {lane.token for lane in QUAL_LANES.values()})

    def test_reusable_select_job_refuses_unknown_lanes(self):
        run = next(step["run"] for step in LANE_JOBS["select"]["steps"] if "run" in step)
        self.assertEqual(run, 'python3 scripts/qualification-gate.py lanes --validate "$LANES"')
        script = str(ROOT / "scripts/qualification-gate.py")
        for selection, code in (("all", 0), ("soak,fuzz-campaign", 0), ("cluster-lifecycle,typo", 1),
                                ("", 1), (",", 1)):
            with self.subTest(selection=selection):
                result = subprocess.run([sys.executable, script, "lanes", "--validate", selection],
                                        capture_output=True, text=True)
                self.assertEqual(result.returncode, code, result.stdout + result.stderr)

    def test_nightly_dispatch_satisfies_ci_ok_with_everything_deselected(self):
        with tempfile.TemporaryDirectory() as tmp:
            outputs = selected_outputs(())
            needs = results(outputs)
            result = subprocess.run(
                gate_command(Path(tmp) / "absent.json", event_name="workflow_dispatch", base_sha="",
                             head_sha="", candidate_parents="", lane_reports={}),
                env={**os.environ, "NEEDS_JSON": json.dumps(needs)}, capture_output=True, text=True,
            )
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_nightly_core_deploys_the_instrumented_server_and_requires_every_b3_row(self):
        steps = LANE_JOBS["core-robustness-nightly"]["steps"]
        build = next(step for step in steps if "robustness-instrumented" in step.get("run", ""))
        run = next(step for step in steps if "core-robustness nightly-core" in step.get("run", ""))
        self.assertLess(steps.index(build), steps.index(run))
        self.assertIn("CAESIUM_ROBUSTNESS_INSTRUMENTED_IMAGE=\"caesiumcloud/caesium:${{ github.sha }}-testfault\"",
                      run["run"])
        self.assertIn("--target instrumented-server", recipe_body("robustness-instrumented"))
        rows = {row["id"] for row in MANIFEST["scenarios"] if "nightly-core" in row["gates"]}
        b3 = {row["id"] for row in MANIFEST["scenarios"] if row["owner_item"] == "B3"}
        # The quorum-loss rows stay unproven, so ungated, until TestCore records
        # an identity for a timed-out minority mutation (PR #593 review).
        unreconciled = {"b3-quorum-loss-uncertain-write", "b3-split-heal-2-1"}
        self.assertEqual(rows, b3 - unreconciled)
        for row in MANIFEST["scenarios"]:
            if row["id"] in unreconciled:
                self.assertEqual((row["status"], row["gates"]), ("unproven", []), row["id"])
        self.assertIn("b3-durable-event-before-delivery-crash", rows)
        # The instrumented case is never part of a release-image gate.
        durable = next(row for row in MANIFEST["scenarios"] if row["id"] == "b3-durable-event-before-delivery-crash")
        self.assertEqual(durable["gates"], ["nightly-core"])

    def test_performance_runs_on_a_stable_fleet_identity(self):
        job = LANE_JOBS["performance-gate"]
        self.assertEqual(job["env"]["CAESIUM_PERF_HOST_ID"], "github-hosted|ubuntu-26.04|x86_64")
        self.assertEqual(job["strategy"]["matrix"]["run"], "${{ fromJSON(inputs.performance-runs || '[1]') }}")
        body = recipe_body("performance-gate")
        for setting in ("CAESIUM_PERF_WORKLOADS=\"${CAESIUM_PERF_WORKLOADS:-closed-baseline}\"",
                        "CAESIUM_PERF_REPEATS=\"${CAESIUM_PERF_REPEATS:-10}\"",
                        "CAESIUM_PERF_BROWSER=\"${CAESIUM_PERF_BROWSER:-1}\"",
                        "CAESIUM_PERF_BUNDLE=\"${CAESIUM_PERF_BUNDLE:-1}\""):
            self.assertIn(setting, body)
        # Advisory mode still fails when no gate record or evidence exists.
        self.assertIn("no gate record or evidence was produced", body)
        # The schedule never enforces an uncalibrated gate.
        self.assertEqual(NIGHTLY_JOBS["qualification"]["with"]["performance-mode"], "advisory")

    def test_fuzz_campaign_is_the_pr_recipe_at_the_long_budget(self):
        job = LANE_JOBS["fuzz-campaign"]
        run = next(step for step in job["steps"] if "generated-fuzz" in step.get("run", ""))
        self.assertEqual(run["env"]["CAESIUM_FUZZ_SECONDS"], "${{ inputs.fuzz-seconds }}")
        self.assertIn("just tag=${{ github.sha }} generated-fuzz", run["run"])
        self.assertIn('fuzz_seconds := env("CAESIUM_FUZZ_SECONDS", "10s")', (ROOT / "justfile").read_text())
        inputs = LANES_WORKFLOW[True]["workflow_call"]["inputs"]
        self.assertEqual(inputs["fuzz-seconds"]["default"], "5m")
        self.assertEqual(NIGHTLY_WORKFLOW[True]["workflow_dispatch"]["inputs"]["fuzz-seconds"]["default"], "5m")
        # The PR lane's Go cache is restored, never overwritten by the campaign.
        cache = [step for step in job["steps"] if "actions/cache" in step.get("uses", "")]
        self.assertEqual([step["uses"] for step in cache], ["actions/cache/restore@v4"])
        pr_cache = next(step for step in JOBS["generated-fuzz"]["steps"] if step.get("uses") == "actions/cache@v4")
        self.assertEqual(cache[0]["with"]["key"], pr_cache["with"]["key"])
        # The explored corpus is retained.
        artifacts = next(step for step in job["steps"]
                         if step.get("with", {}).get("name") == "fuzz-campaign-artifacts")
        self.assertEqual(artifacts["with"]["path"], ".tmp/lane-evidence/generated-fuzz/")
        self.assertIn('mkdir -p "$ARTIFACT_DIR/corpus/${target}-cache"', (ROOT / "scripts/fuzz-tests.sh").read_text())

    def test_soak_is_scheduled_advisory_and_known_failing(self):
        job = LANE_JOBS["soak"]
        run = next(step for step in job["steps"] if "just soak-tests" in step.get("run", ""))
        self.assertEqual(run["run"], "just soak-tests ${{ inputs.soak-profile || 'short' }} advisory")
        self.assertEqual(run["env"], {"CAESIUM_SOAK_SEED": "${{ inputs.soak-seed }}",
                                      "CAESIUM_SOAK_DURATION": "${{ inputs.soak-duration }}"})
        lane = QUAL_LANES["soak"]
        self.assertEqual((lane.role, lane.gate), (QUALIFICATION["ADVISORY"], None))
        for finding in ("#598", "#599", "#600", "#601", "#602", "#603"):
            self.assertIn(finding, lane.title)
        self.assertIn("#598-#603", UNPROMOTED_LANES["soak"])
        # Never publish-blocking: not a release lane, and not in the call CI's
        # release chain makes.
        self.assertNotIn("soak", QUALIFICATION["RELEASE_LANES"])
        self.assertNotIn("soak", QUALIFICATION["RELEASE_LANE_SELECTION"].split(","))
        # Credentials never reach an upload.
        artifacts = next(step for step in job["steps"] if step.get("with", {}).get("name") == "soak-artifacts")
        self.assertIn("!.tmp/lane-evidence/soak/kubeconfig*", artifacts["with"]["path"])
        report = next(step for step in job["steps"] if step.get("with", {}).get("name") == "soak-evidence")
        self.assertIn(".tmp/lane-evidence/soak/soak.json", report["with"]["path"])
        self.assertIn(".tmp/lane-evidence/soak/fault-schedule.jsonl", report["with"]["path"])

    def test_soak_recipe_records_a_verdict_and_fails_without_one(self):
        text = (ROOT / "justfile").read_text()
        self.assertRegex(text, r'(?m)^soak-tests profile="short" mode="advisory": lane-candidate-check$')
        body = recipe_body("soak-tests")
        self.assertIn('CAESIUM_SOAK_ARTIFACTS="$lane" CAESIUM_SOAK_PROFILE="{{ profile }}"', body)
        self.assertIn("bash scripts/soak-tests.sh || status=$?", body)
        self.assertIn('rm -f "$lane"/kubeconfig* "$lane/internal-token.txt"', body)
        self.assertIn('result="$(python3 scripts/soak-report.py result --artifacts "$lane")"', body)
        # pass exits 0; fail/blocked exit 0 only in advisory mode; anything
        # else (no record, `incomplete`) is a harness error in both modes.
        cases = re.search(r'case "\$result" in(.*?)\n    esac', body, re.S).group(1)
        self.assertRegex(cases, r"pass\)\s+exit 0")
        self.assertRegex(cases, r'fail\|blocked\)\s+if \[ "\{\{ mode \}\}" = "advisory" \]; then')
        self.assertRegex(cases, r"\*\)\s+echo \"soak-tests: no verdict was recorded")

    def test_every_workflow_job_is_classified(self):
        preexisting_optional = {
            "helm-integration-test", "podman-integration-test", "integration-extra",
            "integration-arm64", "helm-pod-replacement-test", "reagents-arm64",
        }
        structural = {"ci-ok", "publish", "build-and-integration-test", "build-and-integration-test-agent-auth",
                      "qualification", "release-qualification"}
        for job in JOBS:
            with self.subTest(job=job):
                self.assertTrue(
                    job in JOBS["ci-ok"]["needs"] or job in UNPROMOTED_LANES
                    or job in preexisting_optional or job in structural,
                    f"{job} is neither merge-blocking nor labelled unpromoted",
                )
        self.assertEqual(set(UNPROMOTED_LANES), set(QUAL_LANES))
        for job in UNPROMOTED_LANES:
            self.assertIn(job, LANE_JOBS)


def write_release_inputs(root, sha=CANDIDATE_SHA):
    """Honest release-gate inputs: every required report and both CLI markers."""
    evidence = Path(root) / "evidence"
    for job, (artifact, gate) in QUALIFICATION["RELEASE_REQUIRED_EVIDENCE"].items():
        path = evidence / artifact / "evidence.json"
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(evidence_report(sha=sha, gate=gate)))
    cli = Path(root) / "cli"
    cli.mkdir(parents=True, exist_ok=True)
    for arch, machine in QUALIFICATION["CLI_ARCHES"].items():
        binary = cli / f"caesium-linux-{arch}"
        binary.write_bytes(f"static caesium {arch}".encode())
        digest = hashlib.sha256(binary.read_bytes()).hexdigest()
        (cli / f"caesium-linux-{arch}.smoke-ok").write_text(
            f"binary=caesium-linux-{arch}\nsha256={digest}\nrunner_arch={machine}\nrunner_os=Linux\n")
    return evidence, cli


def release_needs(**overrides):
    needs = {job: {"result": "success"} for job in QUALIFICATION["RELEASE_REQUIRED_JOBS"]}
    for job, result in overrides.items():
        if result is None:
            needs.pop(job, None)
        else:
            needs[job] = {"result": result}
    return needs


def lane_results(**overrides):
    out = {"select": {"result": "success"}}
    for job in QUAL_LANES:
        out[job] = {"result": "success" if job in QUALIFICATION["RELEASE_LANES"] else "skipped"}
    for job, result in overrides.items():
        if result is None:
            out.pop(job, None)
        else:
            out[job] = {"result": result}
    return out


class ReleaseGateTests(unittest.TestCase):
    """G4: `scripts/qualification-gate.py release`, the gate `publish` needs."""

    def run_gate(self, root, sha=CANDIDATE_SHA, ref="refs/tags/v0.2.0", dry_run="false",
                 needs=None, results_json=None, evidence=None, cli=None):
        evidence = evidence or Path(root) / "evidence"
        cli = cli or Path(root) / "cli"
        env = {**os.environ,
               "NEEDS_JSON": json.dumps(release_needs() if needs is None else needs),
               "LANE_RESULTS_JSON": json.dumps(lane_results()) if results_json is None else results_json,
               "GITHUB_STEP_SUMMARY": str(Path(root) / "summary.md")}
        return subprocess.run(
            [sys.executable, str(ROOT / "scripts/qualification-gate.py"), "release",
             "--candidate-sha", sha, "--ref", ref, "--dry-run", dry_run,
             "--evidence-dir", str(evidence), "--cli-dir", str(cli),
             "--manifest", str(MANIFEST_PATH)],
            env=env, capture_output=True, text=True,
        )

    def test_required_set_is_explicit_and_excludes_advisory_lanes(self):
        q = QUALIFICATION
        self.assertEqual(q["RELEASE_LANES"], ("lifecycle-cluster", "console-recovery", "core-robustness-nightly"))
        self.assertEqual(q["RELEASE_LANE_SELECTION"], "cluster-lifecycle,console-recovery,core-robustness")
        self.assertEqual(q["RELEASE_REQUIRED_JOBS"], ("changes", "ci-ok", "images", "images-arm64", "qualification"))
        self.assertEqual({job: gate for job, (_, gate) in q["RELEASE_REQUIRED_EVIDENCE"].items()}, {
            "lifecycle-standalone": "lifecycle",
            "lifecycle-cluster": "nightly-cluster-lifecycle",
            "console-recovery": "nightly-console",
            "core-robustness-nightly": "nightly-core",
        })
        # F4 is also inside ci-ok's promoted set; the release gate re-checks it.
        self.assertEqual(EVIDENCE_LANES["lifecycle-standalone"], "lifecycle")
        self.assertEqual(set(q["NOT_REQUIRED_TO_PUBLISH"]), {"performance-gate", "soak", "fuzz-campaign"})
        for gate in ("lifecycle", "nightly-cluster-lifecycle", "nightly-console", "nightly-core"):
            self.assertTrue(gated_scenarios(gate=gate), gate)

    def test_honest_release_inputs_allow_publication(self):
        with tempfile.TemporaryDirectory() as tmp:
            write_release_inputs(tmp)
            result = self.run_gate(tmp)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertIn("publication allowed", result.stdout)
            self.assertIn("not required to publish: soak", result.stdout)
            self.assertIn("Release qualification: publication allowed", (Path(tmp) / "summary.md").read_text())
            # A dry run on a branch is allowed through the same checks.
            result = self.run_gate(tmp, ref="refs/heads/master", dry_run="true")
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_only_a_tag_or_a_dry_run_can_publish(self):
        with tempfile.TemporaryDirectory() as tmp:
            write_release_inputs(tmp)
            for ref, dry_run in (("refs/heads/master", "false"), ("refs/tags/x1", "false"),
                                 ("refs/tags/v0.2.0", "maybe")):
                with self.subTest(ref=ref, dry_run=dry_run):
                    result = self.run_gate(tmp, ref=ref, dry_run=dry_run)
                    self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
                    self.assertIn("publication blocked", result.stderr)
            result = self.run_gate(tmp, sha="v0.2.0")
            self.assertEqual(result.returncode, 1)
            self.assertIn("is not a full commit SHA", result.stderr)

    def test_failed_cancelled_skipped_or_missing_jobs_block(self):
        with tempfile.TemporaryDirectory() as tmp:
            write_release_inputs(tmp)
            for job in QUALIFICATION["RELEASE_REQUIRED_JOBS"]:
                for result_value in ("failure", "cancelled", "skipped", None):
                    with self.subTest(job=job, result=result_value):
                        result = self.run_gate(tmp, needs=release_needs(**{job: result_value}))
                        self.assertEqual(result.returncode, 1, result.stdout)
                        self.assertIn(f"{job}={result_value or 'missing'}", result.stderr)
            result = self.run_gate(tmp, needs="not-an-object")
            self.assertEqual(result.returncode, 1)

    def test_each_release_lane_result_is_required(self):
        with tempfile.TemporaryDirectory() as tmp:
            write_release_inputs(tmp)
            for job in QUALIFICATION["RELEASE_LANES"]:
                for result_value in ("failure", "skipped", None):
                    with self.subTest(job=job, result=result_value):
                        result = self.run_gate(tmp, results_json=json.dumps(lane_results(**{job: result_value})))
                        self.assertEqual(result.returncode, 1, result.stdout)
                        self.assertIn(f"release lane {job}={result_value or 'missing'}", result.stderr)
            for raw in ("", "not json", "[]"):
                with self.subTest(results=raw):
                    self.assertEqual(self.run_gate(tmp, results_json=raw).returncode, 1)

    def test_advisory_lanes_never_block(self):
        with tempfile.TemporaryDirectory() as tmp:
            write_release_inputs(tmp)
            results_json = json.dumps(lane_results(**{"performance-gate": "failure", "soak": "failure",
                                                      "fuzz-campaign": "cancelled"}))
            result = self.run_gate(tmp, results_json=results_json)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_absent_foreign_or_failed_evidence_blocks(self):
        for job, (artifact, gate) in QUALIFICATION["RELEASE_REQUIRED_EVIDENCE"].items():
            with self.subTest(job=job, case="absent"), tempfile.TemporaryDirectory() as tmp:
                evidence, _ = write_release_inputs(tmp)
                (evidence / artifact / "evidence.json").unlink()
                result = self.run_gate(tmp)
                self.assertEqual(result.returncode, 1)
                self.assertIn(f"{job} evidence: ", result.stderr)
                self.assertIn("is absent", result.stderr)
            with self.subTest(job=job, case="foreign"), tempfile.TemporaryDirectory() as tmp:
                evidence, _ = write_release_inputs(tmp)
                (evidence / artifact / "evidence.json").write_text(
                    json.dumps(evidence_report(sha="f" * 40, gate=gate)))
                result = self.run_gate(tmp)
                self.assertEqual(result.returncode, 1)
                self.assertIn("is not the tested candidate", result.stderr)
            with self.subTest(job=job, case="failed"), tempfile.TemporaryDirectory() as tmp:
                evidence, _ = write_release_inputs(tmp)
                report = evidence_report(gate=gate)
                report["scenarios"][0]["status"] = "fail"
                (evidence / artifact / "evidence.json").write_text(json.dumps(report))
                result = self.run_gate(tmp)
                self.assertEqual(result.returncode, 1)
                self.assertIn("want 'pass'", result.stderr)

    def test_native_cli_gate_blocks_on_any_marker_defect(self):
        def tamper(cli, arch, how):
            binary = cli / f"caesium-linux-{arch}"
            marker = cli / f"caesium-linux-{arch}.smoke-ok"
            if how == "no-binary":
                binary.unlink()
            elif how == "no-marker":
                marker.unlink()
            elif how == "rebuilt":
                binary.write_bytes(b"a different binary")
            elif how == "emulated":
                marker.write_text(marker.read_text().replace("runner_arch=", "runner_arch=emulated-"))
            elif how == "renamed":
                marker.write_text(marker.read_text().replace(f"binary=caesium-linux-{arch}", "binary=other"))
        for arch in QUALIFICATION["CLI_ARCHES"]:
            for how, message in (("no-binary", "is absent"), ("no-marker", "never smoke-tested"),
                                 ("rebuilt", "!= binary sha256"), ("emulated", "not natively"),
                                 ("renamed", "marker names binary")):
                with self.subTest(arch=arch, how=how), tempfile.TemporaryDirectory() as tmp:
                    _, cli = write_release_inputs(tmp)
                    tamper(cli, arch, how)
                    result = self.run_gate(tmp)
                    self.assertEqual(result.returncode, 1, result.stdout)
                    self.assertIn(f"native CLI {arch}", result.stderr)
                    self.assertIn(message, result.stderr)

    @unittest.skipUnless(shutil.which("jq"), "jq is not installed")
    def test_simulated_failure_step_blocks_the_unchanged_gate(self):
        steps = JOBS["release-qualification"]["steps"]
        simulate = next(step for step in steps if step.get("name", "").startswith("Simulate a failed"))
        with tempfile.TemporaryDirectory() as tmp:
            write_release_inputs(tmp)
            workdir = Path(tmp) / "work"
            target = workdir / ".tmp/release/evidence"
            shutil.copytree(Path(tmp) / "evidence", target)
            script = Path(tmp) / "simulate.sh"
            script.write_text(simulate["run"])
            done = subprocess.run(["bash", "--noprofile", "--norc", str(script)], cwd=workdir,
                                  capture_output=True, text=True)
            self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
            self.assertIn("SIMULATION: f2-cluster-lifecycle rewritten", done.stdout)
            result = self.run_gate(tmp, ref="refs/heads/master", dry_run="true", evidence=target)
            self.assertEqual(result.returncode, 1, result.stdout)
            self.assertIn("scenario 'f2-cluster-lifecycle' status 'fail', want 'pass'", result.stderr)


class ReleaseQualificationWiringTests(unittest.TestCase):
    """G4: the publish chain requires same-SHA release qualification."""

    def test_publish_needs_the_gate_and_the_gate_needs_the_qualification(self):
        self.assertIn("release-qualification", JOBS["publish"]["needs"])
        self.assertIn("ci-ok", JOBS["publish"]["needs"])
        gate = JOBS["release-qualification"]
        self.assertEqual(gate["needs"], list(QUALIFICATION["RELEASE_REQUIRED_JOBS"]))
        self.assertEqual(gate["if"],
                         "always() && (startsWith(github.ref, 'refs/tags/v') || inputs.publish-dry-run)")
        # `publish` carries no status function, so GitHub's implicit success()
        # skips it whenever the gate (or any other need) did not succeed.
        self.assertNotRegex(JOBS["publish"]["if"], r"always\(\)|failure\(\)|cancelled\(\)")

    def test_release_chain_runs_the_required_lanes_on_this_commit(self):
        job = JOBS["qualification"]
        self.assertEqual(job["uses"], "./.github/workflows/qualification-lanes.yml")
        self.assertEqual(job["needs"], ["changes"])
        self.assertEqual(job["if"],
                         "inputs.nightly != '' || startsWith(github.ref, 'refs/tags/v') || inputs.publish-dry-run")
        self.assertEqual(job["with"]["lanes"],
                         "${{ inputs.nightly != '' && inputs.nightly || '"
                         + QUALIFICATION["RELEASE_LANE_SELECTION"] + "' }}")
        self.assertEqual(job["permissions"], {"contents": "read"})
        # Never a merge dependency.
        self.assertNotIn("qualification", JOBS["ci-ok"]["needs"])
        self.assertNotIn("release-qualification", JOBS["ci-ok"]["needs"])
        for name in JOBS["ci-ok"]["needs"]:
            self.assertNotIn("qualification", JOBS[name].get("needs") or [], name)

    def test_gate_reads_the_downloaded_reports_and_cli(self):
        steps = JOBS["release-qualification"]["steps"]
        gate = next(step for step in steps if step.get("name") == "Evaluate release qualification")
        self.assertEqual(gate["env"]["NEEDS_JSON"], "${{ toJSON(needs) }}")
        self.assertEqual(gate["env"]["LANE_RESULTS_JSON"], "${{ needs.qualification.outputs.results }}")
        self.assertEqual(gate["env"]["DRY_RUN"], "${{ inputs.publish-dry-run && 'true' || 'false' }}")
        argv = shlex.split(gate["run"].replace("\\\n", " "))
        self.assertEqual(argv[:3], ["python3", "scripts/qualification-gate.py", "release"])
        flags = dict(zip(argv[3::2], argv[4::2]))
        self.assertEqual(flags["--candidate-sha"], "$GITHUB_SHA")
        self.assertEqual(flags["--ref"], "$GITHUB_REF")
        downloads = {step["with"]["pattern"]: step for step in steps
                     if step.get("uses", "").startswith("actions/download-artifact")}
        self.assertEqual(downloads["*-evidence"]["with"]["path"], flags["--evidence-dir"])
        self.assertEqual(downloads["release-cli-*"]["with"]["path"], flags["--cli-dir"])
        self.assertTrue(downloads["release-cli-*"]["with"]["merge-multiple"])
        for step in downloads.values():
            self.assertLess(steps.index(step), steps.index(gate))
        # Every required report and CLI artifact has a producer in this run.
        uploads = {}
        for job in list(JOBS.values()) + list(LANE_JOBS.values()):
            for step in job.get("steps", []):
                if step.get("uses") == "actions/upload-artifact@v7":
                    uploads[step["with"]["name"]] = step["with"]["path"]
        for artifact, _ in QUALIFICATION["RELEASE_REQUIRED_EVIDENCE"].values():
            self.assertTrue(fnmatch.fnmatchcase(artifact, "*-evidence"), artifact)
            self.assertIn(artifact, uploads)
        for arch in QUALIFICATION["CLI_ARCHES"]:
            self.assertIn(f"/tmp/caesium-linux-{arch}.smoke-ok", uploads[f"release-cli-{arch}"])

    def test_simulation_only_exists_in_a_dry_run_and_precedes_the_gate(self):
        steps = JOBS["release-qualification"]["steps"]
        simulate = next(step for step in steps if step.get("name", "").startswith("Simulate a failed"))
        gate = next(step for step in steps if step.get("name") == "Evaluate release qualification")
        self.assertEqual(simulate["if"], "inputs.publish-dry-run && inputs.simulate-failed-qualification")
        self.assertLess(steps.index(simulate), steps.index(gate))
        self.assertIn("lifecycle-cluster-evidence/evidence.json", simulate["run"])
        row = next(row for row in MANIFEST["scenarios"] if row["id"] == "f2-cluster-lifecycle")
        self.assertEqual(row["gates"], [QUAL_LANES["lifecycle-cluster"].gate])
        # A tag push carries no inputs; the dry-run inputs exist only on dispatch.
        self.assertNotIn("inputs", TRIGGERS["push"] or {})

    def test_dispatch_inputs_fail_closed_on_conflicting_requests(self):
        code, stderr, _ = run_changes_set_step("workflow_dispatch", "", SIMULATE_FAILED_QUALIFICATION="true",
                                               PUBLISH_DRY_RUN="false")
        self.assertNotEqual(code, 0)
        self.assertIn("simulate-failed-qualification needs publish-dry-run", stderr)
        code, stderr, _ = run_changes_set_step("workflow_dispatch", "all", PUBLISH_DRY_RUN="true")
        self.assertNotEqual(code, 0)
        self.assertIn("leave nightly empty", stderr)
        # A dry run is a push-equivalent full run: nothing is path-deselected.
        code, _, parsed = run_changes_set_step("workflow_dispatch", "", PUBLISH_DRY_RUN="true",
                                               SIMULATE_FAILED_QUALIFICATION="true")
        self.assertEqual(code, 0)
        self.assertEqual(parsed, {k: "true" for k in ("go", "ui", "helm", "reagents", "ci", "images")})
        # Pull request / merge group / push events never set either input.
        code, _, parsed = run_changes_set_step("push", "", PUBLISH_DRY_RUN="", SIMULATE_FAILED_QUALIFICATION="")
        self.assertEqual(code, 0)

    def test_native_cli_smoke_and_checksum_gates_are_retained(self):
        for job, arch in (("images", "amd64"), ("images-arm64", "arm64")):
            runs = [step.get("run", "") for step in JOBS[job]["steps"]]
            self.assertIn(f"scripts/ci-cli-smoke.sh /tmp/caesium-linux-{arch} {arch}", runs)
        verify = next(step for step in JOBS["publish"]["steps"]
                      if step.get("name") == "Verify CLI smoke markers and checksums")
        # Runs on a tag AND on a dry run (no `if:`), before anything is pushed.
        self.assertNotIn("if", verify)
        for fragment in ('marker="${bin}.smoke-ok"', "sha256sum \"$bin\"", "SHA256SUMS"):
            self.assertIn(fragment, verify["run"])
        steps = JOBS["publish"]["steps"]
        login = next(step for step in steps if step.get("name") == "Login to Docker Hub")
        self.assertLess(steps.index(verify), steps.index(login))
        smoke = (ROOT / "scripts/ci-cli-smoke.sh").read_text()
        self.assertIn('echo "runner_arch=$(uname -m)"', smoke)
        self.assertIn('echo "sha256=${sha}"', smoke)

    def test_actionlint_covers_every_workflow(self):
        commands = [line.strip() for step in JOBS["ci-config"]["steps"]
                    for line in step.get("run", "").splitlines() if line.strip().startswith("actionlint ")]
        self.assertEqual(len(commands), 1)
        linted = set(commands[0].split()[1:])
        self.assertEqual(linted, {".github/workflows/ci.yml", ".github/workflows/qualification-lanes.yml",
                                  ".github/workflows/testing-qualification.yml"})
        # Every called workflow is linted along with its callers.
        called = {job["uses"][2:] for job in list(JOBS.values()) + list(NIGHTLY_JOBS.values()) if "uses" in job}
        self.assertTrue(called <= linted, called - linted)


class TestingQualificationWorkflowTests(unittest.TestCase):
    """G4: the nightly schedule -- hosted only, never on PR/merge-queue events."""

    def test_triggers_are_schedule_and_dispatch_only(self):
        triggers = NIGHTLY_WORKFLOW[True]
        self.assertEqual(set(triggers), {"schedule", "workflow_dispatch"})
        for event in ("pull_request", "pull_request_target", "merge_group", "push", "workflow_call"):
            self.assertNotIn(event, triggers)
        crons = [entry["cron"] for entry in triggers["schedule"]]
        self.assertEqual(len(crons), 1)
        self.assertEqual(len(crons[0].split()), 5)
        # The lanes workflow is only ever called.
        self.assertEqual(set(LANES_WORKFLOW[True]), {"workflow_call"})

    def test_concurrency_never_shares_or_cancels_with_ci(self):
        nightly = NIGHTLY_WORKFLOW["concurrency"]
        self.assertEqual(nightly["group"], "testing-qualification-${{ github.ref }}")
        self.assertFalse(nightly["cancel-in-progress"])
        # CI's groups all start with its own workflow name ("CI-...").
        self.assertTrue(WORKFLOW["concurrency"]["group"].startswith("${{ github.workflow }}-"))
        self.assertEqual(WORKFLOW["name"], "CI")
        # A called workflow's own group would be evaluated in its caller's
        # context and could deadlock against it.
        self.assertNotIn("concurrency", LANES_WORKFLOW)

    def test_schedules_every_lane_with_nightly_defaults(self):
        call = NIGHTLY_JOBS["qualification"]
        self.assertEqual(call["uses"], "./.github/workflows/qualification-lanes.yml")
        self.assertEqual(call["with"]["lanes"], "${{ inputs.lanes || 'all' }}")
        self.assertEqual(QUALIFICATION["selected_jobs"]("all"), list(QUAL_LANES))
        dispatch = NIGHTLY_WORKFLOW[True]["workflow_dispatch"]["inputs"]
        reusable = LANES_WORKFLOW[True]["workflow_call"]["inputs"]
        self.assertEqual(dispatch["lanes"]["default"], "all")
        for name in ("fuzz-seconds", "soak-profile", "soak-seed", "soak-duration", "performance-runs"):
            with self.subTest(input=name):
                self.assertEqual(dispatch[name]["default"], reusable[name]["default"])
                self.assertIn(f"inputs.{name}", call["with"][name])
        self.assertEqual(dispatch["soak-profile"]["options"], ["short", "nightly"])
        # Every value the call passes is a declared input of the lanes workflow.
        self.assertTrue(set(call["with"]) <= set(reusable))
        self.assertTrue(set(JOBS["qualification"]["with"]) <= set(reusable))

    def test_permissions_are_least_privilege(self):
        self.assertEqual(NIGHTLY_WORKFLOW["permissions"], {"contents": "read"})
        self.assertEqual(LANES_WORKFLOW["permissions"], {"contents": "read"})
        self.assertEqual(NIGHTLY_JOBS["qualification"]["permissions"], {"contents": "read"})
        self.assertEqual(NIGHTLY_JOBS["summary"]["permissions"], {"contents": "read", "issues": "write"})
        for job in LANE_JOBS.values():
            self.assertNotIn("permissions", job)

    def test_one_summary_and_one_issue_comment_per_failing_scheduled_night(self):
        summary = NIGHTLY_JOBS["summary"]
        self.assertEqual(summary["needs"], ["qualification"])
        self.assertEqual(summary["if"], "always()")
        steps = summary["steps"]
        fold = next(step for step in steps if step.get("id") == "fold")
        self.assertEqual(fold["env"]["LANE_RESULTS_JSON"], "${{ needs.qualification.outputs.results }}")
        self.assertIn("scripts/qualification-gate.py summary", fold["run"])
        issue = next(step for step in steps if "gh issue" in step.get("run", ""))
        self.assertEqual(issue["if"], "github.event_name == 'schedule' && steps.fold.outputs.failing != ''")
        self.assertIn('title="Nightly qualification failures"', issue["run"])
        self.assertIn("gh issue comment", issue["run"])
        self.assertIn("gh issue create", issue["run"])
        # event_name appears in exactly that step: dispatches never touch issues.
        for job_name, job in NIGHTLY_JOBS.items():
            self.assertNotIn("event_name", job.get("if") or "", job_name)
            for step in job.get("steps", []) or []:
                if "event_name" in (step.get("if") or ""):
                    self.assertIs(step, issue)
        fail = steps[-1]
        self.assertEqual(fail["if"], "steps.fold.outputs.failing != ''")
        self.assertLess(steps.index(issue), steps.index(fail))
        download = next(step for step in steps if step.get("uses", "").startswith("actions/download-artifact"))
        self.assertEqual(download["with"]["pattern"], "*-evidence*")

    def test_results_job_reports_every_lane(self):
        results_job = LANE_JOBS["results"]
        self.assertEqual(results_job["if"], "always()")
        self.assertEqual(set(results_job["needs"]), set(LANE_JOBS) - {"results"})
        self.assertEqual(LANES_WORKFLOW[True]["workflow_call"]["outputs"]["results"]["value"],
                         "${{ jobs.results.outputs.needs }}")
        for job in QUAL_LANES:
            self.assertEqual(LANE_JOBS[job]["needs"], ["select"], job)


class QualificationSummaryTests(unittest.TestCase):
    """G4: the nightly fold names exactly the lanes that fail a night."""

    def args(self, evidence, lanes="all", sha=CANDIDATE_SHA):
        return argparse.Namespace(
            lanes=lanes, candidate_sha=sha, evidence_dir=str(evidence), manifest=str(MANIFEST_PATH),
            run_url="https://example.invalid/run/1", qualification_result="success", fuzz_seconds="5m",
            soak_profile="short", triage_owner="@owner", codeowners="", markdown_out="",
        )

    def honest_night(self, root, sha=CANDIDATE_SHA):
        evidence = Path(root)
        for job, lane in QUAL_LANES.items():
            if lane.gate:
                path = evidence / lane.artifact / "evidence.json"
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(json.dumps(evidence_report(sha=sha, gate=lane.gate)))
        perf = evidence / "performance-gate-evidence-1" / "evidence.json"
        perf.parent.mkdir(parents=True)
        perf.write_text(json.dumps({"scenarios": [{"id": "e4-performance-gate", "status": "fail",
                                                   "performance": {"overall": "inconclusive_unresolved"}}]}))
        soak = evidence / "soak-evidence" / "soak.json"
        soak.parent.mkdir(parents=True)
        soak.write_text(json.dumps({"candidate_sha": sha, "result": "fail", "seed": 314159,
                                    "seed_source": "generated", "profile": "short",
                                    "failed_gates": ["post_drain_containers"]}))
        results = {"select": {"result": "success"}, **{job: {"result": "success"} for job in QUAL_LANES}}
        return evidence, results

    def fold(self, args, results):
        return QUALIFICATION["summarize"](args, json.dumps(results) if isinstance(results, dict) else results)

    def test_known_failing_advisory_verdicts_keep_a_night_green(self):
        with tempfile.TemporaryDirectory() as tmp:
            evidence, results = self.honest_night(tmp)
            markdown, failing = self.fold(self.args(evidence), results)
            self.assertEqual(failing, [])
            self.assertIn("Nightly qualification: green", markdown)
            self.assertIn("advisory: fail", markdown)
            self.assertIn("seed 314159", markdown)
            self.assertIn("CAESIUM_SOAK_SEED=314159 just soak-tests short advisory", markdown)
            self.assertIn("inconclusive_unresolved", markdown)
            self.assertIn("never proof for an individual PR", markdown)
            self.assertIn("Triage owner: @owner", markdown)
            self.assertIn("#598", markdown)

    def test_a_failed_job_or_bad_evidence_fails_the_night(self):
        with tempfile.TemporaryDirectory() as tmp:
            evidence, results = self.honest_night(tmp)
            for job in QUAL_LANES:
                with self.subTest(job=job):
                    _, failing = self.fold(self.args(evidence), {**results, job: {"result": "failure"}})
                    self.assertEqual(failing, [job])
            (evidence / "console-recovery-evidence" / "evidence.json").write_text(
                json.dumps(evidence_report(sha="f" * 40, gate="nightly-console")))
            _, failing = self.fold(self.args(evidence), results)
            self.assertEqual(failing, ["console-recovery"])

    def test_advisory_lanes_without_a_verdict_fail_the_night(self):
        with tempfile.TemporaryDirectory() as tmp:
            evidence, results = self.honest_night(tmp)
            shutil.rmtree(evidence / "performance-gate-evidence-1")
            (evidence / "soak-evidence" / "soak.json").write_text(
                json.dumps({"candidate_sha": CANDIDATE_SHA, "result": "incomplete"}))
            _, failing = self.fold(self.args(evidence), results)
            self.assertEqual(failing, ["performance-gate", "soak"])

    def test_missing_results_or_a_refused_selection_fail_the_night(self):
        with tempfile.TemporaryDirectory() as tmp:
            evidence, results = self.honest_night(tmp)
            _, failing = self.fold(self.args(evidence), "")
            self.assertIn("results", failing)
            _, failing = self.fold(self.args(evidence, lanes="typo"),
                                   {"select": {"result": "failure"}, **{j: {"result": "skipped"} for j in QUAL_LANES}})
            self.assertEqual(failing, ["select"])

    def test_unselected_lanes_are_reported_not_failed(self):
        with tempfile.TemporaryDirectory() as tmp:
            evidence, results = self.honest_night(tmp)
            results["soak"] = {"result": "skipped"}
            markdown, failing = self.fold(self.args(evidence, lanes="cluster-lifecycle,fuzz-campaign"), results)
            self.assertEqual(failing, [])
            self.assertIn("| `soak` | not selected |", markdown)

    def test_triage_owner_comes_from_codeowners(self):
        owners = QUALIFICATION["codeowners_default"](ROOT / ".github/CODEOWNERS")
        self.assertTrue(owners.startswith("@"), owners)


class IntegrationRunnerTests(unittest.TestCase):
    def test_binary_receives_package_cwd_filters_and_exit_status(self):
        with tempfile.TemporaryDirectory() as tmp:
            binary = Path(tmp) / "fake-test-binary"
            binary.write_text('#!/bin/sh\npwd\nprintf "%s\\n" "$@"\nexit 17\n')
            binary.chmod(0o755)
            result = subprocess.run(
                ["sh", str(ROOT / "scripts/integration-test.sh"), "-test.run", "Suite/(TestA|TestB)", "-test.timeout=20m"],
                cwd=tmp,
                env={**os.environ, "CAESIUM_INTEGRATION_TEST_BINARY": str(binary)},
                capture_output=True, text=True,
            )
            self.assertEqual(result.returncode, 17)
            self.assertEqual(result.stdout.splitlines(), [
                str(ROOT / "test"), "-test.count=1", "-test.timeout=30m", "-test.v",
                "-test.run", "Suite/(TestA|TestB)", "-test.timeout=20m",
            ])

    def test_missing_binary_fails_without_compiling(self):
        result = subprocess.run(
            ["sh", str(ROOT / "scripts/integration-test.sh")],
            env={**os.environ, "CAESIUM_INTEGRATION_TEST_BINARY": "/missing/precompiled-test"},
            capture_output=True, text=True,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Precompiled integration test binary is missing", result.stderr)


class ResourceHarnessTests(unittest.TestCase):
    def test_exact_fixture_is_built_before_each_product_artifact(self):
        for arch, job in (("amd64", "images"), ("arm64", "images-arm64")):
            steps = JOBS[job]["steps"]
            build = next(i for i, step in enumerate(steps) if "stress-image-test" in step.get("run", ""))
            save = next(i for i, step in enumerate(steps) if step.get("with", {}).get("name") == f"product-{arch}")
            self.assertLess(build, save)
            self.assertIn(f"tag=${{{{ env.IMAGE_TAG }}}}-{arch}", steps[build]["run"])
            self.assertIn(f"caesiumcloud/resource-stress:${{{{ env.IMAGE_TAG }}}}-{arch}", steps[save]["with"]["images"])

    def test_every_local_server_and_runner_receives_resource_configuration(self):
        for recipe in (
            "integration-test", "integration-test-distributed", "integration-test-owner-memory",
            "integration-test-agent", "integration-test-infra", "integration-test-podman",
            "ui-e2e", "ui-e2e-auth",
        ):
            with self.subTest(recipe=recipe):
                result = subprocess.run(
                    ["just", "--dry-run", "tag=harness-evidence", recipe], cwd=ROOT,
                    env={**os.environ, "CAESIUM_PODMAN": "false"}, capture_output=True, text=True,
                )
                self.assertEqual(result.returncode, 0, result.stderr)
                commands = result.stdout + result.stderr
                # These recipes call their server recipe in a shell command,
                # not a just dependency; dry-run does not recursively execute it.
                server = {
                    "integration-test": "integration-up",
                    "integration-test-distributed": "integration-up-distributed",
                    "integration-test-owner-memory": "integration-up-owner-memory",
                    "integration-test-agent": "integration-up-agent",
                    "integration-test-infra": "integration-up-infra",
                }.get(recipe)
                if server:
                    self.assertIn(f"just tag=harness-evidence {server}", commands)
                    startup = subprocess.run(
                        ["just", "--dry-run", "tag=harness-evidence", server], cwd=ROOT,
                        env={**os.environ, "CAESIUM_PODMAN": "false"}, capture_output=True, text=True,
                    )
                    self.assertEqual(startup.returncode, 0, startup.stderr)
                    commands += startup.stdout + startup.stderr
                for setting in ("RESOURCE_STATS_ENABLED=true", "RESOURCE_STATS_SAMPLE_INTERVAL=100ms", "RIGHT_SIZING_ENABLED=true"):
                    self.assertIn(f"-e CAESIUM_{setting}", commands)
                if recipe.startswith("integration-test"):
                    self.assertIn("-e CAESIUM_RESOURCE_STRESS_IMAGE=caesiumcloud/resource-stress:harness-evidence", commands)
                    self.assertIn("build/Dockerfile.stress", commands)
                if recipe in ("integration-test-distributed", "integration-test-owner-memory", "integration-test-agent", "integration-test-infra"):
                    self.assertIn("|TestResourceStats)", commands)

    def test_inline_servers_and_foreign_image_stores_receive_same_fixture(self):
        for job in ("ui-e2e", "ui-e2e-auth", "podman-integration-test"):
            commands = "\n".join(step.get("run", "") for step in JOBS[job]["steps"])
            for setting in ("RESOURCE_STATS_ENABLED=true", "RESOURCE_STATS_SAMPLE_INTERVAL=100ms", "RIGHT_SIZING_ENABLED=true"):
                self.assertIn(f"-e CAESIUM_{setting}", commands)
        exact = "caesiumcloud/resource-stress:${{ env.IMAGE_TAG }}-amd64"
        for job, transfer in (("podman-integration-test", f"docker save {exact} | podman load"), ("helm-integration-test", f"kind load docker-image {exact}")):
            commands = "\n".join(step.get("run", "") for step in JOBS[job]["steps"])
            self.assertIn(transfer, commands)
            self.assertIn(f"-e CAESIUM_RESOURCE_STRESS_IMAGE={exact}", commands)
        values = yaml.safe_load((ROOT / "helm/caesium/ci/test-values-k8s.yaml").read_text())
        helm_env = {item["name"]: item["value"] for item in values["config"]["extraEnv"]}
        self.assertEqual(helm_env["CAESIUM_RESOURCE_STATS_ENABLED"], "true")
        self.assertEqual(helm_env["CAESIUM_RESOURCE_STATS_SAMPLE_INTERVAL"], "100ms")
        self.assertEqual(helm_env["CAESIUM_RIGHT_SIZING_ENABLED"], "true")

    def test_podman_monitor_and_oom_evidence_are_verified(self):
        steps = JOBS["podman-integration-test"]["steps"]
        commands = "\n".join(step.get("run", "") for step in steps)
        self.assertIn("releases/download/v2.2.1/conmon.amd64", commands)
        self.assertIn("1d97294c14c43d477e0a0826e9cd0f2a2af373ddfafe6f10252e8a3c43f32be6", commands)
        self.assertIn("sha256sum --check --strict", commands)
        self.assertIn('log_driver = "k8s-file"', commands)
        smoke = "build/stress/smoke.sh podman caesiumcloud/resource-stress:${{ env.IMAGE_TAG }}-amd64"
        self.assertIn("bash " + smoke, commands)
        self.assertNotIn("bash -x " + smoke, commands)

    def test_missing_ci_fixture_fails_without_rebuilding(self):
        with tempfile.TemporaryDirectory() as tmp:
            command_log = Path(tmp) / "commands"
            fake_cli = Path(tmp) / "fake-container-cli"
            fake_cli.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >> "$RESOURCE_HARNESS_COMMAND_LOG"\nexit 7\n')
            fake_cli.chmod(0o755)
            result = subprocess.run(
                ["just", "tag=harness-evidence", "build-stress"], cwd=ROOT,
                env={**os.environ, "CAESIUM_SKIP_IMAGE_BUILD": "true", "CAESIUM_PODMAN": "false",
                     "CAESIUM_CONTAINER_CLI": str(fake_cli), "RESOURCE_HARNESS_COMMAND_LOG": str(command_log)},
                capture_output=True, text=True,
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(command_log.read_text().splitlines(), ["image inspect caesiumcloud/resource-stress:harness-evidence"])


if __name__ == "__main__":
    unittest.main()
