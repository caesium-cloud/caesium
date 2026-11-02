"""Exercise the fuzz runner's host entry point without starting containers."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]


class FuzzBuilderTests(unittest.TestCase):
    def test_explicit_builder_skips_build_and_uses_inspected_identity(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp = Path(tmp)
            engine = tmp / "engine"
            engine.write_text("""#!/usr/bin/env python3
import json, os, sys
with open(os.environ['CALLS'], 'a') as f: f.write(json.dumps(sys.argv[1:])+'\\n')
if sys.argv[1:3] == ['image', 'inspect']:
    print('sha256:' + 'a'*64)
else:
    sys.exit(23)
""")
            engine.chmod(0o755)
            just = tmp / "just"
            just.write_text("#!/bin/sh\nexit 99\n")
            just.chmod(0o755)
            calls = tmp / "calls"
            env = {**os.environ, "PATH": str(tmp) + os.pathsep + os.environ['PATH'],
                   "CALLS": str(calls), "CAESIUM_BUILDER_IMAGE": "private/builder:candidate",
                   "CAESIUM_CONTAINER_CLI": str(engine),
                   "CAESIUM_FUZZ_ARTIFACT_DIR": str(tmp / "artifacts")}
            env.pop("CAESIUM_FUZZ_IN_CONTAINER", None)
            result = subprocess.run(['sh', str(ROOT / 'scripts/fuzz-tests.sh')], env=env,
                                    capture_output=True, text=True)
            self.assertEqual(result.returncode, 23, result.stdout + result.stderr)
            commands = [json.loads(line) for line in calls.read_text().splitlines()]
            self.assertEqual(commands[0][-1], "private/builder:candidate")
            self.assertEqual(commands[1][0], "run")
            self.assertIn("sha256:" + "a"*64, commands[1])
            self.assertNotIn("private/builder:candidate", commands[1])


if __name__ == '__main__':
    unittest.main()
