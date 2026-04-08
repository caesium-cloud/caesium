package job

import (
	"context"
	"encoding/json"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/caesium-cloud/caesium/pkg/jsonmap"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"sync"
)

type Job interface {
	WithDatabase(*gorm.DB) Job
	SetBus(event.Bus)
	List(*ListRequest) (models.Jobs, error)
	Get(uuid.UUID) (*models.Job, error)
	Create(*CreateRequest) (*models.Job, error)
	Delete(uuid.UUID) error
	SetPaused(id uuid.UUID, paused bool) (*models.Job, error)
}

type jobService struct {
	ctx        context.Context
	db         *gorm.DB
	bus        event.Bus
	eventStore *event.Store
}

var (
	defaultService   *jobService
	defaultServiceMu sync.Mutex
)

func Service(ctx context.Context) Job {
	defaultServiceMu.Lock()
	defer defaultServiceMu.Unlock()
	if defaultService != nil {
		return &jobService{
			ctx:        ctx,
			db:         defaultService.db,
			bus:        defaultService.bus,
			eventStore: defaultService.eventStore,
		}
	}
	return &jobService{
		ctx:        ctx,
		db:         db.Connection(),
		eventStore: event.NewStore(db.Connection()),
	}
}

func (j *jobService) SetBus(bus event.Bus) {
	j.bus = bus
	defaultServiceMu.Lock()
	defer defaultServiceMu.Unlock()
	if defaultService == nil {
		defaultService = &jobService{db: j.db, eventStore: j.eventStore}
	}
	defaultService.bus = bus
}

func (j *jobService) WithDatabase(conn *gorm.DB) Job {
	j.db = conn
	j.eventStore = event.NewStore(conn)
	return j
}

func (j *jobService) publishEvents(events ...event.Event) {
	if j.bus == nil {
		return
	}
	for _, evt := range events {
		j.bus.Publish(evt)
	}
}

type ListRequest struct {
	Limit     uint64
	Offset    uint64
	OrderBy   []string
	TriggerID string
	Aliases   []string
}

func (j *jobService) List(req *ListRequest) (models.Jobs, error) {
	var (
		jobs = make(models.Jobs, 0)
		q    = j.db.WithContext(j.ctx)
	)

	if req.TriggerID != "" {
		if _, err := uuid.Parse(req.TriggerID); err != nil {
			return nil, err
		}

		q = q.Where("trigger_id = ?", req.TriggerID)
	}
	if len(req.Aliases) > 0 {
		q = q.Where("alias IN ?", req.Aliases)
	}

	for _, orderBy := range req.OrderBy {
		q = q.Order(orderBy)
	}

	if req.Limit > 0 {
		q = q.Limit(int(req.Limit))
	}

	if req.Offset > 0 {
		q = q.Offset(int(req.Offset))
	}

	if err := q.Find(&jobs).Error; err != nil {
		return nil, err
	}

	runStore := runstorage.NewStore(j.db)
	for _, job := range jobs {
		if latest, err := runStore.Latest(job.ID); err == nil && latest != nil {
			job.LatestRun = &models.JobRun{
				ID:            latest.ID,
				JobID:         latest.JobID,
				Status:        string(latest.Status),
				StartedAt:     latest.StartedAt,
				CompletedAt:   latest.CompletedAt,
				Error:         latest.Error,
				CacheHits:     latest.CacheHits,
				ExecutedTasks: latest.ExecutedTasks,
				TotalTasks:    latest.TotalTasks,
			}
		}
	}

	return jobs, nil
}

func (j *jobService) Get(id uuid.UUID) (*models.Job, error) {
	var (
		job = &models.Job{ID: id}
		q   = j.db.WithContext(j.ctx)
	)

	if err := q.First(job).Error; err != nil {
		return nil, err
	}

	runStore := runstorage.NewStore(j.db)
	if latest, err := runStore.Latest(job.ID); err == nil && latest != nil {
		job.LatestRun = &models.JobRun{
			ID:            latest.ID,
			JobID:         latest.JobID,
			Status:        string(latest.Status),
			StartedAt:     latest.StartedAt,
			CompletedAt:   latest.CompletedAt,
			Error:         latest.Error,
			CacheHits:     latest.CacheHits,
			ExecutedTasks: latest.ExecutedTasks,
			TotalTasks:    latest.TotalTasks,
		}
	}

	return job, nil
}

type CreateRequest struct {
	TriggerID   uuid.UUID         `json:"trigger_id"`
	Alias       string            `json:"alias"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

func (j *jobService) Create(req *CreateRequest) (*models.Job, error) {
	var (
		id = uuid.New()
		q  = j.db.WithContext(j.ctx)
	)

	job := &models.Job{
		ID:          id,
		TriggerID:   req.TriggerID,
		Alias:       req.Alias,
		Labels:      jsonmap.FromStringMap(req.Labels),
		Annotations: jsonmap.FromStringMap(req.Annotations),
	}

	pendingEvents := make([]event.Event, 0, 1)
	if err := q.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(job).Error; err != nil {
			return err
		}
		if j.eventStore != nil {
			payload, err := json.Marshal(job)
			if err != nil {
				return err
			}
			evt := event.Event{
				Type:      event.TypeJobCreated,
				JobID:     job.ID,
				Timestamp: time.Now().UTC(),
				Payload:   payload,
			}
			if err := j.eventStore.AppendTx(tx, &evt); err != nil {
				return err
			}
			pendingEvents = append(pendingEvents, evt)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	j.publishEvents(pendingEvents...)
	return job, nil
}

func (j *jobService) Delete(id uuid.UUID) error {
	var (
		q = j.db.WithContext(j.ctx)
	)

	pendingEvents := make([]event.Event, 0, 1)
	err := q.Transaction(func(tx *gorm.DB) error {
		if err := tx.Delete(&models.Job{}, id).Error; err != nil {
			return err
		}
		if j.eventStore != nil {
			evt := event.Event{
				Type:      event.TypeJobDeleted,
				JobID:     id,
				Timestamp: time.Now().UTC(),
			}
			if err := j.eventStore.AppendTx(tx, &evt); err != nil {
				return err
			}
			pendingEvents = append(pendingEvents, evt)
		}
		return nil
	})
	if err == nil {
		j.publishEvents(pendingEvents...)
	}
	return err
}

func (j *jobService) SetPaused(id uuid.UUID, paused bool) (*models.Job, error) {
	q := j.db.WithContext(j.ctx)

	jobModel := &models.Job{ID: id}
	if err := q.First(jobModel).Error; err != nil {
		return nil, err
	}

	pendingEvents := make([]event.Event, 0, 1)
	err := q.Transaction(func(tx *gorm.DB) error {
		jobModel.Paused = paused
		if err := tx.Save(jobModel).Error; err != nil {
			return err
		}
		if j.eventStore != nil {
			eventType := event.TypeJobPaused
			if !paused {
				eventType = event.TypeJobUnpaused
			}
			payload, err := json.Marshal(jobModel)
			if err != nil {
				return err
			}
			evt := event.Event{
				Type:      eventType,
				JobID:     jobModel.ID,
				Timestamp: time.Now().UTC(),
				Payload:   payload,
			}
			if err := j.eventStore.AppendTx(tx, &evt); err != nil {
				return err
			}
			pendingEvents = append(pendingEvents, evt)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	j.publishEvents(pendingEvents...)
	return jobModel, nil
}
