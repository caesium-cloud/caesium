package cache

import (
	"context"
	"time"

	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
)

// Config holds cache configuration from environment.
type Config struct {
	Enabled       bool
	TTL           time.Duration
	PruneInterval time.Duration
	MaxEntries    int
}

// ConfigFromEnv reads cache configuration from environment variables.
func ConfigFromEnv() Config {
	e := env.Variables()
	return Config{
		Enabled:       e.CacheEnabled,
		TTL:           e.CacheTTL,
		PruneInterval: e.CachePruneInterval,
		MaxEntries:    e.CacheMaxEntries,
	}
}

// StartPruner starts a background goroutine that prunes expired cache entries.
func StartPruner(ctx context.Context, store *Store, interval time.Duration) {
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				count, err := store.Prune()
				if err != nil {
					log.Error("cache pruner failed", "error", err)
					continue
				}
				if count > 0 {
					log.Info("cache pruner removed expired entries", "count", count)
				}
			}
		}
	}()
}
