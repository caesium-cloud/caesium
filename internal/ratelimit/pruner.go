package ratelimit

import (
	"context"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"gorm.io/gorm"
)

// Pruner deletes expired rate-limit windows.
type Pruner struct {
	db  *gorm.DB
	now clockFunc
}

// NewPruner creates a rate-limit token pruner.
func NewPruner(db *gorm.DB, opts ...Option) *Pruner {
	if db == nil {
		panic("rate limit pruner requires database connection")
	}
	p := &Pruner{db: db, now: func() time.Time { return time.Now().UTC() }}
	l := &Limiter{db: db, now: p.now}
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		opt(l)
	}
	p.now = l.now
	return p
}

// Prune removes expired windows and returns the number of deleted rows.
func (p *Pruner) Prune(ctx context.Context) (int, error) {
	result := p.db.WithContext(ctx).
		Where("expires_at <= ?", p.now().UTC()).
		Delete(&models.RateLimitToken{})
	return int(result.RowsAffected), result.Error
}

// Run starts the pruning loop and returns when ctx is cancelled.
func (p *Pruner) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			count, err := p.Prune(ctx)
			if err != nil {
				log.Error("rate limit token pruner failed", "error", err)
				continue
			}
			if count > 0 {
				log.Info("rate limit token pruner removed expired rows", "count", count)
			}
		}
	}
}
