package auth

import (
	"sync"
	"time"
)

// RateLimiter tracks failed authentication attempts per source IP
// using a sliding window counter.
type RateLimiter struct {
	mu       sync.Mutex
	windows  map[string]*window
	limit    int
	interval time.Duration
}

type window struct {
	count   int
	resetAt time.Time
}

// NewRateLimiter creates a rate limiter that allows `limit` failures per
// `interval` per source IP.
func NewRateLimiter(limit int, interval time.Duration) *RateLimiter {
	return &RateLimiter{
		windows:  make(map[string]*window),
		limit:    limit,
		interval: interval,
	}
}

// RecordFailure increments the failure count for the given IP.
// Returns true if the IP is now rate-limited.
func (r *RateLimiter) RecordFailure(ip string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	w, ok := r.windows[ip]
	if !ok || now.After(w.resetAt) {
		r.windows[ip] = &window{count: 1, resetAt: now.Add(r.interval)}
		return false
	}

	w.count++
	return w.count > r.limit
}

// IsLimited returns true if the IP has exceeded the failure threshold.
func (r *RateLimiter) IsLimited(ip string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	w, ok := r.windows[ip]
	if !ok {
		return false
	}
	if time.Now().After(w.resetAt) {
		delete(r.windows, ip)
		return false
	}
	return w.count > r.limit
}

// RetryAfter returns the number of seconds until the rate limit window resets
// for the given IP. Returns 0 if not limited.
func (r *RateLimiter) RetryAfter(ip string) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	w, ok := r.windows[ip]
	if !ok {
		return 0
	}
	remaining := time.Until(w.resetAt)
	if remaining <= 0 {
		delete(r.windows, ip)
		return 0
	}
	return int(remaining.Seconds()) + 1
}

// Cleanup removes expired windows. Call periodically to prevent memory growth.
func (r *RateLimiter) Cleanup() {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	for ip, w := range r.windows {
		if now.After(w.resetAt) {
			delete(r.windows, ip)
		}
	}
}

// StartCleanup runs a periodic cleanup goroutine.
func (r *RateLimiter) StartCleanup(done <-chan struct{}) {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				r.Cleanup()
			}
		}
	}()
}
