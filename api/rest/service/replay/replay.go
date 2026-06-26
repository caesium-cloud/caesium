// Package replay implements the REST replay service around the internal replay
// constructor, including B4's scoped idempotency reservation.
package replay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	iauth "github.com/caesium-cloud/caesium/internal/auth"
	jobrunner "github.com/caesium-cloud/caesium/internal/job"
	"github.com/caesium-cloud/caesium/internal/models"
	replaycore "github.com/caesium-cloud/caesium/internal/replay"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/caesium-cloud/caesium/pkg/sqlerr"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

var (
	ErrMissingIdempotencyKey         = errors.New("replay: idempotency key is required")
	ErrReplayReservationCorrupt      = errors.New("replay: idempotency reservation is corrupt")
	ErrReplayReservationIncomplete   = errors.New("replay: idempotency reservation has no materialized work")
	ErrReplayRequiresDistributedMode = errors.New(
		"replay requires distributed execution mode for re-executing tasks",
	)
)

// Request is the service-level request for a quarantined replay.
type Request struct {
	JobID          uuid.UUID
	BaselineRunID  uuid.UUID
	Set            map[string]string
	IdempotencyKey string
	Principal      *iauth.Principal
}

// Result is the replay endpoint result.
type Result struct {
	Run         *runstorage.JobRun
	Fingerprint string
	Existing    bool
}

// Service owns REST replay idempotency and delegates materialization to B3.
type Service struct {
	ctx           context.Context
	store         *runstorage.Store
	dispatcher    replaycore.Dispatcher
	executionMode func() string
}

// New returns a replay service backed by the default run store.
func New(ctx context.Context) *Service {
	store := runstorage.Default()
	return &Service{
		ctx:           ctx,
		store:         store,
		dispatcher:    NewAsyncDispatcher(store),
		executionMode: func() string { return env.Variables().ExecutionMode },
	}
}

// WithStore returns a copy of the service backed by store.
func (s *Service) WithStore(store *runstorage.Store) *Service {
	if store == nil {
		return s
	}
	next := *s
	next.store = store
	if _, ok := s.dispatcher.(*AsyncDispatcher); ok {
		next.dispatcher = NewAsyncDispatcher(store)
	}
	return &next
}

// WithDatabase returns a copy of the service backed by conn; used by tests.
func (s *Service) WithDatabase(conn *gorm.DB) *Service {
	if conn == nil {
		return s
	}
	return s.WithStore(runstorage.NewStore(conn))
}

// WithDispatcher returns a copy of the service with a custom dispatcher.
func (s *Service) WithDispatcher(dispatcher replaycore.Dispatcher) *Service {
	if dispatcher == nil {
		return s
	}
	next := *s
	next.dispatcher = dispatcher
	return &next
}

// WithExecutionMode returns a copy of the service using mode for replay
// dispatch preflight. It is intended for tests and for explicit callers.
func (s *Service) WithExecutionMode(mode string) *Service {
	next := *s
	next.executionMode = func() string { return mode }
	return &next
}

// Replay creates or resumes an idempotent quarantined replay.
func (s *Service) Replay(req Request) (*Result, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("replay: run store is required")
	}
	if s.dispatcher == nil {
		return nil, replaycore.ErrDispatchRequired
	}

	key := strings.TrimSpace(req.IdempotencyKey)
	if key == "" {
		return nil, ErrMissingIdempotencyKey
	}

	overrides := cloneSet(req.Set)
	fingerprint, err := Fingerprint(req.JobID, req.BaselineRunID, req.Principal, overrides, key)
	if err != nil {
		return nil, err
	}

	if existing, err := s.findByFingerprint(fingerprint); err == nil {
		return s.returnExisting(req.JobID, existing.ID, fingerprint)
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	constructor := replaycore.New(s.store, s.dispatcher)
	prepared, err := constructor.Prepare(s.ctx, replaycore.Request{
		BaselineRunID:     req.BaselineRunID,
		Set:               overrides,
		ReplayFingerprint: fingerprint,
	})
	if err != nil {
		return nil, err
	}
	if prepared.RequiresDispatch() && !s.isDistributedExecutionMode() {
		return nil, ErrReplayRequiresDistributedMode
	}

	result, err := constructor.Materialize(s.ctx, prepared)
	if err == nil {
		return &Result{Run: result.Run, Fingerprint: fingerprint}, nil
	}
	if !isUniqueReplayFingerprintError(err) {
		return nil, err
	}

	existing, loadErr := s.findByFingerprint(fingerprint)
	if loadErr != nil {
		return nil, fmt.Errorf("replay: load existing idempotency reservation: %w", loadErr)
	}
	return s.returnExisting(req.JobID, existing.ID, fingerprint)
}

func (s *Service) returnExisting(jobID, runID uuid.UUID, fingerprint string) (*Result, error) {
	if err := s.resumePending(runID, jobID); err != nil {
		return nil, err
	}
	run, err := s.store.Get(runID)
	if err != nil {
		return nil, err
	}
	return &Result{Run: run, Fingerprint: fingerprint, Existing: true}, nil
}

func (s *Service) findByFingerprint(fingerprint string) (*models.JobRun, error) {
	var existing models.JobRun
	if err := s.store.DB().WithContext(s.ctx).
		First(&existing, "replay_fingerprint = ?", fingerprint).Error; err != nil {
		return nil, err
	}
	return &existing, nil
}

func (s *Service) resumePending(runID, expectedJobID uuid.UUID) error {
	var reservation models.JobRun
	if err := s.store.DB().WithContext(s.ctx).
		Select("id", "job_id", "status", "quarantine", "replay_fingerprint").
		First(&reservation, "id = ?", runID).Error; err != nil {
		return err
	}
	if reservation.JobID != expectedJobID {
		return fmt.Errorf("%w: fingerprint resolved to job %s, want %s", ErrReplayReservationCorrupt, reservation.JobID, expectedJobID)
	}
	if !reservation.Quarantine || reservation.ReplayFingerprint == nil || strings.TrimSpace(*reservation.ReplayFingerprint) == "" {
		return fmt.Errorf("%w: run %s is not a quarantined replay reservation", ErrReplayReservationCorrupt, runID)
	}
	switch reservation.Status {
	case string(runstorage.StatusRunning):
	case string(runstorage.StatusSucceeded), string(runstorage.StatusFailed):
		return nil
	default:
		return fmt.Errorf("%w: run %s has unexpected status %q", ErrReplayReservationCorrupt, runID, reservation.Status)
	}

	var taskCount int64
	if err := s.store.DB().WithContext(s.ctx).
		Model(&models.TaskRun{}).
		Where("job_run_id = ?", runID).
		Count(&taskCount).Error; err != nil {
		return err
	}
	if taskCount == 0 {
		return fmt.Errorf("%w: run %s", ErrReplayReservationIncomplete, runID)
	}

	var pendingCount int64
	if err := s.store.DB().WithContext(s.ctx).
		Model(&models.TaskRun{}).
		Where("job_run_id = ? AND status = ?", runID, string(runstorage.TaskStatusPending)).
		Count(&pendingCount).Error; err != nil {
		return err
	}
	if pendingCount == 0 {
		return nil
	}
	if !s.isDistributedExecutionMode() {
		return ErrReplayRequiresDistributedMode
	}
	return s.dispatcher.DispatchReplay(s.ctx, runID)
}

func (s *Service) isDistributedExecutionMode() bool {
	mode := ""
	if s != nil && s.executionMode != nil {
		mode = s.executionMode()
	} else {
		mode = env.Variables().ExecutionMode
	}
	return strings.EqualFold(strings.TrimSpace(mode), "distributed")
}

type fingerprintPayload struct {
	Version        int            `json:"version"`
	JobID          string         `json:"job_id"`
	BaselineRunID  string         `json:"baseline_run_id"`
	Principal      string         `json:"principal"`
	Overrides      []overridePair `json:"overrides"`
	IdempotencyKey string         `json:"idempotency_key"`
}

type overridePair struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Fingerprint derives the durable, scoped replay idempotency key.
func Fingerprint(jobID, baselineRunID uuid.UUID, principal *iauth.Principal, overrides map[string]string, idempotencyKey string) (string, error) {
	key := strings.TrimSpace(idempotencyKey)
	if key == "" {
		return "", ErrMissingIdempotencyKey
	}
	payload := fingerprintPayload{
		Version:        1,
		JobID:          jobID.String(),
		BaselineRunID:  baselineRunID.String(),
		Principal:      principalIdentity(principal),
		Overrides:      normalizeOverrides(overrides),
		IdempotencyKey: key,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("replay: encode idempotency fingerprint input: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return "replay:v1:" + hex.EncodeToString(sum[:]), nil
}

func principalIdentity(principal *iauth.Principal) string {
	if principal == nil {
		return "anonymous"
	}
	if principal.KeyID != nil {
		return string(principal.Kind) + ":" + principal.KeyID.String()
	}
	if principal.UserID != nil {
		return string(principal.Kind) + ":" + principal.UserID.String()
	}
	if subject := strings.TrimSpace(principal.Subject); subject != "" {
		return string(principal.Kind) + ":" + subject
	}
	return string(principal.Kind) + ":unknown"
}

func normalizeOverrides(overrides map[string]string) []overridePair {
	if len(overrides) == 0 {
		return []overridePair{}
	}
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	normalized := make([]overridePair, 0, len(keys))
	for _, key := range keys {
		normalized = append(normalized, overridePair{Key: key, Value: overrides[key]})
	}
	return normalized
}

func cloneSet(set map[string]string) map[string]string {
	if len(set) == 0 {
		return nil
	}
	out := make(map[string]string, len(set))
	for k, v := range set {
		out[k] = v
	}
	return out
}

func isUniqueReplayFingerprintError(err error) bool {
	return sqlerr.IsUniqueConstraint(err)
}

// AsyncDispatcher resumes a materialized replay run through the existing job
// execution surface. Local mode fails closed inside job.Run for quarantined
// replays; distributed mode lets workers reconstruct from task descriptors.
type AsyncDispatcher struct {
	store *runstorage.Store
}

// NewAsyncDispatcher creates the default REST replay dispatcher.
func NewAsyncDispatcher(store *runstorage.Store) *AsyncDispatcher {
	return &AsyncDispatcher{store: store}
}

// DispatchReplay starts or wakes execution for runID and returns once the launch
// was durably accepted.
func (d *AsyncDispatcher) DispatchReplay(ctx context.Context, runID uuid.UUID) error {
	if d == nil || d.store == nil {
		return errors.New("replay: run store is required")
	}

	var runModel models.JobRun
	if err := d.store.DB().WithContext(ctx).Select("id", "job_id").First(&runModel, "id = ?", runID).Error; err != nil {
		return err
	}

	var jobModel models.Job
	if err := d.store.DB().WithContext(ctx).First(&jobModel, "id = ?", runModel.JobID).Error; err != nil {
		return err
	}

	if ls := d.store.LeaseStore(); ls != nil {
		vars := env.Variables()
		if _, err := ls.AcquireLease(ctx, runID, vars.NodeAddress, vars.RunLeaseTTL); err != nil {
			return err
		}
	}

	go func() {
		runCtx := runstorage.WithContext(context.Background(), runID)
		err := jobrunner.New(
			&jobModel,
			jobrunner.WithTriggerID(nil),
			jobrunner.WithRunStoreFactory(func() *runstorage.Store { return d.store }),
		).Run(runCtx)
		if err != nil {
			log.Error("replay dispatch failure", "job_id", runModel.JobID, "run_id", runID, "error", err)
		}
	}()

	return nil
}

var _ replaycore.Dispatcher = (*AsyncDispatcher)(nil)
