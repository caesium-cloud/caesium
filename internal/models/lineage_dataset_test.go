package models

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

func TestLineageDatasetFields(t *testing.T) {
	taskRunID := uuid.New()
	id := uuid.New()
	now := time.Now().UTC()

	summary := map[string]interface{}{
		"stepName":   "extract",
		"outputKeys": []string{"output_path"},
	}
	summaryBytes, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}

	d := LineageDataset{
		ID:           id,
		TaskRunID:    taskRunID,
		Namespace:    "caesium-test",
		Name:         "/data/warehouse/extract/2026-06-19.parquet",
		Direction:    "output",
		FacetSummary: datatypes.JSON(summaryBytes),
		CreatedAt:    now,
	}

	if d.ID != id {
		t.Errorf("ID = %v, want %v", d.ID, id)
	}
	if d.TaskRunID != taskRunID {
		t.Errorf("TaskRunID = %v, want %v", d.TaskRunID, taskRunID)
	}
	if d.Namespace != "caesium-test" {
		t.Errorf("Namespace = %v, want caesium-test", d.Namespace)
	}
	if d.Name != "/data/warehouse/extract/2026-06-19.parquet" {
		t.Errorf("Name = %v", d.Name)
	}
	if d.Direction != "output" {
		t.Errorf("Direction = %v, want output", d.Direction)
	}
	if len(d.FacetSummary) == 0 {
		t.Error("FacetSummary must not be empty")
	}

	// Verify the summary round-trips cleanly.
	var parsed map[string]interface{}
	if err := json.Unmarshal(d.FacetSummary, &parsed); err != nil {
		t.Fatalf("unmarshal summary: %v", err)
	}
	if parsed["stepName"] != "extract" {
		t.Errorf("summary stepName = %v, want extract", parsed["stepName"])
	}
}

// TestLineageDatasetInAllSlice asserts that LineageDataset appears in the
// models.All slice, which drives AutoMigrate.
func TestLineageDatasetInAllSlice(t *testing.T) {
	found := false
	for _, m := range All {
		if _, ok := m.(*LineageDataset); ok {
			found = true
			break
		}
	}
	if !found {
		t.Error("LineageDataset not found in models.All — AutoMigrate will not create the table")
	}
}
