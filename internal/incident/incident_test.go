package incident

import (
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
)

func TestDedupeKeyStable(t *testing.T) {
	id := uuid.New()
	a := DedupeKey("default", id, "extract", ClassDataUnavailable)
	b := DedupeKey("default", id, "extract", ClassDataUnavailable)
	if a != b {
		t.Fatalf("dedupe key not stable: %q != %q", a, b)
	}
	if a == DedupeKey("default", id, "extract", ClassAuthFailure) {
		t.Fatalf("dedupe key must differ by class")
	}
	if a == DedupeKey("marketing", id, "extract", ClassDataUnavailable) {
		t.Fatalf("dedupe key must differ by namespace")
	}
	if a == DedupeKey("default", uuid.New(), "extract", ClassDataUnavailable) {
		t.Fatalf("dedupe key must differ by job")
	}
}

func TestCanTransition(t *testing.T) {
	allowed := [][2]models.IncidentStatus{
		{models.IncidentStatusOpen, models.IncidentStatusTriaging},
		{models.IncidentStatusTriaging, models.IncidentStatusAwaitingApproval},
		{models.IncidentStatusAwaitingApproval, models.IncidentStatusTriaging},
		{models.IncidentStatusTriaging, models.IncidentStatusRemediated},
		{models.IncidentStatusTriaging, models.IncidentStatusEscalated},
		{models.IncidentStatusRemediated, models.IncidentStatusClosed},
		{models.IncidentStatusEscalated, models.IncidentStatusClosed},
		{models.IncidentStatusOpen, models.IncidentStatusSuppressed},
		{models.IncidentStatusTriaging, models.IncidentStatusAbandoned},
	}
	for _, tc := range allowed {
		if !CanTransition(tc[0], tc[1]) {
			t.Errorf("expected %s → %s allowed", tc[0], tc[1])
		}
	}

	denied := [][2]models.IncidentStatus{
		{models.IncidentStatusOpen, models.IncidentStatusOpen},           // self
		{models.IncidentStatusClosed, models.IncidentStatusTriaging},     // out of terminal
		{models.IncidentStatusOpen, models.IncidentStatusClosed},         // must pass through remediated/escalated
		{models.IncidentStatusRemediated, models.IncidentStatusTriaging}, // no going back
		{models.IncidentStatusSuppressed, models.IncidentStatusOpen},     // terminal
	}
	for _, tc := range denied {
		if CanTransition(tc[0], tc[1]) {
			t.Errorf("expected %s → %s denied", tc[0], tc[1])
		}
	}
}
