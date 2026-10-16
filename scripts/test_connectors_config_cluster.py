"""Exercise cluster-driver streams and ownership without creating resources."""
import os
import json
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


class ClusterOwnershipTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        (self.root / "scripts").mkdir()
        (self.root / "bin").mkdir()
        (self.root / "scratch").mkdir()
        source = Path(__file__).with_name("connectors-config-cluster.sh")
        shutil.copyfile(source, self.root / "scripts" / source.name)
        shared = source.with_name("helm-cluster-fixture.sh")
        shutil.copyfile(shared, self.root / "scripts" / shared.name)
        (self.root / "scripts" / "connectors-fixture.sh").write_text("#!/bin/sh\nexit 0\n")
        fake = self.root / "fake.py"
        fake.write_text('''#!/usr/bin/env python3
import json,os,sys
from pathlib import Path
name=Path(sys.argv[0]).name
args=sys.argv[1:]
root=Path(os.environ["PROBE_ROOT"])
with (root/"calls").open("a") as log: log.write(json.dumps([name,*args])+"\\n")
scenario=os.environ["PROBE_SCENARIO"]
if name=="git":
    if args==["rev-parse","HEAD"]: print("a"*40)
elif name=="kind":
    if args==["get","clusters"]:
        counter=root/"cluster-lists"
        count=int(counter.read_text())+1 if counter.exists() else 1
        counter.write_text(str(count))
        if scenario=="existing" or (scenario=="appeared" and count>1): print("probe-owned")
    elif args[:2]==["create","cluster"]:
        if scenario not in ("identity-changed","owned-load-failed"): sys.exit(2)
        Path(args[args.index("--kubeconfig")+1]).touch()
    elif args[:2]==["get","nodes"]: print("probe-owned-control-plane")
    elif args[:2]==["delete","cluster"]:
        if scenario!="owned-load-failed": raise SystemExit("unexpected delete")
elif name=="docker":
    if args[:2]==["image","save"]: sys.exit(2)
    if args[0]=="inspect":
        counter=root/"node-inspects"
        count=int(counter.read_text())+1 if counter.exists() else 1
        counter.write_text(str(count))
        print(("c" if scenario=="identity-changed" and count>1 else "b")*64)
    elif "--format" in args: print("sha256:"+"b"*64)
elif name=="helm": print("v3.22.0")
''')
        fake.chmod(0o700)
        for command in ("git", "kind", "docker", "just", "kubectl", "helm"):
            (self.root / "bin" / command).symlink_to(fake)

    def run_probe(self, scenario, expect_delete=False):
        env = dict(os.environ, PATH=str(self.root / "bin") + os.pathsep + os.environ["PATH"],
                   TMPDIR=str(self.root / "scratch"), PROBE_ROOT=str(self.root),
                   PROBE_SCENARIO=scenario, CAESIUM_CONNECTORS_CLUSTER_ID="probe-owned",
                   CAESIUM_CONNECTORS_CLUSTER_ARTIFACTS=str(self.root / "artifacts"))
        env.pop("CANDIDATE_SHA", None)
        result = subprocess.run(["bash", str(self.root / "scripts" / "connectors-config-cluster.sh")],
                                env=env, capture_output=True, text=True, timeout=15)
        calls = [json.loads(line) for line in (self.root / "calls").read_text().splitlines()]
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        deletes = [call for call in calls if call[:3] == ["kind", "delete", "cluster"]]
        self.assertEqual(len(deletes), int(expect_delete), calls)
        return result, calls

    def test_existing_cluster_is_preserved_before_build(self):
        result, calls = self.run_probe("existing")
        self.assertIn("refusing an existing cluster", result.stderr)
        self.assertFalse(any(call[0] == "just" for call in calls), calls)

    def test_cluster_appearing_during_build_is_preserved(self):
        result, calls = self.run_probe("appeared")
        self.assertIn("appeared during builds", result.stderr)
        self.assertFalse(any(call[:3] == ["kind", "create", "cluster"] for call in calls), calls)

    def test_failed_create_never_claims_or_deletes_cluster(self):
        result, calls = self.run_probe("create-failed")
        self.assertIn("no cluster deletion without confirmed ownership", result.stderr)
        self.assertTrue(any(call[:3] == ["kind", "create", "cluster"] for call in calls), calls)

    def test_existing_name_claim_is_preserved(self):
        claim = self.root / "scratch" / "caesium-connectors-cluster-claims" / "probe-owned"
        claim.mkdir(parents=True)
        (claim / "foreign").write_text("owned elsewhere")
        result, calls = self.run_probe("create-failed")
        self.assertIn("name already claimed", result.stderr)
        self.assertEqual((claim / "foreign").read_text(), "owned elsewhere")
        self.assertFalse(any(call[0] == "kind" for call in calls), calls)

    def test_replaced_node_identity_prevents_cluster_deletion(self):
        result, _ = self.run_probe("identity-changed")
        self.assertIn("ownership could not be verified", result.stderr)

    def test_confirmed_owned_cluster_is_cleaned_after_failure(self):
        self.run_probe("owned-load-failed", expect_delete=True)


class BootstrapLogTests(unittest.TestCase):
    def test_large_bootstrap_log_is_drained_under_pipefail(self):
        source = Path(__file__).with_name("connectors-config-cluster.sh").read_text()
        assignment = next(line for line in source.splitlines() if line.startswith('admin_key="$('))
        # Execute the actual driver pipeline against a producer larger than a
        # pipe buffer. Exiting the key parser early breaks a healthy log stream.
        producer = """kn() {
python3 - <<'PY'
import os
os.write(1, b'  csk_bootstrap_stream_probe\\r\\n')
for _ in range(4096):
    os.write(1, b'ordinary startup log line ' * 64 + b'\\n')
os.write(1, b'  csk_ignored_second_key\\n')
PY
}
"""
        result = subprocess.run(["bash", "-c", "set -euo pipefail\n" + producer + assignment +
                                 '\nprintf "%s" "$admin_key"'],
                                capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "csk_bootstrap_stream_probe")


if __name__ == "__main__":
    unittest.main()
