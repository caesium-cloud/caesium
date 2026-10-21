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

    def run_probe(self, scenario, expect_delete=False, **extra):
        env = dict(os.environ, PATH=str(self.root / "bin") + os.pathsep + os.environ["PATH"],
                   TMPDIR=str(self.root / "scratch"), PROBE_ROOT=str(self.root),
                   PROBE_SCENARIO=scenario, CAESIUM_CONNECTORS_CLUSTER_ID="probe-owned",
                   CAESIUM_CONNECTORS_CLUSTER_ARTIFACTS=str(self.root / "artifacts"))
        env.pop("CANDIDATE_SHA", None)
        env.update(extra)
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
    def reuse_options(self, server='b', runner='b', fixture='b'):
        return {'CAESIUM_CONNECTORS_REUSE_IMAGES':'true',
                'CAESIUM_CONNECTORS_REUSE_SERVER_IMAGE_ID':'sha256:'+server*64,
                'CAESIUM_CONNECTORS_REUSE_RUNNER_IMAGE_ID':'sha256:'+runner*64,
                'CAESIUM_CONNECTORS_REUSE_FIXTURE_IMAGE_ID':'sha256:'+fixture*64}

    def test_reuse_requires_firstphase_identities_before_cluster_creation(self):
        result, calls = self.run_probe('create-failed', CAESIUM_CONNECTORS_REUSE_IMAGES='true')
        self.assertIn('requires first-phase immutable server identity', result.stderr)
        self.assertFalse(any(call[:3] == ['kind','create','cluster'] for call in calls))

    def test_coherently_replaced_tags_refuse_before_cluster_creation(self):
        result, calls = self.run_probe('create-failed', **self.reuse_options('a','a','a'))
        self.assertIn('tag differs from tested first-phase image', result.stderr)
        self.assertFalse(any(call[:3] == ['kind','create','cluster'] for call in calls))
        self.assertFalse(any(call[:3] == ['docker','image','save'] for call in calls))

    def test_changed_runner_tag_refuses_before_cluster_creation(self):
        result, calls = self.run_probe('create-failed', **self.reuse_options(runner='a'))
        self.assertIn('runner tag differs from tested first-phase image', result.stderr)
        self.assertFalse(any(call[:3] == ['kind','create','cluster'] for call in calls))

    def test_changed_fixture_tag_refuses_before_cluster_creation(self):
        result, calls = self.run_probe('create-failed', **self.reuse_options(fixture='a'))
        self.assertIn('fixture tag differs from tested first-phase image', result.stderr)
        self.assertFalse(any(call[:3] == ['kind','create','cluster'] for call in calls))

    def test_reuse_must_agree_with_loaded_producer(self):
        result, calls = self.run_probe('create-failed', **self.reuse_options(),
            CAESIUM_CONNECTORS_EXPECTED_IMAGE_ID='sha256:'+'a'*64)
        self.assertIn('differs from producer server identity', result.stderr)
        self.assertFalse(any(call[:3] == ['kind','create','cluster'] for call in calls))

    def test_malformed_firstphase_fixture_identity_refuses_before_creation(self):
        options = self.reuse_options()
        options['CAESIUM_CONNECTORS_REUSE_FIXTURE_IMAGE_ID'] = 'sha256:invalid'
        result, calls = self.run_probe('create-failed', **options)
        self.assertIn('requires first-phase immutable fixture identity', result.stderr)
        self.assertFalse(any(call[:3] == ['kind','create','cluster'] for call in calls))

    def test_reuse_runner_must_agree_with_loaded_producer(self):
        result, calls = self.run_probe('create-failed', **self.reuse_options(),
            CAESIUM_CONNECTORS_EXPECTED_RUNNER_IMAGE_ID='sha256:'+'a'*64)
        self.assertIn('differs from producer runner identity', result.stderr)
        self.assertFalse(any(call[:3] == ['kind','create','cluster'] for call in calls))

    def test_matching_firstphase_images_are_recorded_and_rechecked_before_import(self):
        result, calls = self.run_probe('owned-load-failed', expect_delete=True, **self.reuse_options())
        self.assertTrue(any(call[:3] == ['kind','create','cluster'] for call in calls))
        receipt = json.loads((self.root/'artifacts/reuse-image-identities.json').read_text())
        for role in ('server','runner','fixture'):
            self.assertEqual(receipt[role]['expected_image_id'], 'sha256:'+'b'*64)
            self.assertEqual(receipt[role]['expected_image_id'], receipt[role]['observed_image_id'])
        # Each reference is checked before creation and again before import.
        for role in ('server','runner','fixture'):
            ref = receipt[role]['tag']
            checks = [call for call in calls if call == ['docker','image','inspect','--format','{{.Id}}',ref]]
            self.assertGreaterEqual(len(checks), 2)


class ClusterBaselineEnvironmentTests(unittest.TestCase):
    def test_cluster_preserves_baseline_and_tracked_kubernetes_topology(self):
        repository = Path(__file__).resolve().parents[1]
        source = (repository/'scripts/connectors-config-cluster.sh').read_text()
        begin = source.index('import json,re,sys\nfrom pathlib import Path\nbase=json.load')
        block = source[begin:].split('\nPY', 1)[0]
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            shutil.copyfile(repository/'justfile', root/'justfile')
            baseline = root/'baseline.json'
            baseline.write_text(json.dumps({'spec': {'template': {'spec': {'containers': [{'env': [
                {'name': 'CAESIUM_ENGINE', 'value': 'kubernetes'},
                {'name': 'CAESIUM_DATABASE_SHARDS', 'value': '4'},
                {'name': 'POD_NAME', 'value': 'chart-native'},
            ]}]}}}}))
            result = subprocess.run(['python3', '-c', block, str(baseline), 'candidate'],
                                    cwd=root, capture_output=True, text=True, check=True)
            values = json.loads(result.stdout)
            env = {v['name']: v.get('value') for v in values['config']['extraEnv']}
            self.assertEqual(env['CAESIUM_ENGINE'], 'kubernetes')
            self.assertEqual(env['CAESIUM_EXECUTION_MODE'], 'distributed')
            self.assertEqual(env['CAESIUM_DATABASE_SHARDS'], '1')
            self.assertEqual(env['CAESIUM_CONNECTORS_ENABLED'], 'true')
            self.assertEqual(env['CAESIUM_AUTH_MODE'], 'api-key')
            self.assertEqual(env['CAESIUM_RUN_QUEUE_ENABLED'], 'true')
            self.assertEqual(env['CAESIUM_RUN_QUEUE_DEQUEUER_ENABLED'], 'true')
            self.assertNotIn('POD_NAME', env)
            self.assertEqual(values['replicaCount'], 3)
            # A future baseline feature must join the cluster without another copy.
            justfile = root/'justfile'
            justfile.write_text(justfile.read_text().replace('integration-up: build-stress build-test',
                'integration-up: build-stress build-test\n    -e CAESIUM_FUTURE_GATE=true '+chr(92)))
            next_result = subprocess.run(['python3', '-c', block, str(baseline), 'candidate'],
                                        cwd=root, capture_output=True, text=True, check=True)
            next_env = {v['name']: v.get('value') for v in json.loads(next_result.stdout)['config']['extraEnv']}
            self.assertEqual(next_env['CAESIUM_FUTURE_GATE'], 'true')


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


class LoadedImageIdentityTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.artifacts = self.root / "artifacts"
        self.artifacts.mkdir()
        (self.root / "bin").mkdir()
        (self.root / "test" / "robustness").mkdir(parents=True)
        repository = Path(__file__).resolve().parents[1]
        shutil.copyfile(repository / "test" / "robustness" / "hostlogic.py",
                        self.root / "test" / "robustness" / "hostlogic.py")
        source = (repository / "scripts" / "connectors-config-cluster.sh").read_text()
        begin = source.index('node="$(head -n1 "$artifacts/kind-nodes.txt")"')
        end = source.index('kc create namespace "$namespace"')
        self.assertLess(begin, end, "candidate identities must be fixed before app deployment")
        # Execute the real driver's complete identity block and its real shared
        # parser. Only containerd/CRI metadata transport is replaced here.
        self.block = source[begin:end]
        self.image = "caesiumcloud/caesium:identity-probe-test"
        (self.artifacts / "kind-nodes.txt").write_text("owned-control-plane\n")
        (self.artifacts / "server-image-id.txt").write_text("sha256:" + "a" * 64 + "\n")
        (self.root / "listing").write_text(
            "REF TYPE DIGEST SIZE\n"
            "docker.io/" + self.image + " application/vnd.oci.image.manifest.v1+json sha256:" + "b" * 64 + " 1MiB\n"
            "sha256:" + "c" * 64 + " application/vnd.oci.image.manifest.v1+json sha256:" + "b" * 64 + " 1MiB\n"
            "import-2026-10-16@sha256:" + "d" * 64 + " application/vnd.oci.image.index.v1+json sha256:" + "d" * 64 + " 1MiB\n"
            "docker.io/library/foreign:tag application/vnd.oci.image.manifest.v1+json sha256:" + "e" * 64 + " 1MiB\n")
        docker = self.root / "bin" / "docker"
        docker.write_text('''#!/usr/bin/env python3
import json,os,sys
from pathlib import Path
root=Path(os.environ["PROBE_ROOT"])
args=sys.argv[1:]
with (root/"calls").open("a") as log: log.write(json.dumps(args)+"\\n")
if args==["exec","owned-control-plane","ctr","-n","k8s.io","images","ls"]:
    print((root/"listing").read_text())
elif args==["exec","owned-control-plane","crictl","inspecti",os.environ["server_image"]]:
    print((root/"cri-payload").read_text())
else:
    raise SystemExit("unexpected metadata command")
''')
        docker.chmod(0o700)

    def metadata(self):
        return {"status": {"id": "sha256:" + "c" * 64,
                           "repoTags": ["docker.io/" + self.image],
                           "repoDigests": ["docker.io/library/import-2026-10-16@sha256:" + "d" * 64]},
                "info": {"imageSpec": {"rootfs": {"diff_ids": ["sha256:" + "f" * 64]},
                                       "config": {"Env": ["PRIVATE=identity-secret-canary"],
                                                  "Labels": {"credential": "identity-label-canary"}}}}}

    def run_identity_block(self, document):
        (self.root / "cri-payload").write_text(document if isinstance(document, str) else json.dumps(document))
        env = dict(os.environ, PATH=str(self.root / "bin") + os.pathsep + os.environ["PATH"],
                   PROBE_ROOT=str(self.root), artifacts=str(self.artifacts), server_image=self.image)
        return subprocess.run(["bash", "-c", "set -euo pipefail\n" + self.block],
                              cwd=self.root, env=env, capture_output=True, text=True, timeout=10)

    def test_loaded_tag_cri_parent_index_is_trusted_only_through_saved_identity(self):
        result = self.run_identity_block(self.metadata())
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        identities = set((self.artifacts / "candidate-identities.txt").read_text().strip().split(","))
        self.assertEqual(identities, {"sha256:" + character * 64 for character in "abcd"})
        projected = json.loads((self.artifacts / "loaded-candidate-cri.json").read_text())
        self.assertEqual(projected, {"status": self.metadata()["status"]})
        persisted = "\n".join(path.read_text() for path in self.artifacts.iterdir() if path.is_file())
        self.assertNotIn("identity-secret-canary", persisted)
        self.assertNotIn("identity-label-canary", persisted)
        self.assertNotIn("sha256:" + "f" * 64, persisted)
        calls = [json.loads(line) for line in (self.root / "calls").read_text().splitlines()]
        self.assertEqual(calls[-1], ["exec", "owned-control-plane", "crictl", "inspecti", self.image])

    def test_foreign_cri_config_cannot_introduce_its_parent_index(self):
        document = self.metadata()
        document["status"]["id"] = "sha256:" + "e" * 64
        result = self.run_identity_block(document)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("not in expected", result.stderr)
        self.assertFalse((self.artifacts / "candidate-identities.txt").exists())

    def test_missing_or_invalid_candidate_cri_proof_fails_closed(self):
        mutations = [lambda doc: doc.update(status=None),
                     lambda doc: doc["status"].pop("id"),
                     lambda doc: doc["status"].update(id="not-a-digest"),
                     lambda doc: doc["status"].update(repoTags=["docker.io/foreign:tag"]),
                     lambda doc: doc["status"].update(repoTags="docker.io/" + self.image),
                     lambda doc: doc["status"].update(repoDigests=[]),
                     lambda doc: doc["status"].update(repoDigests=["not-a-digest"])]
        documents = ["not JSON"]
        for mutate in mutations:
            document = self.metadata()
            mutate(document)
            documents.append(document)
        for document in documents:
            with self.subTest(document=document):
                result = self.run_identity_block(document)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse((self.artifacts / "candidate-identities.txt").exists())


if __name__ == "__main__":
    unittest.main()
