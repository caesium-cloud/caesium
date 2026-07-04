package freshness

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// errEmptyDatasetName guards every state write: a dataset must be named.
var errEmptyDatasetName = errors.New("freshness: dataset name is required")

// nsValue maps a nil (unset) namespace to the empty string, matching the
// DatasetState.Namespace NOT-NULL-default-” column. Every state query and write
// keys on this value so the (namespace, name) UNIQUE index is reliable — SQLite
// treats NULLs as distinct, so a nullable namespace would defeat the index.
func nsValue(namespace *string) string {
	if namespace == nil {
		return ""
	}
	return strings.TrimSpace(*namespace)
}

// Outcome is the result of an Advance/Verify decision — the observable record of
// what the watermark contract did. Regressions and out-of-order opaque writes
// are *recorded* (returned) and dropped, exactly as the design requires.
type Outcome string

const (
	// OutcomeAdvanced — the watermark value changed (increased, for orderable
	// values; a newer producing run, for opaque values). advanced_at moved.
	OutcomeAdvanced Outcome = "advanced"
	// OutcomeVerified — a successful run confirmed the current watermark without
	// changing it (or emitted no watermark key at all: degraded mode). Only
	// verified_at moved.
	OutcomeVerified Outcome = "verified"
	// OutcomeRegressionDropped — an orderable watermark moved backwards. Recorded,
	// never advanced.
	OutcomeRegressionDropped Outcome = "regression_dropped"
	// OutcomeOutOfOrderDropped — an opaque watermark arrived from a run older than
	// the one that set the current value. Recorded, never advanced.
	OutcomeOutOfOrderDropped Outcome = "out_of_order_dropped"
	// OutcomeBackfillDropped — a backfill run never advances a watermark
	// (monotonic guard; derivations ignore backfill runs).
	OutcomeBackfillDropped Outcome = "backfill_dropped"
)

// AdvanceInput carries one producing observation of a dataset's watermark.
type AdvanceInput struct {
	// Namespace is nullable and unused in v1; Name is the dataset identity.
	Namespace *string
	Name      string

	// Watermark is the emitted value. Empty means the producing step declared no
	// watermark key (or emitted none): degraded mode, which refreshes verified_at
	// against CompletedAt rather than advancing.
	Watermark string

	// RunID is the producing run; recorded as last_run_id.
	RunID uuid.UUID

	// RunOrder orders this observation against the run that set the current
	// watermark — the completion (or start) time of the producing run, or a
	// monotonic sequence surrogate. It gates opaque-string advances so a
	// late-finishing older run can't clobber a newer opaque value. Zero falls
	// back to CompletedAt.
	RunOrder time.Time

	// CompletedAt is the producing run's completion time, used for advanced_at /
	// verified_at and for degraded-mode advances.
	CompletedAt time.Time

	// Backfill marks a backfill run: it never advances a watermark.
	Backfill bool
}

// AdvanceResult is what the contract decided, plus the resulting state row.
type AdvanceResult struct {
	Outcome Outcome
	State   models.DatasetState
}

// Store is the durable state store over dataset_states / dataset_derivations.
// It implements the watermark advance/verify contract that distinguishes "a run
// succeeded" from "the output advanced".
type Store struct {
	db *gorm.DB
}

// NewStore constructs a Store over the provided connection.
func NewStore(db *gorm.DB) *Store {
	return &Store{db: db}
}

// Advance applies one producing observation to a dataset's state under the
// watermark contract:
//
//   - Backfill runs never advance (OutcomeBackfillDropped).
//   - An empty watermark (degraded mode) refreshes verified_at with CompletedAt.
//   - An unchanged watermark on a success refreshes verified_at, not advanced_at.
//   - An orderable watermark (numeric / RFC3339) advances only when it increases;
//     a regression is recorded and dropped.
//   - An opaque-string watermark advances only when the producing run is newer
//     than the one that set the current value; an out-of-order write is dropped.
//
// Freshness is later evaluated against max(advanced_at, verified_at). The whole
// decision runs in one transaction against the (namespace, name) natural key.
func (s *Store) Advance(ctx context.Context, in AdvanceInput) (AdvanceResult, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return AdvanceResult{}, errEmptyDatasetName
	}
	in.Name = name

	completed := in.CompletedAt
	if completed.IsZero() {
		completed = time.Now().UTC()
	} else {
		completed = completed.UTC()
	}
	in.CompletedAt = completed

	order := in.RunOrder
	if order.IsZero() {
		order = completed
	}
	in.RunOrder = order.UTC()

	var res AdvanceResult
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		state, err := loadOrInitState(tx, in.Namespace, in.Name)
		if err != nil {
			return err
		}
		res = applyContract(&state, in)
		// Only persist when the contract changed state. A dropped write
		// (backfill, regression, out-of-order) leaves state exactly as-is and
		// must never create a row for a previously-unknown dataset.
		if res.Outcome == OutcomeAdvanced || res.Outcome == OutcomeVerified {
			if err := upsertState(tx, &state); err != nil {
				return err
			}
		}
		res.State = state
		return nil
	})
	if err != nil {
		return AdvanceResult{}, err
	}
	return res, nil
}

// upsertState writes an advance/verify result race-safely. It keys on the
// (namespace, name) UNIQUE index rather than the primary key, so two concurrent
// Advance calls for the same previously-unknown dataset collapse into exactly
// one row (the losing INSERT becomes an UPDATE) instead of racing to create a
// duplicate. ConsumedWatermarks is deliberately excluded from the update set —
// Advance never manages it; RecordConsumed owns that column, so a concurrent
// snapshot is never clobbered by an advance.
func upsertState(tx *gorm.DB, state *models.DatasetState) error {
	return tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "namespace"}, {Name: "name"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"watermark", "watermark_run_at", "advanced_at", "verified_at",
			"status", "reason", "last_run_id", "updated_at",
		}),
	}).Create(state).Error
}

// applyContract mutates state per the watermark contract and returns the
// outcome. It is pure with respect to the DB (all timestamps come from the
// input), so the contract table is fully unit-testable without a database — the
// exported Advance wraps this in a transaction.
func applyContract(state *models.DatasetState, in AdvanceInput) AdvanceResult {
	runID := in.RunID

	// Backfill runs never advance and never verify — derivations ignore them.
	if in.Backfill {
		return AdvanceResult{Outcome: OutcomeBackfillDropped}
	}

	// Degraded mode: no declared/emitted watermark key. A successful non-cached
	// run confirms the output against its completion time.
	if in.Watermark == "" {
		verifyOnly(state, in, runID)
		return AdvanceResult{Outcome: OutcomeVerified}
	}

	// First value the dataset has ever seen always advances.
	if state.Watermark == "" && state.AdvancedAt == nil {
		advance(state, in, runID)
		return AdvanceResult{Outcome: OutcomeAdvanced}
	}

	// Unchanged value: verify, don't advance.
	if in.Watermark == state.Watermark {
		verifyOnly(state, in, runID)
		return AdvanceResult{Outcome: OutcomeVerified}
	}

	// Changed value: gate by ordering. Orderable values (integer / float /
	// RFC3339) compare by value; opaque values compare by producing-run order.
	if greater, ok := orderableGreater(state.Watermark, in.Watermark); ok {
		if greater {
			advance(state, in, runID)
			return AdvanceResult{Outcome: OutcomeAdvanced}
		}
		// Regression (equal handled above as unchanged): recorded, never advanced.
		return AdvanceResult{Outcome: OutcomeRegressionDropped}
	}

	// Opaque: only a newer producing run may overwrite.
	if state.WatermarkRunAt == nil || in.RunOrder.After(*state.WatermarkRunAt) {
		advance(state, in, runID)
		return AdvanceResult{Outcome: OutcomeAdvanced}
	}
	return AdvanceResult{Outcome: OutcomeOutOfOrderDropped}
}

// advance sets a new watermark value and moves advanced_at.
func advance(state *models.DatasetState, in AdvanceInput, runID uuid.UUID) {
	completed := in.CompletedAt
	state.Watermark = in.Watermark
	runAt := in.RunOrder
	state.WatermarkRunAt = &runAt
	state.AdvancedAt = &completed
	state.LastRunID = nonNilRun(runID)
}

// verifyOnly moves verified_at (confirms the current value) without advancing.
func verifyOnly(state *models.DatasetState, in AdvanceInput, runID uuid.UUID) {
	completed := in.CompletedAt
	state.VerifiedAt = &completed
	state.LastRunID = nonNilRun(runID)
}

func nonNilRun(runID uuid.UUID) *uuid.UUID {
	if runID == uuid.Nil {
		return nil
	}
	r := runID
	return &r
}

// orderableGreater reports whether next is strictly greater than cur when BOTH
// parse as the same orderable kind. It tries int64, then uint64, then float64,
// then RFC3339 — integer parsing FIRST so large watermarks (e.g. nanosecond
// timestamps beyond 2^53) compare exactly, without the float64 rounding that
// would silently treat 9007199254740993 and 9007199254740992 as equal. ok is
// false for opaque strings (git SHAs, UUIDs) or mixed kinds, which must be gated
// by producing-run order instead of value.
func orderableGreater(cur, next string) (greater, ok bool) {
	cur = strings.TrimSpace(cur)
	next = strings.TrimSpace(next)

	if ci, err1 := strconv.ParseInt(cur, 10, 64); err1 == nil {
		if ni, err2 := strconv.ParseInt(next, 10, 64); err2 == nil {
			return ni > ci, true
		}
		return false, false
	}
	if cu, err1 := strconv.ParseUint(cur, 10, 64); err1 == nil {
		if nu, err2 := strconv.ParseUint(next, 10, 64); err2 == nil {
			return nu > cu, true
		}
		return false, false
	}
	if cf, err1 := strconv.ParseFloat(cur, 64); err1 == nil {
		if nf, err2 := strconv.ParseFloat(next, 64); err2 == nil {
			return nf > cf, true
		}
		return false, false
	}
	if ct, err1 := parseRFC3339(cur); err1 == nil {
		if nt, err2 := parseRFC3339(next); err2 == nil {
			return nt.After(ct), true
		}
	}
	return false, false
}

func parseRFC3339(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, v)
}

// loadOrInitState loads the state row for a dataset on its natural key, or
// returns a fresh (unpersisted) row when none exists yet. The nil namespace is
// mapped to ” so the query keys on the same value the NOT-NULL column stores;
// race-safe single-row creation is handled by upsertState's ON CONFLICT.
func loadOrInitState(tx *gorm.DB, namespace *string, name string) (models.DatasetState, error) {
	ns := nsValue(namespace)
	var state models.DatasetState
	// Find (not Take/First) so a miss is RowsAffected==0, not an error-logged
	// ErrRecordNotFound — the "unknown dataset" case is the common path.
	res := tx.Where("namespace = ? AND name = ?", ns, name).Limit(1).Find(&state)
	if res.Error != nil {
		return models.DatasetState{}, res.Error
	}
	if res.RowsAffected > 0 {
		return state, nil
	}
	return models.DatasetState{
		ID:        uuid.New(),
		Namespace: ns,
		Name:      name,
		Status:    models.DatasetStatusUnknown,
	}, nil
}

// RecordConsumed snapshots the consumed-input watermarks onto a produced
// dataset's state, so "is my output up to date with my inputs" is a pure row
// comparison. It updates ONLY the consumed_watermarks column (never a full-row
// Save) so a concurrent Advance is not clobbered. A no-op when the produced
// dataset has no state row yet (nothing to attach the snapshot to).
func (s *Store) RecordConsumed(ctx context.Context, namespace *string, name string, consumed map[string]string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errEmptyDatasetName
	}
	blob, err := json.Marshal(consumed)
	if err != nil {
		return err
	}
	return s.db.WithContext(ctx).Model(&models.DatasetState{}).
		Where("namespace = ? AND name = ?", nsValue(namespace), name).
		Update("consumed_watermarks", datatypes.JSON(blob)).Error
}

// Get returns the state row for a dataset, or (zero, false, nil) when none
// exists yet (an unknown dataset the evaluator serves before any run).
func (s *Store) Get(ctx context.Context, namespace *string, name string) (models.DatasetState, bool, error) {
	name = strings.TrimSpace(name)
	var state models.DatasetState
	res := s.db.WithContext(ctx).
		Where("namespace = ? AND name = ?", nsValue(namespace), name).
		Limit(1).Find(&state)
	if res.Error != nil {
		return models.DatasetState{}, false, res.Error
	}
	if res.RowsAffected == 0 {
		return models.DatasetState{}, false, nil
	}
	return state, true, nil
}

// FreshAt returns the effective freshness time for a dataset — max(advanced_at,
// verified_at) — and whether it has ever been observed. This is the single
// clock the evaluator measures the SLO against.
func FreshAt(state models.DatasetState) (time.Time, bool) {
	var out time.Time
	seen := false
	if state.AdvancedAt != nil {
		out = *state.AdvancedAt
		seen = true
	}
	if state.VerifiedAt != nil && state.VerifiedAt.After(out) {
		out = *state.VerifiedAt
		seen = true
	}
	return out, seen
}
