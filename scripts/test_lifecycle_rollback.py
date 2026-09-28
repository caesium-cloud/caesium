"""Fault injection for F2's isolated rollback case (W8-β) in lifecycle-tests.sh.

Runs the real ``lc_rollback_*`` block extracted from the controller under bash
with kubectl, ctr, helm and the in-cluster probe stubbed. No Docker, kind or
cluster. Two guards are exercised:

* an unobserved member (pod listing or HTTP probe missing) blocks the case
  instead of being recorded as an outcome;
* the frozen copy is bounded, and every paused container is resumed on a
  deadline or a signal before the case is recorded blocked.
"""

import json
import os
import signal
import subprocess
import tempfile
import textwrap
import time
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
CONTROLLER = ROOT / "scripts/lifecycle-tests.sh"
CIDS = [f"{n:064d}" for n in range(3)]


def rollback_block():
    source = CONTROLLER.read_text()
    start = source.index("  # BEGIN lc_rollback_*")
    end = source.index("  # END lc_rollback_*")
    return source[start:end]


KUBECTL_SHIM = """#!/bin/sh
# Stub kubectl for the copy workers: records its pid, then stalls or answers.
case " $* " in *" exec "*) ;; *) exit 0 ;; esac
for arg in "$@"; do
  case "$arg" in lifecycle-rb-src-*) member="${arg##*-}" ;; esac
done
echo $$ > "$ART/copy-pid-$member"
if [ "$RB_COPY_MODE" = stall ]; then exec sleep 300; fi
printf '%064d  ./info.yaml\\n' 0
"""

PRELUDE = r"""
set -euo pipefail
export PATH="$ART/bin:$PATH"
LC_ID=lifecycle-rbtest LC_ART="$ART" LC_KUBE="$ART/kubeconfig" LC_TASK=alpine:3.23
LC_RUNNER=runner:test LC_PREV=caesiumcloud/caesium:v0.1.0 LC_VALUES=/dev/null LC_CAND_ID=cand ROOT="$ART/root"
mkdir -p "$ART/cluster-logs"
lc_case() {
  printf 'case %s\n' "$2" >>"$ART/calls.log"
  python3 - "$ART/case.json" "$@" <<'PY'
import json,pathlib,sys
out,name,status,detail=sys.argv[1:5]
rec={'name':name,'status':status,'detail':detail}
if len(sys.argv)>5 and sys.argv[5]:rec['observations']=json.loads(pathlib.Path(sys.argv[5]).read_text())
pathlib.Path(out).write_text(json.dumps(rec,indent=2))
PY
}
lc_ns() {
  printf 'lc_ns %s\n' "$*" >>"$ART/calls.log"
  if [[ "$1 $2" == "get pod" ]]; then
    case "$5" in
      *nodeName*) printf 'node-%s' "${3##*-}" ;;
      *containerID*) printf 'containerd://%064d' "${3##*-}" ;;
    esac
    return 0
  fi
  case "$1" in get) printf '{"items":[]}\n' ;; exec) printf 'stub\n' ;; esac
  return 0
}
# ctr through docker exec: 20 <out> docker exec --privileged NODE ctr NS task VERB [CID]
lc_run_timed() {
  local out="$2" verb cid f
  shift 2
  verb="$8" cid="${9:-}"
  printf '%s %s\n' "$verb" "$cid" >>"$ART/ctr.log"
  case "$verb" in
    pause) echo PAUSED >"$ART/state-$cid" ;;
    resume)
      if [[ -n "${RB_SLOW_RESUME:-}" ]]; then command sleep "$RB_SLOW_RESUME"; fi
      echo RUNNING >"$ART/state-$cid" ;;
    ls)
      printf 'TASK PID STATUS\n' >>"$out"
      for f in "$ART"/state-*; do printf '%s 1 %s\n' "${f##*/state-}" "$(cat "$f")" >>"$out"; done ;;
  esac
}
"""

STUBS_AFTER_BLOCK = r"""
lc_rollback_helper() { return 0; }
lc_rollback_main_baseline() { printf '0\n' >"$LC_RB_DIR/main-baseline-exit.txt"; }
"""

OBSERVE_STUBS = r"""
helm() { return 0; }
# The observation window is bounded by SECONDS; advance it instead of sleeping.
sleep() { SECONDS=$((SECONDS + 30)); }
lc_rb() {
  printf 'lc_rb %s\n' "$*" >>"$ART/calls.log"
  case "$*" in
    "--request-timeout=20s get pods -o json")
      if [[ "$RB_FINAL_LISTING" == fail ]]; then
        echo 'Unable to connect to the server: net/http: request canceled (Client.Timeout exceeded)' >&2
        return 1
      fi
      cat "$ART/pods-final.json" ;;
    "get pods -o json") cat "$ART/pods-poll.json" ;;
    "get statefulset caesium -o json") printf '{"status":{"replicas":1}}\n' ;;
    "get events"*) printf 'events\n' ;;
    "logs caesium-0"*"--previous"*)
      printf '%s\n' '2026-09-27T00:00:00Z level=fatal msg="address \"10.0.0.1:9001\" in info.yaml does not match \"10.0.0.9:9001\""' ;;
    "logs caesium-0"*) printf '2026-09-27T00:00:05Z starting\n' ;;
    "exec"*"info.yaml"*) printf 'Address: 10.0.0.1:9001\n' ;;
    "exec"*) printf '%064d  ./info.yaml\n' 0 ;;
  esac
  return 0
}
lc_rollback_probe() {
  printf 'probe %s\n' "$*" >>"$ART/calls.log"
  [[ "$RB_PROBE" == refused ]] || { printf 'probe pod exec failed\n' >"${4%.json}.log"; return 1; }
  RB_TARGETS="$3" python3 - >"$4" <<'PY'
import json,os
members=[]
for item in os.environ['RB_TARGETS'].split(','):
  label,base=item.split('=',1);host=base.split('//',1)[1]
  err=lambda p:f'Get "{base}{p}": dial tcp {host}: connect: connection refused'
  members.append({'member':label,'base':base,'health':{'path':'/health','error':err('/health')},
    'health_ready':{'path':'/health/ready','error':err('/health/ready')}})
print(json.dumps({'mode':'http','members':members}))
PY
}
"""


def pod(ip):
    status = {
        "phase": "Running",
        "containerStatuses": [{
            "name": "caesium", "ready": False, "restartCount": 3,
            "image": "caesiumcloud/caesium:v0.1.0", "imageID": "sha256:prev",
            "state": {"waiting": {"reason": "CrashLoopBackOff"}},
            "lastState": {"terminated": {"exitCode": 1, "reason": "Error"}},
        }],
    }
    if ip:
        status["podIP"] = ip
    return {"metadata": {"name": "caesium-0"}, "spec": {"nodeName": "node-0"}, "status": status}


class RollbackHarness(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="caesium-f2-rollback-")
        self.addCleanup(self.tmp.cleanup)
        self.art = Path(self.tmp.name)
        (self.art / "bin").mkdir()
        shim = self.art / "bin/kubectl"
        shim.write_text(KUBECTL_SHIM)
        shim.chmod(0o755)
        for cid in CIDS:
            (self.art / f"state-{cid}").write_text("RUNNING\n")
        self.rb = self.art / "cluster-logs/rollback"

    def script(self, scenario, observe=False):
        return "\n".join([PRELUDE, rollback_block(), STUBS_AFTER_BLOCK,
                          OBSERVE_STUBS if observe else "", textwrap.dedent(scenario)])

    def env(self, **extra):
        env = os.environ.copy()
        env.pop("CAESIUM_LIFECYCLE_KEEP", None)
        env.update(ART=str(self.art), RB_COPY_MODE="answer", RB_FINAL_LISTING="ok", RB_PROBE="refused")
        env.update(extra)
        return env

    def case(self):
        return json.loads((self.art / "case.json").read_text())

    def calls(self):
        path = self.art / "calls.log"
        return path.read_text().splitlines() if path.exists() else []

    def ctr(self):
        return (self.art / "ctr.log").read_text().splitlines()

    def assert_all_resumed(self):
        lines = self.ctr()
        for cid in CIDS:
            self.assertIn(f"pause {cid}", lines)
            self.assertIn(f"resume {cid}", lines)
            self.assertGreater(lines.index(f"resume {cid}"), lines.index(f"pause {cid}"))
            self.assertEqual((self.art / f"state-{cid}").read_text().strip(), "RUNNING")

    def assert_copy_workers_dead(self):
        deadline = time.monotonic() + 10
        for n in range(3):
            pid = int((self.art / f"copy-pid-{n}").read_text())
            while True:
                try:
                    os.kill(pid, 0)
                except ProcessLookupError:
                    break
                if time.monotonic() > deadline:
                    self.fail(f"stalled copy worker {pid} for caesium-{n} is still running")
                time.sleep(0.1)


class ObservationCompletenessTest(RollbackHarness):
    """The pod listing and the HTTP probe are evidence; their absence blocks."""

    def observe(self, final_ip="10.0.0.9", **env):
        self.rb.mkdir(parents=True)
        (self.art / "pods-poll.json").write_text(json.dumps({"items": [pod("10.0.0.9")]}))
        (self.art / "pods-final.json").write_text(json.dumps({"items": [pod(final_ip)]}))
        manifest = f"{0:064d}  ./info.yaml\n"
        for n in range(3):
            (self.rb / f"copy-{n}.sha256").write_text(manifest)
            (self.rb / f"restored-{n}.sha256").write_text(manifest)
            (self.rb / f"paused-state-{n}.txt").write_text(f"{CIDS[n]} 1 PAUSED\n")
            (self.rb / f"resumed-state-{n}.txt").write_text(f"{CIDS[n]} 1 RUNNING\n")
        # Record with an empty reason: the record must block on its own even
        # if a caller ignored the observe step's exit status.
        scenario = """
            LC_RB_NODE=(node-0 node-1 node-2) LC_RB_CID=()
            rc=0
            lc_rollback_observe || rc=$?
            printf '%s\\n' "$rc" >"$ART/observe-rc"
            lc_rollback_record ""
        """
        result = subprocess.run(["bash", "-c", self.script(scenario, observe=True)],
                                env=self.env(**env), capture_output=True, text=True, timeout=120)
        self.assertEqual(result.returncode, 0, result.stderr)
        rec = self.case()
        return int((self.art / "observe-rc").read_text()), rec, rec.get("observations", {})

    def test_connection_refusal_is_a_recorded_outcome(self):
        observe_rc, rec, obs = self.observe()
        self.assertEqual(observe_rc, 0)
        self.assertEqual(rec["status"], "recorded-outcome", rec["detail"])
        self.assertEqual(obs["problems"], [])
        first = obs["members"]["caesium-0"]
        self.assertEqual(first["existence"], "created")
        self.assertEqual(first["address"]["pod_ip"], "10.0.0.9")
        self.assertEqual(first["http_outcome"], "connection_refused")
        self.assertEqual(first["exit_codes"], [1])
        for name in ("caesium-1", "caesium-2"):
            self.assertEqual(obs["members"][name]["existence"], "never_created")
        self.assertIn("http=connection_refused", rec["detail"])
        self.assertIn("never created: caesium-1, caesium-2", rec["detail"])
        self.assertTrue(any(c.startswith("probe rb http caesium-0=http://10.0.0.9:8080") for c in self.calls()))

    def test_failed_final_listing_blocks_and_never_infers_not_created(self):
        observe_rc, rec, obs = self.observe(RB_FINAL_LISTING="fail")
        self.assertNotEqual(observe_rc, 0)
        self.assertEqual(rec["status"], "blocked")
        for name in ("caesium-0", "caesium-1", "caesium-2"):
            self.assertEqual(obs["members"][name]["existence"], "unknown")
            self.assertTrue(any(p.startswith(f"{name}: the final pod listing was not read") for p in obs["problems"]))
        self.assertFalse(any(c.startswith("probe ") for c in self.calls()))
        listings = [c for c in self.calls() if c == "lc_rb --request-timeout=20s get pods -o json"]
        self.assertEqual(len(listings), 3)

    def test_probe_that_never_ran_is_not_probed_and_blocks(self):
        observe_rc, rec, obs = self.observe(RB_PROBE="fail")
        self.assertNotEqual(observe_rc, 0)
        self.assertEqual(rec["status"], "blocked")
        first = obs["members"]["caesium-0"]
        self.assertEqual(first["http_outcome"], "not_probed")
        self.assertNotIn("http", first)
        self.assertTrue(any("caesium-0: http not_probed" in p for p in obs["problems"]))

    def test_member_without_pod_ip_is_not_probed_and_blocks(self):
        observe_rc, rec, obs = self.observe(final_ip=None)
        self.assertNotEqual(observe_rc, 0)
        self.assertEqual(rec["status"], "blocked")
        first = obs["members"]["caesium-0"]
        self.assertEqual(first["existence"], "created")
        self.assertIsNone(first["address"]["pod_ip"])
        self.assertEqual(first["http_outcome"], "not_probed")
        self.assertFalse(any(c.startswith("probe ") for c in self.calls()))


class BoundedFreezeTest(RollbackHarness):
    """A stalled copy or a signal never leaves the main cluster frozen."""

    def test_stalled_copy_hits_deadline_thaws_every_member_and_blocks(self):
        scenario = """
            trap 'echo global-exit >>"$ART/calls.log"' EXIT
            trap 'LC_SIGNALLED=1; exit 130' INT
            trap -p >"$ART/traps-before"
            rc=0
            lc_rollback_case || rc=$?
            trap -p >"$ART/traps-after"
            printf '%s\\n' "$rc" >"$ART/case-rc"
        """
        started = time.monotonic()
        result = subprocess.run(["bash", "-c", self.script(scenario)], capture_output=True, text=True,
                                env=self.env(RB_COPY_MODE="stall", CAESIUM_LIFECYCLE_ROLLBACK_COPY_TIMEOUT="3"),
                                timeout=90)
        elapsed = time.monotonic() - started
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertLess(elapsed, 40, "the frozen copy was not bounded by its deadline")
        self.assertEqual((self.art / "case-rc").read_text().strip(), "0")
        self.assert_all_resumed()
        self.assert_copy_workers_dead()
        rec = self.case()
        self.assertEqual(rec["status"], "blocked")
        self.assertIn("timeout: the frozen copy passed its 3s deadline on caesium-0 caesium-1 caesium-2", rec["detail"])
        self.assertIn("resumed and verified RUNNING: caesium-0 caesium-1 caesium-2", rec["detail"])
        freeze = rec["observations"]["freeze"]
        self.assertEqual(freeze["deadline_s"], 3)
        self.assertTrue(freeze["thaw_verified_running"])
        self.assertIsNotNone(freeze["duration_s"])
        self.assertLess(freeze["duration_s"], 30)
        # The pre-freeze handlers are back once the freeze window closes.
        self.assertEqual((self.art / "traps-before").read_text(), (self.art / "traps-after").read_text())
        self.assertIn("passed its deadline", (self.rb / "copy-0.err").read_text())

    def test_signal_during_frozen_copy_thaws_then_runs_pre_freeze_handlers(self):
        self.signal_during_copy(second_signal=False)

    def test_second_signal_does_not_abandon_the_thaw(self):
        # Each resume takes a second; an INT half-way through the TERM-driven
        # thaw is held until every member is resumed.
        self.signal_during_copy(second_signal=True)

    def signal_during_copy(self, second_signal):
        scenario = """
            trap 'echo "global-exit rc=$?" >>"$ART/calls.log"' EXIT
            trap 'echo global-term >>"$ART/calls.log"; LC_SIGNALLED=1; exit 143' TERM
            LC_RB_NODE=() LC_RB_CID=()
            mkdir -p "$LC_RB_DIR"
            if ! lc_rollback_copy; then echo copy-returned >>"$ART/calls.log"; fi
            echo not-reached >>"$ART/calls.log"
        """
        proc = subprocess.Popen(["bash", "-c", self.script(scenario)], stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, text=True, start_new_session=True,
                                env=self.env(RB_COPY_MODE="stall", CAESIUM_LIFECYCLE_KEEP="1",
                                             CAESIUM_LIFECYCLE_ROLLBACK_COPY_TIMEOUT="120",
                                             RB_SLOW_RESUME="1" if second_signal else ""))
        try:
            deadline = time.monotonic() + 60
            while not all((self.art / f"copy-pid-{n}").exists() for n in range(3)):
                self.assertIsNone(proc.poll(), "controller exited before the copy workers started")
                self.assertLess(time.monotonic(), deadline, "copy workers never started")
                time.sleep(0.1)
            for cid in CIDS:
                self.assertEqual((self.art / f"state-{cid}").read_text().strip(), "PAUSED")
            signalled = time.monotonic()
            proc.send_signal(signal.SIGTERM)
            if second_signal:
                time.sleep(1.5)
                proc.send_signal(signal.SIGINT)
            _, stderr = proc.communicate(timeout=60)
            self.assertLess(time.monotonic() - signalled, 30, "thaw did not run promptly on SIGTERM")
        finally:
            if proc.poll() is None:
                os.killpg(proc.pid, signal.SIGKILL)
                proc.wait()
        self.assertEqual(proc.returncode, 143, stderr)
        self.assert_all_resumed()
        self.assert_copy_workers_dead()
        calls = self.calls()
        self.assertNotIn("copy-returned", calls)
        self.assertNotIn("not-reached", calls)
        # Thaw, then the blocked record, then the pre-freeze TERM and EXIT handlers.
        self.assertLess(calls.index("case blocked"), calls.index("global-term"))
        self.assertEqual(calls[-1], "global-exit rc=143")
        rec = self.case()
        self.assertEqual(rec["status"], "blocked")
        self.assertIn("SIGTERM during the frozen three-member copy", rec["detail"])
        self.assertIn("thaw (signal TERM) resumed and verified RUNNING: caesium-0 caesium-1 caesium-2", rec["detail"])


if __name__ == "__main__":
    unittest.main()
