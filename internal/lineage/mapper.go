package lineage

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	parentFacetSchema     = "https://openlineage.io/spec/facets/1-0-1/ParentRunFacet.json"
	errorFacetSchema      = "https://openlineage.io/spec/facets/1-0-1/ErrorMessageRunFacet.json"
	jobTypeFacetSchema    = "https://openlineage.io/spec/facets/2-0-3/JobTypeJobFacet.json"
	sourceCodeFacetSchema = "https://openlineage.io/spec/facets/1-0-1/SourceCodeLocationJobFacet.json"

	defaultCacheTTL = 5 * time.Minute
)

type jobRunPayload struct {
	ID           uuid.UUID `json:"id"`
	JobID        uuid.UUID `json:"job_id"`
	JobAlias     string    `json:"job_alias"`
	Status       string    `json:"status"`
	Error        string    `json:"error"`
	TriggerType  string    `json:"trigger_type"`
	TriggerAlias string    `json:"trigger_alias"`
	Tasks        []struct {
		TaskID uuid.UUID `json:"task_id"`
	} `json:"tasks"`
}

type taskRunPayload struct {
	ID        uuid.UUID `json:"id"`
	JobRunID  uuid.UUID `json:"job_run_id"`
	TaskID    uuid.UUID `json:"task_id"`
	AtomID    uuid.UUID `json:"atom_id"`
	Engine    string    `json:"engine"`
	Image     string    `json:"image"`
	Command   []string  `json:"command"`
	RuntimeID string    `json:"runtime_id"`
	Status    string    `json:"status"`
	ClaimedBy string    `json:"claimed_by"`
	Result    string    `json:"result"`
	Error     string    `json:"error"`

	// Output holds the structured key→value pairs emitted via ##caesium::output.
	// Values may be file paths, table names, URIs, or scalar summaries.
	Output map[string]string `json:"output,omitempty"`

	// TaskName is the human-readable step name; populated from the job record
	// when available and used to name datasets.
	TaskName string `json:"task_name,omitempty"`

	// OutputSchema is the task's declared JSON Schema for its outputs, stored
	// as a raw JSON blob (map[string]any shape).  Non-nil when the step declares
	// outputSchema in the job manifest.
	OutputSchema map[string]interface{} `json:"output_schema,omitempty"`

	// InputSchema maps predecessor step names to JSON Schema fragments
	// describing which keys this step consumes.  Non-nil when the step declares
	// inputSchema in the job manifest.
	InputSchema map[string]map[string]interface{} `json:"input_schema,omitempty"`
}

type jobRecord struct {
	Alias            string `gorm:"column:alias"`
	ProvenanceRepo   string `gorm:"column:provenance_repo"`
	ProvenanceRef    string `gorm:"column:provenance_ref"`
	ProvenanceCommit string `gorm:"column:provenance_commit"`
	ProvenancePath   string `gorm:"column:provenance_path"`
}

func (jobRecord) TableName() string { return "jobs" }

type mapper struct {
	namespace string
	db        *gorm.DB
	cache     *jobCache
}

func newMapper(namespace string, db *gorm.DB) *mapper {
	return &mapper{
		namespace: namespace,
		db:        db,
		cache:     newJobCache(defaultCacheTTL),
	}
}

func (m *mapper) mapEvent(evt event.Event) (*RunEvent, error) {
	switch evt.Type {
	case event.TypeRunStarted:
		return m.mapRunStart(evt)
	case event.TypeRunCompleted:
		return m.mapRunComplete(evt)
	case event.TypeRunFailed:
		return m.mapRunFail(evt)
	case event.TypeTaskStarted:
		return m.mapTaskStart(evt)
	case event.TypeTaskSucceeded:
		return m.mapTaskComplete(evt)
	case event.TypeTaskFailed:
		return m.mapTaskFail(evt)
	case event.TypeTaskSkipped:
		return m.mapTaskAbort(evt)
	default:
		return nil, nil
	}
}

// --- Run-level mappings ---

func (m *mapper) mapRunStart(evt event.Event) (*RunEvent, error) {
	var payload jobRunPayload
	if err := json.Unmarshal(evt.Payload, &payload); err != nil {
		return nil, fmt.Errorf("unmarshal run payload: %w", err)
	}

	jobAlias := m.resolveJobAlias(evt.JobID, payload.JobAlias)

	runFacets := map[string]interface{}{
		"caesium_dag": CaesiumDAGFacet{
			BaseFacet:    newCaesiumBaseFacet("CaesiumDAGFacet"),
			TotalTasks:   len(payload.Tasks),
			TriggerType:  payload.TriggerType,
			TriggerAlias: payload.TriggerAlias,
		},
	}

	return &RunEvent{
		EventTime: evt.Timestamp,
		EventType: EventTypeStart,
		Producer:  producerURI,
		SchemaURL: schemaURL,
		Run: Run{
			RunID:  evt.RunID,
			Facets: runFacets,
		},
		Job: Job{
			Namespace: m.namespace,
			Name:      jobAlias,
			Facets:    m.buildJobFacets(evt.JobID, "JOB"),
		},
		Inputs:  []Dataset{},
		Outputs: []Dataset{},
	}, nil
}

func (m *mapper) mapRunComplete(evt event.Event) (*RunEvent, error) {
	var payload jobRunPayload
	if err := json.Unmarshal(evt.Payload, &payload); err != nil {
		return nil, fmt.Errorf("unmarshal run payload: %w", err)
	}

	jobAlias := m.resolveJobAlias(evt.JobID, payload.JobAlias)

	return &RunEvent{
		EventTime: evt.Timestamp,
		EventType: EventTypeComplete,
		Producer:  producerURI,
		SchemaURL: schemaURL,
		Run: Run{
			RunID:  evt.RunID,
			Facets: map[string]interface{}{},
		},
		Job: Job{
			Namespace: m.namespace,
			Name:      jobAlias,
			Facets:    m.buildJobFacets(evt.JobID, "JOB"),
		},
		Inputs:  []Dataset{},
		Outputs: []Dataset{},
	}, nil
}

func (m *mapper) mapRunFail(evt event.Event) (*RunEvent, error) {
	var payload jobRunPayload
	if err := json.Unmarshal(evt.Payload, &payload); err != nil {
		return nil, fmt.Errorf("unmarshal run payload: %w", err)
	}

	jobAlias := m.resolveJobAlias(evt.JobID, payload.JobAlias)

	runFacets := map[string]interface{}{}
	if payload.Error != "" {
		runFacets["errorMessage"] = ErrorMessageFacet{
			BaseFacet:           newBaseFacet(errorFacetSchema),
			Message:             payload.Error,
			ProgrammingLanguage: "go",
		}
	}

	return &RunEvent{
		EventTime: evt.Timestamp,
		EventType: EventTypeFail,
		Producer:  producerURI,
		SchemaURL: schemaURL,
		Run: Run{
			RunID:  evt.RunID,
			Facets: runFacets,
		},
		Job: Job{
			Namespace: m.namespace,
			Name:      jobAlias,
			Facets:    m.buildJobFacets(evt.JobID, "JOB"),
		},
		Inputs:  []Dataset{},
		Outputs: []Dataset{},
	}, nil
}

// --- Task-level mappings ---

func (m *mapper) mapTaskStart(evt event.Event) (*RunEvent, error) {
	var payload taskRunPayload
	if err := json.Unmarshal(evt.Payload, &payload); err != nil {
		return nil, fmt.Errorf("unmarshal task payload: %w", err)
	}
	m.enrichTaskPayload(&payload)

	jobAlias := m.resolveJobAlias(evt.JobID, "")
	taskJobName := fmt.Sprintf("%s.task.%s", jobAlias, payload.TaskID)

	runFacets := m.buildParentFacet(evt.RunID, jobAlias)
	m.addExecutionFacet(runFacets, payload)

	inputs, outputs := m.buildTaskDatasets(jobAlias, payload)
	// Persist the bounded dataset graph on each task lifecycle event (start
	// through terminal) so the impact query has edges to traverse — eagerly, so
	// even an in-progress task's declared datasets are present. Without this the
	// lineage_datasets table is never written and /lineage/impact always returns
	// empty. The upsert keys on (task_run, namespace, name, direction), so
	// re-emitted events are idempotent.
	m.persistTaskDatasets(payload, inputs, outputs)

	return &RunEvent{
		EventTime: evt.Timestamp,
		EventType: EventTypeStart,
		Producer:  producerURI,
		SchemaURL: schemaURL,
		Run: Run{
			RunID:  payload.ID,
			Facets: runFacets,
		},
		Job: Job{
			Namespace: m.namespace,
			Name:      taskJobName,
			Facets:    m.buildJobFacets(evt.JobID, "TASK"),
		},
		Inputs:  inputs,
		Outputs: outputs,
	}, nil
}

func (m *mapper) mapTaskComplete(evt event.Event) (*RunEvent, error) {
	var payload taskRunPayload
	if err := json.Unmarshal(evt.Payload, &payload); err != nil {
		return nil, fmt.Errorf("unmarshal task payload: %w", err)
	}
	m.enrichTaskPayload(&payload)

	jobAlias := m.resolveJobAlias(evt.JobID, "")
	taskJobName := fmt.Sprintf("%s.task.%s", jobAlias, payload.TaskID)

	runFacets := m.buildParentFacet(evt.RunID, jobAlias)
	m.addExecutionFacet(runFacets, payload)

	inputs, outputs := m.buildTaskDatasets(jobAlias, payload)
	// Persist the bounded dataset graph on each task lifecycle event (start
	// through terminal) so the impact query has edges to traverse — eagerly, so
	// even an in-progress task's declared datasets are present. Without this the
	// lineage_datasets table is never written and /lineage/impact always returns
	// empty. The upsert keys on (task_run, namespace, name, direction), so
	// re-emitted events are idempotent.
	m.persistTaskDatasets(payload, inputs, outputs)

	return &RunEvent{
		EventTime: evt.Timestamp,
		EventType: EventTypeComplete,
		Producer:  producerURI,
		SchemaURL: schemaURL,
		Run: Run{
			RunID:  payload.ID,
			Facets: runFacets,
		},
		Job: Job{
			Namespace: m.namespace,
			Name:      taskJobName,
			Facets:    m.buildJobFacets(evt.JobID, "TASK"),
		},
		Inputs:  inputs,
		Outputs: outputs,
	}, nil
}

func (m *mapper) mapTaskFail(evt event.Event) (*RunEvent, error) {
	var payload taskRunPayload
	if err := json.Unmarshal(evt.Payload, &payload); err != nil {
		return nil, fmt.Errorf("unmarshal task payload: %w", err)
	}
	m.enrichTaskPayload(&payload)

	jobAlias := m.resolveJobAlias(evt.JobID, "")
	taskJobName := fmt.Sprintf("%s.task.%s", jobAlias, payload.TaskID)

	runFacets := m.buildParentFacet(evt.RunID, jobAlias)
	m.addExecutionFacet(runFacets, payload)
	if payload.Error != "" {
		runFacets["errorMessage"] = ErrorMessageFacet{
			BaseFacet:           newBaseFacet(errorFacetSchema),
			Message:             payload.Error,
			ProgrammingLanguage: "go",
		}
	}

	inputs, outputs := m.buildTaskDatasets(jobAlias, payload)
	// A failed task's declared inputs (and any partial outputs) are still
	// recorded so lineage impact queries reflect failed runs too.
	m.persistTaskDatasets(payload, inputs, outputs)

	return &RunEvent{
		EventTime: evt.Timestamp,
		EventType: EventTypeFail,
		Producer:  producerURI,
		SchemaURL: schemaURL,
		Run: Run{
			RunID:  payload.ID,
			Facets: runFacets,
		},
		Job: Job{
			Namespace: m.namespace,
			Name:      taskJobName,
			Facets:    m.buildJobFacets(evt.JobID, "TASK"),
		},
		Inputs:  inputs,
		Outputs: outputs,
	}, nil
}

func (m *mapper) mapTaskAbort(evt event.Event) (*RunEvent, error) {
	var payload taskRunPayload
	if err := json.Unmarshal(evt.Payload, &payload); err != nil {
		return nil, fmt.Errorf("unmarshal task payload: %w", err)
	}
	m.enrichTaskPayload(&payload)

	jobAlias := m.resolveJobAlias(evt.JobID, "")
	taskJobName := fmt.Sprintf("%s.task.%s", jobAlias, payload.TaskID)

	runFacets := m.buildParentFacet(evt.RunID, jobAlias)
	m.addExecutionFacet(runFacets, payload)

	inputs, outputs := m.buildTaskDatasets(jobAlias, payload)
	// Persist the bounded dataset graph on each task lifecycle event (start
	// through terminal) so the impact query has edges to traverse — eagerly, so
	// even an in-progress task's declared datasets are present. Without this the
	// lineage_datasets table is never written and /lineage/impact always returns
	// empty. The upsert keys on (task_run, namespace, name, direction), so
	// re-emitted events are idempotent.
	m.persistTaskDatasets(payload, inputs, outputs)

	return &RunEvent{
		EventTime: evt.Timestamp,
		EventType: EventTypeAbort,
		Producer:  producerURI,
		SchemaURL: schemaURL,
		Run: Run{
			RunID:  payload.ID,
			Facets: runFacets,
		},
		Job: Job{
			Namespace: m.namespace,
			Name:      taskJobName,
			Facets:    m.buildJobFacets(evt.JobID, "TASK"),
		},
		Inputs:  inputs,
		Outputs: outputs,
	}, nil
}

// --- Helpers ---

func (m *mapper) addExecutionFacet(facets map[string]interface{}, payload taskRunPayload) {
	facets["caesium_execution"] = CaesiumExecutionFacet{
		BaseFacet: newCaesiumBaseFacet("CaesiumExecutionFacet"),
		Engine:    payload.Engine,
		Image:     payload.Image,
		Command:   payload.Command,
		RuntimeID: payload.RuntimeID,
		ClaimedBy: payload.ClaimedBy,
	}
}

func (m *mapper) buildParentFacet(parentRunID uuid.UUID, parentJobName string) map[string]interface{} {
	return map[string]interface{}{
		"parent": ParentRunFacet{
			BaseFacet: newBaseFacet(parentFacetSchema),
			Run:       ParentRunRef{RunID: parentRunID},
			Job:       ParentJobRef{Namespace: m.namespace, Name: parentJobName},
		},
	}
}

func (m *mapper) resolveJobAlias(jobID uuid.UUID, hint string) string {
	if hint != "" {
		return hint
	}

	entry, ok := m.getJobInfo(jobID)
	if !ok || entry.alias == "" {
		return jobID.String()
	}
	return entry.alias
}

func (m *mapper) getJobInfo(jobID uuid.UUID) (jobCacheEntry, bool) {
	if entry, ok := m.cache.Get(jobID); ok {
		return entry, true
	}

	if m.db == nil {
		return jobCacheEntry{}, false
	}

	var job jobRecord
	if err := m.db.Select("alias", "provenance_repo", "provenance_ref", "provenance_commit", "provenance_path").
		Where("id = ?", jobID).First(&job).Error; err != nil {
		return jobCacheEntry{}, false
	}

	entry := jobCacheEntry{
		alias:            job.Alias,
		provenanceRepo:   job.ProvenanceRepo,
		provenanceRef:    job.ProvenanceRef,
		provenanceCommit: job.ProvenanceCommit,
		provenancePath:   job.ProvenancePath,
	}
	m.cache.Set(jobID, entry)
	return entry, true
}

func buildSourceCodeFacet(entry jobCacheEntry) SourceCodeLocationFacet {
	return SourceCodeLocationFacet{
		BaseFacet: newBaseFacet(sourceCodeFacetSchema),
		Type:      "git",
		URL:       entry.provenanceRepo,
		RepoURL:   entry.provenanceRepo,
		Branch:    entry.provenanceRef,
		Path:      entry.provenancePath,
		Version:   entry.provenanceCommit,
	}
}

func (m *mapper) buildJobFacets(jobID uuid.UUID, jobType string) map[string]interface{} {
	facets := map[string]interface{}{
		"jobType": JobTypeFacet{
			BaseFacet:      newBaseFacet(jobTypeFacetSchema),
			ProcessingType: "BATCH",
			Integration:    "CAESIUM",
			JobType:        jobType,
		},
	}

	if entry, ok := m.getJobInfo(jobID); ok && entry.provenanceRepo != "" {
		facets["sourceCodeLocation"] = buildSourceCodeFacet(entry)
	}

	return facets
}

// buildTaskDatasets derives OpenLineage Dataset entries from a task run's
// declared I/O contracts and structured outputs.
//
// Strategy (highest to lowest specificity):
//  1. Structured output values (##caesium::output) that look like a path or URI
//     are promoted to individual output Datasets — each key becomes a distinct
//     named dataset so consumers can track per-artifact lineage.
//  2. If outputSchema is declared but no path-like output values exist, a single
//     synthetic output Dataset is emitted using the step name so the job still
//     appears in the lineage graph.
//  3. For inputs, each predecessor name listed in inputSchema becomes an input
//     Dataset referencing that step's logical output namespace.
//
// taskRecord is the subset of the tasks table the lineage mapper reads to
// recover a task's step name and declared schemas (mirrors jobRecord's pattern
// of mapping a local struct to a table to avoid importing internal/models).
type taskRecord struct {
	Name         string `gorm:"column:name"`
	OutputSchema []byte `gorm:"column:output_schema"`
	InputSchema  []byte `gorm:"column:input_schema"`
}

func (taskRecord) TableName() string { return "tasks" }

// enrichTaskPayload fills the step name and declared input/output schemas from
// the persisted task record when the lifecycle event omitted them. The event
// payload carries execution state, not the static task definition, so these
// fields arrive empty — yet buildTaskDatasets needs them:
//   - Without the step name, datasets fall back to the task UUID and never link
//     across steps or jobs (a producer's `job.<uuid>.output` can't match a
//     consumer's `job.<stepName>.output`).
//   - Without the schemas, no input datasets are emitted at all, so the
//     cross-job impact query has no edges to traverse and always returns empty.
//
// On any lookup failure it leaves the payload as-is — the worst case is the
// pre-existing (degraded) behavior, never a wrong dataset.
func (m *mapper) enrichTaskPayload(payload *taskRunPayload) {
	if m.db == nil || payload.TaskID == uuid.Nil {
		return
	}
	if payload.TaskName != "" && payload.OutputSchema != nil && payload.InputSchema != nil {
		return
	}
	var rec taskRecord
	if err := m.db.Select("name", "output_schema", "input_schema").
		Where("id = ?", payload.TaskID).First(&rec).Error; err != nil {
		return
	}
	if payload.TaskName == "" {
		payload.TaskName = rec.Name
	}
	if payload.OutputSchema == nil && len(rec.OutputSchema) > 0 {
		var os map[string]interface{}
		if json.Unmarshal(rec.OutputSchema, &os) == nil {
			payload.OutputSchema = os
		}
	}
	if payload.InputSchema == nil && len(rec.InputSchema) > 0 {
		var is map[string]map[string]interface{}
		if json.Unmarshal(rec.InputSchema, &is) == nil {
			payload.InputSchema = is
		}
	}
}

// persistTaskDatasets writes the task's input/output datasets to the bounded
// lineage_datasets graph that the cross-job impact query (QueryImpact) reads.
// It is best-effort: any failure leaves the graph as-is rather than disrupting
// event emission. Rows are upserted on the (task_run, namespace, name,
// direction) unique key so re-emitted lifecycle events are idempotent.
func (m *mapper) persistTaskDatasets(payload taskRunPayload, inputs, outputs []Dataset) {
	if m.db == nil || (len(inputs) == 0 && len(outputs) == 0) {
		return
	}
	// payload.ID is the task_run PK (the FK target): every task-event publish
	// path sets it to the task run's own ID — the convert-based publishes and
	// recordTaskEventTx alike — and a retried attempt emits its own event with
	// its own ID, so this is already the right attempt. Use it directly rather
	// than a per-event SELECT on the hot lineage path.
	taskRunID := payload.ID
	if taskRunID == uuid.Nil {
		return
	}

	summary, _ := json.Marshal(map[string]string{"step_name": payload.TaskName})
	rows := make([]models.LineageDataset, 0, len(inputs)+len(outputs))
	// Deduplicate within the batch: distinct output keys can resolve to the same
	// dataset value (e.g. the same file path), and a duplicate
	// (task_run, namespace, name, direction) tuple would otherwise violate the
	// unique index within a single multi-row insert.
	seen := make(map[string]struct{}, len(inputs)+len(outputs))
	appendRow := func(ds Dataset, direction string) {
		if ds.Name == "" {
			return
		}
		key := ds.Namespace + "\x00" + ds.Name + "\x00" + direction
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		rows = append(rows, models.LineageDataset{
			ID:           uuid.New(),
			TaskRunID:    taskRunID,
			Namespace:    ds.Namespace,
			Name:         ds.Name,
			Direction:    direction,
			FacetSummary: datatypes.JSON(summary),
			CreatedAt:    time.Now().UTC(),
		})
	}
	for _, ds := range inputs {
		appendRow(ds, "input")
	}
	for _, ds := range outputs {
		appendRow(ds, "output")
	}
	if len(rows) == 0 {
		return
	}
	_ = m.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "task_run_id"}, {Name: "namespace"}, {Name: "name"}, {Name: "direction"},
		},
		DoNothing: true,
	}).Create(&rows).Error
}

// The namespace is always the mapper's configured namespace so datasets from
// the same Caesium instance share a namespace and can be joined across jobs.
func (m *mapper) buildTaskDatasets(jobAlias string, payload taskRunPayload) (inputs, outputs []Dataset) {
	stepName := payload.TaskName
	if stepName == "" {
		stepName = payload.TaskID.String()
	}

	// --- Outputs ---
	// Promote path/URI-like structured output values to individual Datasets.
	outputKeys := pathLikeKeys(payload.Output)
	if len(outputKeys) > 0 {
		for _, key := range outputKeys {
			value := payload.Output[key]
			datasetName := datasetNameFromValue(jobAlias, stepName, key, value)
			facets := map[string]interface{}{
				"caesium_dataset": CaesiumDatasetFacet{
					BaseFacet:  newCaesiumBaseFacet("CaesiumDatasetFacet"),
					StepName:   stepName,
					Direction:  "output",
					OutputKeys: []string{key},
				},
			}
			outputs = append(outputs, Dataset{
				Namespace: m.namespace,
				Name:      datasetName,
				Facets:    facets,
			})
		}
	} else if len(payload.OutputSchema) > 0 {
		// Declared schema but no path-like outputs: emit a synthetic dataset so
		// the step appears in the lineage graph.
		facets := map[string]interface{}{
			"caesium_dataset": CaesiumDatasetFacet{
				BaseFacet: newCaesiumBaseFacet("CaesiumDatasetFacet"),
				StepName:  stepName,
				Direction: "output",
			},
			"caesium_schema": CaesiumSchemaFacet{
				BaseFacet: newCaesiumBaseFacet("CaesiumSchemaFacet"),
				Schema:    payload.OutputSchema,
			},
		}
		outputs = append(outputs, Dataset{
			Namespace: m.namespace,
			Name:      jobAlias + "." + stepName + ".output",
			Facets:    facets,
		})
	}

	// --- Inputs ---
	// Each predecessor in inputSchema becomes an input Dataset, referencing the
	// predecessor step's logical output namespace within the same job.
	// Sort predecessor names so the Inputs slice order is deterministic.
	predNames := make([]string, 0, len(payload.InputSchema))
	for predStepName := range payload.InputSchema {
		predNames = append(predNames, predStepName)
	}
	sort.Strings(predNames)
	for _, predStepName := range predNames {
		schema := payload.InputSchema[predStepName]
		facets := map[string]interface{}{
			"caesium_dataset": CaesiumDatasetFacet{
				BaseFacet: newCaesiumBaseFacet("CaesiumDatasetFacet"),
				StepName:  predStepName,
				Direction: "input",
			},
		}
		if len(schema) > 0 {
			facets["caesium_schema"] = CaesiumSchemaFacet{
				BaseFacet: newCaesiumBaseFacet("CaesiumSchemaFacet"),
				Schema:    schema,
			}
		}
		inputs = append(inputs, Dataset{
			Namespace: m.namespace,
			Name:      jobAlias + "." + predStepName + ".output",
			Facets:    facets,
		})
	}

	// Ensure non-nil slices so JSON serializes as [] rather than null, which
	// is required by the OpenLineage RunEvent spec.
	if inputs == nil {
		inputs = []Dataset{}
	}
	if outputs == nil {
		outputs = []Dataset{}
	}
	return inputs, outputs
}

// pathLikeKeys returns the keys from output whose values look like file paths,
// URIs, or table references — the types of values that form meaningful dataset
// identities.  Scalar summaries (numbers, short words) are excluded to keep
// the dataset graph focused on structural lineage.  The returned slice is
// sorted so callers produce a deterministic Outputs order.
func pathLikeKeys(output map[string]string) []string {
	if len(output) == 0 {
		return nil
	}
	var keys []string
	for k, v := range output {
		if looksLikeDatasetRef(v) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// knownURISchemes are the URI prefixes that unambiguously identify a dataset
// reference.  Checked before the dotted-identifier heuristic.
var knownURISchemes = []string{
	"s3://", "gs://", "hdfs://", "file://", "abfs://", "az://",
	"http://", "https://",
}

// knownFileExtensions are suffixes that unambiguously mark a file path when
// combined with the dotted-identifier heuristic.
var knownFileExtensions = []string{
	".parquet", ".csv", ".json", ".jsonl", ".ndjson", ".avro", ".orc",
	".tsv", ".gz", ".zst", ".snappy", ".yaml", ".yml", ".xml", ".txt",
	".log", ".arrow", ".feather",
}

// looksLikeDatasetRef returns true when v appears to be a file path, URI,
// or dotted table name (schema.table or db.schema.table) rather than a plain
// scalar value such as a decimal number, version string, IP address, or
// relative dot-path.
//
// Inclusion rules (checked in order):
//  1. Absolute file path starting with '/'.
//  2. Known URI scheme prefix (s3://, gs://, hdfs://, file://, etc.).
//  3. Dotted identifier: has a dot, no whitespace, not purely numeric,
//     not a version string (all segments are digits), not an IP address
//     (four numeric octets), no leading dot (relative / hidden paths),
//     and either has a known file extension OR every dot-segment starts
//     with a letter (table names like db.schema.table).
func looksLikeDatasetRef(v string) bool {
	if len(v) == 0 {
		return false
	}

	// Rule 1: absolute file path.
	if v[0] == '/' {
		return true
	}

	// Rule 2: known URI scheme.
	for _, scheme := range knownURISchemes {
		if strings.HasPrefix(v, scheme) && len(v) > len(scheme) {
			return true
		}
	}

	// Rule 3: dotted identifier heuristic — must have a dot, no whitespace,
	// reasonable length, and pass the false-positive filters below.
	if len(v) > 255 {
		return false
	}
	dotIdx := strings.IndexByte(v, '.')
	if dotIdx < 0 {
		return false // no dot at all
	}
	if strings.ContainsAny(v, " \t\n\r") {
		return false
	}
	// Leading dot: relative path (./foo), hidden file (.hidden) — exclude.
	if v[0] == '.' {
		return false
	}

	// Known file extension: accept even if segments contain digits.
	for _, ext := range knownFileExtensions {
		if strings.HasSuffix(v, ext) {
			return true
		}
	}

	// Split on dots and inspect segments.
	segments := strings.Split(v, ".")
	allDigitSegments := true
	allLetterStart := true
	for _, seg := range segments {
		if len(seg) == 0 {
			return false // consecutive dots or trailing dot
		}
		if !isAllDigits(seg) {
			allDigitSegments = false
		}
		if !isLetter(rune(seg[0])) {
			allLetterStart = false
		}
	}

	// Pure-numeric: decimal (3.14), version (1.2.3), IP (192.168.1.1) — exclude.
	if allDigitSegments {
		return false
	}

	// Accept dotted identifiers where every segment starts with a letter
	// (database.schema.table, analytics.public.fact_orders, etc.).
	return allLetterStart
}

// isAllDigits returns true if s contains only ASCII digit characters.
func isAllDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isLetter returns true if c is an ASCII letter (a–z, A–Z).
func isLetter(c rune) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// datasetNameFromValue builds a human-readable dataset name from the output
// value.  If the value itself is short enough to be meaningful, it is used
// directly.  Otherwise the job+step+key triple forms the name.
func datasetNameFromValue(jobAlias, stepName, key, value string) string {
	const maxValueLen = 256
	if len(value) > 0 && len(value) <= maxValueLen {
		return value
	}
	return jobAlias + "." + stepName + "." + key
}
