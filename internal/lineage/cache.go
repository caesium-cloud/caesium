package lineage

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

type jobCacheEntry struct {
	alias            string
	provenanceRepo   string
	provenanceRef    string
	provenanceCommit string
	provenancePath   string
	fetchedAt        time.Time
}

type jobCache struct {
	mu      sync.RWMutex
	entries map[uuid.UUID]jobCacheEntry
	ttl     time.Duration
}

func newJobCache(ttl time.Duration) *jobCache {
	return &jobCache{
		entries: make(map[uuid.UUID]jobCacheEntry),
		ttl:     ttl,
	}
}

func (c *jobCache) Get(id uuid.UUID) (jobCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[id]
	if !ok {
		return jobCacheEntry{}, false
	}
	if time.Since(entry.fetchedAt) > c.ttl {
		delete(c.entries, id)
		return jobCacheEntry{}, false
	}
	return entry, true
}

func (c *jobCache) Set(id uuid.UUID, entry jobCacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for key, old := range c.entries {
		if now.Sub(old.fetchedAt) > c.ttl {
			delete(c.entries, key)
		}
	}
	entry.fetchedAt = now
	c.entries[id] = entry
}
