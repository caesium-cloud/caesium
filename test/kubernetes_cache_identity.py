#!/usr/bin/env python3
"""Real CLI/API Kubernetes cache regression. Run on a Docker/kind test host.

Uses an already deployed Caesium server and its real CLI. All version images
are node-local, forcing the unresolved-identity path; the public-image control
must resolve a digest and hit. Run against both local and distributed servers.
"""
import argparse
import json
from pathlib import Path
import subprocess
import tempfile
import time
import urllib.request
import uuid


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server", default="http://127.0.0.1:8080")
    parser.add_argument("--cli", required=True)
    parser.add_argument("--cli-server", help="CLI URL when a container wrapper uses host.docker.internal")
    parser.add_argument("--fixture-root", help="Host path mounted by a container CLI wrapper")
    parser.add_argument("--cluster", default="kind")
    parser.add_argument("--mode", choices=["local", "distributed"], default="local")
    parser.add_argument("--api-key", default="integration-test-key")
    args = parser.parse_args()
    cli_server = args.cli_server or args.server
    token = uuid.uuid4().hex[:16]
    tag = f"caesium-cache-identity-{token}:mutable"
    job_ids = []
    cli = [args.cli]

    def command(*argv):
        return subprocess.check_output(argv, text=True, timeout=240).strip()

    def api(path, method="GET"):
        request = urllib.request.Request(args.server.rstrip("/") + path, method=method)
        request.add_header("Authorization", "Bearer " + args.api_key)
        with urllib.request.urlopen(request, timeout=20) as response:
            body = response.read()
            return json.loads(body) if body else None

    def apply(name, steps, cache="{pinDigests: true, digestTTL: 0s}"):
        alias = f"cache-identity-{token}-{name}"
        manifest = f'''apiVersion: v1
kind: Job
metadata:
  alias: {alias}
  cache: {cache}
trigger:
  type: cron
  configuration: {{cron: "0 0 31 2 *"}}
steps:
{steps}
'''
        path = Path(directory) / (name + ".job.yaml")
        path.write_text(manifest)
        command(*cli, "job", "apply", "--path", str(path), "--server", cli_server, "--api-key", args.api_key)
        jobs = api("/v1/jobs?limit=1000&order_by=created_at%20desc")
        job_id = next(job["id"] for job in jobs if job["alias"] == alias)
        job_ids.append(job_id)
        return job_id

    def run(job):
        run_id = command(*cli, "run", "start", "--job-id", job, "--server", cli_server, "--api-key", args.api_key)
        uuid.UUID(run_id)  # stdout must contain only the run UUID.
        deadline = time.monotonic() + 180
        while time.monotonic() < deadline:
            snapshot = api(f"/v1/jobs/{job}/runs/{run_id}")
            if snapshot["status"] in ("succeeded", "failed", "cancelled"):
                assert snapshot["status"] == "succeeded", snapshot
                return run_id, snapshot
            time.sleep(1)
        raise AssertionError(f"run {run_id} did not finish")

    def task(snapshot, name):
        catalog = api(f'/v1/jobs/{snapshot["job_id"]}/tasks')
        task_id = next(row["id"] for row in catalog if row["name"] == name)
        return next(row for row in snapshot["tasks"] if row["task_id"] == task_id)

    def why(job_id, run_id, name):
        raw = command(*cli, "why", run_id, "--job-id", job_id, "--task", name, "--json", "--server", cli_server, "--api-key", args.api_key)
        return json.loads(raw)

    def step(name, image, shell, extra=""):
        # JSON flow sequences also form valid YAML sequences.
        return f"  - name: {name}\n    engine: kubernetes\n    image: {image}\n    command: {json.dumps(['sh', '-c', shell])}\n" + extra

    def assert_executed(row):
        assert row["status"] == "succeeded" and not row["cache_hit"], row
        if args.mode == "distributed":
            assert row.get("claim_attempt", 0) > 0, f"distributed worker claim not observed: {row}"
        else:
            assert row.get("claim_attempt", 0) == 0, f"local control unexpectedly worker-claimed: {row}"

    def observation(snapshot, name):
        row = task(snapshot, name)
        return {key: row.get(key) for key in ("id", "task_id", "status", "cache_hit", "claim_attempt", "runtime_id", "resolved_image_digest", "output")}

    with tempfile.TemporaryDirectory(prefix="caesium-cache-identity-", dir=args.fixture_root) as directory:
        Path(directory, "Dockerfile").write_text("FROM alpine:3.23\nARG QA_VERSION\nRUN printf '%s' \"$QA_VERSION\" > /qa-version\n")
        try:
            command("docker", "build", "--build-arg", "QA_VERSION=v1", "-t", tag, directory)
            command("kind", "load", "docker-image", tag, "--name", args.cluster)
            image_v1 = command("docker", "image", "inspect", "--format", "{{.Id}}", tag)
            version_step = step("version", tag, 'printf \'##caesium::output {"version":"%s"}\\n\' "$(cat /qa-version)"')
            visible = apply("visible", version_step)
            _, first = run(visible)
            assert_executed(task(first, "version"))
            assert task(first, "version")["output"]["version"] == "v1", first
            # Equal-output descendants test D2 as well as direct cache lookup.
            same = 'echo \'##caesium::output {"token":"same"}\''
            graph = step("source", tag, same, "    next: [middle, values, skipped_middle, values_middle]\n")
            graph += step("middle", "alpine:3.23", same, "    dependsOn: [source]\n    cache: false\n    next: [leaf]\n")
            graph += step("leaf", "alpine:3.23", same, "    dependsOn: [middle]\n")
            graph += step("values", "alpine:3.23", same, "    dependsOn: [source]\n    cache: {pinDigests: true, digestTTL: 0s, chain: values}\n")
            graph += step("skipped_middle", "alpine:3.23", same, "    dependsOn: [source]\n    triggerRule: all_failed\n    cache: false\n    next: [skipped_leaf]\n")
            graph += step("skipped_leaf", "alpine:3.23", same, "    dependsOn: [skipped_middle]\n    triggerRule: all_done\n")
            graph += step("values_middle", "alpine:3.23", same, "    dependsOn: [source]\n    triggerRule: all_failed\n    cache: {chain: values}\n    next: [values_leaf]\n")
            graph += step("values_leaf", "alpine:3.23", same, "    dependsOn: [values_middle]\n    triggerRule: all_done\n")
            chain = apply("chain", graph)
            _, chain_first = run(chain)
            for name in ("source", "middle", "leaf", "values", "skipped_leaf", "values_leaf"):
                assert_executed(task(chain_first, name))
            for name in ("skipped_middle", "values_middle"):
                assert task(chain_first, name)["status"] == "skipped", chain_first
            command("docker", "build", "--build-arg", "QA_VERSION=v2", "-t", tag, directory)
            command("kind", "load", "docker-image", tag, "--name", args.cluster)
            image_v2 = command("docker", "image", "inspect", "--format", "{{.Id}}", tag)
            assert image_v1 != image_v2, "mutable fixture did not change image content"
            second_id, second = run(visible)
            row = task(second, "version")
            assert_executed(row)
            assert row["output"]["version"] == "v2", second
            assert not row.get("resolved_image_digest"), row
            explanation = why(visible, second_id, "version")
            assert "image identity unavailable" in explanation["summary"], explanation
            chain_id, chain_second = run(chain)
            for name in ("source", "middle", "leaf", "skipped_leaf"):
                assert_executed(task(chain_second, name))
            for name in ("skipped_middle", "values_middle"):
                assert task(chain_second, name)["status"] == "skipped", chain_second
            assert task(chain_second, "values_leaf")["status"] == "cached", chain_second
            assert "image identity unavailable" in why(chain, chain_id, "skipped_leaf")["summary"]
            leaf_why = why(chain, chain_id, "leaf")
            assert "image identity unavailable" in leaf_why["summary"], leaf_why
            assert task(chain_second, "values")["status"] == "cached", chain_second
            uncached = apply("uncached", version_step, "false")
            _, control = run(uncached)
            assert_executed(task(control, "version"))
            assert task(control, "version")["output"]["version"] == "v2", control
            stable = apply("stable", step("stable", "alpine:3.23", same))
            _, stable_first = run(stable)
            stable_id, stable_second = run(stable)
            first_row, second_row = task(stable_first, "stable"), task(stable_second, "stable")
            assert_executed(first_row)
            assert first_row.get("resolved_image_digest", "").startswith("sha256:"), first_row
            assert second_row["status"] == "cached" and second_row["cache_hit"], second_row
            assert second_row["resolved_image_digest"] == first_row["resolved_image_digest"], second_row
            assert why(stable, stable_id, "stable")["verdict"] == "CACHE_HIT"
            observations = {
                "mutable_v1": observation(first, "version"),
                "mutable_v2": observation(second, "version"),
                "transitive_source": observation(chain_second, "source"),
                "disabled_middle": observation(chain_second, "middle"),
                "transitive_leaf": observation(chain_second, "leaf"),
                "values_cached": observation(chain_second, "values"),
                "skipped_middle": observation(chain_second, "skipped_middle"),
                "skipped_transitive_leaf_first": observation(chain_first, "skipped_leaf"),
                "skipped_transitive_leaf": observation(chain_second, "skipped_leaf"),
                "skipped_values_middle": observation(chain_second, "values_middle"),
                "skipped_values_leaf_cached": observation(chain_second, "values_leaf"),
                "uncached_v2": observation(control, "version"),
                "stable_executed": observation(stable_first, "stable"),
                "stable_cached": observation(stable_second, "stable"),
            }
            print(json.dumps({"result": "passed", "mode": args.mode, "mutable_run": second_id, "chain_run": chain_id, "stable_digest": first_row["resolved_image_digest"], "image_v1": image_v1, "image_v2": image_v2, "observations": observations}))
        finally:
            # Only remove our host image; the lane disposes its own kind cluster.
            subprocess.run(["docker", "image", "rm", "-f", tag], check=False, stdout=subprocess.DEVNULL, timeout=30)
            for job_id in job_ids:
                try:
                    api("/v1/jobs/" + job_id, method="DELETE")
                except Exception as error:
                    print(f"fixture job cleanup failed: {error}", file=__import__("sys").stderr)


if __name__ == "__main__":
    main()
