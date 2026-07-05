package report

import (
	"fmt"
	"slices"
	"strings"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
)

// Summary captures aggregate information about a collection of job definitions.
type Summary struct {
	Total          int
	TriggerTypes   map[string]int
	Engines        map[string]int
	CallbackTypes  map[string]int
	MissingAliases []string
}

// Analyze builds a Summary for the provided definitions.
func Analyze(defs []schema.Definition) Summary {
	summary := Summary{
		Total:         len(defs),
		TriggerTypes:  make(map[string]int),
		Engines:       make(map[string]int),
		CallbackTypes: make(map[string]int),
	}

	for i := range defs {
		def := &defs[i]
		alias := strings.TrimSpace(def.Metadata.Alias)
		if alias == "" {
			summary.MissingAliases = append(summary.MissingAliases, fmt.Sprintf("definition[%d]", i))
		}

		summary.TriggerTypes[strings.TrimSpace(def.Trigger.Type)]++

		for _, cb := range def.Callbacks {
			summary.CallbackTypes[strings.TrimSpace(cb.Type)]++
		}

		for _, step := range def.Steps {
			engine := step.Engine
			if engine == "" {
				engine = schema.EngineDocker
			}
			summary.Engines[strings.TrimSpace(engine)]++
		}
	}

	return summary
}

// Markdown renders a high-level schema reference in Markdown format.
func Markdown() string {
	var b strings.Builder

	b.WriteString("# Job Definition Schema\n\n")
	b.WriteString("This document is generated from the job definition Go structs (`pkg/jobdef`). It highlights the required sections and key fields.\n\n")

	b.WriteString("## Top-Level Fields\n\n")
	b.WriteString("| Field | Type | Required | Notes |\n")
	b.WriteString("|-------|------|----------|-------|\n")
	b.WriteString("| `$schema` | string | optional | JSON schema reference for tooling. |\n")
	b.WriteString("| `apiVersion` | string | required | Must be `v1`. |\n")
	b.WriteString("| `kind` | string | required | Must be `Job`. |\n")
	b.WriteString("| `metadata` | object | required | Includes alias, labels, annotations. |\n")
	b.WriteString("| `trigger` | object | required | Defines how the job is invoked. |\n")
	b.WriteString("| `callbacks` | array | optional | Notification hooks executed after runs. |\n")
	b.WriteString("| `volumes` | array | optional | Named BYO storage sources mounted by steps. |\n")
	b.WriteString("| `steps` | array | required | Ordered list of atoms/tasks forming the DAG. |\n\n")

	b.WriteString("## Metadata\n\n")
	b.WriteString("| Field | Type | Required | Notes |\n")
	b.WriteString("|-------|------|----------|-------|\n")
	b.WriteString("| `alias` | string | required | Unique identifier used across APIs and web UI. |\n")
	b.WriteString("| `labels` | map[string]string | optional | Attach metadata for filtering. |\n")
	b.WriteString("| `annotations` | map[string]string | optional | Free-form metadata surfaced to clients. |\n")
	b.WriteString("| `maxParallelTasks` | integer | optional | Caps concurrent runnable steps for a single job run. |\n")
	b.WriteString("| `taskTimeout` | duration | optional | Default timeout applied to each step unless overridden by runtime configuration. |\n")
	b.WriteString("| `runTimeout` | duration | optional | Maximum total wall-clock time for the job run. |\n")
	b.WriteString("| `priority` | string | optional | Run and task scheduling priority: `high`, `normal`, or `low`. Scheduling metadata excluded from the cache identity hash. |\n")
	b.WriteString("| `concurrency` | object | optional | Run-level concurrency control with `maxRuns` and `strategy` (`queue`, `replace`, `skip`, or `fail`); `strategy` defaults to `queue`. Scheduling metadata excluded from the cache identity hash. |\n")
	b.WriteString("| `rateLimits` | array[object] | optional | Shared resource budgets declared as `{resource, limit, window}`. `window` is a duration string. Scheduling metadata excluded from the cache identity hash. |\n")
	b.WriteString("| `schemaValidation` | string | optional | Runtime output validation mode: `warn` or `fail`. Empty disables validation. |\n")
	b.WriteString("| `replaySafe` | boolean | optional | Marks every step in this job as eligible for quarantined what-if replay. Recorded on each baseline task run; excluded from the cache identity hash. |\n")
	b.WriteString("| `cache` | boolean or object | optional | Job-level cache defaults; accepts `true`, `{ttl: \"24h\"}`, or `{pinDigests: true}`. Step-level `cache` overrides these defaults. |\n")
	b.WriteString("| `serviceAccountName` | string | optional | Default Kubernetes ServiceAccount for Kubernetes steps. |\n")
	b.WriteString("| `podAnnotations` | map[string]string | optional | Default annotations applied to Kubernetes step pods. |\n")
	b.WriteString("| `automountServiceAccountToken` | boolean | optional | Default Kubernetes pod service-account token setting. |\n")
	b.WriteString("| `datasets` | object | optional | Freshness-driven scheduling surface: external `sources` the job's steps consume plus the `skipWhenFresh` control. See [Datasets & Freshness](#datasets--freshness). Feature-gated behind `CAESIUM_FRESHNESS_ENABLED`; scheduling metadata excluded from the cache identity hash. |\n")
	b.WriteString("| `remediation` | object | optional | Opt-in to agent-in-the-loop incident remediation: `profile`, `classes`, `maxAttempts`, `autonomy`, `escalation`. See [Remediation](#remediation). Feature-gated behind `CAESIUM_AGENT_REMEDIATION_ENABLED`; policy metadata excluded from the cache identity hash. |\n\n")

	b.WriteString("## Trigger\n\n")
	b.WriteString("Supported trigger types: `cron`, `http`, `event`, `freshness`. Each type accepts a `configuration` map that is persisted verbatim, with type-specific validation. The `freshness` type is feature-gated behind `CAESIUM_FRESHNESS_ENABLED` and derives its cadence from the job's declared datasets; see [Freshness Trigger](#freshness-trigger).\n\n")
	b.WriteString("### Common Trigger Fields\n")
	b.WriteString("| Field | Type | Required | Notes |\n")
	b.WriteString("|-------|------|----------|-------|\n")
	b.WriteString("| `defaultParams` | map[string]string | optional | Seeds run parameters when a trigger fires. |\n\n")
	b.WriteString("### Cron Trigger\n")
	b.WriteString("| Field | Type | Required | Notes |\n")
	b.WriteString("|-------|------|----------|-------|\n")
	b.WriteString("| `cron` | string | required | POSIX cron spec (5 field). |\n")
	b.WriteString("| `timezone` | string | optional | Defaults to `UTC`. |\n\n")
	b.WriteString("### HTTP Trigger\n")
	b.WriteString("| Field | Type | Required | Notes |\n")
	b.WriteString("|-------|------|----------|-------|\n")
	b.WriteString("| `path` | string | required | Webhook route suffix. Caesium serves it at `POST /v1/hooks/<path>` and normalizes legacy `/hooks/<path>` forms. |\n")
	b.WriteString("| `secret` | string | optional | Shared secret used to validate incoming webhook requests. |\n")
	b.WriteString("| `signatureScheme` | string | optional | One of `hmac-sha256`, `hmac-sha1`, `bearer`, `basic`. Defaults to `hmac-sha256` when `secret` is set. |\n")
	b.WriteString("| `signatureHeader` | string | optional | Header containing the signature or token. Default varies by scheme. |\n")
	b.WriteString("| `paramMapping` | map[string]string | optional | Extracts JSON request-body fields into run params using simple JSONPath expressions such as `$.ref`. |\n\n")

	b.WriteString("### Event Trigger\n")
	b.WriteString("| Field | Type | Required | Notes |\n")
	b.WriteString("|-------|------|----------|-------|\n")
	b.WriteString("| `events` | array[object] | required | One or more event patterns. A pattern matches when its `type`, optional `source`, and optional `filter` all match the ingested event. |\n")
	b.WriteString("| `events[].type` | string | required | Event type to match. Exact strings and glob patterns such as `webhook.*` are supported. |\n")
	b.WriteString("| `events[].source` | string | optional | Exact event source filter, such as `github` or `caesium`. |\n")
	b.WriteString("| `events[].filter` | map[string]string | optional | Content filter over event `data`. Keys are dot paths like `repository.full_name`; values are string comparisons. |\n")
	b.WriteString("| `paramMapping` | map[string]string | optional | Extracts JSON event-data fields into run params using simple JSONPath expressions such as `$.run_id`. |\n")
	b.WriteString("| `defaultParams` | map[string]string | optional | Seeds run parameters for event-triggered executions before extracted event params are merged. Values must be strings. |\n\n")
	b.WriteString("For trigger chaining, Caesium routes lifecycle events with `source: caesium` through the same event router. The scheduler-owned `_trigger_depth` run parameter tracks chain depth and is rejected when it reaches `CAESIUM_MAX_TRIGGER_DEPTH`; authors should not set or depend on `_trigger_depth` for business logic.\n\n")

	b.WriteString("### Freshness Trigger\n")
	b.WriteString("A `freshness` trigger (feature-gated behind `CAESIUM_FRESHNESS_ENABLED=true`) carries no cadence of its own — the freshness evaluator derives runs from the job's declared dataset graph. A freshness-triggered job must declare at least one consumed dataset (a step-level `datasets.consumes` entry or a `metadata.datasets.sources` entry) and at least one produced dataset with a `freshness` SLO. Because the requirement lives on the job's datasets rather than the trigger, a `freshness` trigger cannot be created through the trigger-create/update API; declare it on a job definition. See [Datasets & Freshness](#datasets--freshness).\n\n")

	b.WriteString("## Callbacks\n\n")
	b.WriteString("Currently the `notification` callback is supported. Custom handlers consume the JSON payload via the callbacks table.\n\n")

	b.WriteString("## Volumes\n\n")
	b.WriteString("Volumes are declared once at the job level and mounted by steps with `volumeMounts`.\n\n")
	b.WriteString("| Field | Type | Required | Notes |\n")
	b.WriteString("|-------|------|----------|-------|\n")
	b.WriteString("| `name` | string | required | Unique volume name referenced by steps. |\n")
	b.WriteString("| `source` | object | optional | Single-engine shorthand. Exactly one of `source` or `sources` is required. |\n")
	b.WriteString("| `sources` | map[string]object | optional | Engine-keyed sources for portable manifests. Keys are `docker`, `podman`, or `kubernetes`. |\n")
	b.WriteString("| `accessMode` | string | optional | Advisory access mode; supported values are Kubernetes access modes. |\n\n")
	b.WriteString("Docker/Podman source kinds: `bind`, `volume`, `tmpfs`.\n")
	b.WriteString("Kubernetes source kinds: `pvc`, `claimTemplate`, `volumeSource`.\n\n")

	b.WriteString("## Steps\n\n")
	b.WriteString("Each step represents a DAG node backed by a task/atom pair. Steps default to the Docker engine when the `engine` field is omitted. When neither `next` nor `dependsOn` is provided, the importer links steps sequentially.\n\n")
	b.WriteString("| Field | Type | Required | Notes |\n")
	b.WriteString("|-------|------|----------|-------|\n")
	b.WriteString("| `name` | string | required | Unique within the job; used for DAG references. |\n")
	b.WriteString("| `type` | string | optional | Step kind. Defaults to `task`; `branch` enables conditional fan-out. |\n")
	b.WriteString("| `engine` | string | optional | One of `docker`, `podman`, `kubernetes`. Defaults to `docker`. |\n")
	b.WriteString("| `image` | string | required | Container image reference. |\n")
	b.WriteString("| `command` | array[string] | optional | Executed command; defaults to entrypoint. |\n")
	b.WriteString("| `env` | map[string]string | optional | Environment variables passed to the runtime. |\n")
	b.WriteString("| `workdir` | string | optional | Working directory inside the container runtime. |\n")
	b.WriteString("| `mounts` | array[object] | optional | Bind mounts with `source`, `target`, and optional `readOnly`. |\n")
	b.WriteString("| `volumeMounts` | array[object] | optional | Declared volume mounts with `volume`, `path`, optional `readOnly`, and optional `subPath`. |\n")
	b.WriteString("| `nodeSelector` | map[string]string | optional | Node labels required for claiming this step in distributed mode. |\n")
	b.WriteString("| `serviceAccountName` | string | optional | Kubernetes ServiceAccount for this step's pod. |\n")
	b.WriteString("| `podAnnotations` | map[string]string | optional | Kubernetes pod annotations for this step. |\n")
	b.WriteString("| `automountServiceAccountToken` | boolean | optional | Kubernetes pod service-account token setting for this step. |\n")
	b.WriteString("| `kueue` | object | optional | Delegate this step's admission to a Kueue LocalQueue (kubernetes engine only). See [Kueue](#kueue) below. Excluded from the cache identity hash — it is scheduling metadata, not an execution input. |\n")
	b.WriteString("| `rateLimit` | object | optional | Consume units from a job-level `metadata.rateLimits` resource: `{resource, units}`. Scheduling metadata excluded from the cache identity hash. |\n")
	b.WriteString("| `replaySafe` | boolean | optional | Marks this step as eligible for quarantined what-if replay. The effective value (`metadata.replaySafe` or this field) is recorded on the baseline task run and excluded from the cache identity hash. |\n")
	b.WriteString("| `next` | array[string] | optional | Successor steps triggered when this step completes. Accepts either a string or list in manifests. |\n")
	b.WriteString("| `dependsOn` | array[string] | optional | Predecessor steps that must complete before this step can run. |\n")
	b.WriteString("| `retries` | integer | optional | Number of retry attempts after the initial failure. |\n")
	b.WriteString("| `retryDelay` | duration | optional | Base delay between retry attempts. |\n")
	b.WriteString("| `retryBackoff` | boolean | optional | Doubles `retryDelay` for each retry attempt when enabled. |\n")
	b.WriteString("| `triggerRule` | string | optional | Upstream completion policy such as `all_success`, `all_done`, or `one_success`. |\n")
	b.WriteString("| `outputSchema` | object | optional | JSON Schema fragment describing this step's emitted outputs. |\n")
	b.WriteString("| `inputSchema` | map[string]object | optional | Required output keys per predecessor step for contract validation. |\n")
	b.WriteString("| `datasets` | object | optional | Per-step freshness surface: `consumes` (dataset names) and `produces` (datasets with `freshness`/`maxStaleness`/`watermark` SLOs). See [Datasets & Freshness](#datasets--freshness). Scheduling metadata excluded from the cache identity hash. |\n")
	b.WriteString("| `cache` | boolean or object | optional | Enable task caching; accepts `true`, `false`, `{ttl: \"12h\"}`, `{ttl: \"12h\", version: 2}`, `{pinDigests: true}`, or `{pinDigests: true, digestTTL: 0}`. |\n\n")

	b.WriteString("### Cache\n\n")
	b.WriteString("| Field | Type | Required | Notes |\n")
	b.WriteString("|-------|------|----------|-------|\n")
	b.WriteString("| `ttl` | duration string | optional | Cache entry lifetime (e.g. \"24h\", \"7d\"). Defaults to CAESIUM_CACHE_TTL. |\n")
	b.WriteString("| `version` | integer | optional | Bump to invalidate existing cache entries without changing task definition. |\n")
	b.WriteString("| `pinDigests` | boolean | optional | Resolve each step's image tag to its content digest (`sha256:…`) and fold the digest, not the mutable tag, into the cache key. A tag that moves to new content (e.g. a re-pushed `:latest`) then produces a cache **miss** instead of serving a stale hit. Defaults to `CAESIUM_CACHE_PIN_DIGESTS`. Set at job (`metadata.cache`) or step level; a step value overrides the job default. Resolution is opt-in because it costs a registry round-trip on first sight; the resolved tag→digest mapping is cached for `digestTTL` so steady-state runs pay no network cost. |\n")
	b.WriteString("| `digestTTL` | duration string or 0 | optional | How long a resolved tag→digest mapping is reused before re-resolution (a **perf cache**). Within the window a moved tag is **not** re-detected — the prior digest is served. `0` re-resolves on every check, so a moved tag is detected immediately at the cost of a registry round-trip per check. Defaults to `CAESIUM_CACHE_DIGEST_TTL` (5m). Only meaningful with `pinDigests`. |\n\n")

	b.WriteString("### Replay Safety\n\n")
	b.WriteString("`replaySafe` is the durable operator mark required before quarantined what-if replay can re-execute a task. Set `metadata.replaySafe: true` to mark every step in the job, or `steps[].replaySafe: true` to mark a single step. Caesium records the effective value on the baseline `TaskRun` when the task runs; later applies cannot retroactively authorize an older unsafe baseline. This flag is control-plane metadata, not an execution input, so it is excluded from the cache identity hash.\n\n")

	b.WriteString("### Kueue\n\n")
	b.WriteString("`kueue` delegates a step's scheduling to [Kueue](https://kueue.sigs.k8s.io/), the Kubernetes-native job-queueing controller. Caesium does not bin-pack, prioritize, or gang-schedule — when `kueue` is set on a `kubernetes` step, Caesium stamps the `kueue.x-k8s.io/queue-name` label on the created pod and Kueue gates admission against the named LocalQueue's quota, holding the pod (via the `kueue.x-k8s.io/admission` scheduling gate its webhook injects) until capacity is available. This is only valid on the `kubernetes` engine; `docker`/`podman` reject it.\n\n")
	b.WriteString("| Field | Type | Required | Notes |\n")
	b.WriteString("|-------|------|----------|-------|\n")
	b.WriteString("| `queueName` | string | required | The Kueue LocalQueue (in the pod's namespace) to admit through. Becomes the value of the `kueue.x-k8s.io/queue-name` label. |\n\n")
	b.WriteString("The queue is **scheduling metadata, not an execution input**, so it is excluded from the cache identity hash exactly like secrets and workload identity: two otherwise-identical tasks that differ only in queue share one cache identity, and re-queuing a task never busts its cache. Your cluster must have Kueue installed with the LocalQueue (and a backing ClusterQueue) provisioned; see [`kubernetes-deployment.md`](kubernetes-deployment.md#delegating-scheduling-to-kueue).\n\n")

	b.WriteString("## Datasets & Freshness\n\n")
	b.WriteString("Freshness-driven scheduling lets a job declare the datasets its steps produce and consume, plus a freshness SLO on each output, so Caesium can derive execution from data arrival and staleness instead of a cron guess: run when upstream data has arrived and my output is stale against its SLO, don't run when nothing changed, and surface `stale-upstream` (an observable state with a reason) rather than a failed run when upstream is late. The whole surface is scheduling metadata and never enters the cache identity hash. It is feature-gated behind `CAESIUM_FRESHNESS_ENABLED=true`; dataset state is exposed through the `GET /v1/datasets` REST surface and the Console freshness view.\n\n")

	b.WriteString("### Step Datasets (`steps[].datasets`)\n")
	b.WriteString("| Field | Type | Required | Notes |\n")
	b.WriteString("|-------|------|----------|-------|\n")
	b.WriteString("| `consumes` | array[string] | optional | Dataset names this step reads. A consumed name may resolve to a dataset produced by another job; cross-job resolution is a lint/registry concern, not a single-definition error. |\n")
	b.WriteString("| `produces` | array[object] | optional | Datasets this step produces, each carrying its freshness SLO. |\n")
	b.WriteString("| `produces[].name` | string | required | Dataset identity (keyed on name in v1; namespace is reserved). |\n")
	b.WriteString("| `produces[].freshness` | duration | optional | Target staleness SLO as a Go duration (e.g. `6h`) — how stale consumers tolerate this dataset being. |\n")
	b.WriteString("| `produces[].maxStaleness` | duration | optional | Hard bound; a breach emits `freshness_violated`. |\n")
	b.WriteString("| `produces[].watermark` | object | optional | `{key: <output-key>}` names the `##caesium::output` key this step emits to advance the dataset's watermark. It is an output key on the existing zero-SDK output contract, not a JSONPath. |\n\n")

	b.WriteString("### Source Datasets (`metadata.datasets.sources`)\n")
	b.WriteString("External datasets nobody in Caesium produces — the upstreams a consuming step depends on. A late arrival surfaces as stale-upstream rather than a failed run.\n\n")
	b.WriteString("| Field | Type | Required | Notes |\n")
	b.WriteString("|-------|------|----------|-------|\n")
	b.WriteString("| `name` | string | required | Source dataset identity, referenced by a step's `datasets.consumes`. |\n")
	b.WriteString("| `expectedEvery` | duration | optional | Cadence expectation as a Go duration; a missed arrival surfaces as stale-upstream. |\n")
	b.WriteString("| `external` | boolean | optional | Marks the dataset as intentionally produced outside Caesium so the cross-job lint does not demand a producing job. |\n")
	b.WriteString("| `arrival` | object | optional | Binds an ingested event to a source-dataset watermark advance. |\n")
	b.WriteString("| `arrival.event.type` | string | required with `arrival.event` | Event type to match (mirrors the shipped event-trigger matcher). |\n")
	b.WriteString("| `arrival.event.filter` | map[string]string | optional | Content filter over event data. |\n")
	b.WriteString("| `arrival.watermark` | string (JSONPath) | optional | JSONPath into the event payload extracted as the new watermark value. |\n\n")
	b.WriteString("`metadata.datasets.skipWhenFresh` (boolean) controls P1 cron skipping: when a cron-triggered job's outputs are already fresh and its consumed watermarks are unchanged, the scheduled run is recorded as `skipped_fresh` instead of executing. It defaults to `true` whenever a job declares datasets. For a purely data-derived job, drop cron and declare `trigger: {type: freshness}` (see [Freshness Trigger](#freshness-trigger)); the evaluator owns the cadence.\n\n")

	b.WriteString("## Remediation\n\n")
	b.WriteString("`metadata.remediation` opts a job into agent-in-the-loop incident remediation: when a run fails, Caesium opens an incident, classifies the failure, and lets a bounded, server-enforced policy retry, snooze, patch, or escalate — with tier-3 actions always gated behind a human approval. The block is policy metadata enforced server-side by the incident manager and executor; it never participates in step-execution cache identity. It requires `CAESIUM_AGENT_REMEDIATION_ENABLED=true` and an active auth mode on the server.\n\n")
	b.WriteString("| Field | Type | Required | Notes |\n")
	b.WriteString("|-------|------|----------|-------|\n")
	b.WriteString("| `profile` | string | required | Names a server-side `AgentProfile` (managed via `/v1/agentprofiles`). Offline `caesium job lint` cannot verify the reference and emits a scope note; server-side lint (`POST /v1/jobdefs/lint`) and the apply transaction verify it. |\n")
	b.WriteString("| `classes` | array[string] | required | Failure classes this policy applies to (at least one). One or more of `transient_infra`, `schema_violation`, `sla_risk`, `data_unavailable`, `auth_failure`, `oom`, `quota`, `unknown`. |\n")
	b.WriteString("| `maxAttempts` | integer | optional | Bounds remediation attempts before an incident force-escalates. Must be `>= 0`. |\n")
	b.WriteString("| `autonomy` | object | optional | Tiered-autonomy policy (below). |\n")
	b.WriteString("| `escalation` | object | optional | Forced hand-off when remediation does not resolve the incident in time. |\n\n")

	b.WriteString("### Autonomy (`metadata.remediation.autonomy`)\n")
	b.WriteString("| Field | Type | Required | Notes |\n")
	b.WriteString("|-------|------|----------|-------|\n")
	b.WriteString("| `allow` | array[string] | optional | Actions permitted to run without a human, subject to each action's own tier — a tier-3 action always creates an ApprovalRequest regardless of `allow`. |\n")
	b.WriteString("| `paramOverrides` | map[string]array[string] | optional | Whitelists `rerun_with_params` values per key; every key must name an existing `trigger.defaultParams` entry. |\n")
	b.WriteString("| `perClass` | map[string]object | optional | Narrows `allow` for a specific failure class: `perClass.<class>.allow`. |\n")
	b.WriteString("| `requireApproval` | array[string] | optional | Actions that must create an ApprovalRequest for this job even when their default tier would otherwise permit autonomous execution. |\n\n")
	b.WriteString("Remediation action names accepted in `allow`, `perClass[].allow`, and `requireApproval`: `auto_retry_backoff`, `snooze_until_cron`, `snooze_retry`, `retry_from_failure`, `retry_callbacks`, `notify`, `quarantine_replay`, `rerun_with_params`, `pause_job`, `unpause_job`, `clear_cache_entry`, `suppress_downstream_alerts`, `extend_sla_once`, `skip_task`, `override_schema_gate`, `apply_jobdef_patch`, `escalate`.\n\n")

	b.WriteString("### Escalation (`metadata.remediation.escalation`)\n")
	b.WriteString("| Field | Type | Required | Notes |\n")
	b.WriteString("|-------|------|----------|-------|\n")
	b.WriteString("| `channel` | string | optional | Names a server-side NotificationChannel to force-escalate to. |\n")
	b.WriteString("| `after` | duration | optional | Wall-clock cap as a Go duration before the incident is force-escalated. At least one of `channel`/`after` is required when `escalation` is set. |\n\n")

	b.WriteString("## Secret References\n\n")
	b.WriteString("Use `secret://` URIs for sensitive values. Supported providers: `env`, `k8s`, `vault`. See `docs/job-definitions.md` for details.\n")

	return b.String()
}

// RenderSummaryMarkdown converts a Summary into Markdown output.
func RenderSummaryMarkdown(summary Summary) string {
	var b strings.Builder

	b.WriteString("# Job Definition Conformance Report\n\n")
	fmt.Fprintf(&b, "Total definitions: **%d**\n\n", summary.Total)

	if len(summary.MissingAliases) > 0 {
		sorted := slices.Clone(summary.MissingAliases)
		slices.Sort(sorted)
		b.WriteString("## Missing Aliases\n\n")
		for _, entry := range sorted {
			fmt.Fprintf(&b, "- %s\n", entry)
		}
		b.WriteString("\n")
	}

	if len(summary.TriggerTypes) > 0 {
		b.WriteString("## Trigger Types\n\n")
		writeCountTable(&b, summary.TriggerTypes)
	}

	if len(summary.Engines) > 0 {
		b.WriteString("## Step Engines\n\n")
		writeCountTable(&b, summary.Engines)
	}

	if len(summary.CallbackTypes) > 0 {
		b.WriteString("## Callback Types\n\n")
		writeCountTable(&b, summary.CallbackTypes)
	}

	return b.String()
}

func writeCountTable(b *strings.Builder, counts map[string]int) {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	b.WriteString("| Value | Count |\n")
	b.WriteString("|-------|-------|\n")
	for _, key := range keys {
		fmt.Fprintf(b, "| %s | %d |\n", key, counts[key])
	}
	b.WriteString("\n")
}
