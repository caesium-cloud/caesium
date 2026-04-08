package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// DecodeScope normalizes the persisted scope payload into a structured model.
// Nil, empty, or empty-job scopes are treated as unrestricted.
func DecodeScope(scopeJSON []byte) (*models.KeyScope, error) {
	if len(scopeJSON) == 0 {
		return nil, nil
	}

	var scope models.KeyScope
	if err := json.Unmarshal(scopeJSON, &scope); err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(scope.Jobs))
	jobs := make([]string, 0, len(scope.Jobs))
	for _, alias := range scope.Jobs {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			continue
		}
		if _, ok := seen[alias]; ok {
			continue
		}
		seen[alias] = struct{}{}
		jobs = append(jobs, alias)
	}

	if len(jobs) == 0 {
		return nil, nil
	}

	sort.Strings(jobs)
	scope.Jobs = jobs
	return &scope, nil
}

// ScopeJobs returns the normalized scoped job aliases or nil when unrestricted.
func ScopeJobs(scopeJSON []byte) ([]string, error) {
	scope, err := DecodeScope(scopeJSON)
	if err != nil {
		return nil, err
	}
	if scope == nil {
		return nil, nil
	}
	return append([]string(nil), scope.Jobs...), nil
}

// IsScoped reports whether the scope payload restricts access to specific jobs.
func IsScoped(scopeJSON []byte) (bool, error) {
	jobs, err := ScopeJobs(scopeJSON)
	if err != nil {
		return false, err
	}
	return len(jobs) > 0, nil
}

func containsScopedJob(allowed []string, jobAlias string) bool {
	if len(allowed) == 0 {
		return true
	}

	jobAlias = strings.TrimSpace(jobAlias)
	for _, alias := range allowed {
		if alias == jobAlias {
			return true
		}
	}
	return false
}

// JobAliasByID resolves the job alias for a job identifier.
func (s *Service) JobAliasByID(ctx context.Context, id uuid.UUID) (string, error) {
	var job models.Job
	if err := s.db.WithContext(ctx).Select("alias").First(&job, "id = ?", id).Error; err != nil {
		return "", err
	}
	return job.Alias, nil
}

// JobAliasByRunID resolves the job alias for a job run identifier.
func (s *Service) JobAliasByRunID(ctx context.Context, id uuid.UUID) (string, error) {
	var run models.JobRun
	if err := s.db.WithContext(ctx).Select("job_id").First(&run, "id = ?", id).Error; err != nil {
		return "", err
	}
	return s.JobAliasByID(ctx, run.JobID)
}

// JobAliasByBackfillID resolves the job alias for a backfill identifier.
func (s *Service) JobAliasByBackfillID(ctx context.Context, id uuid.UUID) (string, error) {
	var backfill models.Backfill
	if err := s.db.WithContext(ctx).Select("job_id").First(&backfill, "id = ?", id).Error; err != nil {
		return "", err
	}
	return s.JobAliasByID(ctx, backfill.JobID)
}

func formatLookupError(kind string, err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	return fmt.Errorf("lookup %s scope target: %w", kind, err)
}
