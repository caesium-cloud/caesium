package run

import (
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
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

type Task struct {
	ID          uuid.UUID         `json:"id"`
	AtomID      uuid.UUID         `json:"atom_id"`
	Engine      models.AtomEngine `json:"engine"`
	Image       string            `json:"image"`
	Command     []string          `json:"command"`
	RuntimeID   string            `json:"runtime_id,omitempty"`
	Status      TaskStatus        `json:"status"`
	Result      string            `json:"result,omitempty"`
	StartedAt   *time.Time        `json:"started_at,omitempty"`
	CompletedAt *time.Time        `json:"completed_at,omitempty"`
	Error       string            `json:"error,omitempty"`
}

type Run struct {
	ID          uuid.UUID  `json:"id"`
	JobID       uuid.UUID  `json:"job_id"`
	Status      Status     `json:"status"`
	StartedAt   time.Time  `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	Error       string     `json:"error,omitempty"`
	Tasks       []*Task    `json:"tasks"`
}

type Store struct {
	mu       sync.RWMutex
	runs     map[uuid.UUID]*Run
	jobIndex map[uuid.UUID][]uuid.UUID
}

var defaultStore = NewStore()

func NewStore() *Store {
	return &Store{
		runs:     make(map[uuid.UUID]*Run),
		jobIndex: make(map[uuid.UUID][]uuid.UUID),
	}
}

func Default() *Store {
	return defaultStore
}

func (s *Store) Start(jobID uuid.UUID) *Run {
	s.mu.Lock()
	defer s.mu.Unlock()

	run := &Run{
		ID:        uuid.New(),
		JobID:     jobID,
		Status:    StatusRunning,
		StartedAt: time.Now().UTC(),
		Tasks:     make([]*Task, 0),
	}

	s.runs[run.ID] = run
	s.jobIndex[jobID] = append(s.jobIndex[jobID], run.ID)

	return copyRun(run)
}

func (s *Store) RegisterTask(runID uuid.UUID, task *models.Task, atom *models.Atom) {
	s.mu.Lock()
	defer s.mu.Unlock()

	run, ok := s.runs[runID]
	if !ok {
		return
	}

	run.Tasks = append(run.Tasks, &Task{
		ID:      task.ID,
		AtomID:  task.AtomID,
		Engine:  atom.Engine,
		Image:   atom.Image,
		Command: atom.Cmd(),
		Status:  TaskStatusPending,
	})
}

func (s *Store) StartTask(runID, taskID uuid.UUID, runtimeID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	run, ok := s.runs[runID]
	if !ok {
		return
	}

	if task := run.task(taskID); task != nil {
		now := time.Now().UTC()
		task.RuntimeID = runtimeID
		task.Status = TaskStatusRunning
		task.StartedAt = &now
	}
}

func (s *Store) CompleteTask(runID, taskID uuid.UUID, result string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	run, ok := s.runs[runID]
	if !ok {
		return
	}

	if task := run.task(taskID); task != nil {
		now := time.Now().UTC()
		task.CompletedAt = &now
		task.Status = TaskStatusSucceeded
		task.Result = result
	}
}

func (s *Store) FailTask(runID, taskID uuid.UUID, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	run, ok := s.runs[runID]
	if !ok {
		return
	}

	if task := run.task(taskID); task != nil {
		now := time.Now().UTC()
		task.CompletedAt = &now
		task.Status = TaskStatusFailed
		if err != nil {
			task.Error = err.Error()
		}
	}
}

func (s *Store) Complete(runID uuid.UUID, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	run, ok := s.runs[runID]
	if !ok {
		return
	}

	now := time.Now().UTC()
	run.CompletedAt = &now

	if err != nil {
		run.Status = StatusFailed
		run.Error = err.Error()
	} else {
		run.Status = StatusSucceeded
		run.Error = ""
	}
}

func (s *Store) Get(runID uuid.UUID) (*Run, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	run, ok := s.runs[runID]
	if !ok {
		return nil, false
	}

	return copyRun(run), true
}

func (s *Store) List(jobID uuid.UUID) []*Run {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ids := s.jobIndex[jobID]
	runs := make([]*Run, 0, len(ids))

	for _, id := range ids {
		if run, ok := s.runs[id]; ok {
			runs = append(runs, copyRun(run))
		}
	}

	return runs
}

func (s *Store) Latest(jobID uuid.UUID) *Run {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ids := s.jobIndex[jobID]
	if len(ids) == 0 {
		return nil
	}

	run, ok := s.runs[ids[len(ids)-1]]
	if !ok {
		return nil
	}

	return copyRun(run)
}

func (r *Run) task(id uuid.UUID) *Task {
	for _, task := range r.Tasks {
		if task.ID == id {
			return task
		}
	}

	return nil
}

func copyRun(src *Run) *Run {
	if src == nil {
		return nil
	}

	dst := &Run{
		ID:        src.ID,
		JobID:     src.JobID,
		Status:    src.Status,
		StartedAt: src.StartedAt,
		Error:     src.Error,
	}

	if src.CompletedAt != nil {
		completed := *src.CompletedAt
		dst.CompletedAt = &completed
	}

	if len(src.Tasks) > 0 {
		dst.Tasks = make([]*Task, len(src.Tasks))
		for i, task := range src.Tasks {
			if task == nil {
				continue
			}

			copied := *task
			if task.StartedAt != nil {
				started := *task.StartedAt
				copied.StartedAt = &started
			}
			if task.CompletedAt != nil {
				finished := *task.CompletedAt
				copied.CompletedAt = &finished
			}
			dst.Tasks[i] = &copied
		}
	} else {
		dst.Tasks = make([]*Task, 0)
	}

	return dst
}
