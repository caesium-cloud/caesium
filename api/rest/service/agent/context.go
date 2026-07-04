package agent

import (
	"errors"
	"time"

	whysvc "github.com/caesium-cloud/caesium/api/rest/service/why"
	iincident "github.com/caesium-cloud/caesium/internal/incident"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ErrForbiddenJob is returned when a context read targets a job outside the
// incident's frozen allowlist. The controller maps it to 403.
var ErrForbiddenJob = errors.New("agent: job is outside the incident's frozen allowlist")

// ErrNoFailingRun is returned when the incident has no addressable failing run.
var ErrNoFailingRun = errors.New("agent: incident has no failing run")

// RunSummary is one run in the read-only context history passthrough.
type RunSummary struct {
	ID          uuid.UUID  `json:"id"`
	Status      string     `json:"status"`
	Error       string     `json:"error,omitempty"`
	StartedAt   time.Time  `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	DurationMS  int64      `json:"duration_ms,omitempty"`
}

// FailingLog returns the scrubbed log tail of the incident's failing task. The
// A5 scrubber's high-entropy heuristic strips credential-shaped tokens; exact
// secret-value removal is not available post-hoc (resolved secrets are never
// persisted).
func (s *Service) FailingLog(inc *models.Incident) (string, bool, error) {
	if inc.RunID == nil || inc.TaskID == nil {
		return "", false, ErrNoFailingRun
	}
	var tr models.TaskRun
	if err := s.db.WithContext(s.ctx).
		Where("job_run_id = ? AND task_id = ?", *inc.RunID, *inc.TaskID).
		First(&tr).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", false, ErrNoFailingRun
		}
		return "", false, err
	}
	return iincident.NewScrubber(nil).Scrub(tr.LogText), true, nil
}

// History returns recent runs for the incident's own job, or — when jobAlias is
// supplied — for that job PROVIDED it is within the incident's frozen allowlist.
// A request for an out-of-allowlist job is refused (ErrForbiddenJob), which is
// the read-scope boundary for agent tokens.
func (s *Service) History(inc *models.Incident, jobAlias string, allowed []string) ([]RunSummary, error) {
	jobID := inc.JobID
	if jobAlias != "" {
		// The incident's own job is always readable; ANY other job must be
		// explicitly listed in the frozen allowlist. An empty allowlist therefore
		// permits ONLY the incident's own job — empty is never "allow any" (the
		// same "empty means unrestricted" footgun closed in authorizeAgentScope).
		ownAlias, err := s.incidentJobAlias(inc.JobID)
		if err != nil {
			return nil, err
		}
		if jobAlias != ownAlias && !jobInAllowlist(jobAlias, allowed) {
			return nil, ErrForbiddenJob
		}
		var job models.Job
		if err := s.db.WithContext(s.ctx).Select("id").First(&job, "alias = ?", jobAlias).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, ErrForbiddenJob
			}
			return nil, err
		}
		jobID = job.ID
	}

	var runs []models.JobRun
	if err := s.db.WithContext(s.ctx).
		Where("job_id = ?", jobID).
		Order("created_at DESC").
		Limit(50).
		Find(&runs).Error; err != nil {
		return nil, err
	}
	out := make([]RunSummary, 0, len(runs))
	for i := range runs {
		r := runs[i]
		rs := RunSummary{ID: r.ID, Status: r.Status, Error: r.Error, StartedAt: r.StartedAt, CompletedAt: r.CompletedAt}
		if r.CompletedAt != nil && !r.StartedAt.IsZero() {
			rs.DurationMS = r.CompletedAt.Sub(r.StartedAt).Milliseconds()
		}
		out = append(out, rs)
	}
	return out, nil
}

// Why returns the causal explanation for a task in the incident's failing run.
func (s *Service) Why(inc *models.Incident, task string) (*runstorage.WhyExplanation, error) {
	if inc.RunID == nil {
		return nil, ErrNoFailingRun
	}
	return whysvc.New(s.ctx).WithDatabase(s.db).Why(*inc.RunID, task)
}

// incidentJobAlias resolves the alias of the incident's own job (always readable
// through the context passthroughs regardless of the frozen allowlist).
func (s *Service) incidentJobAlias(jobID uuid.UUID) (string, error) {
	var job models.Job
	if err := s.db.WithContext(s.ctx).Select("alias").First(&job, "id = ?", jobID).Error; err != nil {
		return "", err
	}
	return job.Alias, nil
}

// jobInAllowlist reports whether jobAlias is EXPLICITLY within the frozen
// allowlist. An empty allowlist matches nothing — it is never treated as
// "unrestricted", so a scoped agent token with an empty frozen list can read no
// cross-job context (only the incident's own job, handled by the caller).
func jobInAllowlist(jobAlias string, allowed []string) bool {
	for _, a := range allowed {
		if a == jobAlias {
			return true
		}
	}
	return false
}
