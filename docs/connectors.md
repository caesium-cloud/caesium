# Execution connectors

> Status: In progress. This guide records the provider-neutral contract and
> configuration file frozen for review. The catalog store persists identities,
> direct snapshots, relations, the configuration epoch, and operation receipts.
> Startup joins or activates the configuration epoch after migration. Temporal
> clients connect on demand after checking that epoch.
> The adapter implements binding-declared JSON Queries and Updates, trusted
> activity correlation against existing Caesium admissions, and durable receipt
> admission with lookup-only component recovery. Authenticated HTTP monitoring
> exposes discovery, detail, history, relationships and verified local run links.
> Declared HTTP Queries/actions and operation receipts compose those services.
> Console monitoring exposes configured connections and exact external runs,
> typed workflow relationships and verified links to existing Caesium runs.
> The Console approval journey remains plan item E3.
> Job-definition YAML is unchanged.

An execution connector lets Caesium inspect an external workflow system and
submit one declared operator action. The first provider name the
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
before `/health` answers. Provider outages and configuration skew do not fail
local job readiness; connector readiness is reported separately.

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

The optional connection field `uiURL` names the browser console base URL, for
example `https://temporal.example.test/console`. Only absolute HTTP(S) URLs
without credentials, query parameters or fragments are accepted. The value is
returned as `ui_url`; it never changes the connector RPC destination. Omit it
when the provider console is unavailable. The connection response also exposes
`console_refresh_ms` from the configured `consoleRefresh` interval (at least
10 seconds). A configured browser URL changes the configuration fingerprint;
omitting this field preserves existing fingerprints and receipt compatibility.

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

Every replica must load the same file. Initial startup establishes the active
fingerprint and durable connection identities. A matching replica joins without
changing that record. A new file is activated only when the previous-fingerprint
variable matches the stored value. Nodes still running the old file refuse connector
work until they restart with the new file. There is no implicit adoption, and
a connection id cannot be repointed at a new target, including after removal
and readdition. Concurrent incompatible transitions fail closed. Every connector
request and provider RPC checks the active epoch; observation and relationship
writes check it transactionally. Local job execution stays independent of that
check. A mismatched node returns `503` on connector reads and reports
`connectors_ready: false` from `GET /v1/system/features`.

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
The catalog stores the active fingerprint and enforces the previous-fingerprint
check during coordinated changes. Copy that logged value into
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
`external_execution_relations`, `external_execution_correlations`,
`connector_operations`, and
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
same key with a different execution, action, binding version or logical request
fingerprint is refused. Durable retries retain the winning receipt's original
UUID and external Update ID; a losing candidate's freshly generated IDs cannot
replace them or prevent attachment. The receipt stores the request fingerprint
and a bounded actor record, not secret bytes. At most one
operation for a connection, execution, and action stays open. `submitted`,
`accepted`, and `unknown` hold that guard; `unknown` is not completion.
`completed`, `rejected`, and a definitive initial `not_dispatched` preflight refusal are
terminal, clear the guard, and are not reopened.
A later admission is a new row.

The active configuration fingerprint is one catalog row. Activation is a
compare-and-swap on the previous fingerprint. Concurrent callers that start
from the same previous value have one winner, and the loser leaves the stored
fingerprint unchanged. When the connector gate is on, startup activates or joins
this row after migration, retaining its non-secret connection identity history.
An epoch or identity mismatch leaves connector reads unavailable without stopping
the scheduler. Provider connections remain lazy, so an unavailable Temporal
frontend does not prevent startup.

Unreferenced historical snapshots are deleted past the configured age and
then capped at that count. An identity's latest snapshot stays, including
when it is the only row and it is older than the age. The hard ceilings are
24 hours and 1,000 historical snapshots. `RecordSnapshot`, `AdmitOperation`,
and `RecordRelation` take an optional write budget; zero fields use those
ceilings, and C1 passes the connection's configured limits. A referenced
identity — one with relation evidence or an operation — keeps its snapshots.
Identities, relations, receipts, and the epoch row are not pruned. The
catalog is not a full history mirror.

The monitoring surface applies the configured age and total count to all
unreferenced detail snapshots, including the latest snapshot. It retains identity
watermarks after snapshot expiry. A live unchanged poll returns fresh observation
time without recreating an expired snapshot or changing stored evidence.
Explicitly referenced relationship/operation evidence remains retained.

## Authenticated HTTP monitoring

Set the feature gate and mount the same configuration on every server. Use a
Caesium API key or SSO session with an unscoped cluster role of viewer or higher.
Namespace-only grants, job-scoped keys and agent-session credentials cannot read
the global connector surfaces. A connection's external namespace is not a
Caesium namespace grant.

| Request | Response |
| --- | --- |
| `GET /v1/connectors` | Configured connections, wired read capabilities, readiness and the active process fingerprint. |
| `GET /v1/connectors/:connection_id/executions` | One provider's execution page, native metadata, opaque execution IDs and an optional cursor. |
| `GET /v1/connectors/:connection_id/executions/:execution_id` | A direct observation, or explicitly dated stale detail when refresh fails. |
| `GET /v1/connectors/:connection_id/executions/:execution_id/history` | Bounded metadata-only history and an optional cursor. |
| `GET /v1/connectors/:connection_id/executions/:execution_id/relationships` | Typed parent/child/continuation relationships and verified Caesium admission links. |
| `GET /v1/jobs/:id/runs/:run_id/external-executions` | Verified reverse links for an existing local run. |

List requests accept `workflow_type`, `page_size` and `cursor`. History and
relationship requests accept `page_size` and `cursor`. Follow returned cursors
with the same connection, filter and page size; configuration changes invalidate
them. Use the returned opaque execution ID in subsequent URLs. Temporal Workflow
ID and Run ID remain in the provider metadata; every direct target pins both.
Workflow-type restrictions apply to discovery and direct reads.

List and history pages create no catalog executions or snapshots. Direct detail
persists evidence only when it advances; polling unchanged evidence does not
update snapshot time or generation. `observed_at` describes the returned read;
`snapshot_observed_at` dates durable evidence. On an outage, cached detail has
`availability: stale` and retains its original observation time. An uncached or
expired execution returns `503`; no successful workflow outcome is inferred.
Metadata limits cover the whole public JSON response. Partial pages explicitly
report incompleteness rather than silently claiming complete history.

Relationship reads compose trusted Temporal evidence with the declared activity
and local-job allowlist. They can persist verified links to existing admissions;
they never create local runs. Follow a verified local job/run into its existing
logs, receipts and data APIs. Correlation rows include a routable `execution_id`
and public `reference` when their stored evidence matches the catalog identity.
The existing `opaque_id` is a catalog digest, not a route parameter. Historical
rows with unavailable identity evidence omit navigation fields rather than
inventing a target. An authorized scoped caller of the reverse-link route
receives an empty external list; the job/run ownership check still applies.
Verified reverse links remain available after their connection is disabled or
removed, without contacting the provider. The process read budget and each
participating enabled connection's read budget still apply. Response ceilings
use the strictest participating configured limits, including disabled
connections; removed connections use the global ceilings.

Identical concurrent reads share in-flight work. Read budgets, RPC deadlines and
per-connection concurrency limits apply before provider calls; exhaustion returns
`429`. Capabilities advertise the read and declared-message methods wired into this
server. A binding must still declare a supported Query or action for the exact
workflow type and version; a capability does not authorize arbitrary handlers.

The selected C1 fixture extends `just connectors-fixture-check` with real HTTP
reads, faults and process restart. Its owned server enables the existing
admin-only, read-only database diagnostic to observe catalog write and retention
bounds. Ordinary auth-lane startup retains its defaults. The shared-catalog proof uses
`bash scripts/connectors-config-cluster.sh` against a clean candidate. It creates
an isolated kind cluster, the existing distributed Helm topology, a real TLS
Temporal fixture and three Caesium members with retained PVCs. Its selected
`TestConnectorsConfigCluster` exercises matching files, mismatch refusal before
RPC, a guarded epoch transition, stale-node refusal and connection repointing.
Run it serially with other container/cluster lanes. Candidate, image, provider,
topology and fault evidence stays in the reported artifact directory. The
authenticated HTTP/browser CI lane remains F2; this command does not promote it.

The existing G2 coverage cohort uses the same pinned Temporal fixture for a
real publication, authenticated monitoring, verified delegation and reverse
lookup. It requires provider RPC evidence, a succeeded local run and complete
instrumented counters from a cleanly stopped process. It first proves monitoring
issues no Query or Update, then explicitly queries status and submits an
authenticated approval. Receipt recovery, the workflow-visible principal, one
independent approval effect, the succeeded second local job and durable dispatch
audit are required before accepting its profiles. Read and action RPC phases
are validated separately. Full fault/restart and
browser scenario selection remains F2.

## Declared HTTP messages and receipts

Connector reads and the explicit status Query require an unscoped cluster
viewer or higher. Submissions and resubmissions require cluster operator or
admin. Runner credentials can read but cannot submit an action. Namespace-only,
job-scoped and agent-session credentials cannot use these routes. The ordinary
session CSRF checks apply to POSTs.

| Request | Purpose |
| --- | --- |
| `POST /v1/connectors/:connection_id/executions/:execution_id/queries/:query_id` | Invoke the one binding-declared status Query explicitly. |
| `POST /v1/connectors/:connection_id/executions/:execution_id/actions/:action_id` | Admit a pinned action and submit or attach to its durable receipt. |
| `GET /v1/connectors/:connection_id/executions/:execution_id/operations` | Recover stored receipts after refresh or from another tab. |
| `GET /v1/connectors/:connection_id/operations/:operation_id` | Read a receipt and reconcile an uncertain outcome by lookup only. |
| `POST /v1/connectors/:connection_id/operations/:operation_id/resubmit` | Explicitly confirm a resend of an unknown receipt by its original principal. |

Use the opaque execution ID returned by discovery. Its provider reference pins
both Workflow ID and Run ID. A Query body names `binding_name` and
`binding_version`; it accepts no arbitrary handler or workflow input. An action
body additionally contains an `input` JSON object that must match the declared
schema. For the fixture binding, the submission shape is:

```json
{"binding_name":"publication","binding_version":"1","input":{"note":"Reviewed"}}
```

Send exactly one nonempty `Idempotency-Key` header with an action. The server
persists the authenticated actor, pinned target, binding snapshot and stable
external Update ID before dispatch. Public JSON cannot supply `_caesium_actor`
or replace any actor field. The workflow receives the server-derived principal;
an API key remains identified as that key.

The response contains `operation`, `created`, `conflict` and `pending`. A new
synchronous receipt returns `201`, an attached or read receipt returns `200`,
and an unsettled submission returns `202`. An open target/action conflict returns
`409` with the existing receipt. Changed reuse of an idempotency key or stale
binding/revision also returns `409`; invalid input returns `400`. Budget limits
return `429`, and unavailable or mismatched configuration returns `503`.
The receipt distinguishes submitted, accepted, completed, rejected,
not-dispatched and unknown outcomes. A timeout or missing lookup never implies
success, rejection or absence of effects.

New actions can use a valid stateless execution reference from discovery. Before
admitting a fresh key, the server describes that exact run and type and persists
a bounded direct observation. It refuses unavailable or stale evidence. An
existing-key replay continues to recover its durable receipt.

Operation lists accept `page_size` and `cursor`; follow the returned cursor for
the same execution. They read the catalog without issuing Updates. Receipt
lookup can query the original Update handle to strengthen evidence, but cannot
submit it. Public receipts omit request input, idempotency keys, credential
references and internal snapshots. Results remain schema-bound and may be
explicitly omitted to fit the response budget without changing a validated
completion. If descriptive metadata still exceeds the response budget,
`metadata_omitted: true` returns a compact receipt with the immutable operation,
execution target, action, binding, revision, state, Update ID and original
principal kind/stable ID. The complete evidence remains durably stored. Capacity
for this response and paginated recovery is checked before a fresh dispatch or
resend; a budget that cannot fit it is refused before provider work.

An unknown receipt can be resubmitted only by its original principal using the
currently displayed revision:

```json
{"confirmed":true,"revision":5}
```

Refresh the receipt before confirming. The server rechecks current authorization,
configuration and binding compatibility, then resends the immutable original
Update ID to its pinned run. A different principal, stale revision, changed
binding or terminal receipt cannot authorize that resend. Reload, restart and
background lookup never make that decision. A definitive initial preflight
refusal is `not_dispatched`; a refusal while resolving an earlier uncertain
send retains that uncertainty. Redacted audit records retain the actor, target,
operation and outcome without copying action input or arbitrary remote errors.

Queries are explicit, use the configured per-principal Query budget (at most
six per minute), and share bounded RPC concurrency/deadlines with reads and
actions. Closed workflow polling never invokes the Query automatically.

## Console monitoring

With connectors enabled and an unscoped cluster viewer or higher, choose
**Connections** in the sidebar. Select a configured connection and optionally
filter by exact workflow type. Discovery is eventually consistent; pagination
belongs to that one connection and filter.

Follow a Workflow ID to its immutable Run ID, native status, dated provider
observation, reported activity attempts and bounded history pages. History
payloads are unavailable. Requested history page size is a provider hint: native
batches can contain more or fewer events. Partial pages identify unavailable
events and omit a cursor that would skip them. Unbound workflows remain
inspectable without invented business controls.

A provider outage keeps dated cached details visibly stale while automatic
refresh retries at the configured `consoleRefresh` interval, at least 10
seconds. Failed history reads retain an explicit unavailable state. Recovery
observes the same pinned run. For intervals beyond the browser timer range,
automatic refresh is disabled and the view says so. A configured `uiURL` provides
a Temporal UI link containing the exact namespace, Workflow ID and Run ID.
Omitting it leaves that link absent.

Gate-off, scoped, namespace-only, agent and unauthenticated routes disclose no
external workflow details. The monitoring view performs no automatic status
Queries or actions. The approval form remains E3.
The dedicated `connectors` Playwright project uses the real development fixture;
default and auth projects exclude its specifications. A selected connector
browser test fails if its gate, authentication or real-worker markers are absent.

### Follow workflow and run evidence

On an external execution, **External relationships** separates parent, child
and continuation links from **Verified Caesium delegations**. Each external link
pins its own Workflow ID and Run ID. A continuation is another execution; it
never changes the target of the current page. Relationship pages have their own
pagination and explicitly identify partial or unavailable evidence.

A verified admission records the recognized activity, local job, admission
outcome and any queue/run IDs. While an admission is queued, it has no run to
open. After promotion, the same admission and queue identity gain a link to the
admitted run. Refreshing relationships reads existing admission state and never
starts or retries a local job.

Open that run to use its existing task logs, reproducibility receipt and output
or assertion evidence. **External execution links** on the run page returns to the
pinned workflow. These links verify the declared activity/admission protocol;
they do not prove that an arbitrary external workflow produced a dataset.
Dataset declarations describe intended associations. Freshness, materialization
and reproducibility require the local run's actual evidence. An opaque or
unbound workflow receives no invented lineage or freshness.

Run pages retain their existing permissions. A job-scoped or namespace-only
principal can inspect permitted local evidence without revealing external
workflow identities. Only an unscoped cluster viewer or higher sees the
external-execution panel. A provider outage leaves relationship information
explicitly unavailable or stale; historical verified reverse links remain
readable without contacting the provider.

## Temporal observation

`internal/connector/temporal` compiles `go.temporal.io/sdk` v1.49.0.
Health, a filtered visibility list, direct describe, and bounded history are
implemented. Declared Queries, Updates, trusted activity correlation, and durable
receipt admission with lookup-only recovery are implemented as separate
components. HTTP monitoring composes the read adapter and correlation store;
Declared HTTP messages and receipts compose the durable operation service;
Console surfaces are Stream E. With
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

History is one bounded page. Requested page size is a provider hint: Temporal
returns native event batches, so the displayed event count can differ. The
configured event and byte ceilings still apply; truncation marks the page
incomplete and omits the native cursor to avoid skipping discarded events.
`WaitNewEvent` stays false, so the call does not
follow the workflow. Workflow inputs, results, and activity payloads are not
copied. An encoded raw-history blob is not decoded; that response is an
availability error. A history response does not echo a run id. When the caller
omits one, the page describes the execution once and uses that run id.
`OriginalExecutionRunId` is kept across reset while the current run id changes,
and `ContinuedExecutionRunId` is the previous run, so neither labels this page.
The run id is resolved before the metadata budget is measured. A history page
that does not fit keeps an event prefix and sets `incomplete`, with no server
token. Activities, relations, and problems on that page come only from the
kept events. A smaller requested size may reduce the native batches returned, but a single
batch can still exceed the configured ceiling. One event over the byte budget
is an error.
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

## Activity correlation

A trusted adapter read derives the admission key from the enclosing Temporal
Run ID and Activity ID. The read-only lookup searches every local job alias
in the binding's activity allowlist. Exactly one consistent admission can
become a verified `delegation` link; a hint never narrows that search or
supplies the source identity. Optional frozen `v1` heartbeat/result hints
must match every reported value. Unrelated pending activities cannot hide a
recognized activity's heartbeat or consume its evidence budget. A completion
without a decoded hint preserves a reported pending hint; conflicting hints
refuse the mixed read. Current Describe attempt and history evidence must agree:
an older start cannot lower the attempt or advance its source event. A page
containing an older start without the current start withholds that activity as
incomplete; schedule-only evidence remains usable. Custom
payloads can use the metadata-only path without decoding inputs or reconstructing
the start request hash.

`external_execution_correlations` stores the external/activity/admission
identity, local job/queue/run references and bounded source evidence. Queue
promotion updates the same link and keeps its admission and queue identity.
Unchanged evidence writes nothing; old evidence and mismatched hints preserve
newer verified state. No lookup starts or retries local work. These records
verify the declared admission protocol, not arbitrary data provenance.

See [the activity envelope recipe](temporal.md#optional-verified-activity-correlation)
for the frozen payload. The real fixture qualifies provider identity, queued
promotion, retry and actual public job starts. Component tests separately
exercise typed persistence and database reopen. `TestConnectorsRead` drives the
public relationship and reverse-link routes against the running server,
including queued promotion, actual process restarts and historical reverse
links after guarded connection disable/removal.

## Temporal messages

The Temporal adapter supports one explicitly invoked status Query and
binding-declared Update actions. It does not register connector HTTP routes,
persist a receipt, automatically query closed workflows, or resend an Update.
The durable operation component below composes it; public service surfaces remain C2.

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
does not authorize resubmission. The durable component below implements explicit original-principal confirmation
and guard handling; their public HTTP surface remains C2.

These are adapter evidence states, not durable receipt states. The operation
component maps them into receipt and open-guard policy without treating an ambiguous or
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

## Durable action receipts and recovery

`internal/connector/operations` composes the catalog and a neutral
`OperationProvider`. The Temporal bridge delegates one send or one lookup to the
bounded message adapter. No connector action or receipt HTTP routes are added by
this component; those public service surfaces remain C2.

Before any action send, admission stores a generated receipt UUID, deterministic
external Update ID, the exact observed execution coordinates, binding name and
version, the complete binding/action schema snapshot, authenticated actor,
validated immutable input, server-derived connection identity, request
fingerprint and admission configuration epoch. The identity pins the connection
ID, provider, endpoint and scope; callers cannot supply a replacement destination.
The input is private retry material. It is never silently redacted or changed:
known credential bytes and PEM material cause admission to fail instead. The
combined dispatch snapshot fits the configured metadata ceiling. JSON-escaped
credential strings and keys are checked after decoding, as well as raw bytes.

One transactional unique index joins identical connection/idempotency-key
requests to the original receipt. The logical fingerprint includes the principal
and pinned connection identity; it excludes the newly proposed receipt UUID and
global configuration epoch. Concurrent losers and compatible retries after an
unrelated configuration change attach to the winner's immutable actor, Update ID
and admission epoch. Changed input, target, binding or principal under that key
is refused. A separate unique open-operation guard returns the
existing receipt for another key/operator targeting the same execution/action.
Terminal completion and independently proved validator rejection release that
guard. A definitive initial preflight refusal (`NotFound`, `InvalidArgument`,
`FailedPrecondition`, `PermissionDenied`) has a separate terminal `not_dispatched`
receipt; it is never described as business rejection. Transient or unclassified
initial no-send evidence keeps the receipt submitted and its guard open. A later
same-key explicit submission can retry only after a transactional current-epoch
revision claim consumes durable proof that the initial attempt never sent an
Update. That proof defaults false; a crash or failed evidence write cannot grant
it. Generic nonterminal same-key replay performs lookup only; an initial
admission whose dispatch is still unclaimed reports an explicit pending error
rather than claiming success. Replays of terminal receipts never send again.

A receipt starts `submitted` before dispatch. A process crash at that boundary is
uncertain. `Reconcile` looks up only the persisted run and Update ID, with no input
or actor available to the lookup adapter. Background and restart code must use
this method and must never call Submit or ConfirmResend implicitly. Lookup
preflight refusal, `NotFound`, failed outcomes and timeouts remain `unknown` and
keep the guard open. Once acceptance is recorded, weaker or failed lookup
observations retain both the accepted state and its original evidence and cannot
make it eligible for resend. `rejected` requires independent validator-origin
proof; Temporal's failure outcome alone supplies no such proof.

Every request and provider call still requires the service's loaded configuration
to match the active epoch. A compatible new service may recover an older receipt
only while the pinned connection identity and canonical binding remain unchanged.
Removing or repointing the connection, or changing that binding, refuses recovery
before an RPC. Adding an unrelated connection does not strand pending receipts.
The original epoch remains receipt provenance, separate from current-node fencing.

An unknown receipt can be sent again only through `ConfirmResend` with explicit
confirmation, the receipt revision actually confirmed, and a fresh authenticated
principal with the original kind/stable ID and current unscoped operator/admin
role. The service rechecks active configuration and frozen binding and atomically
claims that revision against its current epoch while appending a bounded decision
audit containing that epoch. Delayed copies of
the same confirmation cannot send again after a newer attempt becomes unknown.
The original input, actor, target and Update ID remain unchanged, including after
role/subject changes. At most sixteen decision records are retained; additional
confirmations are refused rather than deleting audit history. A resend preflight
refusal remains unknown because it says nothing about an earlier send.

Provider observations use receipt revision/attempt compare-and-swap. A delayed
lookup or older send cannot overwrite terminal state, a newer attempt's evidence,
or its guard. Provider results are validated against the frozen result schema and
withheld when credential/PEM bytes remain; arbitrary remote errors and failure
messages are never persisted. The complete serialized evidence envelope fits the
metadata ceiling. A validated completion remains terminal and clears the guard
when its result must be omitted to fit that envelope.

`TestConnectorsOperations` uses real authenticated key creation and `/auth/whoami`,
a real mTLS Temporal worker/server and direct observations ingested through the
catalog store. It closes/reopens a separate on-disk component catalog and
reconstructs the adapter and operation service. Raw sends and raw effect counts
prove lost accepted-reply recovery, crash after admission, lost validator reply
followed by eligibility without automatic approval, original-principal
confirmation and one logical effect across terminal/new Update IDs. Its artifact
is `operations-component-evidence.json`. This is component recovery evidence;
public operation receipts and operation recovery through a live Caesium process
restart remain C2. C1's separate HTTP read scenario qualifies snapshot and
verified-link persistence across process restart.

## Real Temporal development fixture

`just connectors-fixture-check` builds the fixture and integration runner from
this checkout and drives real Temporal and authenticated Caesium APIs. It builds
the tracked `integration-up-agent` prerequisites first, then uses that recipe's
shared environment in create-only mode. Container creation reserves the name
atomically; it never removes a same-named server. The immediate source-matched
prerequisite images are reused for that create-only call. The controller must
serialize this recipe with other Docker/browser lanes. No application rows are
seeded. Rootful Linux Docker without user-namespace remapping, Python 3, the
repository builder and an unused tracked `caesium-server-agent-test` name are
required.

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

The selected HTTP scenario requires a server created by this invocation because
it restarts the actual process. It rejects existing-server mode before builds
or container creation. The owned server supplies its own fresh bootstrap
credential; exported host API keys do not replace it.

The script records immutable container IDs as creation succeeds and cleans only
those IDs on ordinary exit, HUP, INT or TERM, including under dash. The fixture and
runner use the host UID/GID, so Linux hosts can read and remove their evidence;
`check` refuses rootless Docker, Podman and unknown engines before builds or
container creation. Docker Desktop with its rootful Linux engine is supported;
private TLS keys retain restrictive permissions. The fixture runs an on-disk
development server, a real SDK worker, an mTLS gRPC proxy, and a separate
append-only start/effect journal. Every effect execution is recorded with its
Activity ID and attempt; the evidence sink does not suppress duplicates.

Evidence remains under `.tmp/connectors-fixture/`, or the absolute directory in
`CAESIUM_CONNECTORS_ARTIFACTS`. A supplied directory must be empty. Artifacts
include the candidate SHA, working-tree status, the diff against HEAD (including
staged changes), hashes for all tracked and untracked source paths, observed
versions, fixture/runner image identities, and the actual server container and
image. Container and image inspection records use an
allowlist of identity, state, platform and network fields. Environment names
are retained without values; commands, arguments, labels, health logs and error
text are omitted. Retained server logs mask bootstrap API keys; these test logs
are not a general-purpose sanitizer for arbitrary borrowed-server output. A
dirty development run is iteration evidence, not final candidate qualification. The `serve` entrypoint expects the controller-created artifact
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

The fixture runner also selects and requires a named PASS for each committed
correlation or durable-operation component scenario. A missing or skipped
selected scenario fails qualification.

Mount the artifact directory at `/artifacts` for the configuration's certificate
paths. Its two job aliases are examples; substitute actual lane job aliases
before later correlation-route qualification. F1's self-check uses real SDK
calls to qualify the adapter boundary. The selected manual fixture also requires
`TestConnectorsRead` and `TestConnectorsActions` to pass against the real HTTP
surface. The authenticated HTTP/browser CI lane remains F2; the existing default
integration lane keeps connectors off.
