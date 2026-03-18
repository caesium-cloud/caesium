package run

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/caesium-cloud/caesium/pkg/jsonmap"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
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
)

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
	ID                      uuid.UUID         `json:"id"`
	JobRunID                uuid.UUID         `json:"job_run_id"`
	TaskID                  uuid.UUID         `json:"task_id"`
	AtomID                  uuid.UUID         `json:"atom_id"`
	Engine                  models.AtomEngine `json:"engine"`
	Image                   string            `json:"image"`
	Command                 []string          `json:"command"`
	RuntimeID               string            `json:"runtime_id,omitempty"`
	Status                  TaskStatus        `json:"status"`
	NodeSelector            map[string]string `json:"node_selector,omitempty"`
	ClaimedBy               string            `json:"claimed_by,omitempty"`
	ClaimExpiresAt          *time.Time        `json:"claim_expires_at,omitempty"`
	ClaimAttempt            int               `json:"claim_attempt"`
	Attempt                 int               `json:"attempt"`
	MaxAttempts             int               `json:"max_attempts"`
	Result                  string            `json:"result,omitempty"`
	Output                  map[string]string `json:"output,omitempty"`
	StartedAt               *time.Time        `json:"started_at,omitempty"`
	CompletedAt             *time.Time        `json:"completed_at,omitempty"`
	Error                   string            `json:"error,omitempty"`
	OutstandingPredecessors int               `json:"outstanding_predecessors"`
	CreatedAt               time.Time         `json:"created_at"`
	UpdatedAt               time.Time         `json:"updated_at"`
}

type JobRun struct {
	ID           uuid.UUID         `json:"id"`
	JobID        uuid.UUID         `json:"job_id"`
	JobAlias     string            `json:"job_alias,omitempty"`
	TriggerType  string            `json:"trigger_type,omitempty"`
	TriggerAlias string            `json:"trigger_alias,omitempty"`
	Status       Status            `json:"status"`
	Params       map[string]string `json:"params,omitempty"`
	StartedAt    time.Time         `json:"started_at"`
	CompletedAt  *time.Time        `json:"completed_at,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
	Error        string            `json:"error,omitempty"`
	Tasks        []*TaskRun        `json:"tasks"`
	Callbacks    []*CallbackRun    `json:"callbacks"`
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

var (
	defaultStore     *Store
	defaultStoreOnce sync.Once
)

var ErrTaskClaimMismatch = errors.New("run: task claim mismatch")

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

	pendingEvents := make([]event.Event, 0, 1)
	if err := s.db.Transaction(func(tx *gorm.DB) error {
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
			pendingEvents = append(pendingEvents, evt)
		}

		return nil
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

func (s *Store) RegisterTask(runID uuid.UUID, task *models.Task, atom *models.Atom, outstanding int) error {
	if task == nil || atom == nil {
		return errors.New("run: task and atom must be provided")
	}

	var existing models.TaskRun
	err := s.db.Where("job_run_id = ? AND task_id = ?", runID, task.ID).First(&existing).Error
	if err == nil {
		return nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}

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

	record := &models.TaskRun{
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
		OutstandingPredecessors: outstanding,
	}

	pendingEvents := make([]event.Event, 0, 1)
	err = s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(record).Error; err != nil {
			return err
		}
		if outstanding == 0 && s.eventStore != nil {
			evt := event.Event{
				Type:      event.TypeTaskReady,
				RunID:     runID,
				TaskID:    task.ID,
				Timestamp: time.Now().UTC(),
			}
			var jobRun models.JobRun
			if err := tx.Select("job_id").First(&jobRun, "id = ?", runID).Error; err == nil {
				evt.JobID = jobRun.JobID
			}
			if err := s.eventStore.AppendTx(tx, &evt); err != nil {
				return err
			}
			pendingEvents = append(pendingEvents, evt)
		}
		return nil
	})
	if err == nil {
		s.publishEvents(pendingEvents...)
	}
	return err
}

func (s *Store) StartTask(runID, taskID uuid.UUID, runtimeID string) error {
	now := time.Now().UTC()
	pendingEvents := make([]event.Event, 0, 1)
	err := s.db.Transaction(func(tx *gorm.DB) error {
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
			pendingEvents = append(pendingEvents, *evt)
		}
		return nil
	})
	if err == nil {
		s.publishEvents(pendingEvents...)
	}
	return err
}

func (s *Store) StartTaskClaimed(runID, taskID uuid.UUID, runtimeID, claimedBy string) error {
	now := time.Now().UTC()
	pendingEvents := make([]event.Event, 0, 1)
	err := s.db.Transaction(func(tx *gorm.DB) error {
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
			pendingEvents = append(pendingEvents, *evt)
		}
		return nil
	})
	if err == nil {
		s.publishEvents(pendingEvents...)
	}
	return err
}

func (s *Store) CompleteTask(runID, taskID uuid.UUID, result string, output map[string]string) error {
	return s.completeTask(runID, taskID, result, "", false, output)
}

func (s *Store) CompleteTaskClaimed(runID, taskID uuid.UUID, result, claimedBy string, output map[string]string) error {
	return s.completeTask(runID, taskID, result, claimedBy, true, output)
}

func (s *Store) completeTask(runID, taskID uuid.UUID, result, claimedBy string, enforceClaim bool, output map[string]string) error {
	pendingEvents := make([]event.Event, 0, 8)
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
			"status":       string(status),
			"completed_at": now,
			"result":       result,
		}
		if len(output) > 0 {
			encoded, marshalErr := json.Marshal(output)
			if marshalErr != nil {
				return fmt.Errorf("marshalling task output: %w", marshalErr)
			}
			updates["output"] = encoded
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
				pendingEvents = append(pendingEvents, *evt)
			}
			return nil
		}

		var edges []models.TaskEdge
		if err := tx.Where("from_task_id = ?", taskID).Find(&edges).Error; err != nil {
			return err
		}
		if len(edges) == 0 {
			// No explicit edges from this task. Check whether the job
			// uses edges at all; if it does, this task is simply a leaf
			// node. Otherwise fall back to creation-order sequencing.
			var task models.Task
			if err := tx.First(&task, "id = ?", taskID).Error; err != nil {
				return err
			}
			var jobEdgeCount int64
			if err := tx.Model(&models.TaskEdge{}).
				Where("job_id = ?", task.JobID).
				Limit(1).
				Count(&jobEdgeCount).Error; err != nil {
				return err
			}
			if jobEdgeCount == 0 {
				// No edges defined for the entire job — use creation order.
				var next models.Task
				err := tx.Where("job_id = ? AND created_at > ?", task.JobID, task.CreatedAt).
					Order("created_at asc").
					First(&next).Error
				if err == nil {
					edges = append(edges, models.TaskEdge{ToTaskID: next.ID})
				} else if !errors.Is(err, gorm.ErrRecordNotFound) {
					return err
				}
			}
		}

		for _, edge := range edges {
			if err := tx.Model(&models.TaskRun{}).
				Where("job_run_id = ? AND task_id = ?", runID, edge.ToTaskID).
				UpdateColumn("outstanding_predecessors", gorm.Expr("CASE WHEN outstanding_predecessors > 0 THEN outstanding_predecessors - 1 ELSE 0 END")).Error; err != nil {
				return err
			}

			var successor models.TaskRun
			if err := tx.Where("job_run_id = ? AND task_id = ?", runID, edge.ToTaskID).First(&successor).Error; err == nil &&
				successor.OutstandingPredecessors == 0 && successor.Status == string(TaskStatusPending) && s.eventStore != nil {
				var jobRun models.JobRun
				if err := tx.Select("job_id").First(&jobRun, "id = ?", runID).Error; err != nil {
					return err
				}
				evt := event.Event{
					Type:      event.TypeTaskReady,
					JobID:     jobRun.JobID,
					RunID:     runID,
					TaskID:    edge.ToTaskID,
					Timestamp: time.Now().UTC(),
				}
				if err := s.eventStore.AppendTx(tx, &evt); err != nil {
					return err
				}
				pendingEvents = append(pendingEvents, evt)
			}
		}

		if s.eventStore != nil {
			evt, err := s.recordTaskEventTx(tx, event.TypeTaskSucceeded, runID, taskID)
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
				"status":       string(TaskStatusFailed),
				"completed_at": now,
				"error":        errMsg,
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
				"status":       string(TaskStatusPending),
				"attempt":      attempt,
				"runtime_id":   "",
				"started_at":   nil,
				"completed_at": nil,
				"error":        "",
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
				"status":       string(TaskStatusSkipped),
				"completed_at": now,
				"error":        reason,
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
			"status":     string(TaskStatusPending),
			"runtime_id": "",
			"started_at": nil,
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

func (s *Store) loadRun(runID uuid.UUID) (*JobRun, error) {
	return s.loadRunWithDB(s.db, runID)
}

func (s *Store) loadRunWithDB(conn *gorm.DB, runID uuid.UUID) (*JobRun, error) {
	var result struct {
		models.JobRun
		JobAlias     string
		TriggerType  string
		TriggerAlias string
	}

	// Use a JOIN to fetch job and trigger information for human readability
	err := conn.Table("job_runs").
		Select("job_runs.*, jobs.alias as job_alias, triggers.type as trigger_type, triggers.alias as trigger_alias").
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
	runValue.TriggerType = result.TriggerType
	runValue.TriggerAlias = result.TriggerAlias

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
		ID:        model.ID,
		JobID:     model.JobID,
		Status:    Status(model.Status),
		StartedAt: model.StartedAt,
		CreatedAt: model.CreatedAt,
		UpdatedAt: model.UpdatedAt,
		Error:     model.Error,
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
	}

	if len(model.Output) > 0 {
		var out map[string]string
		if err := json.Unmarshal(model.Output, &out); err == nil {
			task.Output = out
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

	return task
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
	if err := db.Select("job_id").First(&jobRun, "id = ?", runID).Error; err != nil {
		log.Error("failed to fetch job run for event", "error", err, "run_id", runID)
		return nil, err
	}

	taskPayload := convertRunTaskModel(&taskRun)
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

	result := make(map[string]map[string]string, len(edges))
	for _, edge := range edges {
		var task models.Task
		if err := s.db.First(&task, "id = ?", edge.FromTaskID).Error; err != nil {
			continue
		}

		var taskRun models.TaskRun
		if err := s.db.Where("job_run_id = ? AND task_id = ?", runID, edge.FromTaskID).First(&taskRun).Error; err != nil {
			continue
		}

		if len(taskRun.Output) == 0 {
			continue
		}

		var output map[string]string
		if err := json.Unmarshal(taskRun.Output, &output); err != nil {
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
