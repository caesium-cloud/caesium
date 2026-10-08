# Execution connectors

> Status: In progress. This guide records the provider-neutral contract and
> configuration file frozen for review. Caesium does not yet dial a provider,
> persist a connector catalog, expose connector HTTP routes, or show connector
> pages. Job-definition YAML is unchanged.

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
