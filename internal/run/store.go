package run

import (
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Status string

type TaskStatus string

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
)

type CallbackStatus string

const (
	CallbackStatusRunning   CallbackStatus = "running"
	CallbackStatusSucceeded CallbackStatus = "succeeded"
	CallbackStatusFailed    CallbackStatus = "failed"
)

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
	AtomID                  uuid.UUID         `json:"atom_id"`
	Engine                  models.AtomEngine `json:"engine"`
	Image                   string            `json:"image"`
	Command                 []string          `json:"command"`
	RuntimeID               string            `json:"runtime_id,omitempty"`
	Status                  TaskStatus        `json:"status"`
	Result                  string            `json:"result,omitempty"`
	StartedAt               *time.Time        `json:"started_at,omitempty"`
	CompletedAt             *time.Time        `json:"completed_at,omitempty"`
	Error                   string            `json:"error,omitempty"`
	OutstandingPredecessors int               `json:"outstanding_predecessors"`
}

type JobRun struct {
	ID          uuid.UUID      `json:"id"`
	JobID       uuid.UUID      `json:"job_id"`
	Status      Status         `json:"status"`
	StartedAt   time.Time      `json:"started_at"`
	CompletedAt *time.Time     `json:"completed_at,omitempty"`
	Error       string         `json:"error,omitempty"`
	Tasks       []*TaskRun     `json:"tasks"`
	Callbacks   []*CallbackRun `json:"callbacks"`
}

type Store struct {
	db *gorm.DB
}

var (
	defaultStore     *Store
	defaultStoreOnce sync.Once
)

func NewStore(conn *gorm.DB) *Store {
	if conn == nil {
		panic("run store requires database connection")
	}
	return &Store{db: conn}
}

func Default() *Store {
	defaultStoreOnce.Do(func() {
		defaultStore = NewStore(db.Connection())
	})
	return defaultStore
}

func (s *Store) Start(jobID uuid.UUID) (*JobRun, error) {
	model := &models.JobRun{
		ID:        uuid.New(),
		JobID:     jobID,
		Status:    string(StatusRunning),
		StartedAt: time.Now().UTC(),
	}

	if err := s.db.Create(model).Error; err != nil {
		return nil, err
	}

	metrics.JobsActive.WithLabelValues(jobID.String()).Inc()

	return s.loadRun(model.ID)
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

	record := &models.TaskRun{
		ID:                      uuid.New(),
		JobRunID:                runID,
		TaskID:                  task.ID,
		AtomID:                  task.AtomID,
		Engine:                  atom.Engine,
		Image:                   atom.Image,
		Command:                 command,
		Status:                  string(TaskStatusPending),
		OutstandingPredecessors: outstanding,
	}

	return s.db.Create(record).Error
}

func (s *Store) StartTask(runID, taskID uuid.UUID, runtimeID string) error {
	now := time.Now().UTC()
	return s.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", runID, taskID).
		Updates(map[string]interface{}{
			"status":     string(TaskStatusRunning),
			"runtime_id": runtimeID,
			"started_at": now,
		}).Error
}

func (s *Store) CompleteTask(runID, taskID uuid.UUID, result string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()

		// Capture task metadata for metrics before updating.
		var taskRun models.TaskRun
		if err := tx.Where("job_run_id = ? AND task_id = ?", runID, taskID).First(&taskRun).Error; err == nil {
			var jobRun models.JobRun
			if err := tx.First(&jobRun, "id = ?", runID).Error; err == nil {
				jobID := jobRun.JobID.String()
				engine := string(taskRun.Engine)
				metrics.TaskRunsTotal.WithLabelValues(jobID, taskID.String(), engine, string(TaskStatusSucceeded)).Inc()
				if taskRun.StartedAt != nil {
					duration := now.Sub(*taskRun.StartedAt).Seconds()
					metrics.TaskRunDurationSeconds.WithLabelValues(jobID, engine, string(TaskStatusSucceeded)).Observe(duration)
				}
			}
		}

		if err := tx.Model(&models.TaskRun{}).
			Where("job_run_id = ? AND task_id = ?", runID, taskID).
			Updates(map[string]interface{}{
				"status":       string(TaskStatusSucceeded),
				"completed_at": now,
				"result":       result,
			}).Error; err != nil {
			return err
		}

		var edges []models.TaskEdge
		if err := tx.Where("from_task_id = ?", taskID).Find(&edges).Error; err != nil {
			return err
		}
		if len(edges) == 0 {
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
			if jobEdgeCount > 0 {
				return nil
			}
			if task.NextID != nil {
				edges = append(edges, models.TaskEdge{ToTaskID: *task.NextID})
			} else {
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
		}

		return nil
	})
}

func (s *Store) FailTask(runID, taskID uuid.UUID, failure error) error {
	now := time.Now().UTC()
	errMsg := ""
	if failure != nil {
		errMsg = failure.Error()
	}

	// Record task failure metrics.
	var taskRun models.TaskRun
	if err := s.db.Where("job_run_id = ? AND task_id = ?", runID, taskID).First(&taskRun).Error; err == nil {
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
	}

	return s.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", runID, taskID).
		Updates(map[string]interface{}{
			"status":       string(TaskStatusFailed),
			"completed_at": now,
			"error":        errMsg,
		}).Error
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
		metrics.JobsActive.WithLabelValues(jobID).Dec()
		duration := now.Sub(run.StartedAt).Seconds()
		metrics.JobRunDurationSeconds.WithLabelValues(jobID, string(status)).Observe(duration)
	}

	return s.db.Model(&models.JobRun{}).
		Where("id = ?", runID).
		Updates(map[string]interface{}{
			"status":       string(status),
			"completed_at": now,
			"error":        errMsg,
		}).Error
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
	var modelsRuns []models.JobRun
	if err := s.db.Where("job_id = ?", jobID).
		Order("started_at ASC").
		Preload("Tasks").
		Find(&modelsRuns).Error; err != nil {
		return nil, err
	}

	runs := make([]*JobRun, 0, len(modelsRuns))
	for idx := range modelsRuns {
		runModel := &modelsRuns[idx]
		runValue, err := s.convertRunModel(runModel)
		if err != nil {
			return nil, err
		}
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
	var model models.JobRun
	if err := s.db.Preload("Tasks").
		First(&model, "id = ?", runID).Error; err != nil {
		return nil, err
	}
	return s.convertRunModel(&model)
}

func (s *Store) convertRunModel(model *models.JobRun) (*JobRun, error) {
	if model == nil {
		return nil, nil
	}

	runValue := &JobRun{
		ID:        model.ID,
		JobID:     model.JobID,
		Status:    Status(model.Status),
		StartedAt: model.StartedAt,
		Error:     model.Error,
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

	callbackRuns, err := s.loadCallbackRuns(model.ID)
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
		AtomID:                  model.AtomID,
		Engine:                  model.Engine,
		Image:                   model.Image,
		Command:                 command,
		RuntimeID:               model.RuntimeID,
		Status:                  TaskStatus(model.Status),
		Result:                  model.Result,
		Error:                   model.Error,
		OutstandingPredecessors: model.OutstandingPredecessors,
	}

	if model.StartedAt != nil {
		started := *model.StartedAt
		task.StartedAt = &started
	}
	if model.CompletedAt != nil {
		completed := *model.CompletedAt
		task.CompletedAt = &completed
	}

	return task
}

func (s *Store) loadCallbackRuns(runID uuid.UUID) ([]*CallbackRun, error) {
	var modelRuns []models.CallbackRun
	if err := s.db.
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
