package dataset

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	runstore "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// ErrHoldReleaseUnauthenticated is returned when a release is attempted on a
// deployment with no auth mode active. Releasing a dataset hold is the one
// operator action in this feature that overrides the breaker's own judgement,
// so it must be attributable — and it cannot be attributable when every caller
// is anonymous. See ReleaseHold.
var ErrHoldReleaseUnauthenticated = errors.New(
	"dataset: releasing a hold requires an authenticated principal; set CAESIUM_AUTH_MODE=api-key or enable SSO")

// ReleaseHoldParams is one human acknowledgement of a dataset hold.
type ReleaseHoldParams struct {
	// HoldID is the hold to release.
	HoldID uuid.UUID
	// Principal is the authenticated identity performing the release. Never
	// empty: the controller refuses the request before reaching here otherwise.
	Principal string
	// SourceIP is recorded on the audit entry.
	SourceIP string
	// Reason is the operator's stated justification.
	Reason string
	// Tolerances are optional per-assertion tolerance windows
	// (`--tolerate <assertion>=<duration>`), recorded as evidence on the
	// release so the next reader knows the hold was acked knowingly rather
	// than dismissed.
	Tolerances map[string]string
}

// ReleaseHoldResult is the response shape for POST /v1/datasets/holds/:id/release.
type ReleaseHoldResult struct {
	Hold *models.DatasetHold `json:"hold"`
}

// ReleaseHold acks an active dataset hold as a human and audits the act.
//
// FAIL-CLOSED: Caesium defaults to CAESIUM_AUTH_MODE=none, which attaches no
// auth middleware, so with no auth mode active this returns
// ErrHoldReleaseUnauthenticated (a 403 naming the precondition) and holds can be
// cleared only by the automatic clean_run path. That keeps ReleasedBy an
// authenticated principal by construction rather than by convention.
//
// Errors: ErrHoldReleaseUnauthenticated (403), gorm.ErrRecordNotFound (404),
// run.ErrDatasetHoldNotActive (409 — two operators acking the same hold is an
// ordinary race, not a server fault).
func (s *Service) ReleaseHold(params ReleaseHoldParams) (*ReleaseHoldResult, error) {
	principal := strings.TrimSpace(params.Principal)
	if principal == "" {
		return nil, ErrHoldReleaseUnauthenticated
	}

	store := runstore.Default()
	hold, err := store.ReleaseHold(s.ctx, params.HoldID, runstore.ReleaseHoldParams{
		ReleasedBy: principal,
		Note:       strings.TrimSpace(params.Reason),
		Tolerances: params.Tolerances,
	})
	if err != nil {
		s.auditHoldRelease(params, principal, "failure")
		return nil, err
	}

	s.auditHoldRelease(params, principal, "success")
	log.Info("dataset hold released by operator ack",
		"hold_id", hold.ID, "dataset", hold.Name, "actor", principal)
	return &ReleaseHoldResult{Hold: hold}, nil
}

// auditHoldRelease mirrors the release into AuditLog, the record of every
// state-changing operator action. A hold release overrides an automated safety
// decision, so it is exactly the kind of act the audit log exists for.
func (s *Service) auditHoldRelease(params ReleaseHoldParams, principal, outcome string) {
	metadata := map[string]any{"hold_id": params.HoldID.String()}
	if len(params.Tolerances) > 0 {
		metadata["tolerances"] = params.Tolerances
	}
	if reason := strings.TrimSpace(params.Reason); reason != "" {
		metadata["reason"] = reason
	}
	entry := &models.AuditLog{
		ID:           uuid.New(),
		Timestamp:    time.Now().UTC(),
		Actor:        principal,
		Action:       "dataset.hold.release",
		ResourceType: "dataset_hold",
		ResourceID:   params.HoldID.String(),
		SourceIP:     params.SourceIP,
		Outcome:      outcome,
		Metadata:     encodeAuditMetadata(metadata),
	}
	if err := s.db.WithContext(s.ctx).Create(entry).Error; err != nil {
		log.Warn("failed to write the dataset hold release audit entry",
			"hold_id", params.HoldID, "error", err)
		return
	}
	metrics.AuditLogEntriesTotal.WithLabelValues(entry.Action, outcome).Inc()
}

// encodeAuditMetadata marshals the audit metadata, degrading to no metadata
// rather than losing the audit entry itself.
func encodeAuditMetadata(metadata map[string]any) datatypes.JSON {
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return nil
	}
	return datatypes.JSON(encoded)
}
