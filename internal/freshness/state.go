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

	// Consumed is the snapshot of this dataset's declared-input watermarks as of
	// this producing run. It is persisted onto consumed_watermarks in the SAME
	// transaction that ACCEPTS the advance/verify, tied to the accepted run — so
	// when this Advance loses the race (a newer run's watermark wins under the
	// conflict re-read), this run's input snapshot is NOT written either, and the
	// winning run's snapshot stays authoritative. Empty means "leave the column
	// untouched" (a produced dataset with no consumed inputs). Never written for
	// a dropped outcome (backfill / regression / out-of-order).
	Consumed map[string]string

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

	const maxAttempts = 5
	var (
		res AdvanceResult
		err error
	)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		res, err = s.advanceTx(ctx, in)
		if err == nil {
			return res, nil
		}
		// Retry only the narrow race window (row created/removed between our
		// read and our conditional write) and transient store-busy errors;
		// everything else propagates immediately.
		if errors.Is(err, errStateRaceRetry) || isBusyErr(err) {
			continue
		}
		return AdvanceResult{}, err
	}
	return AdvanceResult{}, err
}

// advanceTx runs one attempt of the advance/verify contract in a single
// transaction. It is race-safe against a concurrent Advance for the SAME
// dataset — not merely against duplicate rows, but against a lost UPDATE of the
// watermark VALUE:
//
//  1. Read the current row (no lock) and run the contract against it.
//  2. A dropped outcome (backfill / regression / out-of-order) writes nothing.
//     This is safe under a concurrent writer because a concurrent Advance only
//     moves the watermark FORWARD, so re-deciding against a newer value would
//     still drop.
//  3. An advance/verify must write. We first `INSERT ... ON CONFLICT DO NOTHING`
//     (fresh id, so only the (namespace,name) unique index can conflict). If the
//     insert created the row we are the first observation and are done. If it
//     CONFLICTED, the row already exists (pre-existing, or a concurrent
//     first-writer) — and the INSERT statement has now taken the transaction's
//     write lock, so we re-SELECT the authoritative row under that lock, re-run
//     the full contract against ITS current watermark, and write only if it
//     still advances/verifies. A regression exposed by the re-read is dropped.
//
// The net invariant: after any set of concurrent Advances the stored watermark
// is the max per the ordering contract, and every non-advancing attempt is
// recorded-and-dropped.
func (s *Store) advanceTx(ctx context.Context, in AdvanceInput) (AdvanceResult, error) {
	var res AdvanceResult
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		current, existed, err := loadState(tx, in.Namespace, in.Name)
		if err != nil {
			return err
		}

		base := current
		if !existed {
			base = models.DatasetState{
				ID:        uuid.New(),
				Namespace: nsValue(in.Namespace),
				Name:      in.Name,
				Status:    models.DatasetStatusUnknown,
			}
		}
		res = applyContract(&base, in)

		// Dropped writes never touch state and never create a row — and never
		// write this run's consumed snapshot, so a losing run cannot leave its
		// input snapshot behind on the winning run's row.
		if res.Outcome != OutcomeAdvanced && res.Outcome != OutcomeVerified {
			res.State = current
			return nil
		}

		// The consumed snapshot is written in THIS same accepting transaction,
		// tied to the accepted advance/verify — not as a separate follow-up.
		consumedBlob, hasConsumed := consumedJSON(in.Consumed)

		// Write path. Attempt an atomic create; a fresh id ensures only the
		// (namespace,name) unique index can be the conflict target.
		insert := base
		insert.ID = uuid.New()
		if hasConsumed {
			insert.ConsumedWatermarks = consumedBlob
		}
		created := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "namespace"}, {Name: "name"}},
			DoNothing: true,
		}).Create(&insert)
		if created.Error != nil {
			return created.Error
		}
		if created.RowsAffected == 1 {
			// We created the row as the first observation.
			res.State = insert
			return nil
		}

		// Conflict: the row exists and the INSERT above took the write lock.
		// Re-read the authoritative row under the lock and re-decide.
		authoritative, ok, err := loadState(tx, in.Namespace, in.Name)
		if err != nil {
			return err
		}
		if !ok {
			// Row vanished between the conflict and the re-read (deletion race);
			// retry the whole transaction.
			return errStateRaceRetry
		}
		res = applyContract(&authoritative, in)
		if res.Outcome == OutcomeAdvanced || res.Outcome == OutcomeVerified {
			updates := map[string]interface{}{
				"watermark":        authoritative.Watermark,
				"watermark_run_at": authoritative.WatermarkRunAt,
				"advanced_at":      authoritative.AdvancedAt,
				"verified_at":      authoritative.VerifiedAt,
				"status":           authoritative.Status,
				"reason":           authoritative.Reason,
				"last_run_id":      authoritative.LastRunID,
			}
			// Consumed rides the accepted advance so watermark, last_run_id, and
			// consumed_watermarks all come from THIS (winning) run. It is only
			// written when this run advances/verifies under the re-read — a run
			// that loses the race here leaves the winner's snapshot untouched.
			if hasConsumed {
				updates["consumed_watermarks"] = consumedBlob
				authoritative.ConsumedWatermarks = consumedBlob
			}
			if err := tx.Model(&models.DatasetState{}).
				Where("id = ?", authoritative.ID).
				Updates(updates).Error; err != nil {
				return err
			}
		}
		res.State = authoritative
		return nil
	})
	if err != nil {
		return AdvanceResult{}, err
	}
	return res, nil
}

// errStateRaceRetry signals the outer retry loop that a benign row-vanished race
// occurred and the transaction should be re-run.
var errStateRaceRetry = errors.New("freshness: dataset state row changed under a concurrent write; retry")

// isBusyErr reports whether err is a transient SQLite/dqlite lock-contention
// error worth retrying (rather than a real failure).
func isBusyErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "locked") || strings.Contains(msg, "busy")
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

// loadState loads the state row for a dataset on its natural key, reporting
// whether it existed. The nil namespace is mapped to ” so the query keys on the
// same value the NOT-NULL column stores.
func loadState(tx *gorm.DB, namespace *string, name string) (models.DatasetState, bool, error) {
	ns := nsValue(namespace)
	var state models.DatasetState
	// Find (not Take/First) so a miss is RowsAffected==0, not an error-logged
	// ErrRecordNotFound — the "unknown dataset" case is the common path.
	res := tx.Where("namespace = ? AND name = ?", ns, name).Limit(1).Find(&state)
	if res.Error != nil {
		return models.DatasetState{}, false, res.Error
	}
	return state, res.RowsAffected > 0, nil
}

// consumedJSON marshals a consumed-watermark snapshot. It returns ok=false for
// an empty snapshot (a produced dataset with no consumed inputs), which callers
// treat as "leave consumed_watermarks untouched". map[string]string never fails
// to marshal, so a marshal error is treated as an empty snapshot.
func consumedJSON(consumed map[string]string) (datatypes.JSON, bool) {
	if len(consumed) == 0 {
		return nil, false
	}
	blob, err := json.Marshal(consumed)
	if err != nil {
		return nil, false
	}
	return datatypes.JSON(blob), true
}

// RecordConsumed snapshots the consumed-input watermarks onto an EXISTING
// produced dataset's state as a standalone write, for the consumed-only path
// where no advance is happening (e.g. an operator/manual surface). The normal
// producing-run path folds the snapshot into Advance (see AdvanceInput.Consumed)
// so watermark, last_run_id, and consumed_watermarks are written atomically and
// a losing run never clobbers the winner's snapshot. This updates ONLY the
// consumed_watermarks column (never a full-row Save) so a concurrent Advance is
// not clobbered, and is a no-op when the dataset has no state row yet.
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
