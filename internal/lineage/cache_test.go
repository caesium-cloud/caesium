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
	cache := newJobCache(1 * time.Millisecond)
	id := uuid.New()

	cache.Set(id, jobCacheEntry{alias: "my-job"})

	_, ok := cache.Get(id)
	if !ok {
		t.Fatal("expected cache hit immediately after set")
	}

	time.Sleep(5 * time.Millisecond)

	_, ok = cache.Get(id)
	if ok {
		t.Error("expected cache miss after TTL expiry")
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
