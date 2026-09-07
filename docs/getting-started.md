# Getting Started

This is the fastest path from "nothing installed" to "a job ran on a real
Caesium server and I can explain why." Every command below is copy-pasteable
and matches the [README Quick Start](../README.md#quick-start) exactly — if
you've already done a step from there, skip ahead.

## 0. Install the CLI

**Linux** — download the static binary from the [`v0.1.0` release](https://github.com/caesium-cloud/caesium/releases/tag/v0.1.0) (no shared-library dependencies, so it runs on a bare host):

```bash
curl -LO https://github.com/caesium-cloud/caesium/releases/download/v0.1.0/caesium-linux-amd64   # or caesium-linux-arm64
curl -LO https://github.com/caesium-cloud/caesium/releases/download/v0.1.0/SHA256SUMS
sha256sum --ignore-missing -c SHA256SUMS
chmod +x caesium-linux-amd64
sudo mv caesium-linux-amd64 /usr/local/bin/caesium
```

Linux only — there is no macOS or Windows binary; the toolchain builds `GOOS=linux` against CGO dqlite.

**macOS, or anywhere Docker is available** — clone the repo and run the CLI inside the release image instead:

```bash
git clone https://github.com/caesium-cloud/caesium.git
cd caesium
just tag=v0.1.0 cli   # writes ./.tmp/caesium-cli/caesium, a wrapper that runs the CLI inside caesiumcloud/caesium:v0.1.0
./.tmp/caesium-cli/caesium job lint --path jobs/
```

On macOS, address a server running on the Mac as `http://host.docker.internal:8080` rather than `localhost`.

The `caesiumcloud/caesium:v0.1.0` image used above is also pullable by
digest, if you want to pin exactly what you run:
`caesiumcloud/caesium@sha256:<filled in by the v0.1.0 release — see docs/ci.md § v0.1.0 image digests>`.

> The rest of this walkthrough calls the binary `caesium`; substitute
> `./.tmp/caesium-cli/caesium` if you're on the Docker-wrapped path.

## 1. Run the server

Run the released image directly. It needs the host's container socket
mounted so it can launch task containers, and a named volume so the
embedded dqlite database survives a restart (see `build/Dockerfile`'s
`release` stage for the image's `ENTRYPOINT` and `pkg/env/env.go` for the
env vars below):

```bash
docker run -d --name caesium-server \
  -p 8080:8080 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e DOCKER_HOST=unix:///var/run/docker.sock \
  -e CAESIUM_AUTH_MODE=none \
  -v caesium-data:/var/lib/caesium \
  --user 0:0 \
  caesiumcloud/caesium:v0.1.0 start
```

`CAESIUM_AUTH_MODE=none` is the default (fine for a local walkthrough);
`CAESIUM_DATABASE_PATH` defaults to `/var/lib/caesium/dqlite`, which is why
the volume above is mounted at `/var/lib/caesium`. `--user 0:0` matches
`just run`'s convention so the container can read `/var/run/docker.sock`.

If you've cloned the repo instead, `just run` builds and starts the same
thing from source:

```bash
just run
```

Either way, confirm it's up:

```bash
curl http://localhost:8080/health
```

## 2. Write your first job

Save this as `nightly-etl.job.yaml` — it's [`docs/examples/minimal.job.yaml`](examples/minimal.job.yaml) in the repo, three sequential steps on the pinned `alpine:3.23` image:

```yaml
apiVersion: v1
kind: Job
metadata:
  alias: nightly-etl
trigger:
  type: cron
  configuration:
    cron: "0 2 * * *"
    timezone: "UTC"
steps:
  - name: extract
    image: alpine:3.23
    command: ["sh", "-c", "echo extracting data"]
  - name: transform
    image: alpine:3.23
    command: ["sh", "-c", "echo transforming data"]
  - name: load
    image: alpine:3.23
    command: ["sh", "-c", "echo loading data"]
```

## 3. Run it locally first

Before touching the server, validate and execute the DAG against your local
Docker daemon — no server, no apply:

```bash
caesium dev --once --path nightly-etl.job.yaml
```

`caesium dev` without `--once` watches the file and re-runs on save.

## 4. Apply it to the server

```bash
caesium job apply --path nightly-etl.job.yaml --server http://localhost:8080
```

## 5. Trigger a run

Look up the job's ID (`job apply` doesn't print it back), then start a run:

```bash
JOB_ID=$(curl -s http://localhost:8080/v1/jobs | jq -r '.[] | select(.alias=="nightly-etl") | .id')
RUN_ID=$(caesium run start --job-id "$JOB_ID" --server http://localhost:8080)
echo "job $JOB_ID run $RUN_ID"
```

`nightly-etl`'s steps finish in a couple of seconds; poll until the run is
done before asking about it:

```bash
until [ "$(curl -s http://localhost:8080/v1/jobs/$JOB_ID/runs/$RUN_ID | jq -r .status)" != "running" ]; do sleep 1; done
```

## 6. Ask Caesium why

This is the payoff — Caesium remembers every run, so you can interrogate it
instead of re-reading logs:

```bash
caesium why "$RUN_ID" --task extract --job-id "$JOB_ID" --server http://localhost:8080
```

## 7. Get a reproducibility receipt

```bash
caesium receipt get --job-id "$JOB_ID" --run-id "$RUN_ID" --server http://localhost:8080
```

Commit the receipt into git next to the pipeline and `caesium verify
<receipt-file>` can later prove exactly what ran.

## Where next

- The rest of the causal-query surface (`blame`, `run diff`, `run replay`,
  `reproduce`, `contract check|graph`, `dataset status|list|advance`,
  `backfill`, `incident`) is listed in the README's
  ["Beyond scheduling"](../README.md#beyond-scheduling--what-you-can-ask-caesium)
  section, each linked to its design record.
- [job-definitions.md](job-definitions.md) — full job-authoring reference:
  linting, diffing, schema tooling, Git sync.
- [ci.md](ci.md) — the CI runbook and the release procedure this walkthrough's
  `v0.1.0` references follow.
- [README.md](../README.md) — server workflow (Podman, backfills, operator
  tools, API reference) beyond this first-run tour.
