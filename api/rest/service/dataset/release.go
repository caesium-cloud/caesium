package dataset

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	runstore "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// ErrHoldReleaseUnauthenticated is returned when a release is attempted on a
// deployment with no auth mode active. Releasing a dataset hold is the one
// operator action in this feature that overrides the breaker's own judgement,
// so it must be attributable — and it cannot be attributable when every caller
// is anonymous. See ReleaseHold.
var ErrHoldReleaseUnauthenticated = errors.New(
	"dataset: releasing a hold requires an authenticated principal; set CAESIUM_AUTH_MODE=api-key or enable SSO")

// ReleaseAuthenticated reports whether an auth mode is active. A release is
// attributable only then, so every caller of ReleaseHold checks it first: the
// REST controller and the incident action's adapter.
func ReleaseAuthenticated() bool {
	vars := env.Variables()
	return vars.AuthMode == "api-key" || vars.SSOEnabled()
}

// toleranceAssertions is the bounded set of assertion kinds a tolerance window
// may name — the enforceable subset of the enum DataViolation.Assertion
// carries. `unavailable` is deliberately absent: it never opens a hold (the
// marker stream was lost, which is evidence of nothing), so there is nothing to
// snooze and accepting it would promise suppression of a verdict that already
// suppresses itself.
var toleranceAssertions = map[string]struct{}{
	runstore.AssertionMin:               {},
	runstore.AssertionMax:               {},
	runstore.AssertionDeltaFromBaseline: {},
	runstore.AssertionMaxLag:            {},
	runstore.AssertionMissing:           {},
}

// ValidateTolerances checks the `--tolerate` grammar: a known assertion kind
// mapped to a positive Go duration. An unparseable entry is refused rather than
// stored as a row nobody will be able to interpret later.
func ValidateTolerances(raw map[string]string) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(raw))
	for assertion, window := range raw {
		assertion = strings.TrimSpace(assertion)
		if _, ok := toleranceAssertions[assertion]; !ok {
			return nil, fmt.Errorf("tolerate: %q is not an assertion kind (one of min, max, deltaFromBaseline, maxLag, missing)", assertion)
		}
		window = strings.TrimSpace(window)
		d, err := time.ParseDuration(window)
		if err != nil {
			return nil, fmt.Errorf("tolerate[%s]: %q is not a duration (e.g. 24h)", assertion, window)
		}
		if d <= 0 {
			return nil, fmt.Errorf("tolerate[%s]: %q must be a positive duration", assertion, window)
		}
		out[assertion] = window
	}
	return out, nil
}

// ReleaseHoldParams is one acknowledgement of a dataset hold: a human's, or an
// approved or playbook-permitted incident action's.
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
	// AuditNamespace is AuditLog.Namespace for the release's audit rows: the
	// namespace of the dataset's owners as the auth middleware resolved it
	// ("*" when they span several, "" when unresolved).
	AuditNamespace string
	// Caller is the authenticated principal whose grants decide. A
	// namespace-confined operator is re-checked inside the release
	// transaction (B6), and the returned hold is shaped for what it may read.
	Caller *auth.Principal
	// Guard, when set, runs inside the release transaction and aborts it on
	// error. An incident action re-checks its boundary here.
	Guard func(tx *gorm.DB) error
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

	// The success entry rides INSIDE the release transaction: an audit row
	// written afterwards is a row a crash in between can lose, and "the hold was
	// released and nobody knows by whom" is precisely the state an audit log
	// exists to make impossible.
	entry := s.holdReleaseAuditEntry(params, principal, "success")

	var hold *models.DatasetHold
	var err error
	if confined(params.Caller, ReleaseRole) {
		hold, err = s.releaseConfined(params, principal, entry)
	} else {
		hold, err = s.holdStore().ReleaseHold(s.ctx, params.HoldID, runstore.ReleaseHoldParams{
			ReleasedBy: principal,
			Note:       strings.TrimSpace(params.Reason),
			Tolerances: params.Tolerances,
			Audit:      entry,
			Guard:      params.Guard,
		})
	}
	if err != nil {
		// A REFUSED release changed nothing, so its audit row cannot ride a
		// committed transaction; it is written on its own, best-effort.
		s.writeHoldReleaseAudit(s.holdReleaseAuditEntry(params, principal, "failure"))
		return nil, err
	}

	metrics.AuditLogEntriesTotal.WithLabelValues(entry.Action, entry.Outcome).Inc()
	log.Info("dataset hold released by operator ack",
		"hold_id", hold.ID, "dataset", hold.Name, "actor", principal)
	if err := s.shapeHolds(viewerFor(params.Caller), []*models.DatasetHold{hold}); err != nil {
		return nil, err
	}
	return &ReleaseHoldResult{Hold: hold}, nil
}

// holdStore is the run store an unconfined release writes through: the default
// store, whose bus delivers dataset_released, unless the Service was built on one.
func (s *Service) holdStore() *runstore.Store {
	if s.store != nil {
		return s.store
	}
	return runstore.Default()
}

// storeFor is the run store a confined release writes through: a store on
// the (possibly lock-holding) connection, publishing on the default store's
// event bus. Tests swap it.
var storeFor = func(conn *gorm.DB) *runstore.Store {
	store := runstore.NewStore(conn)
	store.SetBus(runstore.Default().Bus())
	return store
}

// releaseConfined is a namespace-confined operator's release: the held
// dataset's producers are re-checked inside the release transaction under
// their advisory locks (guardedWrite), and the release runs on the locked
// connection with the default store's event bus.
func (s *Service) releaseConfined(params ReleaseHoldParams, principal string, entry *models.AuditLog) (*models.DatasetHold, error) {
	var target models.DatasetHold
	if err := s.db.WithContext(s.ctx).Select("id", "namespace", "name").First(&target, "id = ?", params.HoldID).Error; err != nil {
		return nil, err
	}
	var hold *models.DatasetHold
	err := s.guardedWrite(params.Caller, ReleaseRole, target.Namespace, target.Name, func(conn *gorm.DB, guard func(tx *gorm.DB) error) error {
		released, err := storeFor(conn).ReleaseHold(s.ctx, params.HoldID, runstore.ReleaseHoldParams{
			ReleasedBy: principal,
			Note:       strings.TrimSpace(params.Reason),
			Tolerances: params.Tolerances,
			Audit:      entry,
			Guard: func(tx *gorm.DB) error {
				if err := guard(tx); err != nil {
					return err
				}
				if params.Guard != nil {
					return params.Guard(tx)
				}
				return nil
			},
		})
		hold = released
		return err
	})
	return hold, err
}

// holdReleaseAuditEntry builds the AuditLog row for one release attempt. A hold
// release overrides an automated safety decision, so it is exactly the kind of
// act the audit log exists for.
func (s *Service) holdReleaseAuditEntry(params ReleaseHoldParams, principal, outcome string) *models.AuditLog {
	metadata := map[string]any{"hold_id": params.HoldID.String()}
	if len(params.Tolerances) > 0 {
		metadata["tolerances"] = params.Tolerances
	}
	if reason := strings.TrimSpace(params.Reason); reason != "" {
		metadata["reason"] = reason
	}
	return &models.AuditLog{
		ID:           uuid.New(),
		Timestamp:    time.Now().UTC(),
		Actor:        principal,
		Action:       "dataset.hold.release",
		ResourceType: "dataset_hold",
		ResourceID:   params.HoldID.String(),
		SourceIP:     params.SourceIP,
		Outcome:      outcome,
		Metadata:     encodeAuditMetadata(metadata),
		Namespace:    params.AuditNamespace,
	}
}

// writeHoldReleaseAudit persists an audit row outside any transaction, for the
// attempts that never got one (a refused or errored release).
func (s *Service) writeHoldReleaseAudit(entry *models.AuditLog) {
	if err := s.db.WithContext(s.ctx).Create(entry).Error; err != nil {
		log.Warn("failed to write the dataset hold release audit entry",
			"hold_id", entry.ResourceID, "error", err)
		return
	}
	metrics.AuditLogEntriesTotal.WithLabelValues(entry.Action, entry.Outcome).Inc()
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
