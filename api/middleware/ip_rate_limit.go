package middleware

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const ipRateLimitMaxCleanupInterval = time.Minute

// IPRateLimitConfig returns the per-minute rate and burst limit.
type IPRateLimitConfig func() (perMinute int, burst int)

// IPRateLimiters tracks token buckets by source IP.
type IPRateLimiters struct {
	mu       sync.Mutex
	clients  map[string]*ipClientLimiter
	staleAge time.Duration
	config   IPRateLimitConfig
}

type ipClientLimiter struct {
	limiter    *rate.Limiter
	lastSeen   time.Time
	perMinute  int
	burstLimit int
}

func NewIPRateLimiters(staleAge time.Duration, config IPRateLimitConfig) *IPRateLimiters {
	limiters := &IPRateLimiters{
		clients:  map[string]*ipClientLimiter{},
		staleAge: staleAge,
		config:   config,
	}
	go limiters.cleanupLoop(cleanupInterval(staleAge))
	return limiters
}

func (l *IPRateLimiters) Allow(ip string) bool {
	if l == nil {
		return true
	}
	perMinute, burst := 0, 0
	if l.config != nil {
		perMinute, burst = l.config()
	}
	if perMinute <= 0 || burst <= 0 {
		return true
	}

	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	client, ok := l.clients[ip]
	if ok && l.isStale(now, client) {
		delete(l.clients, ip)
		ok = false
	}
	if !ok || client.perMinute != perMinute || client.burstLimit != burst {
		limit := rate.Every(time.Minute / time.Duration(perMinute))
		client = &ipClientLimiter{
			limiter:    rate.NewLimiter(limit, burst),
			perMinute:  perMinute,
			burstLimit: burst,
		}
		l.clients[ip] = client
	}
	client.lastSeen = now
	return client.limiter.Allow()
}

func (l *IPRateLimiters) cleanupLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for now := range ticker.C {
		l.cleanupStale(now)
	}
}

func (l *IPRateLimiters) cleanupStale(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for key, client := range l.clients {
		if l.isStale(now, client) {
			delete(l.clients, key)
		}
	}
}

func (l *IPRateLimiters) isStale(now time.Time, client *ipClientLimiter) bool {
	return now.Sub(client.lastSeen) > l.staleAge
}

func cleanupInterval(staleAge time.Duration) time.Duration {
	if staleAge <= 0 {
		return time.Second
	}
	interval := staleAge / 2
	if interval <= 0 {
		return time.Second
	}
	if interval > ipRateLimitMaxCleanupInterval {
		return ipRateLimitMaxCleanupInterval
	}
	return interval
}
