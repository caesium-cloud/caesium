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

// --- Dataset population tests ---

// TestTaskCompleteWithPathOutput asserts that a task with a path-like structured
// output produces a non-empty Outputs slice and an empty Inputs slice.
func TestTaskCompleteWithPathOutput(t *testing.T) {
	m := newMapper("caesium-test", nil)
	payload := testTaskRunPayload()
	payload.Status = "succeeded"
	payload.TaskName = "extract"
	payload.Output = map[string]string{
		"output_path": "/data/warehouse/extract/2026-06-19.parquet",
	}

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

	if len(olEvent.Outputs) == 0 {
		t.Fatal("expected non-empty Outputs for a step with a path-like output value")
	}
	if len(olEvent.Inputs) != 0 {
		t.Errorf("expected empty Inputs, got %d", len(olEvent.Inputs))
	}

	out := olEvent.Outputs[0]
	if out.Name != "/data/warehouse/extract/2026-06-19.parquet" {
		t.Errorf("output dataset name = %v, want the file path", out.Name)
	}
	if out.Namespace != "caesium-test" {
		t.Errorf("output namespace = %v, want caesium-test", out.Namespace)
	}
	df, ok := out.Facets["caesium_dataset"].(CaesiumDatasetFacet)
	if !ok {
		t.Fatal("missing or wrong type for caesium_dataset facet on output dataset")
	}
	if df.Direction != "output" {
		t.Errorf("direction = %v, want output", df.Direction)
	}
	if df.StepName != "extract" {
		t.Errorf("stepName = %v, want extract", df.StepName)
	}
}

// TestTaskStartWithDeclaredOutputSchema asserts that a task with only a declared
// outputSchema (no path-like output values) emits a synthetic output Dataset.
func TestTaskStartWithDeclaredOutputSchema(t *testing.T) {
	m := newMapper("caesium-test", nil)
	payload := testTaskRunPayload()
	payload.TaskName = "transform"
	payload.OutputSchema = map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"row_count": map[string]interface{}{"type": "integer"},
		},
	}

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

	if len(olEvent.Outputs) == 0 {
		t.Fatal("expected a synthetic output Dataset for a step with outputSchema")
	}
	out := olEvent.Outputs[0]
	expectedName := uuid.MustParse("bbbb0000-0000-0000-0000-000000000001").String() + ".transform.output"
	if out.Name != expectedName {
		t.Errorf("synthetic output name = %v, want %v", out.Name, expectedName)
	}
	if _, ok := out.Facets["caesium_schema"]; !ok {
		t.Error("expected caesium_schema facet on synthetic output dataset")
	}
}

// TestTaskStartWithInputSchema asserts that a task with a declared inputSchema
// emits non-empty Inputs, one per predecessor step.
func TestTaskStartWithInputSchema(t *testing.T) {
	m := newMapper("caesium-test", nil)
	payload := testTaskRunPayload()
	payload.TaskName = "load"
	payload.InputSchema = map[string]map[string]interface{}{
		"transform": {
			"type": "object",
			"properties": map[string]interface{}{
				"row_count": map[string]interface{}{"type": "integer"},
			},
		},
	}

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

	if len(olEvent.Inputs) == 0 {
		t.Fatal("expected non-empty Inputs for a step with inputSchema")
	}
	inp := olEvent.Inputs[0]
	if inp.Namespace != "caesium-test" {
		t.Errorf("input namespace = %v, want caesium-test", inp.Namespace)
	}
	// The input dataset name references the predecessor step's output.
	jobID := uuid.MustParse("bbbb0000-0000-0000-0000-000000000001")
	expectedName := jobID.String() + ".transform.output"
	if inp.Name != expectedName {
		t.Errorf("input dataset name = %v, want %v", inp.Name, expectedName)
	}
	df, ok := inp.Facets["caesium_dataset"].(CaesiumDatasetFacet)
	if !ok {
		t.Fatal("missing or wrong type for caesium_dataset facet on input dataset")
	}
	if df.Direction != "input" {
		t.Errorf("direction = %v, want input", df.Direction)
	}
}

// TestTaskNoIOYieldsEmptyDatasets asserts that a task with no output, no
// outputSchema, and no inputSchema produces empty (non-nil) slices so the JSON
// serializes as [] not null.
func TestTaskNoIOYieldsEmptyDatasets(t *testing.T) {
	m := newMapper("caesium-test", nil)
	payload := testTaskRunPayload()

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

	if olEvent.Inputs == nil {
		t.Error("Inputs must be non-nil (serialize as [] not null)")
	}
	if olEvent.Outputs == nil {
		t.Error("Outputs must be non-nil (serialize as [] not null)")
	}
	if len(olEvent.Inputs) != 0 {
		t.Errorf("expected 0 Inputs, got %d", len(olEvent.Inputs))
	}
	if len(olEvent.Outputs) != 0 {
		t.Errorf("expected 0 Outputs, got %d", len(olEvent.Outputs))
	}
}

// TestTaskWithS3OutputPath asserts that an S3-scheme value is recognised as a
// path-like dataset reference.
func TestTaskWithS3OutputPath(t *testing.T) {
	m := newMapper("caesium-test", nil)
	payload := testTaskRunPayload()
	payload.TaskName = "export"
	payload.Output = map[string]string{
		"s3_path": "s3://my-bucket/output/2026-06-19/data.csv",
	}

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

	if len(olEvent.Outputs) == 0 {
		t.Fatal("expected non-empty Outputs for an S3 path output value")
	}
	if olEvent.Outputs[0].Name != "s3://my-bucket/output/2026-06-19/data.csv" {
		t.Errorf("output name = %v", olEvent.Outputs[0].Name)
	}
}

// TestTaskWithDottedTableName asserts that a dotted "schema.table" value is
// recognised as a dataset reference.
func TestTaskWithDottedTableName(t *testing.T) {
	m := newMapper("caesium-test", nil)
	payload := testTaskRunPayload()
	payload.TaskName = "load"
	payload.Output = map[string]string{
		"target_table": "analytics.public.fact_orders",
	}

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

	if len(olEvent.Outputs) == 0 {
		t.Fatal("expected non-empty Outputs for a dotted table name output value")
	}
	if olEvent.Outputs[0].Name != "analytics.public.fact_orders" {
		t.Errorf("output name = %v", olEvent.Outputs[0].Name)
	}
}

// TestLooksLikeDatasetRef exercises the heuristic directly.
func TestLooksLikeDatasetRef(t *testing.T) {
	cases := []struct {
		value string
		want  bool
	}{
		{"/data/output/file.parquet", true},
		{"s3://bucket/key", true},
		{"gs://bucket/key", true},
		{"hdfs://namenode/path", true},
		{"file:///tmp/out.csv", true},
		{"analytics.public.fact_orders", true},
		{"db.schema.table", true},
		{"1234567", false},          // scalar number
		{"succeeded", false},        // plain word
		{"hello world", false},      // has space
		{"", false},                 // empty
		{"nodot", false},            // no dot, no scheme, no leading slash
	}
	for _, tc := range cases {
		got := looksLikeDatasetRef(tc.value)
		if got != tc.want {
			t.Errorf("looksLikeDatasetRef(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}
