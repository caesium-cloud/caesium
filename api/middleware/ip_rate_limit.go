package middleware

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

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
	return &IPRateLimiters{
		clients:  map[string]*ipClientLimiter{},
		staleAge: staleAge,
		config:   config,
	}
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

	for key, client := range l.clients {
		if now.Sub(client.lastSeen) > l.staleAge {
			delete(l.clients, key)
		}
	}

	client, ok := l.clients[ip]
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
