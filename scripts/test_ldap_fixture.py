"""Exercise LDAP fixture TLS generation and CLI wiring without a container engine."""

import base64
import hashlib
import json
import os
from pathlib import Path
import stat
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts/ldap-fixture.sh"
CERT_DIR = "/container/service/slapd/assets/certs"
LDIF_DIR = "/container/service/slapd/assets/config/bootstrap/ldif/custom"
IMAGE = "osixia/openldap:1.5.0@sha256:18742e9c449c9c1afe129d3f2f3ee15fb34cc43e5f940a20f3399728f41d7c28"
# PKCS#3 DER for RFC 7919's public ffdhe2048 group, generated once with:
# openssl genpkey -genparam -algorithm DH -pkeyopt group:ffdhe2048
FFDHE2048_DER_SHA256 = "ca697111b7a89a23ac7ce82ce56d0fa260cbff835dc82bb6f440e8bf395d3155"


class LDAPFixtureTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temp = tempfile.TemporaryDirectory(prefix="ldap-fixture-test-")
        cls.addClassCleanup(cls.temp.cleanup)
        cls.work = Path(cls.temp.name)
        cls.tls_dir = cls.work / "tls"
        cls.command("bash", str(SCRIPT), "tls", str(cls.tls_dir))
        cls.calls = cls.work / "calls.json"
        cls.cli = cls.work / "record-cli"
        cls.cli.write_text(
            "#!/usr/bin/env python3\n"
            "import json, os, sys\n"
            "from pathlib import Path\n"
            "Path(os.environ['LDAP_FIXTURE_TEST_CALLS']).write_text(json.dumps(sys.argv[1:]))\n"
        )
        cls.cli.chmod(0o755)

    @staticmethod
    def command(*args, env=None):
        result = subprocess.run(args, capture_output=True, text=True, timeout=20, env=env)
        if result.returncode:
            raise AssertionError(f"{args[0]} exited {result.returncode}: {result.stderr}")
        return result.stdout

    def invoke_cli(self, command, *args):
        env = dict(os.environ, LDAP_FIXTURE_TEST_CALLS=str(self.calls))
        env.pop("CAESIUM_LDAP_IMAGE", None)
        self.command("bash", str(SCRIPT), command, str(self.cli), *args, env=env)
        return json.loads(self.calls.read_text())

    def test_tls_writes_standard_public_dh_parameters(self):
        path = self.tls_dir / "dhparam.pem"
        self.assertTrue(path.is_file(), "tls must provide DH parameters before OpenLDAP starts")
        self.assertEqual(path.read_bytes(), (ROOT / "test/testdata/ldap/dhparam.pem").read_bytes())
        lines = path.read_text().splitlines()
        self.assertEqual(lines[0], "-----BEGIN DH PARAMETERS-----")
        self.assertEqual(lines[-1], "-----END DH PARAMETERS-----")
        der = base64.b64decode("".join(lines[1:-1]), validate=True)
        self.assertEqual(hashlib.sha256(der).hexdigest(), FFDHE2048_DER_SHA256)
        self.command("openssl", "dhparam", "-in", str(path), "-check", "-noout")
        description = self.command("openssl", "dhparam", "-in", str(path), "-text", "-noout")
        self.assertIn("(2048 bit)", description)

    def test_tls_keeps_verified_localhost_certificate_and_permissions(self):
        for option, name in (("-verify_hostname", "localhost"), ("-verify_ip", "127.0.0.1")):
            result = self.command("openssl", "verify", "-CAfile", str(self.tls_dir / "ca.crt"),
                                  option, name, str(self.tls_dir / "server.crt"))
            self.assertIn(": OK", result)
        self.assertEqual(stat.S_IMODE(self.tls_dir.stat().st_mode), 0o755)
        for name in ("ca.crt", "server.crt", "server.key", "dhparam.pem"):
            self.assertEqual(stat.S_IMODE((self.tls_dir / name).stat().st_mode), 0o644)
        self.assertFalse((self.tls_dir / "ca.key").exists())

    def test_tls_keeps_fresh_ca_and_server_keys(self):
        fresh = self.work / "fresh-tls"
        self.command("bash", str(SCRIPT), "tls", str(fresh))
        for name in ("ca.crt", "server.crt", "server.key"):
            self.assertNotEqual((fresh / name).read_bytes(), (self.tls_dir / name).read_bytes())
        self.assertEqual((fresh / "dhparam.pem").read_bytes(), (self.tls_dir / "dhparam.pem").read_bytes())

    def test_run_mounts_tls_readonly_and_selects_dh_parameters(self):
        args = self.invoke_cli("run", "ldap-fixture", "server-fixture", str(self.tls_dir),
                               "--label", "ldap-fixture-test")
        self.assertEqual(args[:9], ["run", "-d", "--name", "ldap-fixture", "--network",
                                    "container:server-fixture", "--label", "ldap-fixture-test", "-v"])
        mounts = [args[index + 1] for index, value in enumerate(args) if value == "-v"]
        self.assertEqual(mounts, [f"{self.tls_dir}:{CERT_DIR}:ro",
                                  f"{ROOT}/test/testdata/ldap:{LDIF_DIR}:ro"])
        env = dict(args[index + 1].split("=", 1) for index, value in enumerate(args) if value == "-e")
        self.assertEqual(env, {
            "LDAP_ORGANISATION": "Caesium", "LDAP_DOMAIN": "example.org",
            "LDAP_ADMIN_PASSWORD": "iaa-ldap-admin-secret", "LDAP_TLS": "true",
            "LDAP_TLS_CRT_FILENAME": "server.crt", "LDAP_TLS_KEY_FILENAME": "server.key",
            "LDAP_TLS_CA_CRT_FILENAME": "ca.crt", "LDAP_TLS_DH_PARAM_FILENAME": "dhparam.pem",
            "LDAP_TLS_VERIFY_CLIENT": "never", "LDAP_TLS_ENFORCE": "false",
        })
        self.assertEqual(args[-2:], [IMAGE, "--copy-service"])

    def test_wait_keeps_verified_bounded_ldaps_probe(self):
        args = self.invoke_cli("wait", "ldap-fixture")
        self.assertEqual(args, [
            "exec", "-e", f"LDAPTLS_CACERT={CERT_DIR}/ca.crt", "-e", "LDAPTLS_REQCERT=demand", "ldap-fixture",
            "timeout", "5", "ldapsearch", "-o", "nettimeout=5", "-l", "5", "-x",
            "-H", "ldaps://127.0.0.1:636", "-D", "cn=admin,dc=example,dc=org",
            "-w", "iaa-ldap-admin-secret", "-b", "ou=groups,dc=example,dc=org", "-s", "base", "dn",
        ])


if __name__ == "__main__":
    unittest.main()
