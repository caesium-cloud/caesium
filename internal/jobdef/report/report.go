package report

import (
	"fmt"
	"sort"
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
	b.WriteString("| `alias` | string | required | Unique identifier used across APIs and console. |\n")
	b.WriteString("| `labels` | map[string]string | optional | Attach metadata for filtering. |\n")
	b.WriteString("| `annotations` | map[string]string | optional | Free-form metadata surfaced to clients. |\n\n")

	b.WriteString("## Trigger\n\n")
	b.WriteString("Supported trigger types: `cron`, `http`. Each type accepts a `configuration` map that is persisted verbatim.\n\n")
	b.WriteString("### Cron Trigger\n")
	b.WriteString("| Field | Type | Required | Notes |\n")
	b.WriteString("|-------|------|----------|-------|\n")
	b.WriteString("| `cron` | string | required | POSIX cron spec (5 field). |\n")
	b.WriteString("| `timezone` | string | optional | Defaults to `UTC`. |\n\n")
	b.WriteString("### HTTP Trigger\n")
	b.WriteString("| Field | Type | Required | Notes |\n")
	b.WriteString("|-------|------|----------|-------|\n")
	b.WriteString("| `path` | string | required | Route served under `/v1/jobs/:id`. |\n")
	b.WriteString("| `secret` | string | optional | Shared secret validated by console/API clients. |\n\n")

	b.WriteString("## Callbacks\n\n")
	b.WriteString("Currently the `notification` callback is supported. Custom handlers consume the JSON payload via the callbacks table.\n\n")

	b.WriteString("## Steps\n\n")
	b.WriteString("Every step represents a task/atom pair. Steps default to the Docker engine when the `engine` field is omitted and automatically link to the next step unless `next` is specified.\n\n")
	b.WriteString("| Field | Type | Required | Notes |\n")
	b.WriteString("|-------|------|----------|-------|\n")
	b.WriteString("| `name` | string | required | Unique within the job; used for DAG references. |\n")
	b.WriteString("| `engine` | string | optional | One of `docker`, `podman`, `kubernetes`. Defaults to `docker`. |\n")
	b.WriteString("| `image` | string | required | Container image reference. |\n")
	b.WriteString("| `command` | array[string] | optional | Executed command; defaults to entrypoint. |\n")
	b.WriteString("| `next` | string | optional | Explicit link to another step. |\n\n")

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
		slices := append([]string(nil), summary.MissingAliases...)
		sort.Strings(slices)
		b.WriteString("## Missing Aliases\n\n")
		for _, entry := range slices {
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
	sort.Strings(keys)

	b.WriteString("| Value | Count |\n")
	b.WriteString("|-------|-------|\n")
	for _, key := range keys {
		b.WriteString(fmt.Sprintf("| %s | %d |\n", key, counts[key]))
	}
	b.WriteString("\n")
}
