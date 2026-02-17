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
	"github.com/caesium-cloud/caesium/pkg/log"
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
}

type jobService struct {
	ctx context.Context
	db  *gorm.DB
	bus event.Bus
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
			ctx: ctx,
			db:  defaultService.db,
			bus: defaultService.bus,
		}
	}
	return &jobService{
		ctx: ctx,
		db:  db.Connection(),
	}
}

func (j *jobService) SetBus(bus event.Bus) {
	j.bus = bus
	defaultServiceMu.Lock()
	defer defaultServiceMu.Unlock()
	if defaultService == nil {
		defaultService = &jobService{db: j.db}
	}
	defaultService.bus = bus
}

func (j *jobService) WithDatabase(conn *gorm.DB) Job {
	j.db = conn
	return j
}

type ListRequest struct {
	Limit     uint64
	Offset    uint64
	OrderBy   []string
	TriggerID string
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

	for _, orderBy := range req.OrderBy {
		q = q.Order(orderBy)
	}

	if req.Limit > 0 {
		q = q.Limit(int(req.Limit))
	}

	if err := q.Find(&jobs).Error; err != nil {
		return nil, err
	}

	runStore := runstorage.NewStore(j.db)
	for _, job := range jobs {
		if latest, err := runStore.Latest(job.ID); err == nil && latest != nil {
			job.LatestRun = &models.JobRun{
				ID:          latest.ID,
				JobID:       latest.JobID,
				Status:      string(latest.Status),
				StartedAt:   latest.StartedAt,
				CompletedAt: latest.CompletedAt,
				Error:       latest.Error,
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

	return job, q.First(job).Error
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

	if err := q.Create(job).Error; err != nil {
		return nil, err
	}

	if j.bus != nil {
		if payload, err := json.Marshal(job); err != nil {
			log.Error("failed to marshal job created event", "error", err, "job_id", job.ID)
		} else {
			j.bus.Publish(event.Event{
				Type:      event.TypeJobCreated,
				JobID:     job.ID,
				Timestamp: time.Now().UTC(),
				Payload:   payload,
			})
		}
	}

	return job, nil
}

func (j *jobService) Delete(id uuid.UUID) error {
	var (
		q = j.db.WithContext(j.ctx)
	)

	if err := q.Delete(&models.Job{}, id).Error; err != nil {
		return err
	}

	if j.bus != nil {
		j.bus.Publish(event.Event{
			Type:      event.TypeJobDeleted,
			JobID:     id,
			Timestamp: time.Now().UTC(),
		})
	}

	return nil
}
