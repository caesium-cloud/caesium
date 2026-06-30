package ratelimit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/metrics"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

const minWindow = time.Minute

type clockFunc func() time.Time

// Limiter atomically consumes resource units from durable fixed windows.
type Limiter struct {
	db  *gorm.DB
	now clockFunc
}

// Option configures a Limiter.
type Option func(*Limiter)

// WithClock overrides the limiter clock for tests.
func WithClock(now clockFunc) Option {
	return func(l *Limiter) {
		if now != nil {
			l.now = now
		}
	}
}

// NewLimiter creates a database-backed limiter.
func NewLimiter(db *gorm.DB, opts ...Option) *Limiter {
	if db == nil {
		panic("rate limit limiter requires database connection")
	}
	l := &Limiter{db: db, now: func() time.Time { return time.Now().UTC() }}
	for _, opt := range opts {
		if opt != nil {
			opt(l)
		}
	}
	return l
}

// Acquire consumes units for resource if the current fixed window has capacity.
// It is intentionally one guarded raw upsert; GORM's clause.OnConflict cannot
// express the WHERE guard needed for atomic cross-node rejection.
func (l *Limiter) Acquire(ctx context.Context, resource string, units, limit int, window time.Duration) (bool, error) {
	resource = strings.TrimSpace(resource)
	if resource == "" {
		return false, errors.New("rate limit resource is required")
	}
	if units <= 0 {
		return false, fmt.Errorf("rate limit units must be > 0 for resource %q", resource)
	}
	if limit <= 0 {
		return false, fmt.Errorf("rate limit limit must be > 0 for resource %q", resource)
	}
	if units > limit {
		return false, nil
	}
	normalizedWindow := NormalizeWindow(window)

	now := l.now().UTC()
	windowStart := now.Truncate(normalizedWindow)
	windowKey := windowStart.Format(time.RFC3339Nano)
	expiresAt := windowStart.Add(normalizedWindow)

	var acquired bool
	err := l.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// A resource's limit should be consistent across jobs. Within a window,
		// every guard uses the caller's limit, and successful conflict updates
		// persist that most-recent caller's limit.
		result := tx.Exec(`
	INSERT INTO rate_limit_tokens (resource, window_key, consumed, limit_val, expires_at)
	VALUES (?, ?, ?, ?, ?)
	ON CONFLICT(resource, window_key) DO UPDATE SET
		consumed = consumed + ?,
		limit_val = ?
	WHERE consumed + ? <= ?`,
			resource, windowKey, units, limit, expiresAt, units, limit, units, limit,
		)
		if result.Error != nil {
			return result.Error
		}
		acquired = result.RowsAffected == 1
		return nil
	})
	if err != nil {
		return false, err
	}

	if acquired {
		metrics.RateLimitAcquiredTotal.WithLabelValues(resource).Inc()
	} else {
		metrics.RateLimitRejectedTotal.WithLabelValues(resource).Inc()
	}
	return acquired, nil
}

// NormalizeWindow enforces the minimum one-minute bucket granularity.
func NormalizeWindow(window time.Duration) time.Duration {
	if window < minWindow {
		return minWindow
	}
	return window
}

// RetryAfter returns the remaining duration before the current window rolls.
func RetryAfter(now time.Time, window time.Duration) time.Duration {
	window = NormalizeWindow(window)
	now = now.UTC()
	next := now.Truncate(window).Add(window)
	if !next.After(now) {
		return 0
	}
	return next.Sub(now)
}

// Rule is the resolved rate-limit contract for one task.
type Rule struct {
	Resource string
	Units    int
	Limit    int
	Window   time.Duration
	JobAlias string
}

// RuleForTask resolves the persisted task-level rate-limit fields and owning
// job-level declaration for a task run.
func RuleForTask(ctx context.Context, db *gorm.DB, runID, taskID uuid.UUID) (*Rule, bool, error) {
	if db == nil {
		return nil, false, errors.New("rate limit rule lookup requires database connection")
	}

	var row struct {
		Resource   string         `gorm:"column:resource"`
		Units      int            `gorm:"column:units"`
		RateLimits datatypes.JSON `gorm:"column:rate_limits"`
		JobAlias   string         `gorm:"column:job_alias"`
	}
	err := db.WithContext(ctx).
		Table("task_runs AS tr").
		Select("t.rate_limit_resource AS resource, t.rate_limit_units AS units, j.rate_limits AS rate_limits, j.alias AS job_alias").
		Joins("JOIN tasks AS t ON t.id = tr.task_id").
		Joins("JOIN job_runs AS jr ON jr.id = tr.job_run_id").
		Joins("JOIN jobs AS j ON j.id = jr.job_id").
		Where("tr.job_run_id = ? AND tr.task_id = ?", runID, taskID).
		Take(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if strings.TrimSpace(row.Resource) == "" {
		return nil, false, nil
	}

	return RuleFromDeclarations(row.JobAlias, row.RateLimits, row.Resource, row.Units)
}

// RuleFromDeclarations resolves a task resource/units pair against serialized
// job-level metadata.rateLimits declarations.
func RuleFromDeclarations(jobAlias string, raw datatypes.JSON, resource string, units int) (*Rule, bool, error) {
	resource = strings.TrimSpace(resource)
	if resource == "" || units <= 0 {
		return nil, false, nil
	}
	if len(raw) == 0 {
		return nil, false, fmt.Errorf("rate limit resource %q has no job-level declarations", resource)
	}

	var declarations []jobdefschema.RateLimit
	if err := json.Unmarshal(raw, &declarations); err != nil {
		return nil, false, fmt.Errorf("decode job rate limits: %w", err)
	}
	return RuleFromRateLimits(jobAlias, declarations, resource, units)
}

// RuleFromRateLimits resolves a task resource/units pair against parsed
// job-level metadata.rateLimits declarations.
func RuleFromRateLimits(jobAlias string, declarations []jobdefschema.RateLimit, resource string, units int) (*Rule, bool, error) {
	resource = strings.TrimSpace(resource)
	if resource == "" || units <= 0 {
		return nil, false, nil
	}
	for _, declaration := range declarations {
		if strings.TrimSpace(declaration.Resource) != resource {
			continue
		}
		window, err := time.ParseDuration(declaration.Window)
		if err != nil {
			return nil, false, fmt.Errorf("parse rate limit window for %q: %w", resource, err)
		}
		return &Rule{
			Resource: resource,
			Units:    units,
			Limit:    declaration.Limit,
			Window:   window,
			JobAlias: jobAlias,
		}, true, nil
	}
	return nil, false, fmt.Errorf("rate limit resource %q is not declared on job", resource)
}
