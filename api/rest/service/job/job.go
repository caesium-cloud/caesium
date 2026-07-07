package job

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	triggersvc "github.com/caesium-cloud/caesium/api/rest/service/trigger"
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/caesium-cloud/caesium/pkg/jsonmap"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Job interface {
	WithDatabase(*gorm.DB) Job
	SetBus(event.Bus)
	List(*ListRequest) (models.Jobs, error)
	ListRecentRuns([]uuid.UUID, int) (map[uuid.UUID][]RunListSummary, error)
	Get(uuid.UUID) (*models.Job, error)
	GetByIDPrefix(string) (*models.Job, error)
	Queue(uuid.UUID) ([]QueueItem, error)
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

	// ErrAmbiguousJobIDPrefix means a prefix matched more than one job ID.
	ErrAmbiguousJobIDPrefix = errors.New("ambiguous job id prefix")
	// ErrInvalidJobIDPrefix means a job ID prefix cannot match a canonical UUID.
	ErrInvalidJobIDPrefix = errors.New("invalid job id prefix")
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
	return ServiceWithDatabase(ctx, db.Connection())
}

func ServiceWithDatabase(ctx context.Context, conn *gorm.DB) Job {
	return &jobService{
		ctx:        ctx,
		db:         conn,
		eventStore: event.NewStore(conn),
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
	event.PublishAndMarkBusDispatched(j.ctx, j.bus, j.eventStore, events...)
}

type ListRequest struct {
	Limit     uint64
	Offset    uint64
	OrderBy   []string
	TriggerID string
	Aliases   []string
}

type RunListSummary struct {
	ID            uuid.UUID  `json:"-"`
	JobID         uuid.UUID  `json:"-"`
	Status        string     `json:"status"`
	Duration      *float64   `json:"duration,omitempty"`
	StartedAt     time.Time  `json:"-"`
	CompletedAt   *time.Time `json:"-"`
	Error         string     `json:"-"`
	CacheHits     int        `json:"-"`
	ExecutedTasks int        `json:"-"`
	TotalTasks    int        `json:"-"`
}

func (r RunListSummary) JobRun() *runstorage.JobRun {
	return &runstorage.JobRun{
		ID:            r.ID,
		JobID:         r.JobID,
		Status:        runstorage.Status(r.Status),
		StartedAt:     r.StartedAt,
		CompletedAt:   r.CompletedAt,
		Error:         r.Error,
		CacheHits:     r.CacheHits,
		ExecutedTasks: r.ExecutedTasks,
		TotalTasks:    r.TotalTasks,
	}
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

	return jobs, nil
}

func (j *jobService) ListRecentRuns(jobIDs []uuid.UUID, limit int) (map[uuid.UUID][]RunListSummary, error) {
	out := make(map[uuid.UUID][]RunListSummary, len(jobIDs))
	if len(jobIDs) == 0 {
		return out, nil
	}
	if limit <= 0 {
		limit = 10
	}

	type recentRunRow struct {
		ID            uuid.UUID  `gorm:"column:id"`
		JobID         uuid.UUID  `gorm:"column:job_id"`
		Status        string     `gorm:"column:status"`
		StartedAt     time.Time  `gorm:"column:started_at"`
		CompletedAt   *time.Time `gorm:"column:completed_at"`
		Error         string     `gorm:"column:error"`
		CacheHits     int        `gorm:"column:cache_hits"`
		ExecutedTasks int        `gorm:"column:executed_tasks"`
		TotalTasks    int        `gorm:"column:total_tasks"`
	}

	ranked := j.db.WithContext(j.ctx).
		Table("job_runs").
		Select(`
			job_runs.id,
			job_runs.job_id,
			job_runs.status,
			job_runs.started_at,
			job_runs.completed_at,
			job_runs.error,
			ROW_NUMBER() OVER (
				PARTITION BY job_runs.job_id
				ORDER BY job_runs.started_at DESC, job_runs.created_at DESC, job_runs.id DESC
			) AS rn
		`).
		Where("job_runs.job_id IN ? AND job_runs.quarantine IS NOT TRUE", jobIDs)

	var rows []recentRunRow
	err := j.db.WithContext(j.ctx).
		Table("(?) AS ranked", ranked).
		Select(`
			ranked.id,
			ranked.job_id,
			ranked.status,
			ranked.started_at,
			ranked.completed_at,
			ranked.error,
			COALESCE(SUM(CASE WHEN task_runs.cache_hit OR task_runs.status = 'cached' THEN 1 ELSE 0 END), 0) AS cache_hits,
			COALESCE(SUM(CASE WHEN NOT (task_runs.cache_hit OR task_runs.status = 'cached') AND task_runs.status IN ('running', 'succeeded', 'failed') THEN 1 ELSE 0 END), 0) AS executed_tasks,
			COUNT(task_runs.id) AS total_tasks
		`).
		Joins("LEFT JOIN task_runs ON task_runs.job_run_id = ranked.id").
		Where("ranked.rn <= ?", limit).
		Group("ranked.id, ranked.job_id, ranked.status, ranked.started_at, ranked.completed_at, ranked.error").
		Order("ranked.job_id ASC, ranked.started_at ASC, ranked.id ASC").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	for _, row := range rows {
		var duration *float64
		if row.CompletedAt != nil {
			seconds := row.CompletedAt.Sub(row.StartedAt).Seconds()
			if seconds < 0 {
				seconds = 0
			}
			duration = &seconds
		}
		out[row.JobID] = append(out[row.JobID], RunListSummary{
			ID:            row.ID,
			JobID:         row.JobID,
			Status:        row.Status,
			Duration:      duration,
			StartedAt:     row.StartedAt,
			CompletedAt:   row.CompletedAt,
			Error:         row.Error,
			CacheHits:     row.CacheHits,
			ExecutedTasks: row.ExecutedTasks,
			TotalTasks:    row.TotalTasks,
		})
	}

	return out, nil
}

func (j *jobService) Get(id uuid.UUID) (*models.Job, error) {
	var (
		job = &models.Job{ID: id}
		q   = j.db.WithContext(j.ctx)
	)

	if err := q.First(job).Error; err != nil {
		return nil, err
	}

	j.attachLatestRun(job)

	return job, nil
}

func (j *jobService) GetByIDPrefix(rawID string) (*models.Job, error) {
	rawID = strings.TrimSpace(rawID)
	if rawID == "" {
		return nil, ErrInvalidJobIDPrefix
	}

	if id, err := uuid.Parse(rawID); err == nil {
		return j.Get(id)
	}

	prefix := strings.ToLower(rawID)
	if !isCanonicalUUIDPrefix(prefix) {
		return nil, ErrInvalidJobIDPrefix
	}

	var matches []models.Job
	if err := j.db.WithContext(j.ctx).
		Where("id LIKE ?", prefix+"%").
		Limit(2).
		Find(&matches).Error; err != nil {
		return nil, err
	}

	switch len(matches) {
	case 0:
		return nil, gorm.ErrRecordNotFound
	case 1:
		job := &matches[0]
		j.attachLatestRun(job)
		return job, nil
	default:
		return nil, ErrAmbiguousJobIDPrefix
	}
}

func isCanonicalUUIDPrefix(prefix string) bool {
	if len(prefix) == 0 || len(prefix) > 36 {
		return false
	}
	for i, r := range prefix {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !isHexRune(r) {
				return false
			}
		}
	}
	return true
}

func isHexRune(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
}

func (j *jobService) attachLatestRun(job *models.Job) {
	runStore := runstorage.NewStore(j.db.WithContext(j.ctx))
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

type QueueItem struct {
	ID         uuid.UUID         `json:"id"`
	Position   int               `json:"position"`
	Priority   int               `json:"priority"`
	Params     map[string]string `json:"params,omitempty"`
	EnqueuedAt time.Time         `json:"enqueued_at"`
}

func (j *jobService) Queue(id uuid.UUID) ([]QueueItem, error) {
	var rows []models.RunQueue
	if err := j.db.WithContext(j.ctx).
		Where("job_id = ? AND claimed_by = ''", id).
		Order("priority DESC").
		Order("created_at ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}

	items := make([]QueueItem, 0, len(rows))
	for idx := range rows {
		params, err := decodeQueueParams(rows[idx].Params)
		if err != nil {
			return nil, err
		}
		items = append(items, QueueItem{
			ID:         rows[idx].ID,
			Position:   idx + 1,
			Priority:   rows[idx].Priority,
			Params:     params,
			EnqueuedAt: rows[idx].CreatedAt,
		})
	}
	return items, nil
}

func decodeQueueParams(raw []byte) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var params map[string]string
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, err
	}
	return params, nil
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
	deleted := false
	err := q.Transaction(func(tx *gorm.DB) error {
		var jobModel models.Job
		if err := tx.First(&jobModel, "id = ?", id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		if err := tx.Delete(&jobModel).Error; err != nil {
			return err
		}
		deleted = true
		if jobModel.TriggerID != uuid.Nil {
			var remaining int64
			if err := tx.Model(&models.Job{}).
				Where("trigger_id = ?", jobModel.TriggerID).
				Count(&remaining).Error; err != nil {
				return err
			}
			if remaining == 0 {
				if err := tx.Delete(&models.Trigger{}, jobModel.TriggerID).Error; err != nil {
					return err
				}
			}
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
	if err == nil && deleted {
		j.publishEvents(pendingEvents...)
		if notifyErr := triggersvc.NotifyMutation(j.ctx); notifyErr != nil {
			log.Warn("event trigger router reload failed after job delete", "job_id", id, "error", notifyErr)
		}
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
