package lineage

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/google/uuid"
	"gorm.io/gorm"
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

	jobAlias := m.resolveJobAlias(evt.JobID, "")
	taskJobName := fmt.Sprintf("%s.task.%s", jobAlias, payload.TaskID)

	runFacets := m.buildParentFacet(evt.RunID, jobAlias)
	m.addExecutionFacet(runFacets, payload)

	inputs, outputs := m.buildTaskDatasets(jobAlias, payload)

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

	jobAlias := m.resolveJobAlias(evt.JobID, "")
	taskJobName := fmt.Sprintf("%s.task.%s", jobAlias, payload.TaskID)

	runFacets := m.buildParentFacet(evt.RunID, jobAlias)
	m.addExecutionFacet(runFacets, payload)

	inputs, outputs := m.buildTaskDatasets(jobAlias, payload)

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

	jobAlias := m.resolveJobAlias(evt.JobID, "")
	taskJobName := fmt.Sprintf("%s.task.%s", jobAlias, payload.TaskID)

	runFacets := m.buildParentFacet(evt.RunID, jobAlias)
	m.addExecutionFacet(runFacets, payload)

	inputs, outputs := m.buildTaskDatasets(jobAlias, payload)

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
	for predStepName, schema := range payload.InputSchema {
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
// the dataset graph focused on structural lineage.
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
	return keys
}

// looksLikeDatasetRef returns true when v appears to be a file path, URI,
// or dotted table name (schema.table or db.schema.table) rather than a plain
// scalar.
func looksLikeDatasetRef(v string) bool {
	if len(v) == 0 {
		return false
	}
	// Absolute file path.
	if v[0] == '/' {
		return true
	}
	// URI schemes: s3://, gs://, hdfs://, file://, etc.
	for _, scheme := range []string{"s3://", "gs://", "hdfs://", "file://", "abfs://", "az://", "http://", "https://"} {
		if len(v) > len(scheme) && v[:len(scheme)] == scheme {
			return true
		}
	}
	// Dotted table reference (at least one dot, no spaces, reasonable length).
	if len(v) < 256 {
		hasDot := false
		hasSpace := false
		for _, c := range v {
			if c == '.' {
				hasDot = true
			}
			if c == ' ' || c == '\t' || c == '\n' {
				hasSpace = true
				break
			}
		}
		if hasDot && !hasSpace {
			return true
		}
	}
	return false
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
