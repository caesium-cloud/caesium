package lineage

import (
	"encoding/json"
	"testing"
)

func TestCaesiumExecutionFacetSerialization(t *testing.T) {
	facet := CaesiumExecutionFacet{
		BaseFacet: newCaesiumBaseFacet("CaesiumExecutionFacet"),
		Engine:    "docker",
		Image:     "python:3.12",
		Command:   []string{"python", "transform.py"},
		RuntimeID: "abc123",
		ClaimedBy: "worker-1",
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
	if parsed["engine"] != "docker" {
		t.Errorf("engine = %v, want docker", parsed["engine"])
	}
	if parsed["image"] != "python:3.12" {
		t.Errorf("image = %v, want python:3.12", parsed["image"])
	}
	if parsed["runtimeId"] != "abc123" {
		t.Errorf("runtimeId = %v, want abc123", parsed["runtimeId"])
	}
	if parsed["claimedBy"] != "worker-1" {
		t.Errorf("claimedBy = %v, want worker-1", parsed["claimedBy"])
	}
}

func TestCaesiumDAGFacetSerialization(t *testing.T) {
	facet := CaesiumDAGFacet{
		BaseFacet:     newCaesiumBaseFacet("CaesiumDAGFacet"),
		TotalTasks:    5,
		TriggerType:   "cron",
		TriggerAlias:  "nightly-sync",
		FailurePolicy: "halt",
		ExecutionMode: "local",
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
	if parsed["totalTasks"].(float64) != 5 {
		t.Errorf("totalTasks = %v, want 5", parsed["totalTasks"])
	}
	if parsed["triggerType"] != "cron" {
		t.Errorf("triggerType = %v, want cron", parsed["triggerType"])
	}
	if parsed["failurePolicy"] != "halt" {
		t.Errorf("failurePolicy = %v, want halt", parsed["failurePolicy"])
	}
}

func TestCaesiumProvenanceFacetSerialization(t *testing.T) {
	facet := CaesiumProvenanceFacet{
		BaseFacet: newCaesiumBaseFacet("CaesiumProvenanceFacet"),
		SourceID:  "git-sync-1",
		Repo:      "https://github.com/test/repo",
		Ref:       "refs/heads/main",
		Commit:    "abc123def",
		Path:      "jobs/etl.yaml",
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
	if parsed["sourceId"] != "git-sync-1" {
		t.Errorf("sourceId = %v, want git-sync-1", parsed["sourceId"])
	}
	if parsed["repo"] != "https://github.com/test/repo" {
		t.Errorf("repo = %v", parsed["repo"])
	}
	if parsed["commit"] != "abc123def" {
		t.Errorf("commit = %v, want abc123def", parsed["commit"])
	}
}

func TestCaesiumBaseFacetSchemaURL(t *testing.T) {
	base := newCaesiumBaseFacet("CaesiumExecutionFacet")
	expected := caesiumFacetSchemaBase + "/CaesiumExecutionFacet.json"
	if base.SchemaURL != expected {
		t.Errorf("schemaURL = %v, want %v", base.SchemaURL, expected)
	}
}
