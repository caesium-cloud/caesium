package lineage

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRunEventSerialization(t *testing.T) {
	runID := uuid.MustParse("d46e465b-d358-4d32-83d4-df660ff614dd")
	eventTime := time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)

	event := RunEvent{
		EventTime: eventTime,
		EventType: EventTypeStart,
		Producer:  producerURI,
		SchemaURL: schemaURL,
		Run: Run{
			RunID:  runID,
			Facets: map[string]interface{}{},
		},
		Job: Job{
			Namespace: "caesium-prod",
			Name:      "my_pipeline",
			Facets:    map[string]interface{}{},
		},
		Inputs:  []Dataset{},
		Outputs: []Dataset{},
	}

	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if parsed["eventType"] != "START" {
		t.Errorf("eventType = %v, want START", parsed["eventType"])
	}
	if parsed["producer"] != producerURI {
		t.Errorf("producer = %v, want %v", parsed["producer"], producerURI)
	}
	if parsed["schemaURL"] != schemaURL {
		t.Errorf("schemaURL = %v, want %v", parsed["schemaURL"], schemaURL)
	}

	run := parsed["run"].(map[string]interface{})
	if run["runId"] != runID.String() {
		t.Errorf("run.runId = %v, want %v", run["runId"], runID.String())
	}

	job := parsed["job"].(map[string]interface{})
	if job["namespace"] != "caesium-prod" {
		t.Errorf("job.namespace = %v, want caesium-prod", job["namespace"])
	}
	if job["name"] != "my_pipeline" {
		t.Errorf("job.name = %v, want my_pipeline", job["name"])
	}
}

func TestParentRunFacetSerialization(t *testing.T) {
	parentRunID := uuid.MustParse("aaa00000-0000-0000-0000-000000000001")

	facet := ParentRunFacet{
		BaseFacet: newBaseFacet("https://openlineage.io/spec/facets/1-0-1/ParentRunFacet.json"),
		Run:       ParentRunRef{RunID: parentRunID},
		Job:       ParentJobRef{Namespace: "caesium-prod", Name: "my_pipeline"},
	}

	data, err := json.Marshal(facet)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if parsed["_producer"] != producerURI {
		t.Errorf("_producer = %v, want %v", parsed["_producer"], producerURI)
	}
	if parsed["_schemaURL"] == nil || parsed["_schemaURL"] == "" {
		t.Error("_schemaURL must be present")
	}

	run := parsed["run"].(map[string]interface{})
	if run["runId"] != parentRunID.String() {
		t.Errorf("run.runId = %v, want %v", run["runId"], parentRunID.String())
	}

	job := parsed["job"].(map[string]interface{})
	if job["namespace"] != "caesium-prod" {
		t.Errorf("job.namespace = %v, want caesium-prod", job["namespace"])
	}
}

func TestErrorMessageFacetSerialization(t *testing.T) {
	facet := ErrorMessageFacet{
		BaseFacet:           newBaseFacet("https://openlineage.io/spec/facets/1-0-1/ErrorMessageRunFacet.json"),
		Message:             "connection refused",
		ProgrammingLanguage: "go",
		StackTrace:          "goroutine 1 [running]:\nmain.run()",
	}

	data, err := json.Marshal(facet)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if parsed["message"] != "connection refused" {
		t.Errorf("message = %v, want 'connection refused'", parsed["message"])
	}
	if parsed["programmingLanguage"] != "go" {
		t.Errorf("programmingLanguage = %v, want 'go'", parsed["programmingLanguage"])
	}
	if parsed["_producer"] != producerURI {
		t.Error("_producer must be present")
	}
}

func TestJobTypeFacetSerialization(t *testing.T) {
	facet := JobTypeFacet{
		BaseFacet:      newBaseFacet("https://openlineage.io/spec/facets/2-0-3/JobTypeJobFacet.json"),
		ProcessingType: "BATCH",
		Integration:    "CAESIUM",
		JobType:        "JOB",
	}

	data, err := json.Marshal(facet)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if parsed["processingType"] != "BATCH" {
		t.Errorf("processingType = %v, want BATCH", parsed["processingType"])
	}
	if parsed["integration"] != "CAESIUM" {
		t.Errorf("integration = %v, want CAESIUM", parsed["integration"])
	}
	if parsed["jobType"] != "JOB" {
		t.Errorf("jobType = %v, want JOB", parsed["jobType"])
	}
}

func TestAllEventTypes(t *testing.T) {
	types := []EventType{
		EventTypeStart,
		EventTypeRunning,
		EventTypeComplete,
		EventTypeFail,
		EventTypeAbort,
	}

	expected := []string{"START", "RUNNING", "COMPLETE", "FAIL", "ABORT"}

	for i, et := range types {
		if string(et) != expected[i] {
			t.Errorf("EventType[%d] = %v, want %v", i, et, expected[i])
		}
	}
}
