# Execution connectors

> Status: In progress. This guide records the provider-neutral contract and
> configuration file frozen for review. The catalog store persists identities,
> direct snapshots, relations, the configuration epoch, and operation receipts.
> Startup reads the epoch after migration and does not compare it. Temporal
> observation is implemented and dials only when the connector gate is on.
> The adapter also implements binding-declared JSON Queries and Updates.
> There are still no connector HTTP routes or Console pages. Job-definition
> YAML is unchanged.

An execution connector lets Caesium inspect an external workflow system and,
later, submit one declared operator action. The first provider name the
configuration file accepts is `temporal`. The core registry is not a Temporal
client: an adapter registers an opaque identity and a capability set, and
execution references carry only that adapter's coordinates.

## Configuration

Connectors are off unless the process gate is set.

| Variable | Default | Meaning |
| --- | --- | --- |
| `CAESIUM_CONNECTORS_ENABLED` | `false` | When unset or `false`, the config file is ignored and existing environment defaults are unchanged. |
| `CAESIUM_CONNECTORS_CONFIG_FILE` | empty | Path of the versioned connector file. Required only when the gate is true. This file is not a job manifest. |
| `CAESIUM_CONNECTORS_CONFIG_PREVIOUS_FINGERPRINT` | empty | Expected stored fingerprint for a later coordinated rollout. It is not part of the contract fingerprint. |

Enabling the gate requires `CAESIUM_AUTH_MODE=api-key` or an enabled SSO provider
(OIDC, SAML, or LDAP). `CAESIUM_AUTH_MODE` unset or `none`, with no SSO
provider, fails closed. That check runs for every command. Only `caesium start`
reads the file, resolves env secrets, and computes the fingerprint. Other
commands, including `caesium version`, leave the file untouched. An invalid
enabled file fails with an error that does not include credential bytes. A
missing file, a permission error, and a directory keep the operating-system
cause. The file must be a regular file of at most 1 MiB. Hot reload and UI
edits are not part of this contract.

The main integration server started by `just integration-up` does not set this
gate. The auth-enabled agent lane (`just integration-up-agent`) sets the gate
and mounts the example below, so a file the parser rejects fails that lane
before `/health` answers. The multi-node fingerprint rollout is still plan
item C1 in `exec-plans/active/execution-connectors.md`.

```yaml
version: 1
connections:
  - id: primary
    provider: temporal
    endpoint: frontend.temporal.svc:7233
    scope: default
    enabled: true
    credentials:
      secretRefs:
        - secret://env/TEMPORAL_TOKEN?name=TEMPORAL_TOKEN
      certificatePaths:
        - /var/run/secrets/caesium/temporal/tls.crt
        - /var/run/secrets/caesium/temporal/tls.key
    limits:
      consoleRefresh: 10s
      readsPerMinute: 60
      queriesPerMinute: 6
      maxInFlightRPCs: 4
      rpcDeadline: 10s
      maxPageEntries: 100
      maxPageMetadataBytes: 65536
      unreferencedSnapshotMaxAge: 24h
      maxUnreferencedSnapshots: 1000
    bindings:
      - name: publication
        version: "1"
        displayName: Publication
        statusQuery: publication_status
        statusQueryResultSchema:
          type: object
          additionalProperties: false
          required: [stage]
          properties:
            stage:
              type: string
        activityAllowlist:
          - activityType: caesium.start
            jobs: [publish, notify]
        actions:
          - name: approve_publication
            inputSchema:
              type: object
              additionalProperties: false
              properties:
                note:
                  type: string
            resultSchema:
              type: object
              additionalProperties: false
              properties:
                approved:
                  type: boolean
```

`version` must be `1`. Unknown fields, duplicate connection ids, and provider
names other than `temporal` are rejected. `scope` is an opaque external scope
string (for a Temporal deployment, the namespace). The file has no workflow-id
or run-id fields.

Credential material is a `secret://` reference (`env`, `k8s` / `kubernetes`, or
`vault`, the same providers as the rest of Caesium) or an absolute mounted
certificate path. Inline tokens, PEM bodies, userinfo in a reference, and
query parameters the resolver does not read are rejected. The endpoint is
`host:port` only. A scheme, path, query, fragment, or userinfo
(`https://user:secret@host`, `host:7233?api_key=secret`, `host:7233#secret`,
or a path) is rejected, and the error does not echo the value. References use
the resolver's own path shape: `secret://k8s/<secret>/<key>` or
`secret://k8s/<namespace>/<secret>/<key>`, and
`secret://vault/<path>/<field>` or `?field=`. Fragments are rejected, because
the resolvers do not read them. A connection with `enabled: false` stays in
the fingerprint, and its env secrets are not resolved.

Omitted `limits` use the hard ceilings below. A value may be stricter. Integer
budgets are plain base-10 integers; `60.9` is rejected rather than truncated.
These are rejected:

- console refresh faster than 10 seconds
- more than 60 reads per minute per principal
- more than 6 explicit Queries per minute
- more than 4 in-flight RPCs per connection
- an RPC deadline over 10 seconds
- more than 100 entries or 64 KiB (65536 bytes) of exposed metadata per page
- unreferenced detail snapshots kept longer than 24 hours
- more than 1,000 unreferenced snapshots per connection

## Contract

Connections have an immutable identity: id, provider, endpoint, and scope.
Changing the endpoint, provider, or scope under an existing id is refused.
Allocate a new id instead.

Execution references are stateless. They are the connection id plus opaque
coordinates chosen by the adapter, and they do not insert a catalog row.

The reserved field `_caesium_actor` is a server-derived envelope. The reservation
applies to the action schema root; nested schema properties and nested user data
(including object-valued `const` and `enum`) may use that name. The root
object schema sets `additionalProperties: false`. Nested objects do not have
to. That root schema cannot declare the field, including through `required`,
`patternProperties`, or `propertyNames`. A nested `patternProperties` entry,
a property named `$id` or `$ref`, and a data value such as a description are
not declarations. The config file cannot override the field. Admission
validates the caller payload against the action schema first.
`AcceptActionInput` then stamps the envelope with `ApplyActor`. Validating
after the stamp rejects every payload, because the field is not part of the
public schema. The envelope fields are `principal_kind` (`user` or
`api_key`), `stable_id`, `subject`, `role`, `operation_id`, and
`binding_version`. API keys stay identified as keys.

Action schemas are JSON Schema. `$ref` and `$id` keywords must be fragments
inside the same document. `$schema` may be
`https://json-schema.org/draft/2020-12/schema` or a fragment. Property names,
and values under `const`, `enum`, `default`, and `examples`, are data, not
references. `file://` and other external references are refused, so a schema
change cannot hide outside the fingerprint. Schema scalars must be JSON
strings, numbers, booleans, or null. YAML timestamps and hex integers are
rejected; quote a date to keep it a string. Numbers keep the digits written
in the file, including integers past 2^53 and past 17 significant digits. An
activity type belongs to one binding on a connection.

The v1 correlation hint, carried on a recognized activity, is fixed:

| Field | Required |
| --- | --- |
| `version` | yes, `v1` |
| `job_id` | yes |
| `idempotency_key` | yes |
| `outcome` | yes |
| `queue_id` | no |
| `run_id` | no |

A binding's `activityAllowlist` maps an external activity type to local job
aliases. That allowlist is the only set of jobs a later admission lookup may
search. Bindings may also name one status query and zero or more actions.
Each action has a JSON object input schema and a JSON object result schema.

A binding's `name` is the exact Temporal workflow type it permits. Before a
Query or Update, the adapter describes the pinned workflow/run and checks that
type and the binding version. A status Query exposes only a JSON object that
validates against `statusQueryResultSchema`, whose root must set
`additionalProperties: false`. The field is optional for existing files;
omitting it makes the Query unavailable. Supplying or changing it changes the
configuration fingerprint.

Adapter capabilities are optional and advertised only when a server wires them:
discovery, inspection, history, relationships, status query, action submission,
and receipt lookup. A test adapter can register a different identity and a
different subset. Nothing in the registry requires a Temporal module.

## Fingerprint and rollout

Equivalent files produce one canonical SHA-256 fingerprint. Key order,
comments, and duration spellings that parse to the same budget do not change
it. The fingerprint covers enabled configuration, immutable connection targets,
credential references, bindings and schemas, and limits. Schema integers keep
the digits written in the file, so values above 2^53 and integers with more
than 17 significant digits stay distinct. The digest is the schema document
itself, not a copy loaded from another file.

It does not cover resolved secret bytes. Rotating a secret under the same
`secret://` reference leaves the fingerprint unchanged.
`CAESIUM_CONNECTORS_CONFIG_PREVIOUS_FINGERPRINT` is also outside the fingerprint.
It names the stored value a later rollout expects to replace.

That rollout is not enforced yet. When it is, every replica must load the same
file. A new file is activated only when the previous-fingerprint variable
matches the stored value. Nodes still running the old file refuse connector
work until they restart with the new file. There is no implicit adoption, and
a connection id cannot be repointed at a new target. Concurrent incompatible
transitions fail closed. Local job execution stays independent of that check.

Mount the same file on every Helm replica with the chart's existing
`config.extraEnv`, `extraVolumes`, and `extraVolumeMounts`. Do not add a chart
hook or a connector-specific value.

```yaml
config:
  extraEnv:
    - name: CAESIUM_CONNECTORS_ENABLED
      value: "true"
    - name: CAESIUM_AUTH_MODE
      value: api-key
    - name: CAESIUM_AUTH_KEY_HASH_SECRET
      valueFrom:
        secretKeyRef:
          name: caesium-auth
          key: key-hash-secret
    - name: TEMPORAL_TOKEN
      valueFrom:
        secretKeyRef:
          name: caesium-connectors
          key: temporal-token
    - name: CAESIUM_CONNECTORS_CONFIG_FILE
      value: /etc/caesium/connectors/connections.yaml
    - name: CAESIUM_CONNECTORS_CONFIG_PREVIOUS_FINGERPRINT
      value: ""
extraVolumes:
  - name: connectors
    configMap:
      name: caesium-connectors
extraVolumeMounts:
  - name: connectors
    mountPath: /etc/caesium/connectors
    readOnly: true
```

`caesium start` logs the loaded fingerprint as `connector config loaded`.
Nothing else stores it yet, and the previous-fingerprint check is not
enforced. Copy that logged value into
`CAESIUM_CONNECTORS_CONFIG_PREVIOUS_FINGERPRINT` before replacing the
ConfigMap, then roll the replicas together so they all see one file. Secret
rotation does not require that variable to change. API-key mode also requires
`CAESIUM_AUTH_KEY_HASH_SECRET` of at least 32 characters. The chart does not
set it. The snippet above reads it from a Secret. A pod that sets
`CAESIUM_AUTH_MODE=api-key` without that value exits on startup. The example
file resolves `secret://env/TEMPORAL_TOKEN` while the connection is enabled,
so the same snippet sets `TEMPORAL_TOKEN` from a Secret. Without that
variable every replica exits because the env var is unset.

See [kubernetes-deployment.md](kubernetes-deployment.md) for the Helm chart and
[temporal.md](temporal.md) for the current REST-only way to call Caesium from
Temporal. The plan that tracks the unshipped work is
`exec-plans/active/execution-connectors.md`.

## Storage and retention

Connector identities, direct snapshots, relation evidence, the configuration
epoch, and operation receipts are catalog metadata. The tables are
`external_executions`, `external_execution_snapshots`,
`external_execution_relations`, `connector_operations`, and
`connector_configurations`. They are registered with `models.All` and are not
hot-path tables: they are absent from hot-shard routing and from the hot
execution migrator. Nothing in this store creates a Caesium job run or task run.

An execution's primary key is the opaque digest of its connection id and
adapter coordinates. Provider coordinates stay in an opaque JSON blob. The
catalog has no workflow id, run id, or namespace column.

A direct observation ensures that identity and appends a snapshot only when
the source evidence changes. Evidence is the source event id, native status,
display status, availability, completeness, metadata, and the terminal bit.
The source kind is stored on the row and is not part of that digest, so a
direct re-read of state already recorded by discovery is an unchanged poll.
An unchanged poll does not write a snapshot and does not refresh the stored
observation time. A higher generation with that same evidence advances only
the generation watermark. List and history pages write nothing. Discovery
does not insert an identity and does not snapshot an execution that has not
already been read directly. A write is conditional on the observation
generation: a stale or conflicting generation writes nothing. Generation is
checked before terminal regression, so a late older read is stale rather than
a regression. A newer non-terminal observation cannot regress a terminal
snapshot. Discovery cannot overwrite newer direct state. A successful advance
inserts a new snapshot row and leaves the previous row in place.

Action admission stores one receipt per connection and idempotency key. The
same key with a different execution, action, binding version, external update
id, or request fingerprint is refused. The receipt stores the request
fingerprint and a bounded actor record, not secret bytes. At most one
operation for a connection, execution, and action stays open. `submitted`,
`accepted`, and `unknown` hold that guard; `unknown` is not completion.
`completed` and `rejected` are terminal, clear the guard, and are not reopened.
A later admission is a new row.

The active configuration fingerprint is one catalog row. Activation is a
compare-and-swap on the previous fingerprint. Concurrent callers that start
from the same previous value have one winner, and the loser leaves the stored
fingerprint unchanged. When the connector gate is on, startup reads this
row after migration. A missing row is left empty. A query error stops the
process. Startup does not compare the fingerprint to the loaded config.

Unreferenced historical snapshots are deleted past the configured age and
then capped at that count. An identity's latest snapshot stays, including
when it is the only row and it is older than the age. The hard ceilings are
24 hours and 1,000 historical snapshots. `RecordSnapshot`, `AdmitOperation`,
and `RecordRelation` take an optional write budget; zero fields use those
ceilings, and C1 passes the connection's configured limits. A referenced
identity — one with relation evidence or an operation — keeps its snapshots.
Identities, relations, receipts, and the epoch row are not pruned. The
catalog is not a full history mirror.

Public read proof is still plan item C1. This store has no connector HTTP
routes and no Console pages.

## Temporal observation

`internal/connector/temporal` compiles `go.temporal.io/sdk` v1.49.0.
Health, a filtered visibility list, direct describe, and bounded history are
implemented. Declared Queries and Updates are implemented separately; durable
receipts, correlation, and HTTP routes remain later plan items. With
`CAESIUM_CONNECTORS_ENABLED` left at its default
false, the process dials no Temporal server, starts no worker, and needs no
Temporal credentials.

Health calls `GetSystemInfo` and `DescribeNamespace` for the configured
namespace. In this SDK release, `CheckHealth` is a gRPC health RPC rather than
a wrapper over `GetSystemInfo`, so the adapter does not use it. A health
failure, including an unknown namespace, is an availability error. It is not a
workflow display status, and a missing worker is not a workflow failure.
Local adapter tests do not certify Temporal Cloud.

List passes the visibility query through and does not add an order. The page
size comes from the caller and is clamped to the observer's maximum. That
maximum defaults to 100 (`connector.MaxPageEntries`) and a larger value is
rejected.
The next page token is the server token when every execution in that response
fits in the exposed-metadata budget. The budget defaults to 64 KiB
(`connector.MaxPageMetadataBytes`); a larger value is rejected. Exposed JSON
is what counts, so coordinates count as well as the execution fields. When
the page does not fit, the adapter keeps a prefix, sets `incomplete`, and
omits the server token. That token is not returned, because it would skip the
dropped executions. The caller repeats the same request token with a smaller
page size. A single execution over the budget is an error. Each entry's opaque
coordinates are `workflow_id` and `run_id`. The configured namespace is
returned separately as scope. It is not a Temporal column on the core
reference.

Describe reads one execution and stamps the caller's clock as the observation
time on the returned value. That time is not stored. List and describe set
relation type `parent` from `ParentExecution` when it is present. A child
workflow is not a parent and is not delegation. Describe also projects
`PendingActivities` as id, type, attempt, maximum attempts, and the protobuf
state name. `LastFailure` and heartbeat payloads are not copied. The same
metadata budget applies. `incomplete` on a describe means the pending list was
shortened to a prefix. Describe is not paged.

History is one bounded page. `WaitNewEvent` stays false, so the call does not
follow the workflow. Workflow inputs, results, and activity payloads are not
copied. An encoded raw-history blob is not decoded; that response is an
availability error. A history response does not echo a run id. When the caller
omits one, the page describes the execution once and uses that run id.
`OriginalExecutionRunId` is kept across reset while the current run id changes,
and `ContinuedExecutionRunId` is the previous run, so neither labels this page.
The run id is resolved before the metadata budget is measured. A history page
that does not fit keeps an event prefix and sets `incomplete`, with no server
token. Activities, relations, and problems on that page come only from the
kept events. The caller repeats the same request token with a smaller page
size. One event over the budget is an error.
Activity attempts
come from `ActivityTaskStarted` and carry the scheduled event id, attempt, and
history event id. Activity id and type are filled only when `ActivityTaskScheduled`
is on the same page. The observer does not remember scheduled events across
RPCs. A closed attempt split across pages keeps `ScheduledEventID` and leaves
id and type empty. In-flight attempts are the describe pending list, not a
reconstruction across history pages. An attempt is not a workflow failure.
`WorkflowExecutionStarted` records relation type `parent` from
`ParentWorkflowExecution` and relation type `continuation` from
`ContinuedExecutionRunId` on the same workflow id.
`ChildWorkflowExecutionStarted` is relation type `child`, with the child
workflow id and run id. A non-empty `NewExecutionRunId` on
`WorkflowExecutionContinuedAsNew`, `WorkflowExecutionFailed`,
`WorkflowExecutionTimedOut`, or `WorkflowExecutionCompleted` is relation type
`continuation` to that new run id on the same workflow id. Retry and cron use
the closing event; there is no separate relation kind. An empty new run id is
not a continuation. Relation type `delegation` is not emitted.

`WorkflowTaskFailed` and `WorkflowTaskTimedOut` are a separate problem list
of event id and cause. They do not change a running describe into `failed`.

Native status is the protobuf enum name, not the SDK's shortened `String()`
(`Running`). v1.49.0's generated `String()` returns that short name, so the
adapter uses the generated name map instead. Display status is:

| Native status | Display | Terminal | Workflow failure |
| --- | --- | --- | --- |
| `WORKFLOW_EXECUTION_STATUS_RUNNING` | `running` | no | no |
| `WORKFLOW_EXECUTION_STATUS_COMPLETED` | `completed` | yes | no |
| `WORKFLOW_EXECUTION_STATUS_FAILED` | `failed` | yes | yes |
| `WORKFLOW_EXECUTION_STATUS_CANCELED` | `canceled` | yes | no |
| `WORKFLOW_EXECUTION_STATUS_TERMINATED` | `terminated` | yes | no |
| `WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW` | `continued` | yes | no |
| `WORKFLOW_EXECUTION_STATUS_TIMED_OUT` | `timed_out` | yes | no |
| unspecified, `PAUSED`, or unrecognized | `unknown` | no | no |

`CONTINUED_AS_NEW` closes this run. It is not a failure.

Transport is derived from the connection file. The endpoint is `host:port`
and the namespace is the connection scope. An API key, when set, uses
`client.NewAPIKeyStaticCredentials`. It is not logged and is stripped from
errors. Certificate paths are classified by base name: `ca.crt` or `ca.pem`
are root CAs; `tls.crt` or `client.crt` together with `tls.key` or
`client.key` are the mTLS client certificate. One of the certificate or key
without the other, or any other base name, is an error. Plaintext is only
for `localhost`, `127.0.0.1`, or `::1` when no certificate path is set. The
TLS config is then nil, and `TLSDisabled` is set so an API key does not make
the SDK turn TLS on at dial time. Any other host uses TLS, including an
in-cluster frontend. There is no plaintext opt-in. Empty certificate
paths mean the system roots, which is how a remote frontend or Temporal Cloud
API-key connection works without a new YAML field. Loopback with certificate
paths uses TLS. File-read errors are redacted when they would contain the API
key or a PEM body. The configured RPC deadline defaults to
`connector.MaxRPCDeadline` and a larger value is rejected. A caller's shorter
deadline is kept.

## Temporal messages

The Temporal adapter supports one explicitly invoked status Query and
binding-declared Update actions. It does not register connector HTTP routes,
persist a receipt, automatically query closed workflows, or resend an Update.
Those service surfaces remain plan items C and D.

In v1, `binding.name` is the exact Temporal workflow type. Every message requires
a nonempty observed Workflow ID and Run ID, connection id, binding name and
binding version. Before the message RPC, a direct describe must return those
same execution coordinates and workflow type. An unbound workflow, a different
type, a changed binding version, or a missing Run ID is refused. Continue-as-new
cannot turn an old action into an action on the latest run.

`statusQueryResultSchema` is optional in the configuration file so existing
files remain valid. Query invocation requires that schema. It is a closed JSON
object schema with the same local-reference and actor-reservation restrictions
as action schemas. Adding or changing it changes the configuration fingerprint;
omitting it preserves the existing fingerprint. `QueryStatus` sends no arguments
and invokes only `statusQuery`, then validates the result before exposing it.
There is no raw-payload fallback for a binding without this schema.

`SubmitAction` validates the caller object against `inputSchema` before adding
the server actor. A root `_caesium_actor` in caller input is refused before RPC.
The remote Update receives one default JSON argument:

```json
{
  "input": {"note": "Publication reviewed"},
  "_caesium_actor": {
    "principal_kind": "api_key",
    "stable_id": "authenticated-key-id",
    "subject": "authenticated-key-subject",
    "role": "operator",
    "operation_id": "persisted-operation-id",
    "binding_version": "1"
  }
}
```

The actor comes from the authenticated Caesium principal, with `operator` or
`admin` role. API keys stay identified as keys. The adapter checks the actor's
binding version and stable Update ID against its operation id; the composing
service must enforce authorization and persist the immutable receipt, actor,
input and target before dispatch. `StableUpdateID` derives one deterministic
SHA-256-based external id from that persisted operation id. Dispatch never
generates a new Update ID. The remote validator remains authoritative for
business eligibility and trusts the authenticated connector service to supply
the envelope; other authorized Temporal writers are outside that assertion.

Submission waits for acceptance. It makes one Update RPC and returns the
actual lifecycle stage: `submitted` for admitted, `accepted` for accepted,
and `completed` for a validated successful outcome. A completed failure returned
by submission stays `unknown` with `failure_kind: failure_origin_unknown`:
the wire response does not distinguish validator rejection from an accepted
handler that failed, possibly after effects. `rejected` requires independent
proof of validator rejection and is not inferred by this adapter. Failure
messages and details are omitted. A completed response with an invalid result
also stays `unknown`, so it cannot authorize another effect.

Local validation, binding/identity mismatch, cancellation before dispatch, or
a failed preflight describe return `not_dispatched`. The Update RPC has not
been issued. Identity mismatches retain a descriptive error and have no RPC
code. `preflight_rpc_code` records only the actual code of a failed describe;
`rpc_code` records only a failed Update or Poll RPC. A canceled context detected
before any RPC has neither code. This avoids presenting a provably unsent
action as an uncertain send.

`LookupAction` makes one nonblocking `PollWorkflowExecutionUpdate` RPC for the
same pinned run and Update ID. It does not take input or an actor, call an Update
handler, or follow a newer run. RPC failures preserve the exact gRPC status code
and leave the operation `unknown`. A completed failure returned by lookup also
stays `unknown` with `failure_kind: failure_origin_unknown`. A Poll can retain
an in-flight Update while its validator rejects it, so the failure outcome
alone does not prove acceptance. It also does not prove validator origin or
absence of effects. `NotFound` remains `unknown`, including after
a lost validator-rejection reply: a rejected-before-acceptance Update can be
absent from history. Absence proves neither rejection nor lack of effect and
does not authorize resubmission. Explicit original-principal confirmation and
durable guard handling remain D2/C2.

These are adapter evidence states, not durable receipt states. D2 must explicitly
map them into receipt and open-guard policy without treating an ambiguous or
returned failure as a no-effect rejection. On lookup, `not_dispatched`
means that call issued no Poll; it says nothing about the original Update and
cannot release an existing receipt's guard or authorize resubmission.

Only a single `json/plain` result payload is decoded. Custom or encrypted
payloads remain unavailable. Input, stamped envelope, and exposed result each
fit the configured payload ceiling, at most 64 KiB. Result objects are checked
against their declared schema; known API-key bytes and PEM bodies are redacted,
then the redacted object is validated again. Arbitrary history/input/failure
payloads are not exposed. One bounded context covers describe and message RPCs,
and a shorter caller deadline is preserved. A timeout is not completion or
rejection.

Sensitive object keys cause the entire result to be withheld. Known API-key
bytes are scrubbed even for short credentials; a result is withheld if those
bytes remain in its encoded JSON, including numeric or boolean values. PEM
blocks are scrubbed before credential replacement so overlapping text cannot
break their delimiters and expose their contents.

`CheckWorker` is an explicit diagnostic. It reports `recent_pollers`,
`no_recent_pollers`, or `unknown` from the workflow task queue's observed poller
evidence. Recent polling does not prove present worker liveness, and its absence
does not prove that a worker cannot arrive. Query, submission and receipt lookup
do not invoke this diagnostic or infer a workflow failure from it. Server-side
rate, concurrency, authentication and configuration-epoch policy remain with
the composing service.

## Real Temporal development fixture

`just connectors-fixture-check` builds the fixture and integration runner from
this checkout and drives real Temporal and authenticated Caesium APIs. It builds
the tracked `integration-up-agent` prerequisites first, then uses that recipe's
shared environment in create-only mode. Container creation reserves the name
atomically; it never removes a same-named server. The immediate source-matched
prerequisite images are reused for that create-only call. The controller must
serialize this recipe with other Docker/browser lanes. No application rows are
seeded. Docker, Python 3, the repository builder and an unused tracked
`caesium-server-agent-test` name are required.

The fixture pins Go SDK **1.49.0**, Temporal CLI **1.9.1**, and its bundled
Temporal Server **1.32.0**. Linux amd64 and arm64 CLI archives are verified
against the checksums from the
[official release](https://github.com/temporalio/cli/releases/tag/v1.9.1), and the
runtime pins the Alpine 3.23 multi-platform image digest. At startup, the worker
observes its SDK build identity, invokes `temporal --version`, and reads the
server version through `GetSystemInfo`. A mismatch prevents readiness. Health
and the startup journal report those checked observations, including when only
the B2 adapter scenario is selected. This local matrix does not qualify Temporal
Cloud or other server versions.

```sh
just tag=connector-local connectors-fixture-check
```

For a controller-owned existing tracked server, pass its exact name without
transferring cleanup ownership. Only this mode uses an exported
`CAESIUM_AUTH_ADMIN_KEY` or `CAESIUM_API_KEY`; an owned server always supplies its
own fresh bootstrap credential.

```sh
CAESIUM_CONNECTORS_EXISTING_SERVER=true \
CAESIUM_CONNECTORS_SERVER_CONTAINER=caesium-server-agent-test \
just tag=connector-local connectors-fixture-check
```

The script records immutable container IDs as creation succeeds and cleans only
those IDs on ordinary exit, HUP, INT or TERM, including under dash. A borrowed
server name is resolved once and never grants cleanup ownership. The fixture and
runner use the host UID/GID, so Linux hosts can read and remove their evidence;
private TLS keys retain restrictive permissions. The fixture runs an on-disk
development server, a real SDK worker, an mTLS gRPC proxy, and a separate
append-only start/effect journal. Every effect execution is recorded with its
Activity ID and attempt; the evidence sink does not suppress duplicates.

Evidence remains under `.tmp/connectors-fixture/`, or the absolute directory in
`CAESIUM_CONNECTORS_ARTIFACTS`. A supplied directory must be empty. Artifacts
include the candidate SHA, working-tree status, the diff against HEAD (including
staged changes), hashes for all tracked and untracked source paths, observed
versions, fixture/runner image identities, and the actual server container and
image in both ownership modes. Server metadata and retained logs redact
credentials. A dirty development run is iteration evidence, not final candidate
qualification. The `serve` entrypoint expects the controller-created artifact
directory; it is not a standalone host launcher.

The fixture control API is test infrastructure at port 7240, never a Caesium
product endpoint. The trusted service-producer boundary for actor context is
unchanged: another authorized Temporal writer is outside the claim.

The selected test fails if the fixture or worker is absent. It verifies:

- External-only inspection and a publication workflow using the actual
  `POST /v1/jobs/:id/run` API. Activity retries send the same
  `<RunID>/<ActivityID>` key and return the frozen `v1` correlation envelope.
- Explicit status Query, remote validator rejection, recorded actor context,
  server-derived operator principal from real key creation and `/auth/whoami`,
  one raw external effect execution across distinct Update IDs/operators and
  worker restart, and a second actual Caesium job admission.
- Worker stop/restart, child Run ID, continuation without retargeting an old
  action, connector-path interruption/recovery, and activity results with an
  unsupported encoding that retain metadata without revealing their payload.
- Real accepted and rejected Update replies dropped after the server responds.
  On the pinned server, a completed accepted Update looks up as
  `COMPLETED/OK`; a validator rejection before acceptance looks up as
  `NOT_FOUND`, including after eligibility changes. F1 records this raw Poll
  behavior. B2's separate adapter scenario qualifies receipt lookup without
  resubmission. A deliberate explicit submission of the same ID demonstrates
  the remote behavior needed for confirmation.
- Actual mTLS option handling: the configured CA/client pair succeeds, and a
  missing client certificate fails against the same endpoint.

`test/connectors/testdata/connections.yaml` declares the `publication` workflow
binding and bounded Query schema. It requires merged B2 Query schema support.
`just tagged-unit-test` requires the hermetic fixture config/state-schema,
observed-version and raw-effect journal regressions to pass by name; it fails on
an unsupported config, a missing test or a skip. The publication state is
serialized and validated against the committed schema, without a live server.
The orchestration probes run without Docker:

```sh
python3 -m unittest scripts/test_connectors_fixture.py scripts/test_integration_hermetic_tests.py
```

Mount the artifact directory at `/artifacts` for the configuration's certificate
paths. Its two job aliases are examples; substitute actual lane job aliases
before later correlation-route qualification. F1's self-check uses real SDK
calls because connector REST routes land in C1/C2. The authenticated HTTP/browser
CI lane and receipt/restart qualification remain later plan items, and the
existing default integration lane stays off by default.
