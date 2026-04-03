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
	b.WriteString("| `schemaValidation` | string | optional | Runtime output validation mode: `warn` or `fail`. Empty disables validation. |\n")
	b.WriteString("| `cache` | boolean or object | optional | Job-level cache defaults; accepts `true` or `{ttl: \"24h\"}`. |\n\n")

	b.WriteString("## Trigger\n\n")
	b.WriteString("Supported trigger types: `cron`, `http`. Each type accepts a `configuration` map that is persisted verbatim.\n\n")
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
	b.WriteString("| `path` | string | required | Route served under `/v1/jobs/:id`. |\n")
	b.WriteString("| `secret` | string | optional | Shared secret validated by web UI/API clients. |\n\n")

	b.WriteString("## Callbacks\n\n")
	b.WriteString("Currently the `notification` callback is supported. Custom handlers consume the JSON payload via the callbacks table.\n\n")

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
	b.WriteString("| `nodeSelector` | map[string]string | optional | Node labels required for claiming this step in distributed mode. |\n")
	b.WriteString("| `next` | array[string] | optional | Successor steps triggered when this step completes. Accepts either a string or list in manifests. |\n")
	b.WriteString("| `dependsOn` | array[string] | optional | Predecessor steps that must complete before this step can run. |\n")
	b.WriteString("| `retries` | integer | optional | Number of retry attempts after the initial failure. |\n")
	b.WriteString("| `retryDelay` | duration | optional | Base delay between retry attempts. |\n")
	b.WriteString("| `retryBackoff` | boolean | optional | Doubles `retryDelay` for each retry attempt when enabled. |\n")
	b.WriteString("| `triggerRule` | string | optional | Upstream completion policy such as `all_success`, `all_done`, or `one_success`. |\n")
	b.WriteString("| `outputSchema` | object | optional | JSON Schema fragment describing this step's emitted outputs. |\n")
	b.WriteString("| `inputSchema` | map[string]object | optional | Required output keys per predecessor step for contract validation. |\n")
	b.WriteString("| `cache` | boolean or object | optional | Enable task caching; accepts `true`, `false`, `{ttl: \"12h\"}`, or `{ttl: \"12h\", version: 2}`. |\n\n")

	b.WriteString("### Cache\n\n")
	b.WriteString("| Field | Type | Required | Notes |\n")
	b.WriteString("|-------|------|----------|-------|\n")
	b.WriteString("| `ttl` | duration string | optional | Cache entry lifetime (e.g. \"24h\", \"7d\"). Defaults to CAESIUM_CACHE_TTL. |\n")
	b.WriteString("| `version` | integer | optional | Bump to invalidate existing cache entries without changing task definition. |\n\n")

	b.WriteString("## Secret References\n\n")
	b.WriteString("Use `secret://` URIs for sensitive values. Supported providers: `env`, `k8s`, `vault`. See `docs/job-definitions.md` for details.\n")

	return b.String()
}

// RenderSummaryMarkdown converts a Summary into Markdown output.
func RenderSummaryMarkdown(summary Summary) string {
	var b strings.Builder

	b.WriteString("# Job Definition Conformance Report\n\n")
	b.WriteString(fmt.Sprintf("Total definitions: **%d**\n\n", summary.Total))

	if len(summary.MissingAliases) > 0 {
		sorted := slices.Clone(summary.MissingAliases)
		slices.Sort(sorted)
		b.WriteString("## Missing Aliases\n\n")
		for _, entry := range sorted {
			b.WriteString(fmt.Sprintf("- %s\n", entry))
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
