package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/lineage"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrDatasetHoldNotActive is returned when a release targets a hold that is
// already released. It is a 409, not a 500: two operators acking the same hold
// is an ordinary race, not a server fault.
var ErrDatasetHoldNotActive = errors.New("run: dataset hold is not active")

// HoldReasonOpenFailed is the caesium_dataset_holds_total label for a breach
// whose hold could NOT be opened after the store's contention-retry budget. It
// is not an assertion kind: it means the breaker itself failed, the task was
// failed fail-closed instead, and the dataset is NOT held. Alert on it.
const HoldReasonOpenFailed = "open_failed"

// errDatasetHoldRace is the internal retry signal: the conditional insert lost
// the race to a twin that was then released before the occurrence append could
// find it, so the active key is free again and the insert should be re-tried.
var errDatasetHoldRace = errors.New("run: dataset hold race; retry")

// DatasetHoldEvent is the payload of dataset_held and dataset_released.
//
// It is deliberately self-contained: Stream F's incident entry point keys off
// dataset_held and must be able to open an incident from the event alone,
// without a second read that could observe a moved baseline or an
// already-released hold. So the violated assertion, the observed value, the
// breached bound AND the baseline snapshot all travel here, inside the
// DataViolation objects the evaluator produced.
type DatasetHoldEvent struct {
	HoldID    uuid.UUID `json:"hold_id"`
	Namespace string    `json:"namespace,omitempty"`
	Dataset   string    `json:"dataset"`
	Status    string    `json:"status"`
	// Reason is the bounded assertion kind that opened the hold.
	Reason          string `json:"reason,omitempty"`
	OccurrenceCount int    `json:"occurrence_count,omitempty"`
	JobAlias        string `json:"job_alias,omitempty"`
	Step            string `json:"step,omitempty"`
	// Violations carries observed / bound / baseline_median / baseline_samples
	// per breached assertion.
	Violations []DataViolation `json:"violations,omitempty"`
	// ImpactedDatasets is how many datasets the observed lineage graph put
	// downstream of this one when the hold opened (0 when lineage records none).
	ImpactedDatasets int `json:"impacted_datasets,omitempty"`
	// Release* are set only on dataset_released.
	ReleaseReason string `json:"release_reason,omitempty"`
	ReleasedBy    string `json:"released_by,omitempty"`
	ReleaseRunID  string `json:"release_run_id,omitempty"`
}

// RunHeldUpstreamEvent is the payload of run_held_upstream: the admission gate
// refused a run because a dataset it consumes is held.
type RunHeldUpstreamEvent struct {
	HoldID     uuid.UUID `json:"hold_id"`
	Namespace  string    `json:"namespace,omitempty"`
	Dataset    string    `json:"dataset"`
	SkipReason string    `json:"skip_reason"`
	JobAlias   string    `json:"job_alias,omitempty"`
	// SkippedTasks is how many task rows the gate materialised and marked
	// skipped, so a reader knows the run is not task-less.
	SkippedTasks int `json:"skipped_tasks"`
}

// datasetHoldRequest is one hold-open request: everything the row and the
// event need, resolved by the evaluator before it reaches the store.
type datasetHoldRequest struct {
	namespace string
	name      string
	reason    string
	jobID     uuid.UUID
	jobAlias  string
	runID     uuid.UUID
	taskID    uuid.UUID
	taskRunID uuid.UUID
	stepName  string
	// claim fences the open on the breaching attempt still owning taskRunID.
	// Nil on the local executor, which holds no claim. See TaskClaim; the check
	// runs INSIDE the open transaction, not before it, because a preflight
	// would leave the reclaim window between the check and the insert open —
	// and a hold is the one write here that outlives the attempt that made it.
	claim      *TaskClaim
	violations []DataViolation
}

// errDatasetHoldStaleClaim reports that the attempt asking for a hold no longer
// owns its TaskRun row. It is NOT a failure to open: nothing was written and
// nothing should be, so the caller abandons quietly instead of failing the task
// closed the way it does for a durable write error.
var errDatasetHoldStaleClaim = errors.New("run: dataset hold refused: stale claim")

// openOrAppendDatasetHold opens exactly one active hold per dataset, or folds a
// repeat breach into the existing one as an occurrence.
//
// The open is an ATOMIC conditional insert on the nullable unique active_key
// (ON CONFLICT DO NOTHING), copied from incident.Store.OpenOrAppend: it is
// exactly the "one active row per key, append occurrences" problem, and a
// select-then-insert would open twins under concurrent fanned verdicts and is
// not correct under Postgres READ COMMITTED regardless.
//
// It returns opened=true only for the insert that WON. That is what makes
// alert-once structural: dataset_held is emitted inside the same transaction
// as the winning insert and nowhere else.
func (s *Store) openOrAppendDatasetHold(ctx context.Context, req datasetHoldRequest) (*models.DatasetHold, bool, error) {
	if s == nil || s.db == nil {
		return nil, false, errors.New("run: dataset hold requires a store")
	}
	key := models.DatasetHoldKey(req.namespace, req.name)
	violations, err := json.Marshal(req.violations)
	if err != nil {
		return nil, false, fmt.Errorf("run: encode hold violations: %w", err)
	}

	// The blast radius is frozen at open, like the incident manager's read-scope
	// allowlist: it is what the hold MEANT when it tripped. Best-effort — a
	// deployment without observed lineage simply records no cone, and the hold
	// is no less correct for it.
	impact, impacted := s.datasetHoldImpact(ctx, req.namespace, req.name)

	const maxOpenAttempts = 3
	for range maxOpenAttempts {
		now := time.Now().UTC()
		activeKey := key
		hold := &models.DatasetHold{
			ID:              uuid.New(),
			Namespace:       req.namespace,
			Name:            req.name,
			Status:          models.DatasetHoldStatusActive,
			ActiveKey:       &activeKey,
			Reason:          req.reason,
			HeldByJobID:     req.jobID,
			HeldByJobAlias:  req.jobAlias,
			HeldByStepName:  req.stepName,
			Violations:      datatypes.JSON(violations),
			Impact:          impact,
			OccurrenceCount: 1,
			OpenedAt:        now,
			LastBreachAt:    &now,
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		if req.runID != uuid.Nil {
			runID := req.runID
			hold.HeldByRunID = &runID
			hold.LastBreachRunID = &runID
		}
		if req.taskID != uuid.Nil {
			taskID := req.taskID
			hold.HeldByTaskID = &taskID
		}
		if req.taskRunID != uuid.Nil {
			taskRunID := req.taskRunID
			hold.HeldByTaskRunID = &taskRunID
		}

		var (
			opened   bool
			existing models.DatasetHold
			evt      *event.Event
		)
		// The breaker's own write goes through the shared contention-retry
		// budget like every other store transaction. Without it a transient
		// dqlite "database is locked" would mean the circuit silently does not
		// trip for that breach — the violation recorded, no hold, and every
		// downstream consumer admitted onto the bad data.
		err := withStoreBusyRetryContext(ctx, func() error {
			// BOTH are reset per attempt. An attempt that won the insert, built
			// its dataset_held event and then rolled back on a lock leaves no
			// row and no persisted event behind; carrying that stale event into
			// a later attempt that LOSES the race would publish a page for a
			// hold this call never opened, with nothing in /v1/events behind it.
			opened = false
			evt = nil
			return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				// THE CLAIM FENCE, inside the write transaction (issue #438,
				// maintainer review). A superseded worker reaches this seam
				// with a real breach in hand, and opening its hold would break
				// the circuit on evidence the metric fence had already refused
				// — blocking every consumer after the worker that owns the row
				// has already recovered the dataset. Checking here rather than
				// before the call is what closes the reclaim window: on
				// Postgres the row lock taskRunClaimHeldTx takes holds until
				// this transaction commits, so a takeover cannot land between
				// the check and the insert.
				if req.claim != nil {
					held, fenceErr := taskRunClaimHeldTx(tx, req.taskRunID, *req.claim)
					if fenceErr != nil {
						return fenceErr
					}
					if !held {
						return errDatasetHoldStaleClaim
					}
				}
				res := tx.Clauses(clause.OnConflict{
					Columns:   []clause.Column{{Name: "active_key"}},
					DoNothing: true,
				}).Create(hold)
				if res.Error != nil {
					return res.Error
				}
				if res.RowsAffected == 1 {
					opened = true
					existing = *hold
					appended, err := s.appendDatasetHoldEventTx(tx, event.TypeDatasetHeld, hold, impacted, req.violations)
					if err != nil {
						return err
					}
					evt = appended
					return nil
				}

				// Lost the race: fold this breach into the live hold as an
				// occurrence. No event, no counter — the dataset is already broken
				// and somebody has already been told.
				appendUpdates := map[string]any{
					"occurrence_count": gorm.Expr("occurrence_count + 1"),
					"violations":       datatypes.JSON(violations),
					"reason":           req.reason,
					"updated_at":       now,
					// The LATEST breach moves; HeldBy* stay pinned to the first
					// open. A clean run may only release evidence that postdates
					// this, and never from this run.
					"last_breach_at": now,
				}
				if req.runID != uuid.Nil {
					appendUpdates["last_breach_run_id"] = req.runID
				}
				update := tx.Model(&models.DatasetHold{}).
					Where("active_key = ?", key).
					Updates(appendUpdates)
				if update.Error != nil {
					return update.Error
				}
				if update.RowsAffected == 0 {
					// The twin was released between our conflict and this update,
					// so the key is free; retry the insert rather than dropping the
					// breach on the floor.
					return errDatasetHoldRace
				}
				return tx.Where("active_key = ?", key).First(&existing).Error
			})
		})
		if errors.Is(err, errDatasetHoldRace) {
			continue
		}
		if errors.Is(err, errDatasetHoldStaleClaim) {
			// Not a retry and not a failure: the row belongs to another
			// attempt, so there is nothing here to write on any attempt.
			return nil, false, errDatasetHoldStaleClaim
		}
		if err != nil {
			return nil, false, err
		}

		if opened {
			metrics.DatasetHoldsTotal.WithLabelValues(req.reason).Inc()
		}
		s.syncDatasetHoldsActiveGauge(ctx)
		if evt != nil {
			s.publishEvents(*evt)
		}
		return &existing, opened, nil
	}
	return nil, false, fmt.Errorf("run: exhausted retries opening a dataset hold for %q", key)
}

// datasetHoldImpact resolves the observed-lineage blast radius for a declared
// dataset name. It returns the marshalled lineage.ImpactResult and the number
// of downstream datasets, or (nil, 0) when nothing downstream is recorded.
func (s *Store) datasetHoldImpact(ctx context.Context, namespace, name string) (datatypes.JSON, int) {
	result, err := lineage.QueryImpact(ctx, s.db, namespace, name, 0)
	if err != nil {
		log.Warn("failed to resolve blast radius for a dataset hold; recording the hold without an impact cone",
			"dataset", name, "error", err)
		return nil, 0
	}
	if result == nil || len(result.Downstream) == 0 {
		return nil, 0
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		log.Warn("failed to encode blast radius for a dataset hold", "dataset", name, "error", err)
		return nil, 0
	}
	return datatypes.JSON(encoded), len(result.Downstream)
}

// datasetHoldReleaseRequest describes one release.
type datasetHoldReleaseRequest struct {
	reason     string
	by         string
	note       string
	runID      uuid.UUID
	tolerances map[string]string
	// audit, when set, is written in the SAME transaction as the release. An
	// audit row created afterwards is a row that a crash in between can lose,
	// which is exactly the case an audit log exists to cover.
	audit *models.AuditLog
	// guardNotBreachRun and guardBreachBefore carry releasableHold's recency
	// conditions INTO the UPDATE's WHERE clause. The clean-run path sets them;
	// the human ack deliberately does not, because a person acking a hold is
	// overriding the breaker's judgement on purpose and does not have to lose a
	// race with it.
	//
	// They are what makes the release correct under Postgres READ COMMITTED,
	// where an occurrence appended between the caller's read and this write is
	// invisible to the read. Without them the release is a select-then-update —
	// the very shape the hold-OPEN path is written to avoid.
	guardNotBreachRun uuid.UUID
	guardBreachBefore time.Time
}

// releaseDatasetHold closes one active hold by id and emits dataset_released.
//
// The write is a GUARDED update on status, so two concurrent releases (a human
// ack racing the holder's clean run) resolve to exactly one release and one
// event; the loser gets ErrDatasetHoldNotActive.
func (s *Store) releaseDatasetHold(ctx context.Context, id uuid.UUID, req datasetHoldReleaseRequest) (*models.DatasetHold, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("run: dataset hold release requires a store")
	}

	var (
		released models.DatasetHold
		evt      *event.Event
	)
	err := withStoreBusyRetryContext(ctx, func() error {
		// Reset per attempt, for the same reason the open path does: a rolled
		// back attempt's event must never outlive its transaction.
		evt = nil
		return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			ok, err := s.releaseDatasetHoldTx(tx, id, req, &released)
			if err != nil {
				return err
			}
			if !ok {
				return ErrDatasetHoldNotActive
			}
			if req.audit != nil {
				if err := tx.Create(req.audit).Error; err != nil {
					return err
				}
			}
			appended, err := s.appendDatasetHoldEventTx(tx, event.TypeDatasetReleased, &released, 0, nil)
			if err != nil {
				return err
			}
			evt = appended
			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	s.syncDatasetHoldsActiveGauge(ctx)
	if evt != nil {
		s.publishEvents(*evt)
	}
	return &released, nil
}

// releaseDatasetHoldTx is the guarded release UPDATE, usable inside a caller's
// transaction so a clean-run release commits with the metrics that proved the
// dataset recovered. It reports whether THIS call was the one that released.
func (s *Store) releaseDatasetHoldTx(tx *gorm.DB, id uuid.UUID, req datasetHoldReleaseRequest, out *models.DatasetHold) (bool, error) {
	now := time.Now().UTC()
	updates := map[string]any{
		"status":         models.DatasetHoldStatusReleased,
		"active_key":     nil,
		"released_at":    now,
		"released_by":    req.by,
		"release_reason": req.reason,
		"release_note":   req.note,
		"updated_at":     now,
	}
	if req.runID != uuid.Nil {
		updates["release_run_id"] = req.runID
	}
	if len(req.tolerances) > 0 {
		encoded, err := json.Marshal(req.tolerances)
		if err != nil {
			return false, fmt.Errorf("run: encode release tolerances: %w", err)
		}
		updates["tolerances"] = datatypes.JSON(encoded)
	}

	guarded := tx.Model(&models.DatasetHold{}).
		Where("id = ? AND status = ?", id, models.DatasetHoldStatusActive)
	if req.guardNotBreachRun != uuid.Nil {
		guarded = guarded.Where("(last_breach_run_id IS NULL OR last_breach_run_id <> ?)", req.guardNotBreachRun)
	}
	if !req.guardBreachBefore.IsZero() {
		// NULL last_breach_at is a hold from before the column existed (see the
		// model); it cannot be shown to postdate the release, so it is compared
		// on opened_at instead.
		guarded = guarded.Where(
			"(CASE WHEN last_breach_at IS NULL THEN opened_at ELSE last_breach_at END) < ?",
			req.guardBreachBefore.UTC())
	}

	res := guarded.Updates(updates)
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected == 0 {
		return false, nil
	}
	if out != nil {
		if err := tx.First(out, "id = ?", id).Error; err != nil {
			return false, err
		}
	}
	return true, nil
}

// appendDatasetHoldEventTx persists a dataset_held / dataset_released event so
// it survives for /v1/events, `caesium why` and any NotificationPolicy that
// matches on it. The returned event is published on the bus AFTER the
// transaction commits.
func (s *Store) appendDatasetHoldEventTx(
	tx *gorm.DB,
	kind event.Type,
	hold *models.DatasetHold,
	impacted int,
	violations []DataViolation,
) (*event.Event, error) {
	if s.eventStore == nil || hold == nil {
		return nil, nil
	}
	if violations == nil && len(hold.Violations) > 0 {
		// A release re-reads the verdicts off the row so the all-clear names the
		// same assertion the page did.
		_ = json.Unmarshal(hold.Violations, &violations)
	}
	payload := DatasetHoldEvent{
		HoldID:           hold.ID,
		Namespace:        hold.Namespace,
		Dataset:          hold.Name,
		Status:           hold.Status,
		Reason:           hold.Reason,
		OccurrenceCount:  hold.OccurrenceCount,
		JobAlias:         hold.HeldByJobAlias,
		Step:             hold.HeldByStepName,
		Violations:       violations,
		ImpactedDatasets: impacted,
		ReleaseReason:    hold.ReleaseReason,
		ReleasedBy:       hold.ReleasedBy,
	}
	if hold.ReleaseRunID != nil {
		payload.ReleaseRunID = hold.ReleaseRunID.String()
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("run: encode dataset hold event: %w", err)
	}

	evt := &event.Event{
		Type:      kind,
		JobID:     hold.HeldByJobID,
		Timestamp: time.Now().UTC(),
		Payload:   encoded,
	}
	if kind == event.TypeDatasetHeld && hold.HeldByRunID != nil {
		evt.RunID = *hold.HeldByRunID
	}
	if err := s.eventStore.AppendTx(tx, evt); err != nil {
		return nil, err
	}
	return evt, nil
}

// syncDatasetHoldsActiveGauge re-derives caesium_dataset_holds_active from the
// table rather than incrementing a counter in memory.
//
// The gauge must be right after a restart, after a failover, and after a
// release performed by ANOTHER node — none of which an in-process counter
// survives. A COUNT over a table with at most one row per broken dataset is
// cheap, and it only runs when a hold opens or closes (plus once at startup via
// SyncDatasetHoldsActive), never on the dispatch path.
func (s *Store) syncDatasetHoldsActiveGauge(ctx context.Context) {
	if s == nil || s.db == nil {
		return
	}
	var active int64
	if err := s.db.WithContext(ctx).
		Model(&models.DatasetHold{}).
		Where("status = ?", models.DatasetHoldStatusActive).
		Count(&active).Error; err != nil {
		log.Warn("failed to recompute the active dataset-hold gauge", "error", err)
		return
	}
	metrics.DatasetHoldsActive.Set(float64(active))
}

// SyncDatasetHoldsActive publishes the active-hold gauge from persisted state.
// Call it once at startup so a process that restarts while datasets are held
// does not report zero until the next open or release.
func SyncDatasetHoldsActive(ctx context.Context, conn *gorm.DB) {
	if conn == nil || !DataAssertionsEnabled() {
		return
	}
	(&Store{db: conn}).syncDatasetHoldsActiveGauge(ctx)
}

// activeHoldForConsumerTx resolves the FIRST active hold on any dataset the job
// declares under `datasets.consumes`, inside the caller's transaction.
//
// Doing the lookup in the SAME transaction that inserts the run is the whole
// point: a hold opened between a pre-check and the insert would otherwise admit
// a run onto data that is already known bad. One indexed join, and only on a
// flag-on deployment (the caller gates on DataAssertionsEnabled first).
//
// COALESCE on the declaration side is required because DatasetDeclaration
// carries a NULLABLE namespace (reserved, empty in v1) while DatasetHold's is
// NOT NULL — the two must compare equal for an unnamespaced dataset.
func activeHoldForConsumerTx(tx *gorm.DB, jobID uuid.UUID) (*models.DatasetHold, error) {
	var hold models.DatasetHold
	err := tx.Model(&models.DatasetHold{}).
		Joins(`JOIN dataset_declarations ON dataset_declarations.name = dataset_holds.name`+
			` AND COALESCE(dataset_declarations.namespace, '') = dataset_holds.namespace`).
		Where("dataset_declarations.job_id = ?", jobID).
		Where("dataset_declarations.direction = ?", models.DatasetDirectionConsumes).
		Where("dataset_holds.status = ?", models.DatasetHoldStatusActive).
		Order("dataset_holds.opened_at ASC").
		First(&hold).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &hold, nil
}

// releasableHold decides whether ONE clean verdict may close ONE active hold.
//
// Three conditions, and every one of them exists because a real ordering breaks
// without it:
//
//   - The holder job must be the job running now (plan Open Question 6). A
//     second producer of the same dataset name re-produced its own slice, not
//     the one that broke.
//   - The releasing run must not BE the run that last breached. This evaluator
//     runs per instance, so partition 3 of a fanned step passing says nothing
//     about partition 2 of the same trigger having failed — and two steps of one
//     run producing the same name are the same shape. Without this, which
//     partition finished last decided whether a broken dataset stayed held.
//   - The releasing run must have STARTED after the last breach. Two concurrent
//     runs of one job (concurrency.maxRuns > 1) otherwise let the older, cleaner
//     run clear a hold the newer run opened while it was in flight: evidence
//     gathered before the breach cannot disprove it.
//
// LastBreachAt/LastBreachRunID (not OpenedAt/HeldByRunID) are the reference
// points, so an occurrence appended by a second run also has to be outlived.
//
// This function is the cheap PRE-FILTER only. The same two recency conditions
// are repeated inside releaseDatasetHoldTx's WHERE clause, because a decision
// taken from a row read earlier in the transaction is not binding under
// Postgres READ COMMITTED.
//
// ON THE TIME COMPARISON: these are two Go wall clocks, not one monotonic
// source. `JobRun.StartedAt` is stamped by whichever node started the run;
// `LastBreachAt` by whichever node processed the breaching task's completion.
// On a multi-node deployment with the run-starting node's clock ahead, a run
// that genuinely predates a breach could satisfy this test. Two things bound
// the damage: the run-identity guard covers the same-run case regardless of any
// clock, and NTP-scale skew is small against the interval between a breach and
// a later run of the same job. A monotonic ordering would need a shared
// sequence, which is not worth a new coordination primitive here.
func releasableHold(hold *models.DatasetHold, contract declaredContract, runID uuid.UUID, startedAt time.Time) bool {
	if hold == nil || hold.HeldByJobID != contract.jobID {
		return false
	}
	if hold.LastBreachRunID != nil && *hold.LastBreachRunID == runID {
		return false
	}
	// A run with no recorded start time cannot be shown to postdate the breach,
	// so it does not release — the safe direction for a breaker.
	if startedAt.IsZero() || !startedAt.After(datasetHoldLastBreach(hold)) {
		return false
	}
	return true
}

// datasetHoldLastBreach resolves a hold's most recent breach time. NULL means
// the row predates the column, in which case the open IS the only breach this
// version can prove — see models.DatasetHold.LastBreachAt.
func datasetHoldLastBreach(hold *models.DatasetHold) time.Time {
	if hold.LastBreachAt != nil {
		return hold.LastBreachAt.UTC()
	}
	return hold.OpenedAt.UTC()
}

// runStartedAtTx reads one run's start time inside the caller's transaction.
// Zero means "not resolvable", which releasableHold treats as "do not release".
func runStartedAtTx(tx *gorm.DB, runID uuid.UUID) (time.Time, error) {
	var row models.JobRun
	err := tx.Select("id", "started_at").First(&row, "id = ?", runID).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	return row.StartedAt.UTC(), nil
}

// activeHoldsForDatasetsTx returns the active holds on the given dataset names,
// keyed by name, inside the caller's transaction. Used by the clean-run release
// path, which only ever asks about the datasets one task just re-produced.
func activeHoldsForDatasetsTx(tx *gorm.DB, namespaces map[string]string, names []string) (map[string]*models.DatasetHold, error) {
	if len(names) == 0 {
		return nil, nil
	}
	var rows []models.DatasetHold
	if err := tx.Where("status = ? AND name IN ?", models.DatasetHoldStatusActive, names).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	holds := make(map[string]*models.DatasetHold, len(rows))
	for i := range rows {
		row := &rows[i]
		if row.Namespace != namespaces[row.Name] {
			// A same-named dataset in another namespace is a different dataset.
			continue
		}
		holds[row.Name] = row
	}
	return holds, nil
}

// encodeEventPayload marshals an event payload, keeping the error handling at
// the one call site that can do something about it.
func encodeEventPayload(payload any) ([]byte, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("run: encode event payload: %w", err)
	}
	return encoded, nil
}

// DatasetHoldSkipReason renders the SkipReason a hold-gated run records and the
// prefix the task rows' error column carries. Both readers (run history, the
// DAG view, `caesium why`) parse one shape.
func DatasetHoldSkipReason(namespace, name string) string {
	return "dataset_hold:" + models.DatasetHoldKey(namespace, name)
}

// datasetHoldTaskSkipReason is the per-task-row error text: the run's skip
// reason plus the hold id, so a reader can go from a skipped task straight to
// the hold that caused it.
func datasetHoldTaskSkipReason(hold *models.DatasetHold) string {
	return fmt.Sprintf("%s hold=%s", DatasetHoldSkipReason(hold.Namespace, hold.Name), hold.ID)
}

// ReleaseHoldParams is the exported release request the REST surface passes.
type ReleaseHoldParams struct {
	// ReleasedBy MUST be an authenticated principal. The endpoint is refused
	// outright when no auth mode is active, so this is never a placeholder.
	ReleasedBy string
	// Note is the operator's stated reason.
	Note string
	// Tolerances are the optional per-assertion tolerance windows
	// (--tolerate <assertion>=<duration>), recorded as evidence on the release.
	Tolerances map[string]string
	// Audit is written in the SAME transaction as the release, so a completed
	// release can never exist without its audit row.
	Audit *models.AuditLog
}

// ReleaseHold acks one hold as a human. It is the exported entry point for
// POST /v1/datasets/holds/:id/release.
//
// Errors: gorm.ErrRecordNotFound for an unknown id, ErrDatasetHoldNotActive
// when the hold has already been released (a 409 — two operators acking the
// same hold is an ordinary race).
func (s *Store) ReleaseHold(ctx context.Context, id uuid.UUID, params ReleaseHoldParams) (*models.DatasetHold, error) {
	if strings.TrimSpace(params.ReleasedBy) == "" {
		return nil, errors.New("run: releasing a dataset hold requires an authenticated principal")
	}
	var existing models.DatasetHold
	if err := s.db.WithContext(ctx).First(&existing, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return s.releaseDatasetHold(ctx, id, datasetHoldReleaseRequest{
		reason:     models.DatasetHoldReleaseManualAck,
		by:         strings.TrimSpace(params.ReleasedBy),
		note:       strings.TrimSpace(params.Note),
		tolerances: params.Tolerances,
		audit:      params.Audit,
	})
}

// ActiveHoldForDataset returns the active hold on one dataset, or nil.
func (s *Store) ActiveHoldForDataset(ctx context.Context, namespace, name string) (*models.DatasetHold, error) {
	var hold models.DatasetHold
	err := s.db.WithContext(ctx).
		Where("namespace = ? AND name = ? AND status = ?", namespace, name, models.DatasetHoldStatusActive).
		First(&hold).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &hold, nil
}
