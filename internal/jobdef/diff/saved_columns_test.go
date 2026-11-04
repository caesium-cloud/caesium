package diff

import (
	"sync"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
	gormschema "gorm.io/gorm/schema"
)

// columnDecision records whether one persisted column takes part in Compare.
// Every column apply can write needs a decision: a new column with no entry
// fails TestSavedColumnsAreProjectedOrExcluded, and an excluded column needs
// a reason so the omission cannot be silent.
type columnDecision struct {
	compared bool
	reason   string
}

func comparedColumn(reason string) columnDecision {
	return columnDecision{compared: true, reason: reason}
}

func excludedColumn(reason string) columnDecision {
	return columnDecision{compared: false, reason: reason}
}

// savedColumnDecisions is the inventory of columns job apply persists.
// Compared columns are projected into JobSpec (see project.go). Excluded
// columns stay out of Compare for the reason given.
func savedColumnDecisions() map[string]map[string]columnDecision {
	identity := excludedColumn("row identity, not manifest content")
	timestamps := excludedColumn("row lifecycle timestamps, not manifest content")
	softDelete := excludedColumn("soft-delete lifecycle, not manifest content")
	provenance := excludedColumn("git-sync provenance, not definition content; a new commit would otherwise dirty every diff")
	foreignKey := excludedColumn("foreign key; the referenced row's content is compared instead")

	return map[string]map[string]columnDecision{
		"jobs": {
			"id":                   identity,
			"alias":                comparedColumn("metadata.alias"),
			"namespace":            comparedColumn("metadata.namespace, with the empty legacy value read as default"),
			"trigger_id":           foreignKey,
			"labels":               comparedColumn("metadata.labels"),
			"annotations":          comparedColumn("metadata.annotations"),
			"provenance_source_id": provenance,
			"provenance_repo":      provenance,
			"provenance_ref":       provenance,
			"provenance_commit":    provenance,
			"provenance_path":      provenance,
			"max_parallel_tasks":   comparedColumn("metadata.maxParallelTasks"),
			"task_timeout":         comparedColumn("metadata.taskTimeout, stored as a duration"),
			"run_timeout":          comparedColumn("metadata.runTimeout, stored as a duration"),
			"priority":             comparedColumn("metadata.priority"),
			"concurrency":          comparedColumn("metadata.concurrency"),
			"rate_limits":          comparedColumn("metadata.rateLimits"),
			"sla":                  comparedColumn("metadata.sla"),
			"schema_validation":    comparedColumn("metadata.schemaValidation"),
			"on_upstream_hold":     comparedColumn("metadata.onUpstreamHold"),
			"replay_safe":          comparedColumn("metadata.replaySafe; step replaySafe is the effective per-step value"),
			"cache_config":         comparedColumn("metadata.cache, the authored document"),
			"remediation":          comparedColumn("metadata.remediation"),
			"paused":               excludedColumn("operator runtime state; job apply's upsert does not write it"),
			"deleted_at":           softDelete,
			"created_at":           timestamps,
			"updated_at":           timestamps,
		},
		"tasks": {
			"id":                  identity,
			"job_id":              foreignKey,
			"atom_id":             foreignKey,
			"name":                comparedColumn("step name; steps are compared in position order"),
			"position":            excludedColumn("storage index of step order; the Steps slice is compared in that order, and edges are a separate set"),
			"type":                comparedColumn("step type, defaulting an empty value to task"),
			"node_selector":       comparedColumn("step nodeSelector"),
			"retries":             comparedColumn("step retries"),
			"retry_delay":         comparedColumn("step retryDelay, stored as a duration"),
			"retry_backoff":       comparedColumn("step retryBackoff"),
			"trigger_rule":        comparedColumn("step triggerRule, defaulting an empty value to all_success"),
			"replay_safe":         comparedColumn("effective replaySafe (job flag or step flag)"),
			"rate_limit_resource": comparedColumn("step rateLimit.resource"),
			"rate_limit_units":    comparedColumn("step rateLimit.units"),
			"fan_out_config":      comparedColumn("step fanOut, with apply's default env, onEmpty, and failurePolicy filled"),
			"cache_config":        comparedColumn("step cache, the authored document"),
			"output_schema":       comparedColumn("step outputSchema"),
			"input_schema":        comparedColumn("step inputSchema"),
			"deleted_at":          softDelete,
			"created_at":          timestamps,
			"updated_at":          timestamps,
		},
		"atoms": {
			"id":                   identity,
			"engine":               comparedColumn("step engine, defaulting an empty value to docker"),
			"image":                comparedColumn("step image"),
			"command":              comparedColumn("step command"),
			"spec":                 comparedColumn("resolved runtime spec (env redacted, workdir, mounts, resolved volumes, kubernetes including kueue queue)"),
			"replay_safe":          excludedColumn("duplicate of tasks.replay_safe, which is the effective value Compare uses"),
			"provenance_source_id": provenance,
			"provenance_repo":      provenance,
			"provenance_ref":       provenance,
			"provenance_commit":    provenance,
			"provenance_path":      provenance,
			"deleted_at":           softDelete,
			"created_at":           timestamps,
			"updated_at":           timestamps,
		},
		"triggers": {
			"id":                   identity,
			"alias":                excludedColumn("copy of the job alias, which is compared on jobs.alias"),
			"type":                 comparedColumn("trigger type"),
			"normalized_path":      excludedColumn("derived from an HTTP trigger configuration path"),
			"configuration":        comparedColumn("trigger configuration, including defaultParams folded the way apply stores them"),
			"provenance_source_id": provenance,
			"provenance_repo":      provenance,
			"provenance_ref":       provenance,
			"provenance_commit":    provenance,
			"provenance_path":      provenance,
			"deleted_at":           softDelete,
			"created_at":           timestamps,
			"updated_at":           timestamps,
		},
		"callbacks": {
			"id":            identity,
			"type":          comparedColumn("callback type; callbacks are compared in position order"),
			"configuration": comparedColumn("callback configuration"),
			"job_id":        foreignKey,
			"position":      excludedColumn("storage index of callback order; the Callbacks slice is compared in that order"),
			"deleted_at":    softDelete,
			"created_at":    timestamps,
			"updated_at":    timestamps,
		},
		"task_edges": {
			"id":                   identity,
			"job_id":               excludedColumn("the compared edge is the from/to step-name pair, not the owning job id"),
			"from_task_id":         comparedColumn("edge source, resolved to the step name and compared as an order-independent set"),
			"to_task_id":           comparedColumn("edge target, resolved to the step name and compared as an order-independent set"),
			"provenance_source_id": provenance,
			"provenance_repo":      provenance,
			"provenance_ref":       provenance,
			"provenance_commit":    provenance,
			"provenance_path":      provenance,
			"deleted_at":           softDelete,
			"created_at":           timestamps,
			"updated_at":           timestamps,
		},
		"dataset_declarations": {
			"id":              identity,
			"job_id":          foreignKey,
			"job_alias":       excludedColumn("denormalized copy of jobs.alias"),
			"step_name":       comparedColumn("declaration step name"),
			"namespace":       excludedColumn("reserved and not populated in v1"),
			"name":            comparedColumn("dataset name"),
			"direction":       comparedColumn("produces, consumes, or source"),
			"freshness":       comparedColumn("produced dataset freshness"),
			"max_staleness":   comparedColumn("produced dataset maxStaleness"),
			"expected_every":  comparedColumn("source dataset expectedEvery"),
			"watermark_key":   comparedColumn("produced dataset watermark key"),
			"schema_json":     comparedColumn("inline dataset schema"),
			"schema_from":     comparedColumn("dataset schemaFrom"),
			"schema_version":  comparedColumn("produced dataset schema version"),
			"assertions_json": comparedColumn("produced dataset assertions"),
			"on_violation":    comparedColumn("produced dataset onViolation"),
			"release":         comparedColumn("produced dataset release"),
			"skip_when_fresh": comparedColumn("effective metadata.datasets.skipWhenFresh"),
			"external":        comparedColumn("source dataset external flag"),
			"arrival_binding": comparedColumn("source dataset arrival binding"),
			"created_at":      timestamps,
			"updated_at":      timestamps,
		},
		"dag_snapshots": {
			"id":           excludedColumn("append-only derived history; live tasks and task_edges are compared"),
			"job_id":       excludedColumn("append-only derived history; live tasks and task_edges are compared"),
			"namespace":    excludedColumn("historical tenancy of a snapshot, not the live job namespace"),
			"content_hash": excludedColumn("append-only derived history; live tasks and task_edges are compared"),
			"git_commit":   excludedColumn("append-only derived history; live tasks and task_edges are compared"),
			"tasks":        excludedColumn("append-only derived history; live tasks are compared"),
			"edges":        excludedColumn("append-only derived history; live task_edges are compared"),
			"created_at":   excludedColumn("append-only derived history; live tasks and task_edges are compared"),
		},
	}
}

func TestSavedColumnsAreProjectedOrExcluded(t *testing.T) {
	decisions := savedColumnDecisions()
	namer := gormschema.NamingStrategy{}
	cache := &sync.Map{}
	seen := make(map[string]map[string]struct{}, len(decisions))

	for _, model := range []any{
		&models.Job{},
		&models.Task{},
		&models.Atom{},
		&models.Trigger{},
		&models.Callback{},
		&models.TaskEdge{},
		&models.DatasetDeclaration{},
		&models.DagSnapshot{},
	} {
		parsed, err := gormschema.Parse(model, cache, namer)
		require.NoError(t, err)
		tableDecisions, ok := decisions[parsed.Table]
		require.Truef(t, ok, "table %s has no projection decisions", parsed.Table)
		if seen[parsed.Table] == nil {
			seen[parsed.Table] = map[string]struct{}{}
		}
		for column := range parsed.FieldsByDBName {
			decision, ok := tableDecisions[column]
			require.Truef(t, ok, "%s.%s has no projection decision", parsed.Table, column)
			require.NotEmptyf(t, decision.reason, "%s.%s needs a reason", parsed.Table, column)
			seen[parsed.Table][column] = struct{}{}
		}
	}

	for table, columns := range decisions {
		for column, decision := range columns {
			require.NotEmptyf(t, decision.reason, "%s.%s needs a reason", table, column)
			_, ok := seen[table][column]
			require.Truef(t, ok, "%s.%s is not a column on the model", table, column)
		}
	}
}
