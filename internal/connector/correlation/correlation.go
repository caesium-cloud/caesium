// Package correlation verifies declared activity delegations against existing
// local admissions. It is composed explicitly by connector services and imports
// the core contract; the core configuration/catalog package does not import it.
package correlation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/connector"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/runadmission"
	"github.com/google/uuid"
)

// MaxCorrelationJobs bounds every alias/key admission search, independently of
// configuration size. An oversized declaration is unavailable, never truncated.
const MaxCorrelationJobs = 100

var ErrUnverifiedCorrelation = errors.New("connector: activity correlation is unverified")

// ActivityEvidence is produced by a trusted adapter read of one pinned execution.
// Coordinates and keys are adapter-owned; no HTTP request may supply these fields.
// The neutral core does not interpret provider coordinates or decode payloads.
type ActivityEvidence struct {
	Execution        connector.ExecutionReference   `json:"execution"`
	WorkflowType     string                         `json:"workflow_type"`
	ActivityID       string                         `json:"activity_id"`
	ActivityType     string                         `json:"activity_type"`
	IdempotencyKey   string                         `json:"idempotency_key"`
	SourceEventID    int64                          `json:"source_event_id"`
	SourceEventType  string                         `json:"source_event_type"`
	ScheduledEventID int64                          `json:"scheduled_event_id"`
	Attempt          int32                          `json:"attempt"`
	Hint             *connector.CorrelationEnvelope `json:"hint,omitempty"`
}

// AdmissionLookup is deliberately read-only. It resolves all declared aliases;
// a payload's job_id must never select a candidate or hide ambiguity.
type AdmissionLookup interface {
	LookupStartAdmissions(context.Context, []string, string) ([]runadmission.StartAdmission, error)
}

// Correlator joins provider evidence to existing admissions. It cannot start,
// retry, promote, or otherwise execute local work.
type Correlator struct {
	admissions AdmissionLookup
	catalog    *Store
}

func NewCorrelator(admissions AdmissionLookup, catalog *Store) *Correlator {
	return &Correlator{admissions: admissions, catalog: catalog}
}

// Verify requires exactly one consistent admission, validates every optional
// reported hint, then persists one stable typed local link. The external identity
// must already have been inspected. Failed verification preserves prior evidence.
func (c *Correlator) Verify(ctx context.Context, binding connector.Binding, evidence ActivityEvidence, bounds ...connector.WriteBounds) (models.ExternalExecutionCorrelation, error) {
	if c == nil || c.admissions == nil || c.catalog == nil {
		return models.ExternalExecutionCorrelation{}, ErrUnverifiedCorrelation
	}
	ref, err := connector.NewExecutionReference(evidence.Execution.ConnectionID, evidence.Execution.Coordinates)
	if err != nil || binding.Name == "" || binding.Version == "" || evidence.WorkflowType != binding.Name ||
		strings.TrimSpace(evidence.ActivityID) == "" || strings.TrimSpace(evidence.ActivityType) == "" ||
		evidence.SourceEventID <= 0 || evidence.ScheduledEventID <= 0 || evidence.SourceEventType == "" || evidence.Attempt < 1 {
		return models.ExternalExecutionCorrelation{}, ErrUnverifiedCorrelation
	}
	evidence.Execution = ref
	key, err := runadmission.ValidateIdempotencyKey(evidence.IdempotencyKey)
	if err != nil || key == "" || key != evidence.IdempotencyKey {
		return models.ExternalExecutionCorrelation{}, ErrUnverifiedCorrelation
	}
	var aliases []string
	for _, allowed := range binding.ActivityAllowlist {
		if allowed.ActivityType == evidence.ActivityType {
			aliases = append(aliases, allowed.Jobs...)
		}
	}
	if len(aliases) == 0 || len(aliases) > MaxCorrelationJobs {
		return models.ExternalExecutionCorrelation{}, ErrUnverifiedCorrelation
	}
	rows, err := c.admissions.LookupStartAdmissions(ctx, aliases, key)
	if err != nil {
		return models.ExternalExecutionCorrelation{}, fmt.Errorf("%w: admission lookup: %w", ErrUnverifiedCorrelation, err)
	}
	if len(rows) != 1 {
		return models.ExternalExecutionCorrelation{}, ErrUnverifiedCorrelation
	}
	admission := rows[0]
	if admission.ID == uuid.Nil || admission.JobID == uuid.Nil || admission.IdempotencyKey != key || !validAdmission(admission) {
		return models.ExternalExecutionCorrelation{}, ErrUnverifiedCorrelation
	}
	if evidence.Hint != nil && !hintMatches(*evidence.Hint, admission) {
		return models.ExternalExecutionCorrelation{}, ErrUnverifiedCorrelation
	}
	return c.catalog.recordCorrelation(ctx, binding.Version, evidence, admission, time.Now().UTC(), bounds...)
}

func validAdmission(a runadmission.StartAdmission) bool {
	if a.RunID != nil && *a.RunID == uuid.Nil || a.QueueID != nil && *a.QueueID == uuid.Nil {
		return false
	}
	switch a.Outcome {
	case runadmission.StartOutcomeCreated:
		return a.RunID != nil
	case runadmission.StartOutcomeQueued, runadmission.StartOutcomeDropped:
		return a.QueueID != nil && a.RunID == nil
	case runadmission.StartOutcomeSkipped:
		return true
	default:
		return false
	}
}

func hintMatches(h connector.CorrelationEnvelope, a runadmission.StartAdmission) bool {
	if h.Validate() != nil || h.JobID != a.JobID.String() || h.IdempotencyKey != a.IdempotencyKey || h.Outcome != string(a.Outcome) {
		return false
	}
	if h.QueueID != "" && (a.QueueID == nil || h.QueueID != a.QueueID.String()) {
		return false
	}
	if h.RunID != "" && (a.RunID == nil || h.RunID != a.RunID.String()) {
		return false
	}
	return true
}
