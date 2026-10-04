package lineage

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCacheGetSet(t *testing.T) {
	cache := newJobCache(5 * time.Minute)
	id := uuid.New()

	_, ok := cache.Get(id)
	if ok {
		t.Error("expected cache miss")
	}

	cache.Set(id, jobCacheEntry{alias: "my-job", provenanceRepo: "https://github.com/test/repo"})
	entry, ok := cache.Get(id)
	if !ok {
		t.Fatal("expected cache hit")
	}
	if entry.alias != "my-job" {
		t.Errorf("alias = %v, want my-job", entry.alias)
	}
	if entry.provenanceRepo != "https://github.com/test/repo" {
		t.Errorf("provenanceRepo = %v", entry.provenanceRepo)
	}
}

func TestCacheExpiry(t *testing.T) {
	cache := newJobCache(200 * time.Millisecond)
	id := uuid.New()

	cache.entries[id] = jobCacheEntry{alias: "my-job", fetchedAt: time.Now().Add(-time.Hour)}

	_, ok := cache.Get(id)
	if ok {
		t.Error("expected cache miss after TTL expiry")
	}
	if _, exists := cache.entries[id]; exists {
		t.Fatal("expired entry retained")
	}
}

func TestCacheOverwrite(t *testing.T) {
	cache := newJobCache(5 * time.Minute)
	id := uuid.New()

	cache.Set(id, jobCacheEntry{alias: "old-name"})
	cache.Set(id, jobCacheEntry{alias: "new-name"})

	entry, ok := cache.Get(id)
	if !ok {
		t.Fatal("expected cache hit")
	}
	if entry.alias != "new-name" {
		t.Errorf("alias = %v, want new-name", entry.alias)
	}
}

func TestCacheMultipleEntries(t *testing.T) {
	cache := newJobCache(5 * time.Minute)
	id1 := uuid.New()
	id2 := uuid.New()

	cache.Set(id1, jobCacheEntry{alias: "job-1"})
	cache.Set(id2, jobCacheEntry{alias: "job-2"})

	e1, ok := cache.Get(id1)
	if !ok || e1.alias != "job-1" {
		t.Errorf("id1: alias = %v, want job-1", e1.alias)
	}

	e2, ok := cache.Get(id2)
	if !ok || e2.alias != "job-2" {
		t.Errorf("id2: alias = %v, want job-2", e2.alias)
	}
}

func TestCacheSetPrunesExpired(t *testing.T) {
	c := newJobCache(time.Hour)
	expired, current := uuid.New(), uuid.New()
	c.entries[expired] = jobCacheEntry{fetchedAt: time.Now().Add(-2 * time.Hour)}
	c.entries[current] = jobCacheEntry{alias: "current", fetchedAt: time.Now()}
	c.Set(uuid.New(), jobCacheEntry{alias: "new"})
	if _, exists := c.entries[expired]; exists {
		t.Fatal("expired entry retained")
	}
	if entry, ok := c.Get(current); !ok || entry.alias != "current" {
		t.Fatal("current entry pruned")
	}
}
func TestCacheConcurrentRefresh(t *testing.T) {
	c := newJobCache(time.Hour)
	id := uuid.New()
	c.entries[id] = jobCacheEntry{fetchedAt: time.Now().Add(-2 * time.Hour)}
	done := make(chan struct{})
	go func() {
		for range 100 {
			c.Get(id)
		}
		close(done)
	}()
	c.Set(id, jobCacheEntry{alias: "refreshed"})
	<-done
	if entry, ok := c.Get(id); !ok || entry.alias != "refreshed" {
		t.Fatal("refresh lost")
	}
}
