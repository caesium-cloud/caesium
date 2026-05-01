package run

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/cache"
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/jsonmap"
	"github.com/caesium-cloud/caesium/pkg/log"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"github.com/mattn/go-sqlite3"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type Status string

type TaskStatus string

type Result string

const (
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
)

const (
	TaskStatusPending   TaskStatus = "pending"
	TaskStatusRunning   TaskStatus = "running"
	TaskStatusSucceeded TaskStatus = "succeeded"
	TaskStatusFailed    TaskStatus = "failed"
	TaskStatusSkipped   TaskStatus = "skipped"
	TaskStatusCached    TaskStatus = "cached"
)

// IsTerminalSuccess returns true for task statuses that represent successful completion.
func IsTerminalSuccess(status TaskStatus) bool {
	return status == TaskStatusSucceeded || status == TaskStatusCached
}

type CallbackStatus string

const (
	CallbackStatusRunning   CallbackStatus = "running"
	CallbackStatusSucceeded CallbackStatus = "succeeded"
	CallbackStatusFailed    CallbackStatus = "failed"
)

func IsSuccessfulTaskResult(result string) bool {
	return taskStatusFromResult(result) == TaskStatusSucceeded
}

func taskStatusFromResult(result string) TaskStatus {
	switch Result(result) {
	case "", "success", "ok":
		return TaskStatusSucceeded
	default:
		return TaskStatusFailed
	}
}

type CallbackRun struct {
	ID          uuid.UUID      `json:"id"`
	CallbackID  uuid.UUID      `json:"callback_id"`
	Status      CallbackStatus `json:"status"`
	Error       string         `json:"error,omitempty"`
	StartedAt   time.Time      `json:"started_at"`
	CompletedAt *time.Time     `json:"completed_at,omitempty"`
}

type TaskRun struct {
	ID                      uuid.UUID                 `json:"id"`
	JobRunID                uuid.UUID                 `json:"job_run_id"`
	TaskID                  uuid.UUID                 `json:"task_id"`
	JobAlias                string                    `json:"job_alias,omitempty"`
	JobLabels               map[string]string         `json:"job_labels,omitempty"`
	AtomID                  uuid.UUID                 `json:"atom_id"`
	Engine                  models.AtomEngine         `json:"engine"`
	Image                   string                    `json:"image"`
	Command                 []string                  `json:"command"`
	RuntimeID               string                    `json:"runtime_id,omitempty"`
	Status                  TaskStatus                `json:"status"`
	NodeSelector            map[string]string         `json:"node_selector,omitempty"`
	ClaimedBy               string                    `json:"claimed_by,omitempty"`
	ClaimExpiresAt          *time.Time                `json:"claim_expires_at,omitempty"`
	ClaimAttempt            int                       `json:"claim_attempt"`
	Attempt                 int                       `json:"attempt"`
	MaxAttempts             int                       `json:"max_attempts"`
	Result                  string                    `json:"result,omitempty"`
	Output                  map[string]string         `json:"output,omitempty"`
	SchemaViolations        []pkgtask.SchemaViolation `json:"schema_violations,omitempty"`
	BranchSelections        []string                  `json:"branch_selections,omitempty"`
	CacheHit                bool                      `json:"cache_hit"`
	CacheOriginRunID        *uuid.UUID                `json:"cache_origin_run_id,omitempty"`
	CacheCreatedAt          *time.Time                `json:"cache_created_at,omitempty"`
	CacheExpiresAt          *time.Time                `json:"cache_expires_at,omitempty"`
	StartedAt               *time.Time                `json:"started_at,omitempty"`
	CompletedAt             *time.Time                `json:"completed_at,omitempty"`
	Error                   string                    `json:"error,omitempty"`
	OutstandingPredecessors int                       `json:"outstanding_predecessors"`
	CreatedAt               time.Time                 `json:"created_at"`
	UpdatedAt               time.Time                 `json:"updated_at"`
}

type JobRun struct {
	ID            uuid.UUID         `json:"id"`
	JobID         uuid.UUID         `json:"job_id"`
	JobAlias      string            `json:"job_alias,omitempty"`
	JobLabels     map[string]string `json:"job_labels,omitempty"`
	BackfillID    *uuid.UUID        `json:"backfill_id,omitempty"`
	TriggerType   string            `json:"trigger_type,omitempty"`
	TriggerAlias  string            `json:"trigger_alias,omitempty"`
	Status        Status            `json:"status"`
	Params        map[string]string `json:"params,omitempty"`
	StartedAt     time.Time         `json:"started_at"`
	CompletedAt   *time.Time        `json:"completed_at,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
	Error         string            `json:"error,omitempty"`
	Tasks         []*TaskRun        `json:"tasks"`
	Callbacks     []*CallbackRun    `json:"callbacks"`
	CacheHits     int               `json:"cache_hits"`
	ExecutedTasks int               `json:"executed_tasks"`
	TotalTasks    int               `json:"total_tasks"`
}

type Store struct {
	db         *gorm.DB
	bus        event.Bus
	eventStore *event.Store

	// startedMu guards startedRuns.
	startedMu sync.Mutex
	// startedRuns tracks run IDs that were started via Start() in this
	// process so that Complete() only decrements the active-jobs gauge
	// for runs it actually incremented.
	startedRuns map[uuid.UUID]struct{}
}

type RegisterTaskInput struct {
	Task                    *models.Task
	Atom                    *models.Atom
	OutstandingPredecessors int
}

var (
	defaultStore     *Store
	defaultStoreOnce sync.Once
)

var ErrTaskClaimMismatch = errors.New("run: task claim mismatch")

var storeBusyRetryBackoffs = []time.Duration{
	10 * time.Millisecond,
	20 * time.Millisecond,
	40 * time.Millisecond,
	80 * time.Millisecond,
	160 * time.Millisecond,
}

func NewStore(conn *gorm.DB) *Store {
	if conn == nil {
		panic("run store requires database connection")
	}
	return &Store{
		db:          conn,
		eventStore:  event.NewStore(conn),
		startedRuns: make(map[uuid.UUID]struct{}),
	}
}

func Default() *Store {
	defaultStoreOnce.Do(func() {
		defaultStore = NewStore(db.Connection())
	})
	return defaultStore
}

func (s *Store) SetBus(bus event.Bus) {
	s.bus = bus
}

func (s *Store) Bus() event.Bus {
	return s.bus
}

func (s *Store) EventStore() *event.Store {
	return s.eventStore
}

func (s *Store) RecordEventTx(tx *gorm.DB, evt *event.Event) error {
	if evt == nil || s.eventStore == nil {
		return nil
	}
	return s.eventStore.AppendTx(tx, evt)
}

func (s *Store) DB() *gorm.DB {
	return s.db
}

func decodeCacheConfig(raw []byte) interface{} {
	if len(raw) == 0 {
		return nil
	}

	var decoded interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil
	}
	return decoded
}

func (s *Store) SetTaskHash(runID, taskID uuid.UUID, hash string) error {
	return s.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", runID, taskID).
		Update("hash", hash).Error
}

func (s *Store) Start(jobID uuid.UUID, triggerID *uuid.UUID, params ...map[string]string) (*JobRun, error) {
	model := &models.JobRun{
		ID:        uuid.New(),
		JobID:     jobID,
		Status:    string(StatusRunning),
		StartedAt: time.Now().UTC(),
	}

	if triggerID != nil {
		model.TriggerID = *triggerID
	}
	if len(params) > 0 && len(params[0]) > 0 {
		encoded, err := json.Marshal(params[0])
		if err != nil {
			return nil, fmt.Errorf("run: failed to marshal params: %w", err)
		}
		model.Params = encoded
	}

	var pendingEvents []event.Event
	if err := withStoreBusyRetry(func() error {
		attemptEvents := make([]event.Event, 0, 1)
		err := s.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Create(model).Error; err != nil {
				return err
			}

			if s.eventStore != nil {
				payload, err := json.Marshal(&JobRun{
					ID:        model.ID,
					JobID:     model.JobID,
					Status:    Status(model.Status),
					StartedAt: model.StartedAt,
					CreatedAt: model.CreatedAt,
					UpdatedAt: model.UpdatedAt,
					Tasks:     []*TaskRun{},
				})
				if err != nil {
					return err
				}

				evt := event.Event{
					Type:      event.TypeRunStarted,
					JobID:     jobID,
					RunID:     model.ID,
					Timestamp: time.Now().UTC(),
					Payload:   payload,
				}
				if err := s.eventStore.AppendTx(tx, &evt); err != nil {
					return err
				}
				attemptEvents = append(attemptEvents, evt)
			}

			return nil
		})
		if err == nil {
			pendingEvents = attemptEvents
		}
		return err
	}); err != nil {
		return nil, err
	}

	// Publish events immediately after commit, before loadRun, so that
	// run_started reaches the bus before any task events that the executor
	// may emit once Start returns.
	s.publishEvents(pendingEvents...)

	metrics.JobsActive.WithLabelValues(jobID.String()).Inc()
	s.startedMu.Lock()
	s.startedRuns[model.ID] = struct{}{}
	s.startedMu.Unlock()

	run, err := s.loadRun(model.ID)
	return run, err
}

// StartForBackfill creates a JobRun pre-linked to a backfill ID. The caller
// should then execute the job with run.WithContext(ctx, r.ID) so the executor
// resumes from this pre-created record rather than creating a new one.
func (s *Store) StartForBackfill(jobID, backfillID uuid.UUID, params map[string]string) (*JobRun, error) {
	model := &models.JobRun{
		ID:         uuid.New(),
		JobID:      jobID,
		BackfillID: &backfillID,
		Status:     string(StatusRunning),
		StartedAt:  time.Now().UTC(),
	}

	if len(params) > 0 {
		encoded, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("run: failed to marshal params: %w", err)
		}
		model.Params = encoded
	}

	var pendingEvents []event.Event
	if err := withStoreBusyRetry(func() error {
		attemptEvents := make([]event.Event, 0, 1)
		err := s.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Create(model).Error; err != nil {
				return err
			}

			if s.eventStore != nil {
				payload, err := json.Marshal(&JobRun{
					ID:        model.ID,
					JobID:     model.JobID,
					Status:    Status(model.Status),
					StartedAt: model.StartedAt,
					CreatedAt: model.CreatedAt,
					UpdatedAt: model.UpdatedAt,
					Tasks:     []*TaskRun{},
				})
				if err != nil {
					return err
				}

				evt := event.Event{
					Type:      event.TypeRunStarted,
					JobID:     jobID,
					RunID:     model.ID,
					Timestamp: time.Now().UTC(),
					Payload:   payload,
				}
				if err := s.eventStore.AppendTx(tx, &evt); err != nil {
					return err
				}
				attemptEvents = append(attemptEvents, evt)
			}

			return nil
		})
		if err == nil {
			pendingEvents = attemptEvents
		}
		return err
	}); err != nil {
		return nil, err
	}

	s.publishEvents(pendingEvents...)

	metrics.JobsActive.WithLabelValues(jobID.String()).Inc()
	s.startedMu.Lock()
	s.startedRuns[model.ID] = struct{}{}
	s.startedMu.Unlock()

	return s.loadRun(model.ID)
}

func (s *Store) RegisterTask(runID uuid.UUID, task *models.Task, atom *models.Atom, outstanding int) error {
	return s.RegisterTasks(runID, []RegisterTaskInput{{
		Task:                    task,
		Atom:                    atom,
		OutstandingPredecessors: outstanding,
	}})
}

func (s *Store) RegisterTasks(runID uuid.UUID, inputs []RegisterTaskInput) error {
	if len(inputs) == 0 {
		return nil
	}

	taskIDs := make([]uuid.UUID, 0, len(inputs))
	seenInputTaskIDs := make(map[uuid.UUID]struct{}, len(inputs))
	for _, input := range inputs {
		if input.Task == nil || input.Atom == nil {
			return errors.New("run: task and atom must be provided")
		}
		if _, ok := seenInputTaskIDs[input.Task.ID]; ok {
			continue
		}
		seenInputTaskIDs[input.Task.ID] = struct{}{}
		taskIDs = append(taskIDs, input.Task.ID)
	}

	var jobRun models.JobRun
	if err := s.db.Select("job_id").First(&jobRun, "id = ?", runID).Error; err != nil {
		return fmt.Errorf("run: job run %s not found: %w", runID, err)
	}
	jobID := jobRun.JobID

	var job models.Job
	jobFound := true
	if err := s.db.Select("id", "schema_validation", "cache_config").First(&job, "id = ?", jobID).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		jobFound = false
	}

	envCache := cache.ConfigFromEnv()
	jobCacheConfig := interface{}(nil)
	if jobFound {
		jobCacheConfig = decodeCacheConfig(job.CacheConfig)
	}

	var pendingEvents []event.Event
	err := withStoreBusyRetry(func() error {
		var attemptEvents []event.Event
		err := s.db.Transaction(func(tx *gorm.DB) error {
			existingTaskIDs := make([]uuid.UUID, 0)
			if len(taskIDs) > 0 {
				if err := tx.Model(&models.TaskRun{}).
					Where("job_run_id = ? AND task_id IN ?", runID, taskIDs).
					Pluck("task_id", &existingTaskIDs).Error; err != nil {
					return err
				}
			}
			existing := make(map[uuid.UUID]struct{}, len(existingTaskIDs))
			for _, taskID := range existingTaskIDs {
				existing[taskID] = struct{}{}
			}

			records := make([]models.TaskRun, 0, len(inputs))
			readyEvents := make([]event.Event, 0, len(inputs))
			seenNewTaskIDs := make(map[uuid.UUID]struct{}, len(inputs))
			for _, input := range inputs {
				task := input.Task
				atom := input.Atom
				if _, ok := existing[task.ID]; ok {
					continue
				}
				if _, ok := seenNewTaskIDs[task.ID]; ok {
					continue
				}
				seenNewTaskIDs[task.ID] = struct{}{}

				command := atom.Command
				if command == "" {
					if cmd := atom.Cmd(); len(cmd) > 0 {
						if encoded, marshalErr := json.Marshal(cmd); marshalErr == nil {
							command = string(encoded)
						}
					}
				}

				maxAttempts := task.Retries + 1
				if maxAttempts < 1 {
					maxAttempts = 1
				}

				schemaValidation := ""
				if jobFound && len(task.OutputSchema) > 0 {
					schemaValidation = job.SchemaValidation
				}

				resolvedCache := jobdefschema.ResolveCacheConfig(
					decodeCacheConfig(task.CacheConfig),
					jobCacheConfig,
					envCache.Enabled,
					envCache.TTL,
				)

				records = append(records, models.TaskRun{
					ID:                      uuid.New(),
					JobRunID:                runID,
					TaskID:                  task.ID,
					AtomID:                  task.AtomID,
					Engine:                  atom.Engine,
					Image:                   atom.Image,
					Command:                 command,
					Status:                  string(TaskStatusPending),
					NodeSelector:            maps.Clone(task.NodeSelector),
					Attempt:                 1,
					MaxAttempts:             maxAttempts,
					OutstandingPredecessors: input.OutstandingPredecessors,
					CacheEnabled:            resolvedCache.Enabled,
					CacheTTL:                resolvedCache.TTL,
					CacheVersion:            resolvedCache.Version,
					OutputSchema:            append(datatypes.JSON(nil), task.OutputSchema...),
					SchemaValidation:        schemaValidation,
				})

				if input.OutstandingPredecessors == 0 && s.eventStore != nil {
					readyEvents = append(readyEvents, event.Event{
						Type:      event.TypeTaskReady,
						JobID:     jobID,
						RunID:     runID,
						TaskID:    task.ID,
						Timestamp: time.Now().UTC(),
					})
				}
			}

			if len(records) == 0 {
				return nil
			}
			if err := tx.Create(&records).Error; err != nil {
				return err
			}
			if len(readyEvents) > 0 {
				eventRecords := make([]models.ExecutionEvent, 0, len(readyEvents))
				for _, evt := range readyEvents {
					eventRecords = append(eventRecords, executionEventRecord(evt))
				}
				if err := tx.Create(&eventRecords).Error; err != nil {
					return err
				}
				for idx := range readyEvents {
					readyEvents[idx].Sequence = eventRecords[idx].Sequence
					readyEvents[idx].Timestamp = eventRecords[idx].CreatedAt
				}
				attemptEvents = readyEvents
			}
			return nil
		})
		if err == nil {
			pendingEvents = attemptEvents
		}
		return err
	})
	if err == nil {
		s.publishEvents(pendingEvents...)
	}
	return err
}

func executionEventRecord(evt event.Event) models.ExecutionEvent {
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now().UTC()
	}

	record := models.ExecutionEvent{
		Type:      string(evt.Type),
		Payload:   []byte(evt.Payload),
		CreatedAt: evt.Timestamp,
	}
	if evt.JobID != uuid.Nil {
		jobID := evt.JobID
		record.JobID = &jobID
	}
	if evt.RunID != uuid.Nil {
		runID := evt.RunID
		record.RunID = &runID
	}
	if evt.TaskID != uuid.Nil {
		taskID := evt.TaskID
		record.TaskID = &taskID
	}
	return record
}

func (s *Store) StartTask(runID, taskID uuid.UUID, runtimeID string) error {
	var pendingEvents []event.Event
	err := withStoreBusyRetry(func() error {
		attemptEvents := make([]event.Event, 0, 1)
		err := s.db.Transaction(func(tx *gorm.DB) error {
			now := time.Now().UTC()
			if err := tx.Model(&models.TaskRun{}).
				Where("job_run_id = ? AND task_id = ?", runID, taskID).
				Updates(map[string]interface{}{
					"status":     string(TaskStatusRunning),
					"runtime_id": runtimeID,
					"started_at": now,
				}).Error; err != nil {
				return err
			}
			if s.eventStore != nil {
				evt, err := s.recordTaskEventTx(tx, event.TypeTaskStarted, runID, taskID)
				if err != nil {
					return err
				}
				attemptEvents = append(attemptEvents, *evt)
			}
			return nil
		})
		if err == nil {
			pendingEvents = attemptEvents
		}
		return err
	})
	if err == nil {
		s.publishEvents(pendingEvents...)
	}
	return err
}

func (s *Store) StartTaskClaimed(runID, taskID uuid.UUID, runtimeID, claimedBy string) error {
	var pendingEvents []event.Event
	err := withStoreBusyRetry(func() error {
		attemptEvents := make([]event.Event, 0, 1)
		err := s.db.Transaction(func(tx *gorm.DB) error {
			now := time.Now().UTC()
			result := tx.Model(&models.TaskRun{}).
				Where("job_run_id = ? AND task_id = ? AND claimed_by = ? AND status = ?", runID, taskID, claimedBy, string(TaskStatusRunning)).
				Updates(map[string]interface{}{
					"runtime_id": runtimeID,
					"started_at": now,
				})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 {
				return ErrTaskClaimMismatch
			}
			if s.eventStore != nil {
				evt, err := s.recordTaskEventTx(tx, event.TypeTaskStarted, runID, taskID)
				if err != nil {
					return err
				}
				attemptEvents = append(attemptEvents, *evt)
			}
			return nil
		})
		if err == nil {
			pendingEvents = attemptEvents
		}
		return err
	})
	if err == nil {
		s.publishEvents(pendingEvents...)
	}
	return err
}

func (s *Store) CompleteTask(runID, taskID uuid.UUID, result string, output map[string]string, branchSelections []string) error {
	skipped, err := s.completeTask(runID, taskID, result, "", false, output, branchSelections)
	_ = skipped
	return err
}

// CompleteTaskResult holds the result of a task completion, including any
// tasks that were skipped due to branch filtering.
type CompleteTaskResult struct {
	SkippedTaskIDs []uuid.UUID
}

type TaskLogSnapshot struct {
	Text      string
	Truncated bool
}

type CacheHitSource struct {
	RunID     uuid.UUID
	CreatedAt time.Time
	ExpiresAt *time.Time
}

// CompleteTaskWithResult completes a task and returns details about branch
// skips so the local executor can update its in-memory state.
func (s *Store) CompleteTaskWithResult(runID, taskID uuid.UUID, result string, output map[string]string, branchSelections []string) (*CompleteTaskResult, error) {
	skipped, err := s.completeTask(runID, taskID, result, "", false, output, branchSelections)
	if err != nil {
		return nil, err
	}
	return &CompleteTaskResult{SkippedTaskIDs: skipped}, nil
}

func (s *Store) CompleteTaskClaimed(runID, taskID uuid.UUID, result, claimedBy string, output map[string]string, branchSelections []string) error {
	_, err := s.completeTask(runID, taskID, result, claimedBy, true, output, branchSelections)
	return err
}

// CacheHitTask marks a task as completed via cache hit (local mode).
// It mirrors the CompleteTaskWithResult flow but sets status to "cached".
func (s *Store) CacheHitTask(runID, taskID uuid.UUID, source CacheHitSource, result string, output map[string]string, branchSelections []string) (*CompleteTaskResult, error) {
	skipped, err := s.cacheHitTask(runID, taskID, source, result, "", false, output, branchSelections)
	if err != nil {
		return nil, err
	}
	return &CompleteTaskResult{SkippedTaskIDs: skipped}, nil
}

// CacheHitTaskClaimed marks a claimed task as completed via cache hit (distributed mode).
func (s *Store) CacheHitTaskClaimed(runID, taskID uuid.UUID, source CacheHitSource, result, claimedBy string, output map[string]string, branchSelections []string) error {
	_, err := s.cacheHitTask(runID, taskID, source, result, claimedBy, true, output, branchSelections)
	return err
}

func (s *Store) cacheHitTask(runID, taskID uuid.UUID, source CacheHitSource, result, claimedBy string, enforceClaim bool, output map[string]string, branchSelections []string) ([]uuid.UUID, error) {
	var pendingEvents []event.Event
	var skippedTaskIDs []uuid.UUID
	err := withStoreBusyRetry(func() error {
		attemptEvents := make([]event.Event, 0, 8)
		attemptSkippedTaskIDs := make([]uuid.UUID, 0)

		err := s.db.Transaction(func(tx *gorm.DB) error {
			now := time.Now().UTC()

			// Verify the task run exists (and matches claim if enforced).
			var taskRun models.TaskRun
			taskQuery := tx.Where("job_run_id = ? AND task_id = ?", runID, taskID)
			if enforceClaim {
				taskQuery = taskQuery.Where("claimed_by = ?", claimedBy)
			}
			if err := taskQuery.First(&taskRun).Error; err != nil {
				if enforceClaim {
					return ErrTaskClaimMismatch
				}
				return err
			}

			updateQuery := tx.Model(&models.TaskRun{}).
				Where("job_run_id = ? AND task_id = ?", runID, taskID)
			if enforceClaim {
				updateQuery = updateQuery.Where("claimed_by = ?", claimedBy)
			}

			updates := map[string]interface{}{
				"status":              string(TaskStatusCached),
				"completed_at":        now,
				"result":              result,
				"cache_hit":           true,
				"cache_origin_run_id": source.RunID,
				"cache_created_at":    source.CreatedAt,
				"cache_expires_at":    source.ExpiresAt,
			}
			if len(output) > 0 {
				encoded, marshalErr := json.Marshal(output)
				if marshalErr != nil {
					return fmt.Errorf("marshalling task output: %w", marshalErr)
				}
				updates["output"] = encoded
			}
			if len(branchSelections) > 0 {
				encoded, marshalErr := json.Marshal(branchSelections)
				if marshalErr != nil {
					return fmt.Errorf("marshalling branch selections: %w", marshalErr)
				}
				updates["branch_selections"] = encoded
			}

			resultUpdate := updateQuery.Updates(updates)
			if resultUpdate.Error != nil {
				return resultUpdate.Error
			}
			if enforceClaim && resultUpdate.RowsAffected == 0 {
				return ErrTaskClaimMismatch
			}

			// Load the task model for edge traversal and branch detection.
			var taskModel models.Task
			if err := tx.First(&taskModel, "id = ?", taskID).Error; err != nil {
				return err
			}

			edges, err := s.successorEdgesTx(tx, taskModel)
			if err != nil {
				return err
			}

			// Determine branch filtering if this is a branch-type task.
			var branchSelectedIDs map[uuid.UUID]bool
			if len(edges) > 0 && taskModel.Type == "branch" {
				successorIDs := make([]uuid.UUID, 0, len(edges))
				for _, edge := range edges {
					successorIDs = append(successorIDs, edge.ToTaskID)
				}
				var successorTasks []models.Task
				if err := tx.Where("id IN ?", successorIDs).Find(&successorTasks).Error; err != nil {
					return err
				}
				successorNameToID := make(map[string]uuid.UUID, len(successorTasks))
				for _, st := range successorTasks {
					if st.Name != "" {
						successorNameToID[st.Name] = st.ID
					}
				}

				branchSelectedIDs = make(map[uuid.UUID]bool, len(branchSelections))
				for _, name := range branchSelections {
					if id, ok := successorNameToID[name]; ok {
						branchSelectedIDs[id] = true
					}
				}
			}

			for _, edge := range edges {
				// Branch filtering: skip successors not selected by the branch.
				if branchSelectedIDs != nil && !branchSelectedIDs[edge.ToTaskID] {
					reason := fmt.Sprintf("not selected by branch task %s", taskID)
					skipped, err := s.skipTaskAndDescendantsTx(tx, runID, edge.ToTaskID, reason, &attemptEvents)
					if err != nil {
						return err
					}
					attemptSkippedTaskIDs = append(attemptSkippedTaskIDs, skipped...)
					continue
				}

				if err := tx.Model(&models.TaskRun{}).
					Where("job_run_id = ? AND task_id = ?", runID, edge.ToTaskID).
					UpdateColumn("outstanding_predecessors", gorm.Expr("CASE WHEN outstanding_predecessors > 0 THEN outstanding_predecessors - 1 ELSE 0 END")).Error; err != nil {
					return err
				}

				var successor models.TaskRun
				if err := tx.Where("job_run_id = ? AND task_id = ?", runID, edge.ToTaskID).First(&successor).Error; err == nil &&
					successor.OutstandingPredecessors == 0 && successor.Status == string(TaskStatusPending) {
					shouldRun, rule, err := s.shouldRunTaskTx(tx, runID, edge.ToTaskID)
					if err != nil {
						return err
					}
					if shouldRun {
						if err := s.appendTaskReadyEventTx(tx, runID, edge.ToTaskID, &attemptEvents); err != nil {
							return err
						}
						continue
					}

					skipRuleReason := fmt.Sprintf("trigger rule %q not satisfied", rule)
					skipped, err := s.skipTaskAndDescendantsTx(tx, runID, edge.ToTaskID, skipRuleReason, &attemptEvents)
					if err != nil {
						return err
					}
					attemptSkippedTaskIDs = append(attemptSkippedTaskIDs, skipped...)
				}
			}

			if s.eventStore != nil {
				evt, err := s.recordTaskEventTx(tx, event.TypeTaskCached, runID, taskID)
				if err != nil {
					return err
				}
				attemptEvents = append(attemptEvents, *evt)
			}

			return nil
		})
		if err == nil {
			pendingEvents = attemptEvents
			skippedTaskIDs = attemptSkippedTaskIDs
		}
		return err
	})
	if err == nil {
		s.publishEvents(pendingEvents...)
	}
	return skippedTaskIDs, err
}

func (s *Store) SaveTaskLogSnapshot(runID, taskID uuid.UUID, snapshot *TaskLogSnapshot) error {
	if snapshot == nil {
		return nil
	}

	return s.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", runID, taskID).
		Updates(map[string]interface{}{
			"log_text":      snapshot.Text,
			"log_truncated": snapshot.Truncated,
		}).Error
}

// SaveSchemaViolations persists schema validation violations for a task run.
func (s *Store) SaveSchemaViolations(runID, taskID uuid.UUID, violations []pkgtask.SchemaViolation) error {
	if len(violations) == 0 {
		return nil
	}
	b, err := json.Marshal(violations)
	if err != nil {
		return err
	}
	return s.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", runID, taskID).
		Update("schema_violations", datatypes.JSON(b)).Error
}

func (s *Store) GetTaskLogSnapshot(runID, taskID uuid.UUID) (*TaskLogSnapshot, error) {
	var task models.TaskRun
	if err := s.db.
		Select("log_text", "log_truncated").
		Where("job_run_id = ? AND task_id = ?", runID, taskID).
		First(&task).Error; err != nil {
		return nil, err
	}

	if task.LogText == "" && !task.LogTruncated {
		return nil, nil
	}

	return &TaskLogSnapshot{
		Text:      task.LogText,
		Truncated: task.LogTruncated,
	}, nil
}

func (s *Store) successorEdgesTx(tx *gorm.DB, task models.Task) ([]models.TaskEdge, error) {
	var edges []models.TaskEdge
	if err := tx.Where("from_task_id = ?", task.ID).Find(&edges).Error; err != nil {
		return nil, err
	}
	if len(edges) > 0 {
		return edges, nil
	}

	var jobEdgeCount int64
	if err := tx.Model(&models.TaskEdge{}).
		Where("job_id = ?", task.JobID).
		Limit(1).
		Count(&jobEdgeCount).Error; err != nil {
		return nil, err
	}
	if jobEdgeCount > 0 {
		return edges, nil
	}

	var next models.Task
	err := tx.Where(
		"job_id = ? AND (position > ? OR (position = ? AND created_at > ?))",
		task.JobID,
		task.Position,
		task.Position,
		task.CreatedAt,
	).
		Order("position asc").
		Order("created_at asc").
		First(&next).Error
	if err == nil {
		edges = append(edges, models.TaskEdge{ToTaskID: next.ID})
		return edges, nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return edges, nil
	}
	return nil, err
}

func (s *Store) appendTaskReadyEventTx(tx *gorm.DB, runID, taskID uuid.UUID, pendingEvents *[]event.Event) error {
	if s.eventStore == nil {
		return nil
	}

	var jobRun models.JobRun
	if err := tx.Select("job_id").First(&jobRun, "id = ?", runID).Error; err != nil {
		return err
	}

	evt := event.Event{
		Type:      event.TypeTaskReady,
		JobID:     jobRun.JobID,
		RunID:     runID,
		TaskID:    taskID,
		Timestamp: time.Now().UTC(),
	}
	if err := s.eventStore.AppendTx(tx, &evt); err != nil {
		return err
	}
	*pendingEvents = append(*pendingEvents, evt)
	return nil
}

func (s *Store) markTaskSkippedTx(tx *gorm.DB, runID, taskID uuid.UUID, reason string, pendingEvents *[]event.Event) (bool, error) {
	result := tx.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ? AND status = ?", runID, taskID, string(TaskStatusPending)).
		Updates(map[string]interface{}{
			"status":              string(TaskStatusSkipped),
			"completed_at":        time.Now().UTC(),
			"error":               reason,
			"cache_hit":           false,
			"cache_origin_run_id": nil,
			"cache_created_at":    nil,
			"cache_expires_at":    nil,
		})
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected == 0 {
		return false, nil
	}

	if s.eventStore != nil {
		evt, err := s.recordTaskEventTx(tx, event.TypeTaskSkipped, runID, taskID)
		if err != nil {
			return false, err
		}
		*pendingEvents = append(*pendingEvents, *evt)
	}

	return true, nil
}

func (s *Store) predecessorStatusesTx(tx *gorm.DB, runID, taskID uuid.UUID) ([]TaskStatus, error) {
	var edges []models.TaskEdge
	if err := tx.Where("to_task_id = ?", taskID).Find(&edges).Error; err != nil {
		return nil, err
	}
	if len(edges) == 0 {
		return nil, nil
	}

	predIDs := make([]uuid.UUID, 0, len(edges))
	for _, edge := range edges {
		predIDs = append(predIDs, edge.FromTaskID)
	}

	var taskRuns []models.TaskRun
	if err := tx.Where("job_run_id = ? AND task_id IN ?", runID, predIDs).Find(&taskRuns).Error; err != nil {
		return nil, err
	}

	statuses := make([]TaskStatus, 0, len(taskRuns))
	for _, taskRun := range taskRuns {
		statuses = append(statuses, TaskStatus(taskRun.Status))
	}
	return statuses, nil
}

func satisfiesTriggerRule(rule string, predStatuses []TaskStatus) bool {
	if rule == "" {
		rule = jobdefschema.TriggerRuleAllSuccess
	}
	if len(predStatuses) == 0 {
		return true
	}

	isTerminal := func(s TaskStatus) bool {
		return s == TaskStatusSucceeded || s == TaskStatusCached || s == TaskStatusFailed || s == TaskStatusSkipped
	}

	switch rule {
	case jobdefschema.TriggerRuleAllSuccess:
		for _, s := range predStatuses {
			if s != TaskStatusSucceeded && s != TaskStatusCached {
				return false
			}
		}
		return true
	case jobdefschema.TriggerRuleAllDone, jobdefschema.TriggerRuleAlways:
		for _, s := range predStatuses {
			if !isTerminal(s) {
				return false
			}
		}
		return true
	case jobdefschema.TriggerRuleAllFailed:
		for _, s := range predStatuses {
			if s != TaskStatusFailed {
				return false
			}
		}
		return true
	case jobdefschema.TriggerRuleOneSuccess:
		for _, s := range predStatuses {
			if s == TaskStatusSucceeded || s == TaskStatusCached {
				return true
			}
		}
		return false
	default:
		for _, s := range predStatuses {
			if s != TaskStatusSucceeded && s != TaskStatusCached {
				return false
			}
		}
		return true
	}
}

func normalizedTriggerRule(rule string) string {
	if rule == "" {
		return jobdefschema.TriggerRuleAllSuccess
	}
	return rule
}

func (s *Store) shouldRunTaskTx(tx *gorm.DB, runID, taskID uuid.UUID) (bool, string, error) {
	var task models.Task
	if err := tx.Select("trigger_rule").First(&task, "id = ?", taskID).Error; err != nil {
		return false, "", err
	}

	predStatuses, err := s.predecessorStatusesTx(tx, runID, taskID)
	if err != nil {
		return false, "", err
	}

	return satisfiesTriggerRule(task.TriggerRule, predStatuses), normalizedTriggerRule(task.TriggerRule), nil
}

func (s *Store) completeTask(runID, taskID uuid.UUID, result, claimedBy string, enforceClaim bool, output map[string]string, branchSelections []string) ([]uuid.UUID, error) {
	var pendingEvents []event.Event
	var skippedTaskIDs []uuid.UUID
	err := withStoreBusyRetry(func() error {
		attemptEvents := make([]event.Event, 0, 8)
		attemptSkippedTaskIDs := make([]uuid.UUID, 0)

		err := s.db.Transaction(func(tx *gorm.DB) error {
			now := time.Now().UTC()

			status := taskStatusFromResult(result)

			// Capture task metadata for metrics before updating.
			var taskRun models.TaskRun
			taskQuery := tx.Where("job_run_id = ? AND task_id = ?", runID, taskID)
			if enforceClaim {
				taskQuery = taskQuery.Where("claimed_by = ?", claimedBy)
			}
			if err := taskQuery.First(&taskRun).Error; err == nil {
				var jobRun models.JobRun
				if err := tx.First(&jobRun, "id = ?", runID).Error; err == nil {
					jobID := jobRun.JobID.String()
					engine := string(taskRun.Engine)
					metrics.TaskRunsTotal.WithLabelValues(jobID, taskID.String(), engine, string(status)).Inc()
					if taskRun.StartedAt != nil {
						duration := now.Sub(*taskRun.StartedAt).Seconds()
						metrics.TaskRunDurationSeconds.WithLabelValues(jobID, engine, string(status)).Observe(duration)
					}
				}
			} else if enforceClaim && errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrTaskClaimMismatch
			}

			updateQuery := tx.Model(&models.TaskRun{}).
				Where("job_run_id = ? AND task_id = ?", runID, taskID)
			if enforceClaim {
				updateQuery = updateQuery.Where("claimed_by = ?", claimedBy)
			}

			updates := map[string]interface{}{
				"status":              string(status),
				"completed_at":        now,
				"result":              result,
				"cache_hit":           false,
				"cache_origin_run_id": nil,
				"cache_created_at":    nil,
				"cache_expires_at":    nil,
			}
			if len(output) > 0 {
				encoded, marshalErr := json.Marshal(output)
				if marshalErr != nil {
					return fmt.Errorf("marshalling task output: %w", marshalErr)
				}
				updates["output"] = encoded
			}
			if len(branchSelections) > 0 {
				encoded, marshalErr := json.Marshal(branchSelections)
				if marshalErr != nil {
					return fmt.Errorf("marshalling branch selections: %w", marshalErr)
				}
				updates["branch_selections"] = encoded
			}
			if status == TaskStatusFailed {
				msg := result
				switch Result(result) {
				case "failure":
					msg = "command exited with non-zero status"
				case "startup_failure":
					msg = "atom failed to start (check image/command)"
				case "resource_failure":
					msg = "atom exhausted resources (e.g. OOM)"
				case "killed":
					msg = "atom was forcefully killed"
				case "terminated":
					msg = "atom was gracefully terminated"
				}
				updates["error"] = msg
			}

			resultUpdate := updateQuery.Updates(updates)
			if resultUpdate.Error != nil {
				return resultUpdate.Error
			}
			if enforceClaim && resultUpdate.RowsAffected == 0 {
				return ErrTaskClaimMismatch
			}

			if status == TaskStatusFailed {
				if s.eventStore != nil {
					evt, err := s.recordTaskEventTx(tx, event.TypeTaskFailed, runID, taskID)
					if err != nil {
						return err
					}
					attemptEvents = append(attemptEvents, *evt)
				}
				return nil
			}

			// Load the task model once — needed for both edge fallback and branch
			// type detection.
			var taskModel models.Task
			if err := tx.First(&taskModel, "id = ?", taskID).Error; err != nil {
				return err
			}

			edges, err := s.successorEdgesTx(tx, taskModel)
			if err != nil {
				return err
			}

			// Determine branch filtering if this is a branch-type task.
			var branchSelectedIDs map[uuid.UUID]bool
			if len(edges) > 0 && taskModel.Type == "branch" {
				// Build valid target names from successor tasks.
				successorIDs := make([]uuid.UUID, 0, len(edges))
				for _, edge := range edges {
					successorIDs = append(successorIDs, edge.ToTaskID)
				}
				var successorTasks []models.Task
				if err := tx.Where("id IN ?", successorIDs).Find(&successorTasks).Error; err != nil {
					return err
				}
				successorNameToID := make(map[string]uuid.UUID, len(successorTasks))
				validTargets := make([]string, 0, len(successorTasks))
				for _, st := range successorTasks {
					if st.Name != "" {
						successorNameToID[st.Name] = st.ID
						validTargets = append(validTargets, st.Name)
					}
				}

				// Validate selections.
				validSet := make(map[string]bool, len(validTargets))
				for _, name := range validTargets {
					validSet[name] = true
				}
				for _, name := range branchSelections {
					if !validSet[name] {
						return fmt.Errorf("branch selected unknown step %q; valid targets: %v", name, validTargets)
					}
				}

				// Build the set of selected successor IDs.
				// An empty set means "skip all downstream" (no markers emitted).
				branchSelectedIDs = make(map[uuid.UUID]bool, len(branchSelections))
				for _, name := range branchSelections {
					if id, ok := successorNameToID[name]; ok {
						branchSelectedIDs[id] = true
					}
				}
			}

			for _, edge := range edges {
				// Branch filtering: skip successors not selected by the branch.
				if branchSelectedIDs != nil && !branchSelectedIDs[edge.ToTaskID] {
					reason := fmt.Sprintf("not selected by branch task %s", taskID)
					skipped, err := s.skipTaskAndDescendantsTx(tx, runID, edge.ToTaskID, reason, &attemptEvents)
					if err != nil {
						return err
					}
					attemptSkippedTaskIDs = append(attemptSkippedTaskIDs, skipped...)
					continue
				}

				if err := tx.Model(&models.TaskRun{}).
					Where("job_run_id = ? AND task_id = ?", runID, edge.ToTaskID).
					UpdateColumn("outstanding_predecessors", gorm.Expr("CASE WHEN outstanding_predecessors > 0 THEN outstanding_predecessors - 1 ELSE 0 END")).Error; err != nil {
					return err
				}

				var successor models.TaskRun
				if err := tx.Where("job_run_id = ? AND task_id = ?", runID, edge.ToTaskID).First(&successor).Error; err == nil &&
					successor.OutstandingPredecessors == 0 && successor.Status == string(TaskStatusPending) {
					shouldRun, rule, err := s.shouldRunTaskTx(tx, runID, edge.ToTaskID)
					if err != nil {
						return err
					}
					if shouldRun {
						if err := s.appendTaskReadyEventTx(tx, runID, edge.ToTaskID, &attemptEvents); err != nil {
							return err
						}
						continue
					}

					skipRuleReason := fmt.Sprintf("trigger rule %q not satisfied", rule)
					skipped, err := s.skipTaskAndDescendantsTx(tx, runID, edge.ToTaskID, skipRuleReason, &attemptEvents)
					if err != nil {
						return err
					}
					attemptSkippedTaskIDs = append(attemptSkippedTaskIDs, skipped...)
				}
			}

			if s.eventStore != nil {
				evt, err := s.recordTaskEventTx(tx, event.TypeTaskSucceeded, runID, taskID)
				if err != nil {
					return err
				}
				attemptEvents = append(attemptEvents, *evt)
			}

			return nil
		})
		if err == nil {
			pendingEvents = attemptEvents
			skippedTaskIDs = attemptSkippedTaskIDs
		}
		return err
	})
	if err == nil {
		s.publishEvents(pendingEvents...)
	}
	return skippedTaskIDs, err
}

// skipTaskAndDescendantsTx marks a task and all its transitive descendants as
// skipped within the given transaction. Descendants are only skipped once all
// of their predecessors are terminal and their trigger rules remain
// unsatisfied.
func (s *Store) skipTaskAndDescendantsTx(tx *gorm.DB, runID, taskID uuid.UUID, reason string, pendingEvents *[]event.Event) ([]uuid.UUID, error) {
	type queuedSkip struct {
		taskID uuid.UUID
		reason string
	}

	queue := []queuedSkip{{taskID: taskID, reason: reason}}
	var skipped []uuid.UUID

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		markedSkipped, err := s.markTaskSkippedTx(tx, runID, current.taskID, current.reason, pendingEvents)
		if err != nil {
			return skipped, err
		}
		if !markedSkipped {
			// Task was not pending (already completed/skipped) — don't propagate.
			continue
		}

		skipped = append(skipped, current.taskID)

		var task models.Task
		if err := tx.First(&task, "id = ?", current.taskID).Error; err != nil {
			return skipped, err
		}

		edges, err := s.successorEdgesTx(tx, task)
		if err != nil {
			return skipped, err
		}

		for _, edge := range edges {
			if err := tx.Model(&models.TaskRun{}).
				Where("job_run_id = ? AND task_id = ?", runID, edge.ToTaskID).
				UpdateColumn("outstanding_predecessors", gorm.Expr("CASE WHEN outstanding_predecessors > 0 THEN outstanding_predecessors - 1 ELSE 0 END")).Error; err != nil {
				return skipped, err
			}

			var successor models.TaskRun
			if err := tx.Where("job_run_id = ? AND task_id = ?", runID, edge.ToTaskID).First(&successor).Error; err != nil {
				return skipped, err
			}
			if successor.Status != string(TaskStatusPending) || successor.OutstandingPredecessors != 0 {
				continue
			}

			shouldRun, rule, err := s.shouldRunTaskTx(tx, runID, edge.ToTaskID)
			if err != nil {
				return skipped, err
			}
			if shouldRun {
				if err := s.appendTaskReadyEventTx(tx, runID, edge.ToTaskID, pendingEvents); err != nil {
					return skipped, err
				}
				continue
			}

			queue = append(queue, queuedSkip{
				taskID: edge.ToTaskID,
				reason: fmt.Sprintf("trigger rule %q not satisfied", rule),
			})
		}
	}

	return skipped, nil
}

func (s *Store) FailTask(runID, taskID uuid.UUID, failure error) error {
	return s.failTask(runID, taskID, failure, "", false)
}

func (s *Store) FailTaskClaimed(runID, taskID uuid.UUID, failure error, claimedBy string) error {
	return s.failTask(runID, taskID, failure, claimedBy, true)
}

func (s *Store) failTask(runID, taskID uuid.UUID, failure error, claimedBy string, enforceClaim bool) error {
	now := time.Now().UTC()
	errMsg := ""
	if failure != nil {
		errMsg = failure.Error()
	}

	// Record task failure metrics.
	var taskRun models.TaskRun
	taskQuery := s.db.Where("job_run_id = ? AND task_id = ?", runID, taskID)
	if enforceClaim {
		taskQuery = taskQuery.Where("claimed_by = ?", claimedBy)
	}
	if err := taskQuery.First(&taskRun).Error; err == nil {
		var jobRun models.JobRun
		if err := s.db.First(&jobRun, "id = ?", runID).Error; err == nil {
			jobID := jobRun.JobID.String()
			engine := string(taskRun.Engine)
			metrics.TaskRunsTotal.WithLabelValues(jobID, taskID.String(), engine, string(TaskStatusFailed)).Inc()
			if taskRun.StartedAt != nil {
				duration := now.Sub(*taskRun.StartedAt).Seconds()
				metrics.TaskRunDurationSeconds.WithLabelValues(jobID, engine, string(TaskStatusFailed)).Observe(duration)
			}
		}
	} else if enforceClaim && errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrTaskClaimMismatch
	}

	pendingEvents := make([]event.Event, 0, 1)
	err := s.db.Transaction(func(tx *gorm.DB) error {
		updateQuery := tx.Model(&models.TaskRun{}).
			Where("job_run_id = ? AND task_id = ?", runID, taskID)
		if enforceClaim {
			updateQuery = updateQuery.Where("claimed_by = ?", claimedBy)
		}
		resultUpdate := updateQuery.
			Updates(map[string]interface{}{
				"status":              string(TaskStatusFailed),
				"completed_at":        now,
				"error":               errMsg,
				"cache_hit":           false,
				"cache_origin_run_id": nil,
				"cache_created_at":    nil,
				"cache_expires_at":    nil,
			})
		if resultUpdate.Error != nil {
			return resultUpdate.Error
		}
		if enforceClaim && resultUpdate.RowsAffected == 0 {
			return ErrTaskClaimMismatch
		}

		if s.eventStore != nil {
			evt, err := s.recordTaskEventTx(tx, event.TypeTaskFailed, runID, taskID)
			if err != nil {
				return err
			}
			pendingEvents = append(pendingEvents, *evt)
		}
		return nil
	})
	if err == nil {
		s.publishEvents(pendingEvents...)
	}
	return err
}

// RetryTask resets a failed task run back to pending and increments its Attempt counter.
func (s *Store) RetryTask(runID, taskID uuid.UUID, attempt int) error {
	return s.retryTask(runID, taskID, attempt, "", false)
}

// RetryTaskClaimed resets a claimed failed task run and increments its Attempt counter.
func (s *Store) RetryTaskClaimed(runID, taskID uuid.UUID, attempt int, claimedBy string) error {
	return s.retryTask(runID, taskID, attempt, claimedBy, true)
}

func (s *Store) retryTask(runID, taskID uuid.UUID, attempt int, claimedBy string, enforceClaim bool) error {
	pendingEvents := make([]event.Event, 0, 2)
	err := s.db.Transaction(func(tx *gorm.DB) error {
		updateQuery := tx.Model(&models.TaskRun{}).
			Where("job_run_id = ? AND task_id = ?", runID, taskID)
		if enforceClaim {
			updateQuery = updateQuery.Where("claimed_by = ?", claimedBy)
		}
		resultUpdate := updateQuery.
			Updates(map[string]interface{}{
				"status":              string(TaskStatusPending),
				"attempt":             attempt,
				"runtime_id":          "",
				"started_at":          nil,
				"completed_at":        nil,
				"result":              "",
				"output":              nil,
				"branch_selections":   nil,
				"log_text":            "",
				"log_truncated":       false,
				"error":               "",
				"cache_hit":           false,
				"cache_origin_run_id": nil,
				"cache_created_at":    nil,
				"cache_expires_at":    nil,
			})
		if resultUpdate.Error != nil {
			return resultUpdate.Error
		}
		if enforceClaim && resultUpdate.RowsAffected == 0 {
			return ErrTaskClaimMismatch
		}

		if s.eventStore != nil {
			evt, err := s.recordTaskEventTx(tx, event.TypeTaskRetrying, runID, taskID)
			if err != nil {
				return err
			}
			pendingEvents = append(pendingEvents, *evt)

			var taskRun models.TaskRun
			if err := tx.Where("job_run_id = ? AND task_id = ?", runID, taskID).First(&taskRun).Error; err == nil &&
				taskRun.OutstandingPredecessors == 0 {
				var jobRun models.JobRun
				if err := tx.Select("job_id").First(&jobRun, "id = ?", runID).Error; err != nil {
					return err
				}
				readyEvt := event.Event{
					Type:      event.TypeTaskReady,
					JobID:     jobRun.JobID,
					RunID:     runID,
					TaskID:    taskID,
					Timestamp: time.Now().UTC(),
				}
				if err := s.eventStore.AppendTx(tx, &readyEvt); err != nil {
					return err
				}
				pendingEvents = append(pendingEvents, readyEvt)
			}
		}

		return nil
	})
	if err == nil {
		s.publishEvents(pendingEvents...)
	}
	return err
}

func (s *Store) SkipTask(runID, taskID uuid.UUID, reason string) error {
	now := time.Now().UTC()
	pendingEvents := make([]event.Event, 0, 1)
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.TaskRun{}).
			Where("job_run_id = ? AND task_id = ? AND status = ?", runID, taskID, string(TaskStatusPending)).
			Updates(map[string]interface{}{
				"status":              string(TaskStatusSkipped),
				"completed_at":        now,
				"error":               reason,
				"cache_hit":           false,
				"cache_origin_run_id": nil,
				"cache_created_at":    nil,
				"cache_expires_at":    nil,
			}).Error; err != nil {
			return err
		}
		if s.eventStore != nil {
			evt, err := s.recordTaskEventTx(tx, event.TypeTaskSkipped, runID, taskID)
			if err != nil {
				return err
			}
			pendingEvents = append(pendingEvents, *evt)
		}
		return nil
	})
	if err == nil {
		s.publishEvents(pendingEvents...)
	}
	return err
}

func (s *Store) Complete(runID uuid.UUID, result error) error {
	now := time.Now().UTC()
	status := StatusSucceeded
	errMsg := ""
	if result != nil {
		status = StatusFailed
		errMsg = result.Error()
	}

	// Look up the run to get jobID and startedAt for metrics.
	var run models.JobRun
	if err := s.db.First(&run, "id = ?", runID).Error; err == nil {
		jobID := run.JobID.String()
		metrics.JobRunsTotal.WithLabelValues(jobID, string(status)).Inc()
		// Only decrement the active gauge if this process incremented it.
		s.startedMu.Lock()
		_, started := s.startedRuns[runID]
		if started {
			delete(s.startedRuns, runID)
		}
		s.startedMu.Unlock()
		if started {
			metrics.JobsActive.WithLabelValues(jobID).Dec()
		}
		duration := now.Sub(run.StartedAt).Seconds()
		metrics.JobRunDurationSeconds.WithLabelValues(jobID, string(status)).Observe(duration)
	}

	pendingEvents := make([]event.Event, 0, 2)
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.JobRun{}).
			Where("id = ?", runID).
			Updates(map[string]interface{}{
				"status":       string(status),
				"completed_at": now,
				"error":        errMsg,
			}).Error; err != nil {
			return err
		}

		if s.eventStore != nil {
			run, loadErr := s.loadRunWithDB(tx, runID)
			if loadErr != nil {
				return loadErr
			}

			eventType := event.TypeRunCompleted
			if status == StatusFailed {
				eventType = event.TypeRunFailed
			}
			payload, marshalErr := json.Marshal(run)
			if marshalErr != nil {
				return marshalErr
			}

			completionEvent := event.Event{
				Type:      eventType,
				JobID:     run.JobID,
				RunID:     runID,
				Timestamp: time.Now().UTC(),
				Payload:   payload,
			}
			if err := s.eventStore.AppendTx(tx, &completionEvent); err != nil {
				return err
			}
			pendingEvents = append(pendingEvents, completionEvent)

			terminalEvent := event.Event{
				Type:      event.TypeRunTerminal,
				JobID:     run.JobID,
				RunID:     runID,
				Timestamp: time.Now().UTC(),
				Payload:   payload,
			}
			if err := s.eventStore.AppendTx(tx, &terminalEvent); err != nil {
				return err
			}
			pendingEvents = append(pendingEvents, terminalEvent)
		}

		return nil
	})
	if err == nil {
		s.publishEvents(pendingEvents...)
	}
	return err
}

func (s *Store) ResetInFlightTasks(runID uuid.UUID) error {
	return s.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND status = ?", runID, string(TaskStatusRunning)).
		Updates(map[string]interface{}{
			"status":              string(TaskStatusPending),
			"runtime_id":          "",
			"started_at":          nil,
			"cache_hit":           false,
			"cache_origin_run_id": nil,
			"cache_created_at":    nil,
			"cache_expires_at":    nil,
		}).Error
}

func (s *Store) FindRunning(jobID uuid.UUID) (*JobRun, error) {
	var model models.JobRun
	err := s.db.Where("job_id = ? AND status = ?", jobID, string(StatusRunning)).
		Order("started_at DESC").
		First(&model).Error
	if err != nil {
		return nil, err
	}
	return s.loadRun(model.ID)
}

func (s *Store) Get(runID uuid.UUID) (*JobRun, error) {
	return s.loadRun(runID)
}

func (s *Store) List(jobID uuid.UUID) ([]*JobRun, error) {
	var results []struct {
		models.JobRun
		JobAlias     string
		TriggerType  string
		TriggerAlias string
	}

	err := s.db.Table("job_runs").
		Select("job_runs.*, jobs.alias as job_alias, triggers.type as trigger_type, triggers.alias as trigger_alias").
		Joins("join jobs on jobs.id = job_runs.job_id").
		Joins("left join triggers on triggers.id = job_runs.trigger_id").
		Where("job_runs.job_id = ?", jobID).
		Order("job_runs.started_at ASC").
		Preload("Tasks").
		Scan(&results).Error

	if err != nil {
		return nil, err
	}

	runs := make([]*JobRun, 0, len(results))
	for i := range results {
		runValue, err := s.convertRunModel(&results[i].JobRun)
		if err != nil {
			return nil, err
		}
		runValue.JobAlias = results[i].JobAlias
		runValue.TriggerType = results[i].TriggerType
		runValue.TriggerAlias = results[i].TriggerAlias
		runs = append(runs, runValue)
	}

	return runs, nil
}

func (s *Store) Latest(jobID uuid.UUID) (*JobRun, error) {
	var model models.JobRun
	err := s.db.Where("job_id = ?", jobID).
		Order("started_at DESC").
		First(&model).Error
	if err != nil {
		return nil, err
	}
	return s.loadRun(model.ID)
}

// LatestSuccessfulCronRun returns the most recent cron-triggered run for a job
// that completed with status "succeeded". It returns gorm.ErrRecordNotFound
// when no such run exists.
func (s *Store) LatestSuccessfulCronRun(jobID uuid.UUID) (*JobRun, error) {
	var model models.JobRun
	err := s.db.
		Where("job_id = ? AND status = ? AND trigger_type = ?", jobID, string(StatusSucceeded), "cron").
		Order("started_at DESC").
		First(&model).Error
	if err != nil {
		return nil, err
	}
	return s.loadRun(model.ID)
}

func (s *Store) loadRun(runID uuid.UUID) (*JobRun, error) {
	return s.loadRunWithDB(s.db, runID)
}

func (s *Store) loadRunWithDB(conn *gorm.DB, runID uuid.UUID) (*JobRun, error) {
	var result struct {
		models.JobRun
		JobAlias     string
		JobLabels    datatypes.JSONMap
		TriggerType  string
		TriggerAlias string
	}

	// Use a JOIN to fetch job and trigger information for human readability
	err := conn.Table("job_runs").
		Select("job_runs.*, jobs.alias as job_alias, jobs.labels as job_labels, triggers.type as trigger_type, triggers.alias as trigger_alias").
		Joins("left join jobs on jobs.id = job_runs.job_id").
		Joins("left join triggers on triggers.id = job_runs.trigger_id").
		Where("job_runs.id = ?", runID).
		Preload("Tasks").
		First(&result).Error
	if err != nil {
		return nil, err
	}

	runValue, err := s.convertRunModelWithDB(conn, &result.JobRun)
	if err != nil {
		return nil, err
	}

	runValue.JobAlias = result.JobAlias
	runValue.JobLabels = jsonmap.ToStringMap(result.JobLabels)
	runValue.TriggerType = result.TriggerType
	runValue.TriggerAlias = result.TriggerAlias

	// Propagate job metadata to task runs for downstream event payloads.
	for i := range runValue.Tasks {
		runValue.Tasks[i].JobAlias = runValue.JobAlias
		runValue.Tasks[i].JobLabels = runValue.JobLabels
	}

	return runValue, nil
}

func (s *Store) convertRunModel(model *models.JobRun) (*JobRun, error) {
	return s.convertRunModelWithDB(s.db, model)
}

func (s *Store) convertRunModelWithDB(conn *gorm.DB, model *models.JobRun) (*JobRun, error) {
	if model == nil {
		return nil, nil
	}

	runValue := &JobRun{
		ID:         model.ID,
		JobID:      model.JobID,
		BackfillID: model.BackfillID,
		Status:     Status(model.Status),
		StartedAt:  model.StartedAt,
		CreatedAt:  model.CreatedAt,
		UpdatedAt:  model.UpdatedAt,
		Error:      model.Error,
	}

	if len(model.Params) > 0 {
		var p map[string]string
		if err := json.Unmarshal(model.Params, &p); err == nil {
			runValue.Params = p
		}
	}

	if model.CompletedAt != nil {
		completed := *model.CompletedAt
		runValue.CompletedAt = &completed
	}

	runValue.Tasks = make([]*TaskRun, 0, len(model.Tasks))
	for _, task := range model.Tasks {
		if task == nil {
			continue
		}
		runValue.Tasks = append(runValue.Tasks, convertRunTaskModel(task))
	}
	runValue.CacheHits, runValue.ExecutedTasks, runValue.TotalTasks = summarizeTasks(runValue.Tasks)

	callbackRuns, err := s.loadCallbackRunsWithDB(conn, model.ID)
	if err != nil {
		return nil, err
	}
	runValue.Callbacks = callbackRuns

	return runValue, nil
}

func convertRunTaskModel(model *models.TaskRun) *TaskRun {
	if model == nil {
		return nil
	}

	var command []string
	if model.Command != "" {
		if err := json.Unmarshal([]byte(model.Command), &command); err != nil {
			command = []string{model.Command}
		}
	}

	task := &TaskRun{
		ID:                      model.TaskID,
		JobRunID:                model.JobRunID,
		TaskID:                  model.TaskID,
		AtomID:                  model.AtomID,
		Engine:                  model.Engine,
		Image:                   model.Image,
		Command:                 command,
		RuntimeID:               model.RuntimeID,
		Status:                  TaskStatus(model.Status),
		NodeSelector:            jsonmap.ToStringMap(model.NodeSelector),
		ClaimedBy:               model.ClaimedBy,
		ClaimAttempt:            model.ClaimAttempt,
		Attempt:                 model.Attempt,
		MaxAttempts:             model.MaxAttempts,
		Result:                  model.Result,
		Error:                   model.Error,
		OutstandingPredecessors: model.OutstandingPredecessors,
		CreatedAt:               model.CreatedAt,
		UpdatedAt:               model.UpdatedAt,
		CacheHit:                model.CacheHit || TaskStatus(model.Status) == TaskStatusCached,
	}

	if len(model.Output) > 0 {
		var out map[string]string
		if err := json.Unmarshal(model.Output, &out); err == nil {
			task.Output = out
		}
	}

	if len(model.SchemaViolations) > 0 {
		var violations []pkgtask.SchemaViolation
		if err := json.Unmarshal(model.SchemaViolations, &violations); err == nil {
			task.SchemaViolations = violations
		}
	}

	if len(model.BranchSelections) > 0 {
		var bs []string
		if err := json.Unmarshal(model.BranchSelections, &bs); err == nil {
			task.BranchSelections = bs
		}
	}

	if model.StartedAt != nil {
		started := *model.StartedAt
		task.StartedAt = &started
	}
	if model.ClaimExpiresAt != nil {
		expiresAt := *model.ClaimExpiresAt
		task.ClaimExpiresAt = &expiresAt
	}
	if model.CompletedAt != nil {
		completed := *model.CompletedAt
		task.CompletedAt = &completed
	}
	if model.CacheOriginRunID != nil {
		originRunID := *model.CacheOriginRunID
		task.CacheOriginRunID = &originRunID
	}
	if model.CacheCreatedAt != nil {
		cacheCreatedAt := *model.CacheCreatedAt
		task.CacheCreatedAt = &cacheCreatedAt
	}
	if model.CacheExpiresAt != nil {
		cacheExpiresAt := *model.CacheExpiresAt
		task.CacheExpiresAt = &cacheExpiresAt
	}

	return task
}

func summarizeTasks(tasks []*TaskRun) (cacheHits, executedTasks, totalTasks int) {
	totalTasks = len(tasks)
	for _, task := range tasks {
		if task == nil {
			continue
		}
		if task.CacheHit || task.Status == TaskStatusCached {
			cacheHits++
			continue
		}
		switch task.Status {
		case TaskStatusRunning, TaskStatusSucceeded, TaskStatusFailed:
			executedTasks++
		}
	}
	return cacheHits, executedTasks, totalTasks
}

func (s *Store) loadCallbackRunsWithDB(conn *gorm.DB, runID uuid.UUID) ([]*CallbackRun, error) {
	var modelRuns []models.CallbackRun
	if err := conn.
		Where("job_run_id = ?", runID).
		Order("started_at ASC").
		Find(&modelRuns).Error; err != nil {
		return nil, err
	}

	result := make([]*CallbackRun, 0, len(modelRuns))
	for idx := range modelRuns {
		result = append(result, convertCallbackRunModel(&modelRuns[idx]))
	}
	return result, nil
}

func convertCallbackRunModel(model *models.CallbackRun) *CallbackRun {
	if model == nil {
		return nil
	}
	return &CallbackRun{
		ID:          model.ID,
		CallbackID:  model.CallbackID,
		Status:      CallbackStatus(model.Status),
		Error:       model.Error,
		StartedAt:   model.StartedAt,
		CompletedAt: model.CompletedAt,
	}
}

func (s *Store) recordTaskEventTx(db *gorm.DB, eventType event.Type, runID, taskID uuid.UUID) (*event.Event, error) {
	var taskRun models.TaskRun
	if err := db.Where("job_run_id = ? AND task_id = ?", runID, taskID).First(&taskRun).Error; err != nil {
		log.Error("failed to fetch task run for event", "error", err, "run_id", runID, "task_id", taskID)
		return nil, err
	}

	var jobRun models.JobRun
	if err := db.Preload("Job").First(&jobRun, "id = ?", runID).Error; err != nil {
		log.Error("failed to fetch job run for event", "error", err, "run_id", runID)
		return nil, err
	}

	taskPayload := convertRunTaskModel(&taskRun)
	taskPayload.JobAlias = jobRun.Job.Alias
	taskPayload.JobLabels = jsonmap.ToStringMap(jobRun.Job.Labels)
	// Use task-run row ID for event payloads so downstream consumers can identify
	// each task execution uniquely across retries/runs.
	taskPayload.ID = taskRun.ID

	payload, err := json.Marshal(taskPayload)
	if err != nil {
		log.Error("failed to marshal task run for event", "error", err, "run_id", runID, "task_id", taskID)
		return nil, err
	}

	evt := event.Event{
		Type:      eventType,
		JobID:     jobRun.JobID,
		RunID:     runID,
		TaskID:    taskID,
		Timestamp: time.Now().UTC(),
		Payload:   payload,
	}
	if s.eventStore != nil {
		if err := s.eventStore.AppendTx(db, &evt); err != nil {
			return nil, err
		}
	}
	return &evt, nil
}

func (s *Store) publishEvents(events ...event.Event) {
	if s.bus == nil {
		return
	}
	for _, evt := range events {
		s.bus.Publish(evt)
	}
}

func (s *Store) PublishEvents(events ...event.Event) {
	s.publishEvents(events...)
}

func withStoreBusyRetry(fn func() error) error {
	var err error
	for attempt := 0; ; attempt++ {
		err = fn()
		if err == nil || !isStoreContentionErr(err) {
			return err
		}
		if attempt >= len(storeBusyRetryBackoffs) {
			return err
		}

		metrics.DBBusyRetriesTotal.Inc()
		time.Sleep(jitterStoreBusyRetryBackoff(storeBusyRetryBackoffs[attempt]))
	}
}

func jitterStoreBusyRetryBackoff(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}

	maxJitter := int64(base / 5)
	if maxJitter <= 0 {
		return base
	}
	return base - time.Duration(rand.Int64N(maxJitter+1))
}

func isStoreContentionErr(err error) bool {
	if err == nil {
		return false
	}

	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.Code == sqlite3.ErrBusy || sqliteErr.Code == sqlite3.ErrLocked
	}

	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked") ||
		strings.Contains(msg, "database schema is locked") ||
		strings.Contains(msg, "database is busy") ||
		strings.Contains(msg, "checkpoint in progress") ||
		strings.Contains(msg, "sqlite_busy") ||
		strings.Contains(msg, "sqlite_locked")
}

// PredecessorOutputs returns a map of step-name → output key-values for all
// predecessors of the given task within a run.  This is used by the distributed
// executor to inject CAESIUM_OUTPUT_* env vars before starting a task.
func (s *Store) PredecessorOutputs(runID, taskID uuid.UUID) (map[string]map[string]string, error) {
	// Find predecessor task IDs via edges.
	var edges []models.TaskEdge
	if err := s.db.Where("to_task_id = ?", taskID).Find(&edges).Error; err != nil {
		return nil, err
	}

	if len(edges) == 0 {
		return nil, nil
	}

	// Batch-fetch predecessor tasks and task runs to avoid N+1 queries.
	predTaskIDs := make([]uuid.UUID, len(edges))
	for i, edge := range edges {
		predTaskIDs[i] = edge.FromTaskID
	}

	var tasks []models.Task
	if err := s.db.Where("id IN ?", predTaskIDs).Find(&tasks).Error; err != nil {
		log.Warn("failed to find predecessor tasks for output", "run_id", runID, "task_id", taskID, "error", err)
		return nil, nil
	}
	tasksByID := make(map[uuid.UUID]models.Task, len(tasks))
	for i := range tasks {
		tasksByID[tasks[i].ID] = tasks[i]
	}

	var taskRuns []models.TaskRun
	if err := s.db.Where("job_run_id = ? AND task_id IN ?", runID, predTaskIDs).Find(&taskRuns).Error; err != nil {
		log.Warn("failed to find predecessor task runs for output", "run_id", runID, "task_id", taskID, "error", err)
		return nil, nil
	}
	taskRunsByTaskID := make(map[uuid.UUID]models.TaskRun, len(taskRuns))
	for i := range taskRuns {
		taskRunsByTaskID[taskRuns[i].TaskID] = taskRuns[i]
	}

	result := make(map[string]map[string]string, len(edges))
	for _, edge := range edges {
		task, ok := tasksByID[edge.FromTaskID]
		if !ok {
			continue
		}
		taskRun, ok := taskRunsByTaskID[edge.FromTaskID]
		if !ok || len(taskRun.Output) == 0 {
			continue
		}

		var output map[string]string
		if err := json.Unmarshal(taskRun.Output, &output); err != nil {
			log.Warn("failed to unmarshal predecessor task output", "run_id", runID, "task_id", taskID, "predecessor_task_id", edge.FromTaskID, "error", err)
			continue
		}

		stepName := task.Name
		if stepName == "" {
			stepName = task.ID.String()
		}
		result[stepName] = output
	}

	if len(result) == 0 {
		return nil, nil
	}
	return result, nil
}

// PredecessorHashes returns the execution hashes recorded on predecessor task
// runs that completed successfully in the current run. This keeps distributed
// cache hashing aligned with local execution, including transitive cache hits.
func (s *Store) PredecessorHashes(runID, taskID uuid.UUID) ([]string, error) {
	var edges []models.TaskEdge
	if err := s.db.Where("to_task_id = ?", taskID).Find(&edges).Error; err != nil {
		return nil, err
	}

	if len(edges) == 0 {
		return nil, nil
	}

	predTaskIDs := make([]uuid.UUID, len(edges))
	for i, edge := range edges {
		predTaskIDs[i] = edge.FromTaskID
	}

	var taskRuns []models.TaskRun
	if err := s.db.
		Select("hash").
		Where("job_run_id = ? AND task_id IN ? AND status IN ? AND hash <> ''",
			runID,
			predTaskIDs,
			[]string{string(TaskStatusSucceeded), string(TaskStatusCached)},
		).
		Find(&taskRuns).Error; err != nil {
		log.Warn("failed to find predecessor task runs for hashes", "run_id", runID, "task_id", taskID, "error", err)
		return nil, nil
	}
	if len(taskRuns) == 0 {
		return nil, nil
	}

	hashes := make([]string, 0, len(taskRuns))
	for _, taskRun := range taskRuns {
		if taskRun.Hash != "" {
			hashes = append(hashes, taskRun.Hash)
		}
	}
	if len(hashes) == 0 {
		return nil, nil
	}
	sort.Strings(hashes)
	return hashes, nil
}

// RetryFromFailure resets a failed run so that previously-succeeded and cached
// tasks are preserved and only failed/pending/skipped tasks are re-executed.
func (s *Store) RetryFromFailure(runID uuid.UUID) (*JobRun, error) {
	pendingEvents := make([]event.Event, 0, 2)
	var jobID uuid.UUID

	err := s.db.Transaction(func(tx *gorm.DB) error {
		// 1. Verify the run exists and is in a terminal state (failed/succeeded).
		var jobRun models.JobRun
		if err := tx.First(&jobRun, "id = ?", runID).Error; err != nil {
			return err
		}
		if jobRun.Status != string(StatusFailed) && jobRun.Status != string(StatusSucceeded) {
			return fmt.Errorf("can only retry runs in terminal state, current: %s", jobRun.Status)
		}
		jobID = jobRun.JobID

		// 2. Reset the job run status to running.
		if err := tx.Model(&jobRun).Updates(map[string]interface{}{
			"status":       string(StatusRunning),
			"completed_at": nil,
			"error":        "",
		}).Error; err != nil {
			return err
		}

		// 3. Get all task runs for this run.
		var taskRuns []models.TaskRun
		if err := tx.Where("job_run_id = ?", runID).Find(&taskRuns).Error; err != nil {
			return err
		}

		// Build a set of task IDs that are in a terminal success state.
		terminalSuccessIDs := make(map[uuid.UUID]struct{})
		for i := range taskRuns {
			tr := &taskRuns[i]
			if IsTerminalSuccess(TaskStatus(tr.Status)) {
				terminalSuccessIDs[tr.TaskID] = struct{}{}
			}
		}

		// 4. Reset failed and skipped tasks to pending.
		// Leave succeeded and cached tasks as-is.
		resetTaskIDs := make([]uuid.UUID, 0)
		for i := range taskRuns {
			tr := &taskRuns[i]
			status := TaskStatus(tr.Status)
			if status == TaskStatusFailed || status == TaskStatusSkipped {
				updates := map[string]interface{}{
					"status":              string(TaskStatusPending),
					"completed_at":        nil,
					"result":              "",
					"error":               "",
					"started_at":          nil,
					"claimed_by":          "",
					"claim_expires_at":    nil,
					"runtime_id":          "",
					"attempt":             1,
					"cache_hit":           false,
					"cache_origin_run_id": nil,
					"cache_created_at":    nil,
					"cache_expires_at":    nil,
				}
				if err := tx.Model(tr).Updates(updates).Error; err != nil {
					return err
				}
				resetTaskIDs = append(resetTaskIDs, tr.TaskID)
			}
		}

		// 5. Recalculate outstanding_predecessors for each reset (pending) task.
		// For each pending task, count predecessors that are NOT in terminal success state.
		for _, taskID := range resetTaskIDs {
			var edges []models.TaskEdge
			if err := tx.Where("to_task_id = ?", taskID).Find(&edges).Error; err != nil {
				return err
			}

			outstanding := 0
			for _, edge := range edges {
				if _, ok := terminalSuccessIDs[edge.FromTaskID]; !ok {
					outstanding++
				}
			}

			if err := tx.Model(&models.TaskRun{}).
				Where("job_run_id = ? AND task_id = ?", runID, taskID).
				Update("outstanding_predecessors", outstanding).Error; err != nil {
				return err
			}

			// If all predecessors are already done, emit a task_ready event.
			if outstanding == 0 {
				if err := s.appendTaskReadyEventTx(tx, runID, taskID, &pendingEvents); err != nil {
					return err
				}
			}
		}

		// 6. Emit a run_retried event.
		if s.eventStore != nil {
			run, loadErr := s.loadRunWithDB(tx, runID)
			if loadErr != nil {
				return loadErr
			}
			payload, marshalErr := json.Marshal(run)
			if marshalErr != nil {
				return marshalErr
			}
			evt := event.Event{
				Type:      event.TypeRunRetried,
				JobID:     jobRun.JobID,
				RunID:     runID,
				Timestamp: time.Now().UTC(),
				Payload:   payload,
			}
			if err := s.eventStore.AppendTx(tx, &evt); err != nil {
				return err
			}
			pendingEvents = append(pendingEvents, evt)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	s.publishEvents(pendingEvents...)

	// Track this run in the active set so Complete() will decrement the gauge.
	s.startedMu.Lock()
	s.startedRuns[runID] = struct{}{}
	s.startedMu.Unlock()
	metrics.JobsActive.WithLabelValues(jobID.String()).Inc()

	return s.loadRun(runID)
}
