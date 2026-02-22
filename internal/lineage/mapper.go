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
		Inputs:  []Dataset{},
		Outputs: []Dataset{},
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
		Inputs:  []Dataset{},
		Outputs: []Dataset{},
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
		Inputs:  []Dataset{},
		Outputs: []Dataset{},
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
		Inputs:  []Dataset{},
		Outputs: []Dataset{},
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
