# Tour: the data loop in ten minutes

This walks one dataset through the whole data loop on your own machine: a step
reports what it produced, an assertion judges it, a bad load **holds** the
dataset, the downstream job is **skipped with a reason** instead of eating the
poison, the hold opens an **incident**, an agent proposes a release, a human
**approves** it, and the gate reopens. `caesium why` names the hold along the
way. Every command below was run against a server built from this repository.

```text
 step prints ##caesium::metrics ─▶ assertions judge it ─▶ violation: hold the dataset
                                                              │
   consumer run ◀─ skipped: dataset_hold:<name> ◀─────────────┤
                                                              ▼
   gate reopens ◀─ human approves release_hold ◀─ agent proposes ◀─ data_quality_hold incident
```

For the reference behind each piece, see
[Data Assertions & the Circuit Breaker](job-definitions.md#data-assertions--the-circuit-breaker).
You need Docker, [`just`](https://github.com/casey/just), `curl`, and
[`jq`](https://jqlang.org/), and a checkout of this repository. The circuit
breaker is newer than the `v0.1.0` release, so build the image from source.

## 1. Start a server with the breaker on

```bash
just build        # builds caesiumcloud/caesium:latest

docker run -d --name caesium-tour -p 8080:8080 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e DOCKER_HOST=unix:///var/run/docker.sock \
  -v "$PWD/docs/examples:/examples:ro" \
  -e CAESIUM_AUTH_MODE=api-key \
  -e CAESIUM_AUTH_KEY_HASH_SECRET="$(openssl rand -hex 24)" \
  -e CAESIUM_AUTH_REQUIRE_TLS=false \
  -e CAESIUM_DATA_ASSERTIONS_ENABLED=true \
  -e CAESIUM_AGENT_REMEDIATION_ENABLED=true \
  --user 0:0 caesiumcloud/caesium:latest start
```

Three settings matter here. `CAESIUM_DATA_ASSERTIONS_ENABLED=true` is the one
switch for the whole feature; without it the manifest below is refused.
`CAESIUM_AGENT_REMEDIATION_ENABLED=true` opens an incident for each hold.
Releasing a hold is a decision, so both it and the incident approvals need a
real identity: `CAESIUM_AUTH_MODE=api-key` is required (the server refuses to
start with remediation on and auth off), and `CAESIUM_AUTH_REQUIRE_TLS=false` is
for this local `http://` tour only.

On first start with auth on, the server prints a bootstrap admin key once.
Take it, and run the CLI inside the server container so nothing else needs
installing:

```bash
until curl -fsS http://localhost:8080/health >/dev/null; do sleep 1; done
export CAESIUM_API_KEY="$(docker logs caesium-tour 2>&1 | grep -o 'csk_[A-Za-z0-9_-]*' | head -1)"

caesium() { docker exec -e CAESIUM_API_KEY caesium-tour caesium "$@"; }
api() { curl -fsS -H "Authorization: Bearer $CAESIUM_API_KEY" -H 'Content-Type: application/json' "$@"; }
```

The CLI writes a JSON debug log line to stderr on every call. The outputs below
show stdout only; add `2>/dev/null` to hide it.

## 2. Declare a dataset with an assertion

[`docs/examples/data-circuit-breaker.job.yaml`](examples/data-circuit-breaker.job.yaml)
holds two jobs. `orders-load` produces `warehouse/orders`; `orders-report`
consumes it. The producer's step prints what it loaded, and the dataset carries
the checks:

```yaml
    datasets:
      produces:
        - name: warehouse/orders
          assertions:
            rowCount: {min: 1000, deltaFromBaseline: "50%"}
            nullRate: {metric: null_rate_customer_id, max: 0.02}
            freshness: {watermark: max_event_time, maxLag: 26h}
          onViolation: hold
          release: manual
```

`rowCount.min` is an absolute floor. `deltaFromBaseline` compares against the
median of recent clean runs, and only warns until five exist. `onViolation:
hold` lets a violating task succeed but holds the dataset. `release: manual`
means a clean run alone will not reopen it. Lint and apply it, and look up the
two job ids:

```bash
caesium job lint --path /examples/data-circuit-breaker.job.yaml
caesium job apply --path /examples/data-circuit-breaker.job.yaml

LOAD=$(api "http://localhost:8080/v1/jobs?alias=orders-load" | jq -r '.[0].id')
REPORT=$(api "http://localhost:8080/v1/jobs?alias=orders-report" | jq -r '.[0].id')
```

```text
Validated 2 job definition(s)
Applied 2 job definition(s)
orders-load 94fc1948-db7d-4bfb-bf37-b450377a0892
orders-report 22faa5d1-315e-4924-ae28-ffda453fceda
```

## 3. Observe and assert: two clean runs

```bash
for n in 1 2; do
  RUN=$(caesium run start --alias orders-load)
  caesium run wait --run-id "$RUN" --job-id "$LOAD"
done
caesium dataset metrics warehouse/orders --metric rowCount
```

```text
DATASET               _/warehouse/orders
METRIC                rowCount
BASELINE WINDOW       20
CLEAN SAMPLES         2
MEDIAN                10400
P10                   10400
P90                   10400
SEEDING               true
OBSERVED AT           VALUE  VIOLATED  IN BASELINE
2026-11-03T07:04:46Z  10400  false     true
2026-11-03T07:04:46Z  10400  false     true
```

Each run printed one `##caesium::metrics` line; Caesium recorded it as a sample
and built the baseline from it. `SEEDING true` means fewer than five clean
samples exist, so `deltaFromBaseline` cannot hold anything yet. The absolute
bounds already apply. Every check's outcome, passes included, is kept per run:

```bash
api "http://localhost:8080/v1/jobs/$LOAD/runs/$RUN/assertions" |
  jq -c '.tasks[0].instances[0].results[] | {metric, assertion, outcome, observed, bound}'
```

```text
{"metric":"rowCount","assertion":"min","outcome":"passed","observed":10400,"bound":1000}
{"metric":"rowCount","assertion":"deltaFromBaseline","outcome":"passed","observed":10400,"bound":5200}
{"metric":"null_rate_customer_id","assertion":"max","outcome":"passed","observed":0.001,"bound":0.02}
{"metric":"max_event_time","assertion":"maxLag","outcome":"passed","observed":1793689486,"bound":93600}
```

The report job runs normally while the dataset is healthy:

```bash
caesium run start --alias orders-report    # prints a run id
```

## 4. A bad load opens a hold

Start the producer again, simulating a truncated feed that loaded 12 rows:

```bash
BAD=$(caesium run start --alias orders-load --params rows=12)
caesium run wait --run-id "$BAD" --job-id "$LOAD"
caesium dataset holds
```

```text
succeeded
NAMESPACE  NAME              STATUS  REASON  OCCURRENCES  OPENED                HOLD ID
_          warehouse/orders  active  min     1            2026-11-03T07:04:47Z  7599c16b-b0cb-4fc7-8297-46cd34fb9d54
```

The run **succeeded**: the work is done, and failing it would only invite a
retry of the same data. The dataset is what is held. The recorded results say
why:

```bash
api "http://localhost:8080/v1/jobs/$LOAD/runs/$BAD/assertions" |
  jq -c '.tasks[0].instances[0].results[] | select(.outcome != "passed") | {metric, assertion, outcome, disposition, observed, bound}'
```

```text
{"metric":"rowCount","assertion":"min","outcome":"violated","disposition":"hold","observed":12,"bound":1000}
{"metric":"rowCount","assertion":"deltaFromBaseline","outcome":"violated","disposition":"seeding","observed":12,"bound":5200}
```

The second line is the cold start: the baseline check noticed too, but with
only two samples it is `seeding` and never holds anything. The `min` bound did.

## 5. The consumer is skipped, with a reason

```bash
caesium run start --alias orders-report
```

```text
Error: run start skipped (dataset_hold); recorded as skipped run fca1ce95-5cd8-4748-977b-84a1dcfd3d88
```

The command exits non-zero, and a skipped run was recorded at admission, in
the same transaction that would have created the run. Nothing ran on the bad
data. Ask why (take the run id from the message):

```bash
SKIPPED=fca1ce95-5cd8-4748-977b-84a1dcfd3d88
caesium why "$SKIPPED" --job-id "$REPORT" --task build-report
```

```text
SKIPPED — dataset /warehouse/orders held since run b5c0bd1f-faf5-41ca-b86e-c7bde2700d0e (assertion rowCount.min: observed 12 vs bound 1000; hold 7599c16b-b0cb-4fc7-8297-46cd34fb9d54)

TASK         build-report
VERDICT      SKIPPED
STATUS       skipped
SKIP-REASON  dataset_hold:/warehouse/orders hold=7599c16b-b0cb-4fc7-8297-46cd34fb9d54
COMPARED-TO  none

Dataset holds (skipped, 1):
DATASET            HOLD                                  HELD-SINCE-RUN                        ASSERTIONS                              DETAIL
/warehouse/orders  7599c16b-b0cb-4fc7-8297-46cd34fb9d54  b5c0bd1f-faf5-41ca-b86e-c7bde2700d0e  rowCount.min observed 12 vs bound 1000  recorded
```

The explanation names the hold, the run that opened it, the assertion, and the
observed value against its bound. `--json` returns the same facts as data. A
job that wants to run on held data anyway (a monitor, say) sets
`metadata.onUpstreamHold: run`.

## 6. The incident and its bundle

The hold also opened one `data_quality_hold` incident, carrying the violation,
the baseline, and the impact cone:

```bash
INC=$(caesium incident list --json | jq -r '.incidents[0].id')
caesium incident get "$INC" --json |
  jq '{status: .incident.status, class: .incident.class, violations: [.incident.evidence.violations[] | {metric, assertion, observed, bound}]}'
```

```text
{
  "status": "open",
  "class": "data_quality_hold",
  "violations": [{"metric": "rowCount", "assertion": "min", "observed": 12, "bound": 1000}]
}
```

(Incidents appear a moment after the hold, so repeat the first line if the list
is empty.) A green run does not close this incident; the hold's release does.

## 7. An agent proposes, a human approves

In a real deployment Caesium starts your agent container (a profile you
configure; see [Agent-in-the-Loop Remediation](job-definitions.md#agent-in-the-loop-remediation))
with a session key scoped to this one incident. The tour needs no model, so it
plays the agent by hand: mint the same kind of incident-scoped key and call the
agent routes with it.

```bash
AGENT=$(api -X POST http://localhost:8080/v1/auth/keys -d "{\"description\":\"tour stand-in agent\",\"role\":\"runner\",\"expires_in\":\"1h\",\"scope\":{\"agent\":{\"incident_id\":\"$INC\",\"jobs\":[\"orders-load\"]}}}" | jq -r '.key')
agent() { curl -fsS -H "Authorization: Bearer $AGENT" -H 'Content-Type: application/json' "$@"; }

agent "http://localhost:8080/v1/agent/incidents/$INC/bundle" | jq '.data_quality | {dataset, hold_status, occurrence_count}'
agent "http://localhost:8080/v1/agent/incidents/$INC/context/metrics?metric=rowCount" | jq -c '.series[0].samples[] | {value, violated}'
```

```text
{"dataset":"warehouse/orders","hold_status":"active","occurrence_count":1}
{"value":10400,"violated":false}
{"value":10400,"violated":false}
{"value":12,"violated":true}
```

The agent sees the hold and the metric history of its own dataset, and nothing
of other tenants'. It decides the dip is known and proposes `release_hold`.
Alone, with no tolerance window, the default policy does not let it act:

```bash
agent -X POST "http://localhost:8080/v1/agent/incidents/$INC/actions" \
  -d '{"type":"release_hold","params":{"reason":"looks fine"}}' | jq -c '{disposition, tier: .action.tier}'
```

```text
{"disposition":"rejected","tier":2}
```

A release that carries a tolerance window is a tier-3 action, which always ends
at a human:

```bash
agent -X POST "http://localhost:8080/v1/agent/incidents/$INC/actions" \
  -d '{"type":"release_hold","params":{"reason":"The vendor confirmed a short load for one day.","tolerate":{"min":"24h"}}}' | jq -c '{disposition, tier: .action.tier}'
```

```text
{"disposition":"awaiting_approval","tier":3}
```

Nothing is released yet. The incident now needs a decision, and the agent's own
key cannot make it (the approve route answers 403 to it). As the operator:

```bash
caesium incident list --needs-approval --json | jq -r '.incidents[] | [.id, .status] | @tsv'
APPROVAL=$(caesium incident get "$INC" --json | jq -r '.approvals[] | select(.decision == "pending") | .id')
caesium incident approve "$INC" --approval "$APPROVAL" --reason "Vendor confirmed the short load."
```

The approval is recorded with your key as the decider, and the release runs
through the same service as `caesium dataset release`. The tolerance window is
recorded as evidence only; Caesium does not enforce it, and a new breach inside
it holds the dataset again. Give the incident a few seconds to close:

```bash
caesium dataset holds --status all
caesium incident get "$INC" --json | jq -c '{status: .incident.status, resolution: .incident.resolution_summary}'
```

```text
NAMESPACE  NAME              STATUS    REASON  OCCURRENCES  OPENED                HOLD ID
_          warehouse/orders  released  min     1            2026-11-03T07:04:47Z  7599c16b-b0cb-4fc7-8297-46cd34fb9d54
{"status":"closed","resolution":"dataset hold released: manual_ack by csk_live_RSb7"}
```

## 8. The gate reopens

```bash
RUN3=$(caesium run start --alias orders-report)
caesium run wait --run-id "$RUN3" --job-id "$REPORT"
caesium why "$SKIPPED" --job-id "$REPORT" --task build-report | head -1
```

```text
succeeded
SKIPPED — dataset /warehouse/orders held since run b5c0bd1f-faf5-41ca-b86e-c7bde2700d0e (assertion rowCount.min: observed 12 vs bound 1000; hold 7599c16b-b0cb-4fc7-8297-46cd34fb9d54)
```

The report ran. The earlier skipped run still explains itself the same way:
release is level-triggered, so runs already skipped stay skipped and are yours
to retry.

## Other ways out

You can skip the agent. Load another bad batch and release the hold directly;
this needs the same authenticated identity, and is refused under
`CAESIUM_AUTH_MODE=none`:

```bash
BAD2=$(caesium run start --alias orders-load --params rows=7)
caesium run wait --run-id "$BAD2" --job-id "$LOAD"
caesium dataset release warehouse/orders --reason "Reviewed the vendor note."
```

```text
Released hold 613bfc02-4225-4c6f-8d61-497fa35f91a4 on dataset _/warehouse/orders
```

Or let the data heal it. The example uses `release: manual` so you can watch a
human decide. Change it to `release: auto` (the default) and the next run of
`orders-load` whose assertions all pass releases the hold with no one involved.

## Clean up

```bash
docker rm -f caesium-tour
```

## Where next

- Every field of the contract, the `##caesium::metrics` marker, and the release
  rules: [Data Assertions & the Circuit Breaker](job-definitions.md#data-assertions--the-circuit-breaker)
  and the [generated schema reference](job-schema-reference.md#data-assertions--the-circuit-breaker).
- Why a run was skipped, and what each check decided:
  [Explaining a skipped task](job-definitions.md#explaining-a-skipped-task) and
  [Reading a run's assertion results](job-definitions.md#reading-a-runs-assertion-results).
- A producer fix instead of a release: an approved `apply_jobdef_patch` can open
  a pull request for a Git-synced job, see
  [Pull requests for agent patches](job-definitions.md#pull-requests-for-agent-patches).
- The design and its limits: [design/data-circuit-breaker.md](design/data-circuit-breaker.md).
