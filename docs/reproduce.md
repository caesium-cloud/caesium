# `caesium reproduce`

`caesium reproduce` re-executes one historical task on the operator's local Docker daemon from the recorded task execution descriptor. The server only serves the descriptor; no `JobRun` or `TaskRun` is created, no events are emitted, and no server-side execution happens. The design record is [`design-reproduce.md`](design-reproduce.md).

## Quickstart

Reproduce a failed task with the recorded image, command, env, params, and predecessor outputs:

```sh
export CAESIUM_API_KEY=...
caesium reproduce "$RUN_ID" --job-id "$JOB_ID" --task transform
```

Compare reproduced `##caesium::output` markers against the recorded task output:

```sh
caesium reproduce "$RUN_ID" --job-id "$JOB_ID" --task transform --diff
```

Test a locally built candidate fix against the exact recorded inputs:

```sh
docker build -t transform:fix .
caesium reproduce "$RUN_ID" --job-id "$JOB_ID" --task transform --image transform:fix --diff
```

Open an interactive shell in the reconstructed environment:

```sh
caesium reproduce "$RUN_ID" --job-id "$JOB_ID" --task transform --shell
```

Resolve recorded `secret://` refs from local providers by explicit opt-in:

```sh
caesium reproduce "$RUN_ID" --job-id "$JOB_ID" --task transform --resolve-secrets --dry-run --json
```

## Flags

Usage:

```text
caesium reproduce <run-id> --job-id <job-id> --task <task> [flags]
```

`<run-id>` is required and must be the only positional argument.

| Flag | Default | Repeatable | Meaning |
|---|---:|:---:|---|
| `--job-id string` | none, required | no | Job ID that owns the run. |
| `--task string` | none, required | no | Task name or ID within the run. |
| `--server string` | `http://localhost:8080` | no | Caesium server base URL. |
| `--api-key string` | none | no | API key for authentication. Prefer `CAESIUM_API_KEY`; the flag is visible in process listings. |
| `--dry-run` | `false` | no | Print the reconstructed envelope as JSON without executing. |
| `--json` | `false` | no | Emit machine-readable JSON in run mode. `--dry-run` is JSON either way. |
| `--set key=value` | none | yes | Override a run param and rederive `CAESIUM_PARAM_<KEY>`. The key cannot be empty. |
| `--set-env KEY=VALUE` | none | yes | Override or add a raw container env var. The key cannot be empty. |
| `--mount old=new` | none | yes | Remap a recorded bind mount source to a local path. Both paths are required. |
| `--timeout duration` | recorded task timeout | no | Local task timeout. |
| `--platform string` | none | no | Platform to use when pulling the image, for example `linux/amd64`. |
| `--diff` | `false` | no | Compare reproduced output markers against recorded output. |
| `--shell` | `false` | no | Open an interactive `/bin/sh` shell in the reconstructed environment. |
| `--image ref` | none | no | Override the image for fix testing. Marks the output `OVERRIDDEN`. |
| `--resolve-secrets` | `false` | no | Resolve `secret://` refs via local providers. Default is omit and warn. |

Mode conflicts are enforced before descriptor fetch:

| Combination | Result |
|---|---|
| `--shell` with `--diff` | usage error, exit 2 |
| `--shell` with `--dry-run` | usage error, exit 2 |
| `--shell` with `--json` | usage error, exit 2 |
| `--diff` with `--dry-run` | usage error, exit 2 |

Environment reconstruction order is: recorded literal `ContainerSpec.Env`, recorded run params as `CAESIUM_PARAM_*`, recorded predecessor outputs as `CAESIUM_OUTPUT_*`, `--set` overrides, then `--set-env` overrides.

`--dry-run` and `--json` write machine-readable JSON to stdout. Warnings, progress, and fetch/setup messages go to stderr.

## Exit Codes

| Code | Meaning |
|---:|---|
| `0` | The local task succeeded. If `--diff` was set, reproduced output also matched recorded output. |
| `1` | The local task ran and failed. |
| `2` | Fetch, auth, usage, descriptor decode, reconstruction, image-pull, setup, or missing-descriptor error. |
| `3` | The local task succeeded, `--diff` ran, and reproduced output differed from recorded output. |

Registry pull failures include the registry host and `docker login <host>` guidance. If the selected image is already present in the local Docker daemon, reproduce uses it without pulling.

In `--shell` mode, a successful shell exits `0`. If the interactive shell process exits non-zero, reproduce propagates that shell exit code; setup and shell-unavailable errors still exit `2`.

## Fidelity Contract

The CLI emits a fidelity summary in human output and under `fidelity.dimensions[]` in JSON. Status values are `faithful`, `degraded`, `overridden`, `not_reproduced`, and `listed_not_applied`.

| JSON dimension | Status | What it means |
|---|---|---|
| `image_content` | `faithful`, `degraded`, or `overridden` | Faithful when a resolved digest was recorded and pulled. Degraded when no digest was recorded and Docker pulls a mutable tag. Overridden when `--image` is used. |
| `command_argv_workdir` | `faithful` | Recorded command, argv, and workdir are used verbatim. |
| `literal_env_vars` | `faithful` | Recorded literal env vars are restored. |
| `run_params` | `faithful` | Recorded run params are restored as `CAESIUM_PARAM_*`; `--set` intentionally overrides selected values after reconstruction. |
| `predecessor_outputs` | `faithful` or `degraded` | Scalar predecessor outputs are restored as `CAESIUM_OUTPUT_*`. Output refs carry the recorded path and digest, but the payload must exist through local storage or a remapped mount. |
| `schema_config` | `faithful` | Recorded output schema and validation mode are applied to the local run. |
| `secret_values` | `faithful`, `degraded`, or `not_reproduced` | Faithful only when no recorded secret refs exist. Degraded when refs are resolved from local providers. Not reproduced when refs are omitted by default or cannot be resolved locally. |
| `host_mounts_volumes` | `faithful` or `not_reproduced` | Faithful when no recorded host mounts or volumes exist. Bind mounts use recorded host paths unless remapped with `--mount`; Docker volumes use local volume contents; tmpfs mounts are recreated; Kubernetes-only mounts are skipped. |
| `engine_workload_identity` | `faithful` or `listed_not_applied` | Non-Docker engines and workload identity fields such as `ServiceAccountName`, pod annotations, node selector, and Kueue queue have no local Docker equivalent, so they are listed but not applied. |
| `cpu_architecture` | `faithful` or `degraded` | Faithful when no platform override is requested or the requested platform matches the local architecture. Degraded when `--platform` asks Docker to use a different architecture and emulation may be involved. |
| `resource_limits` | `not_reproduced` | Descriptor schema v1 does not record resource limits. |
| `wall_clock_time` | `not_reproduced` | The task observes the current local wall clock. |
| `external_system_state` | `not_reproduced` | Databases, APIs, object stores, and other external systems are not rewound. |
| `side_effects` | `not_reproduced` | Side effects are not suppressed; the container can affect systems reachable from this machine. |

Warnings use stable JSON codes such as `secret_omitted`, `secret_resolution_failed`, `secret_provider_mismatch`, `secret_drift`, `degraded_image_pull`, `image_overridden`, `output_ref_unresolved`, `predecessor_output_missing_name`, `mount_not_remapped`, `mount_skipped`, `retry_policy_not_applied`, `workload_identity_listed_not_applied`, `cross_arch_emulation`, `resource_limits_not_reproduced`, `wall_clock_not_reproduced`, `external_state_not_reproduced`, `side_effects_not_suppressed`, and `local_image_used`.

## Secrets

By default, every recorded `secret://` env var is omitted and named in a warning. This keeps reproduction inert against credential-gated systems unless the operator explicitly opts in.

`--resolve-secrets` builds a local secret resolver from the operator's local provider config. Values are never fetched from the Caesium server. If a local provider cannot resolve a ref, or resolves it from a different provider than the recorded ref, the env var remains omitted and a warning is emitted.

Drift checks are best-effort. Reproduce compares the recorded ref string and provider identity fields when the local provider matches, including Vault version and Kubernetes `resourceVersion`. The recorded HMAC identity is server-keyed and is not client-verifiable.

When `--resolve-secrets` is used with `--dry-run`, `--json`, or run-mode JSON, resolved secret values appear in the JSON envelope by design because the envelope is the local execution input. Protect stdout accordingly.

## Image Overrides

`--image ref` is for fix testing: it keeps the recorded env, params, predecessor outputs, mounts, timeout, and command, but runs a different image. The run is not a faithful reproduction. Human output labels the task as `OVERRIDDEN`, JSON sets `image_pull_mode: "OVERRIDDEN"` and `image_overridden: true`, and the `image_content` fidelity dimension is `overridden`. `--diff` can still compare the candidate image's output against recorded output.
