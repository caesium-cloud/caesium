package lineage

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/google/uuid"
)

func mustMarshal(t *testing.T, v interface{}) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

func testJobRunPayload() jobRunPayload {
	return jobRunPayload{
		ID:       uuid.MustParse("aaaa0000-0000-0000-0000-000000000001"),
		JobID:    uuid.MustParse("bbbb0000-0000-0000-0000-000000000001"),
		JobAlias: "etl-pipeline",
		Status:   "running",
	}
}

func testTaskRunPayload() taskRunPayload {
	return taskRunPayload{
		ID:       uuid.MustParse("cccc0000-0000-0000-0000-000000000001"),
		JobRunID: uuid.MustParse("aaaa0000-0000-0000-0000-000000000001"),
		TaskID:   uuid.MustParse("dddd0000-0000-0000-0000-000000000001"),
		AtomID:   uuid.MustParse("eeee0000-0000-0000-0000-000000000001"),
		Engine:   "docker",
		Image:    "python:3.12",
		Command:  []string{"python", "transform.py"},
		Status:   "running",
	}
}

func TestMapRunStart(t *testing.T) {
	m := newMapper("caesium-test", nil)
	payload := testJobRunPayload()

	evt := event.Event{
		Type:      event.TypeRunStarted,
		JobID:     payload.JobID,
		RunID:     payload.ID,
		Timestamp: time.Now().UTC(),
		Payload:   mustMarshal(t, payload),
	}

	olEvent, err := m.mapEvent(evt)
	if err != nil {
		t.Fatalf("mapEvent: %v", err)
	}

	if olEvent.EventType != EventTypeStart {
		t.Errorf("eventType = %v, want START", olEvent.EventType)
	}
	if olEvent.Job.Name != "etl-pipeline" {
		t.Errorf("job.name = %v, want etl-pipeline", olEvent.Job.Name)
	}
	if olEvent.Job.Namespace != "caesium-test" {
		t.Errorf("job.namespace = %v, want caesium-test", olEvent.Job.Namespace)
	}
	if olEvent.Run.RunID != payload.ID {
		t.Errorf("run.runId = %v, want %v", olEvent.Run.RunID, payload.ID)
	}
	if olEvent.Producer != producerURI {
		t.Errorf("producer = %v, want %v", olEvent.Producer, producerURI)
	}

	if _, ok := olEvent.Job.Facets["jobType"]; !ok {
		t.Error("missing jobType job facet")
	}
}

func TestMapRunComplete(t *testing.T) {
	m := newMapper("caesium-test", nil)
	payload := testJobRunPayload()
	payload.Status = "succeeded"

	evt := event.Event{
		Type:      event.TypeRunCompleted,
		JobID:     payload.JobID,
		RunID:     payload.ID,
		Timestamp: time.Now().UTC(),
		Payload:   mustMarshal(t, payload),
	}

	olEvent, err := m.mapEvent(evt)
	if err != nil {
		t.Fatalf("mapEvent: %v", err)
	}

	if olEvent.EventType != EventTypeComplete {
		t.Errorf("eventType = %v, want COMPLETE", olEvent.EventType)
	}
}

func TestMapRunFail(t *testing.T) {
	m := newMapper("caesium-test", nil)
	payload := testJobRunPayload()
	payload.Status = "failed"
	payload.Error = "task xyz failed: exit code 1"

	evt := event.Event{
		Type:      event.TypeRunFailed,
		JobID:     payload.JobID,
		RunID:     payload.ID,
		Timestamp: time.Now().UTC(),
		Payload:   mustMarshal(t, payload),
	}

	olEvent, err := m.mapEvent(evt)
	if err != nil {
		t.Fatalf("mapEvent: %v", err)
	}

	if olEvent.EventType != EventTypeFail {
		t.Errorf("eventType = %v, want FAIL", olEvent.EventType)
	}

	errorFacet, ok := olEvent.Run.Facets["errorMessage"]
	if !ok {
		t.Fatal("missing errorMessage run facet")
	}
	emf, ok := errorFacet.(ErrorMessageFacet)
	if !ok {
		t.Fatal("errorMessage facet is not ErrorMessageFacet")
	}
	if emf.Message != "task xyz failed: exit code 1" {
		t.Errorf("error message = %v, want 'task xyz failed: exit code 1'", emf.Message)
	}
	if emf.ProgrammingLanguage != "go" {
		t.Errorf("programmingLanguage = %v, want 'go'", emf.ProgrammingLanguage)
	}
}

func TestMapRunFailNoError(t *testing.T) {
	m := newMapper("caesium-test", nil)
	payload := testJobRunPayload()
	payload.Status = "failed"
	payload.Error = ""

	evt := event.Event{
		Type:      event.TypeRunFailed,
		JobID:     payload.JobID,
		RunID:     payload.ID,
		Timestamp: time.Now().UTC(),
		Payload:   mustMarshal(t, payload),
	}

	olEvent, err := m.mapEvent(evt)
	if err != nil {
		t.Fatalf("mapEvent: %v", err)
	}

	if _, ok := olEvent.Run.Facets["errorMessage"]; ok {
		t.Error("errorMessage facet should not be present when error is empty")
	}
}

func TestMapTaskStart(t *testing.T) {
	m := newMapper("caesium-test", nil)
	payload := testTaskRunPayload()

	jobID := uuid.MustParse("bbbb0000-0000-0000-0000-000000000001")
	runID := payload.JobRunID

	evt := event.Event{
		Type:      event.TypeTaskStarted,
		JobID:     jobID,
		RunID:     runID,
		TaskID:    payload.TaskID,
		Timestamp: time.Now().UTC(),
		Payload:   mustMarshal(t, payload),
	}

	olEvent, err := m.mapEvent(evt)
	if err != nil {
		t.Fatalf("mapEvent: %v", err)
	}

	if olEvent.EventType != EventTypeStart {
		t.Errorf("eventType = %v, want START", olEvent.EventType)
	}

	if olEvent.Run.RunID != payload.ID {
		t.Errorf("run.runId = %v, want %v (TaskRun ID)", olEvent.Run.RunID, payload.ID)
	}

	expectedJobName := jobID.String() + ".task." + payload.TaskID.String()
	if olEvent.Job.Name != expectedJobName {
		t.Errorf("job.name = %v, want %v", olEvent.Job.Name, expectedJobName)
	}

	parentFacet, ok := olEvent.Run.Facets["parent"]
	if !ok {
		t.Fatal("missing parent run facet")
	}
	pf, ok := parentFacet.(ParentRunFacet)
	if !ok {
		t.Fatal("parent facet is not ParentRunFacet")
	}
	if pf.Run.RunID != runID {
		t.Errorf("parent run.runId = %v, want %v", pf.Run.RunID, runID)
	}
	if pf.Job.Namespace != "caesium-test" {
		t.Errorf("parent job.namespace = %v, want caesium-test", pf.Job.Namespace)
	}
}

func TestMapTaskComplete(t *testing.T) {
	m := newMapper("caesium-test", nil)
	payload := testTaskRunPayload()
	payload.Status = "succeeded"

	evt := event.Event{
		Type:      event.TypeTaskSucceeded,
		JobID:     uuid.MustParse("bbbb0000-0000-0000-0000-000000000001"),
		RunID:     payload.JobRunID,
		TaskID:    payload.TaskID,
		Timestamp: time.Now().UTC(),
		Payload:   mustMarshal(t, payload),
	}

	olEvent, err := m.mapEvent(evt)
	if err != nil {
		t.Fatalf("mapEvent: %v", err)
	}

	if olEvent.EventType != EventTypeComplete {
		t.Errorf("eventType = %v, want COMPLETE", olEvent.EventType)
	}
	if olEvent.Run.RunID != payload.ID {
		t.Errorf("run.runId = %v, want %v", olEvent.Run.RunID, payload.ID)
	}

	if _, ok := olEvent.Run.Facets["parent"]; !ok {
		t.Error("missing parent run facet on task complete")
	}
}

func TestMapTaskFail(t *testing.T) {
	m := newMapper("caesium-test", nil)
	payload := testTaskRunPayload()
	payload.Status = "failed"
	payload.Error = "container exited with code 137"

	evt := event.Event{
		Type:      event.TypeTaskFailed,
		JobID:     uuid.MustParse("bbbb0000-0000-0000-0000-000000000001"),
		RunID:     payload.JobRunID,
		TaskID:    payload.TaskID,
		Timestamp: time.Now().UTC(),
		Payload:   mustMarshal(t, payload),
	}

	olEvent, err := m.mapEvent(evt)
	if err != nil {
		t.Fatalf("mapEvent: %v", err)
	}

	if olEvent.EventType != EventTypeFail {
		t.Errorf("eventType = %v, want FAIL", olEvent.EventType)
	}

	if _, ok := olEvent.Run.Facets["parent"]; !ok {
		t.Error("missing parent run facet")
	}

	errorFacet, ok := olEvent.Run.Facets["errorMessage"]
	if !ok {
		t.Fatal("missing errorMessage run facet")
	}
	emf, ok := errorFacet.(ErrorMessageFacet)
	if !ok {
		t.Fatal("errorMessage facet is not ErrorMessageFacet")
	}
	if emf.Message != "container exited with code 137" {
		t.Errorf("error message = %v", emf.Message)
	}
}

func TestMapTaskSkipped(t *testing.T) {
	m := newMapper("caesium-test", nil)
	payload := testTaskRunPayload()
	payload.Status = "skipped"

	evt := event.Event{
		Type:      event.TypeTaskSkipped,
		JobID:     uuid.MustParse("bbbb0000-0000-0000-0000-000000000001"),
		RunID:     payload.JobRunID,
		TaskID:    payload.TaskID,
		Timestamp: time.Now().UTC(),
		Payload:   mustMarshal(t, payload),
	}

	olEvent, err := m.mapEvent(evt)
	if err != nil {
		t.Fatalf("mapEvent: %v", err)
	}

	if olEvent.EventType != EventTypeAbort {
		t.Errorf("eventType = %v, want ABORT", olEvent.EventType)
	}

	if _, ok := olEvent.Run.Facets["parent"]; !ok {
		t.Error("missing parent run facet on task abort")
	}
}

func TestMapUnknownEventReturnsNil(t *testing.T) {
	m := newMapper("caesium-test", nil)

	evt := event.Event{
		Type:      event.TypeLogChunk,
		Timestamp: time.Now().UTC(),
	}

	olEvent, err := m.mapEvent(evt)
	if err != nil {
		t.Fatalf("mapEvent: %v", err)
	}
	if olEvent != nil {
		t.Error("expected nil for unknown event type")
	}
}

func TestResolveJobAliasFallsBackToID(t *testing.T) {
	m := newMapper("caesium-test", nil)
	jobID := uuid.MustParse("bbbb0000-0000-0000-0000-000000000001")

	alias := m.resolveJobAlias(jobID, "")
	if alias != jobID.String() {
		t.Errorf("alias = %v, want %v", alias, jobID.String())
	}
}

func TestResolveJobAliasUsesHint(t *testing.T) {
	m := newMapper("caesium-test", nil)

	alias := m.resolveJobAlias(uuid.New(), "my-pipeline")
	if alias != "my-pipeline" {
		t.Errorf("alias = %v, want my-pipeline", alias)
	}
}

func TestRunStartHasDAGFacet(t *testing.T) {
	m := newMapper("caesium-test", nil)
	payload := testJobRunPayload()
	payload.TriggerType = "cron"
	payload.TriggerAlias = "nightly"

	evt := event.Event{
		Type:      event.TypeRunStarted,
		JobID:     payload.JobID,
		RunID:     payload.ID,
		Timestamp: time.Now().UTC(),
		Payload:   mustMarshal(t, payload),
	}

	olEvent, err := m.mapEvent(evt)
	if err != nil {
		t.Fatalf("mapEvent: %v", err)
	}

	dagFacet, ok := olEvent.Run.Facets["caesium_dag"]
	if !ok {
		t.Fatal("missing caesium_dag run facet")
	}
	df, ok := dagFacet.(CaesiumDAGFacet)
	if !ok {
		t.Fatal("caesium_dag is not CaesiumDAGFacet")
	}
	if df.TriggerType != "cron" {
		t.Errorf("triggerType = %v, want cron", df.TriggerType)
	}
	if df.TriggerAlias != "nightly" {
		t.Errorf("triggerAlias = %v, want nightly", df.TriggerAlias)
	}
	if df.Producer != producerURI {
		t.Error("_producer must be set on custom facet")
	}
}

func TestTaskStartHasExecutionFacet(t *testing.T) {
	m := newMapper("caesium-test", nil)
	payload := testTaskRunPayload()
	payload.RuntimeID = "abc123"
	payload.ClaimedBy = "worker-1"

	evt := event.Event{
		Type:      event.TypeTaskStarted,
		JobID:     uuid.MustParse("bbbb0000-0000-0000-0000-000000000001"),
		RunID:     payload.JobRunID,
		TaskID:    payload.TaskID,
		Timestamp: time.Now().UTC(),
		Payload:   mustMarshal(t, payload),
	}

	olEvent, err := m.mapEvent(evt)
	if err != nil {
		t.Fatalf("mapEvent: %v", err)
	}

	execFacet, ok := olEvent.Run.Facets["caesium_execution"]
	if !ok {
		t.Fatal("missing caesium_execution run facet")
	}
	ef, ok := execFacet.(CaesiumExecutionFacet)
	if !ok {
		t.Fatal("caesium_execution is not CaesiumExecutionFacet")
	}
	if ef.Engine != "docker" {
		t.Errorf("engine = %v, want docker", ef.Engine)
	}
	if ef.Image != "python:3.12" {
		t.Errorf("image = %v, want python:3.12", ef.Image)
	}
	if ef.RuntimeID != "abc123" {
		t.Errorf("runtimeId = %v, want abc123", ef.RuntimeID)
	}
	if ef.ClaimedBy != "worker-1" {
		t.Errorf("claimedBy = %v, want worker-1", ef.ClaimedBy)
	}
	if ef.Producer != producerURI {
		t.Error("_producer must be set on custom facet")
	}
}

func TestCachePopulatedOnResolve(t *testing.T) {
	m := newMapper("caesium-test", nil)
	jobID := uuid.MustParse("bbbb0000-0000-0000-0000-000000000001")

	alias1 := m.resolveJobAlias(jobID, "")
	if alias1 != jobID.String() {
		t.Errorf("alias1 = %v, want %v", alias1, jobID.String())
	}

	m.cache.Set(jobID, jobCacheEntry{alias: "cached-pipeline"})

	alias2 := m.resolveJobAlias(jobID, "")
	if alias2 != "cached-pipeline" {
		t.Errorf("alias2 = %v, want cached-pipeline", alias2)
	}

	alias3 := m.resolveJobAlias(jobID, "explicit-hint")
	if alias3 != "explicit-hint" {
		t.Errorf("alias3 = %v, want explicit-hint", alias3)
	}
}
