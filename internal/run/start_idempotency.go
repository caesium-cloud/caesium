package run

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/runadmission"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Idempotent run starts.
//
// A caller that retries a run start — a Temporal activity, a CI job, any
// at-least-once client that retries on timeout — passes an idempotency key so
// the retry returns the original admission outcome instead of admitting a
// second run. The outcome is recorded in run_start_idempotency inside the same
// transaction that admits the run, so a concurrent duplicate either finds the
// committed record up front or loses the (job_id, idempotency_key) unique index
// inside its own transaction and reads the winner's record back.
//
// Only accepted outcomes are recorded: created, queued and skipped. A refused
// start (ErrMaxConcurrentRunsReached under the fail strategy, or any error)
// records nothing, so a retry with the same key re-attempts admission.

// MaxIdempotencyKeyLength bounds a run-start idempotency key.
const MaxIdempotencyKeyLength = runadmission.MaxIdempotencyKeyLength

var (
	// ErrInvalidIdempotencyKey rejects a key that is too long or carries
	// control characters.
	ErrInvalidIdempotencyKey = runadmission.ErrInvalidIdempotencyKey
	// ErrIdempotencyKeyReused is returned when a key already recorded for this
	// job is presented with different params or priority.
	ErrIdempotencyKeyReused = errors.New("run: idempotency key was already used for a different request")
	// ErrIdempotencyRecordCorrupt reports a recorded outcome that cannot be
	// resolved (an unknown outcome, or a created outcome with no run).
	ErrIdempotencyRecordCorrupt = errors.New("run: idempotency record is corrupt")
)

// StartOutcome is how an accepted run start was admitted.
type StartOutcome = runadmission.StartOutcome

const (
	// StartOutcomeCreated: a run was created and is executing.
	StartOutcomeCreated = runadmission.StartOutcomeCreated
	// StartOutcomeQueued: the concurrency policy parked the start in
	// run_queue; the dequeuer creates the run when a slot frees.
	StartOutcomeQueued = runadmission.StartOutcomeQueued
	// StartOutcomeSkipped: nothing will execute — the concurrency policy
	// dropped the start, or a consumed dataset is held.
	StartOutcomeSkipped = runadmission.StartOutcomeSkipped
	// StartOutcomeDropped: a queued start whose queue entry was removed
	// (cancelled by an operator or evicted by queue overflow) before it was
	// promoted. Only a replay of an idempotent start can observe it.
	StartOutcomeDropped = runadmission.StartOutcomeDropped
)

// Skip reasons reported on a skipped StartResult.
const (
	StartSkipReasonMaxConcurrency = "max_concurrency"
	StartSkipReasonDatasetHold    = "dataset_hold"
)

// StartResult describes how a run start was admitted.
type StartResult struct {
	Outcome StartOutcome
	// Run is the created run. Set only when Outcome is created.
	Run *JobRun
	// RunID is the created run, or the terminal skipped run a dataset hold
	// wrote in place of executing.
	RunID *uuid.UUID
	// QueueID is the run_queue entry of a queued (or dropped) start.
	QueueID *uuid.UUID
	// Reason explains a skipped outcome (StartSkipReason*).
	Reason string
	// Replayed reports that an idempotency key matched an earlier start and
	// this result is that start's outcome, not a new admission.
	Replayed bool
}

// ValidateIdempotencyKey trims key and reports whether it is usable. An empty
// key is valid and means "not idempotent".
func ValidateIdempotencyKey(key string) (string, error) {
	return runadmission.ValidateIdempotencyKey(key)
}

// WithStartIdempotencyKey makes the start idempotent under key, scoped to the
// job. See ValidateIdempotencyKey.
func WithStartIdempotencyKey(key string) StartOption {
	return func(opts *StartOptions) {
		opts.IdempotencyKey = strings.TrimSpace(key)
	}
}

// StartWithResult starts a run and reports how it was admitted. Unlike
// StartWithContext, a queued or skipped start is an outcome, not an error; the
// errors it returns are refusals (ErrMaxConcurrentRunsReached,
// ErrIdempotencyKeyReused, ErrInvalidPriority, ...) and failures.
func (s *Store) StartWithResult(ctx context.Context, jobID uuid.UUID, triggerID *uuid.UUID, opts ...StartOption) (StartResult, error) {
	startOpts := startOptionsFrom(opts)
	key, err := ValidateIdempotencyKey(startOpts.IdempotencyKey)
	if err != nil {
		return StartResult{}, err
	}
	var result StartResult
	run, err := s.startRun(startRunRequest{
		ctx:              ctx,
		jobID:            jobID,
		triggerID:        triggerID,
		params:           startOpts.Params,
		priorityOverride: startOpts.Priority,
		idempotencyKey:   key,
		result:           &result,
		namespaceGuard:   startOpts.NamespaceGuard,
		replayGuard:      startOpts.ReplayGuard,
	})
	if err != nil && !errors.Is(err, ErrRunSkipped) && !errors.Is(err, ErrRunQueued) {
		return StartResult{}, err
	}
	if run != nil {
		result.Outcome = StartOutcomeCreated
		result.Run = run
		result.RunID = &run.ID
	}
	return result, nil
}

// FindIdempotentStart returns the recorded outcome of an earlier start made
// with the same idempotency key, or found=false when there is none. It lets a
// caller answer a retry before checks that would refuse a NEW start (a job
// paused after the original start, say) without admitting anything.
func (s *Store) FindIdempotentStart(ctx context.Context, jobID uuid.UUID, opts ...StartOption) (StartResult, bool, error) {
	startOpts := startOptionsFrom(opts)
	key, err := ValidateIdempotencyKey(startOpts.IdempotencyKey)
	if err != nil {
		return StartResult{}, false, err
	}
	if key == "" {
		return StartResult{}, false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	conn := s.db.WithContext(ctx)
	existing, err := findStartIdempotency(conn, jobID, key)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return StartResult{}, false, nil
	}
	if err != nil {
		return StartResult{}, false, err
	}
	var result StartResult
	_, err = s.replayIdempotentStart(conn, existing, startRequestHash(startOpts.Params, startOpts.Priority), startOpts.ReplayGuard, &result)
	if err != nil && !errors.Is(err, ErrRunSkipped) && !errors.Is(err, ErrRunQueued) {
		return StartResult{}, false, err
	}
	return result, true, nil
}

// startRequestHash fingerprints the caller-supplied request — params as sent,
// before any enricher runs, and the priority override — so a reused key can be
// told apart from a retry.
func startRequestHash(params map[string]string, priority string) string {
	keys := slices.Sorted(maps.Keys(params))
	pairs := make([][2]string, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, [2]string{key, params[key]})
	}
	// json.Marshal of string pairs and a string cannot fail.
	encoded, _ := json.Marshal(struct {
		Version  int         `json:"version"`
		Params   [][2]string `json:"params"`
		Priority string      `json:"priority"`
	}{
		Version:  1,
		Params:   pairs,
		Priority: strings.ToLower(strings.TrimSpace(priority)),
	})
	sum := sha256.Sum256(encoded)
	return "run-start:v1:" + hex.EncodeToString(sum[:])
}

// findStartIdempotency returns gorm.ErrRecordNotFound when the key is new.
// It reads with Find rather than Take so the expected miss on every first
// request is not logged as a failed query.
func findStartIdempotency(conn *gorm.DB, jobID uuid.UUID, key string) (*models.RunStartIdempotency, error) {
	var rows []models.RunStartIdempotency
	if err := conn.Where("job_id = ? AND idempotency_key = ?", jobID, key).Limit(1).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, gorm.ErrRecordNotFound
	}
	return &rows[0], nil
}

// recordStartOutcomeTx runs inside the admission transaction once the decision
// is made. It records the outcome of an idempotent start, and — when this
// admission is the promotion of a queued start — resolves any idempotency
// record waiting on that queue entry. Refusals (admissionFailed) record
// nothing: the queue entry is released and a later drain retries it.
func recordStartOutcomeTx(tx *gorm.DB, req startRunRequest, model *models.JobRun, admission admissionResult) error {
	outcome, reason, recordRunID, queueID, ok := startOutcomeOf(model.ID, admission)
	if !ok {
		return nil
	}
	now := time.Now().UTC()

	if req.queueID != nil {
		// A promotion is never itself a keyed request; the key, if any, rode
		// in on the original enqueue and is found by its queue entry.
		if err := tx.Model(&models.RunStartIdempotency{}).
			Where("queue_id = ?", *req.queueID).
			Updates(map[string]any{
				"outcome":    string(outcome),
				"reason":     reason,
				"run_id":     recordRunID,
				"updated_at": now,
			}).Error; err != nil {
			return fmt.Errorf("run: resolve queued idempotent start: %w", err)
		}
	}

	if req.idempotencyKey == "" {
		return nil
	}
	return tx.Create(&models.RunStartIdempotency{
		ID:             uuid.New(),
		JobID:          req.jobID,
		IdempotencyKey: req.idempotencyKey,
		RequestHash:    startRequestHash(req.requestParams, req.priorityOverride),
		Outcome:        string(outcome),
		Reason:         reason,
		RunID:          recordRunID,
		QueueID:        queueID,
		// admit froze the owning job's namespace on the model before deciding.
		Namespace: models.NamespaceOrDefault(model.Namespace),
		CreatedAt: now,
		UpdatedAt: now,
	}).Error
}

// startOutcomeOf maps an admission decision to the outcome a caller sees.
// ok is false for decisions that are not accepted outcomes.
func startOutcomeOf(runID uuid.UUID, admission admissionResult) (outcome StartOutcome, reason string, recordRunID, queueID *uuid.UUID, ok bool) {
	switch admission.decision {
	case admissionCreated:
		return StartOutcomeCreated, "", &runID, nil, true
	case admissionHeld:
		return StartOutcomeSkipped, StartSkipReasonDatasetHold, &runID, nil, true
	case admissionSkipped:
		reason := admission.skipReason
		if reason == "" {
			reason = StartSkipReasonMaxConcurrency
		}
		return StartOutcomeSkipped, reason, nil, nil, true
	case admissionQueued:
		id := admission.queueID
		return StartOutcomeQueued, "", nil, &id, true
	default:
		return "", "", nil, nil, false
	}
}

// replayIdempotentStart answers a start whose key is already recorded. It fills
// result and returns what startRun would have returned for the original
// admission: the run for created, ErrRunQueued for queued, ErrRunSkipped for
// skipped and dropped.
//
// guard (StartOptions.ReplayGuard), when set, authorises the record before
// anything about it is revealed: it runs ahead of the request-hash comparison,
// so a caller that may not read the recorded start gets the guard's error
// whether or not its request matches.
func (s *Store) replayIdempotentStart(conn *gorm.DB, existing *models.RunStartIdempotency, requestHash string, guard func([]string) error, result *StartResult) (*JobRun, error) {
	if guard != nil {
		namespaces, err := idempotentStartNamespaces(conn, existing)
		if err != nil {
			return nil, err
		}
		if err := guard(namespaces); err != nil {
			return nil, err
		}
	}
	if existing.RequestHash != requestHash {
		return nil, ErrIdempotencyKeyReused
	}

	out := StartResult{
		Outcome:  StartOutcome(existing.Outcome),
		RunID:    existing.RunID,
		QueueID:  existing.QueueID,
		Reason:   existing.Reason,
		Replayed: true,
	}

	if out.Outcome == StartOutcomeQueued && existing.RunID == nil && existing.QueueID != nil {
		var pending int64
		if err := conn.Model(&models.RunQueue{}).Where("id = ?", *existing.QueueID).Count(&pending).Error; err != nil {
			return nil, err
		}
		if pending == 0 {
			// The queue entry is gone. A promotion rewrites this record in the
			// transaction that creates the run, which commits before the
			// dequeuer deletes the entry — so re-read the record: if it still
			// says queued, nothing promoted it and the entry was dropped.
			refreshed, err := findStartIdempotency(conn, existing.JobID, existing.IdempotencyKey)
			if err != nil {
				return nil, err
			}
			if StartOutcome(refreshed.Outcome) != StartOutcomeQueued {
				// The promotion may have named a run: authorise it too.
				return s.replayIdempotentStart(conn, refreshed, requestHash, guard, result)
			}
			out.Outcome = StartOutcomeDropped
		}
	}

	var (
		run *JobRun
		err error
	)
	switch out.Outcome {
	case StartOutcomeCreated:
		if existing.RunID == nil {
			return nil, fmt.Errorf("%w: created outcome without a run (key %q)", ErrIdempotencyRecordCorrupt, existing.IdempotencyKey)
		}
		run, err = s.loadRunWithDB(conn, *existing.RunID)
		if err != nil {
			return nil, err
		}
		out.Run = run
	case StartOutcomeQueued:
		err = ErrRunQueued
	case StartOutcomeSkipped, StartOutcomeDropped:
		err = ErrRunSkipped
	default:
		return nil, fmt.Errorf("%w: unknown outcome %q (key %q)", ErrIdempotencyRecordCorrupt, existing.Outcome, existing.IdempotencyKey)
	}
	if result != nil {
		*result = out
	}
	return run, err
}

// idempotentStartNamespaces returns, sorted, every namespace a recorded start
// is attributed to: the namespace admission recorded it in and, when the
// record names a run (a created start, a dataset-hold skip, a promoted queued
// start), that run's persisted namespace. The result is empty when the record
// cannot be attributed: it names a run that no longer exists, or it predates
// the namespace column and names no run.
func idempotentStartNamespaces(conn *gorm.DB, existing *models.RunStartIdempotency) ([]string, error) {
	var namespaces []string
	if namespace := strings.TrimSpace(existing.Namespace); namespace != "" {
		namespaces = append(namespaces, namespace)
	}
	if existing.RunID != nil {
		var runs []models.JobRun
		if err := conn.Model(&models.JobRun{}).Select("id", "namespace").Where("id = ?", *existing.RunID).Limit(1).Find(&runs).Error; err != nil {
			return nil, err
		}
		if len(runs) == 0 {
			return nil, nil
		}
		namespaces = append(namespaces, models.NamespaceOrDefault(runs[0].Namespace))
	}
	slices.Sort(namespaces)
	return slices.Compact(namespaces), nil
}

// StartAdmission is a read-only projection of the accepted start protocol. It
// deliberately excludes request hashes, params, and task/run payloads.
type StartAdmission = runadmission.StartAdmission

// LookupStartAdmission reads job/key without reconstructing the HTTP request or
// replaying admission. It never starts work or writes a dropped outcome. Queue
// promotion retains the original admission and QueueID.
func (s *Store) LookupStartAdmission(ctx context.Context, jobID uuid.UUID, key string) (StartAdmission, bool, error) {
	normalized, err := ValidateIdempotencyKey(key)
	if err != nil {
		return StartAdmission{}, false, err
	}
	if jobID == uuid.Nil || normalized == "" || normalized != key {
		return StartAdmission{}, false, ErrInvalidIdempotencyKey
	}
	if ctx == nil {
		ctx = context.Background()
	}
	conn := s.db.WithContext(ctx)
	row, err := findStartIdempotency(conn, jobID, key)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return StartAdmission{}, false, nil
	}
	if err != nil {
		return StartAdmission{}, false, err
	}
	// A promoted admission retains QueueID after the queue row is deleted.
	// If the queue still exists, it must belong to the same job for every
	// outcome, including the promotion/deletion window.
	var queue []models.RunQueue
	if row.QueueID != nil {
		if err := conn.Select("id", "job_id").Where("id = ?", *row.QueueID).Limit(1).Find(&queue).Error; err != nil {
			return StartAdmission{}, false, err
		}
		if len(queue) > 0 && queue[0].JobID != jobID {
			return StartAdmission{}, false, ErrIdempotencyRecordCorrupt
		}
	}
	// The promotion transaction updates the admission before removing its queue
	// row. Re-read after an absent queue so a concurrent promotion is not dropped.
	if row.Outcome == string(StartOutcomeQueued) && row.RunID == nil && row.QueueID != nil && len(queue) == 0 {
		row, err = findStartIdempotency(conn, jobID, key)
		if err != nil {
			return StartAdmission{}, false, err
		}
		if row.Outcome == string(StartOutcomeQueued) {
			row.Outcome = string(StartOutcomeDropped)
		}
	}
	out := StartAdmission{ID: row.ID, JobID: row.JobID, IdempotencyKey: row.IdempotencyKey, Outcome: StartOutcome(row.Outcome), Reason: row.Reason, QueueID: row.QueueID, RunID: row.RunID}
	if out.ID == uuid.Nil || out.JobID != jobID || out.IdempotencyKey != key || out.QueueID != nil && *out.QueueID == uuid.Nil || out.RunID != nil && *out.RunID == uuid.Nil {
		return StartAdmission{}, false, ErrIdempotencyRecordCorrupt
	}
	switch out.Outcome {
	case StartOutcomeCreated:
		if out.RunID == nil {
			return StartAdmission{}, false, ErrIdempotencyRecordCorrupt
		}
	case StartOutcomeQueued, StartOutcomeDropped:
		if out.QueueID == nil || out.RunID != nil {
			return StartAdmission{}, false, ErrIdempotencyRecordCorrupt
		}
	case StartOutcomeSkipped:
	default:
		return StartAdmission{}, false, ErrIdempotencyRecordCorrupt
	}
	if out.RunID != nil {
		var runs []models.JobRun
		if err := conn.Select("id", "job_id").Where("id = ?", *out.RunID).Limit(1).Find(&runs).Error; err != nil {
			return StartAdmission{}, false, err
		}
		if len(runs) != 1 || runs[0].JobID != jobID {
			return StartAdmission{}, false, ErrIdempotencyRecordCorrupt
		}
	}
	return out, true, nil
}

// LookupStartAdmissions resolves a bounded set of local job aliases and reads
// every matching job/key admission. It never uses a reported hint to narrow the
// search: two valid admissions remain ambiguous to a correlation caller.
func (s *Store) LookupStartAdmissions(ctx context.Context, aliases []string, key string) ([]StartAdmission, error) {
	const maxAliases = 100
	if len(aliases) == 0 || len(aliases) > maxAliases {
		return nil, errors.New("run: admission alias lookup exceeds its bound")
	}
	unique := make(map[string]bool, len(aliases))
	for _, alias := range aliases {
		if strings.TrimSpace(alias) == "" || strings.TrimSpace(alias) != alias {
			return nil, errors.New("run: invalid admission job alias")
		}
		unique[alias] = true
	}
	var jobs []models.Job
	if err := s.db.WithContext(ctx).Select("id").Where("alias IN ?", slices.Collect(maps.Keys(unique))).Limit(maxAliases + 1).Find(&jobs).Error; err != nil {
		return nil, err
	}
	if len(jobs) > maxAliases {
		return nil, errors.New("run: admission alias lookup exceeds its bound")
	}
	var admissions []StartAdmission
	for _, job := range jobs {
		admission, found, err := s.LookupStartAdmission(ctx, job.ID, key)
		if err != nil {
			return nil, err
		}
		if found {
			admissions = append(admissions, admission)
		}
	}
	return admissions, nil
}
