package webhook

import (
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/pkg/env"
	"golang.org/x/time/rate"
)

type ipRateLimiters struct {
	mu       sync.Mutex
	clients  map[string]*clientLimiter
	staleAge time.Duration
}

type clientLimiter struct {
	limiter    *rate.Limiter
	lastSeen   time.Time
	perMinute  int
	burstLimit int
}

var webhookRateLimiters = &ipRateLimiters{
	clients:  map[string]*clientLimiter{},
	staleAge: 15 * time.Minute,
}

func (l *ipRateLimiters) Allow(ip string) bool {
	perMinute := env.Variables().WebhookRateLimitPerMinute
	burst := env.Variables().WebhookRateLimitBurst
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
		client = &clientLimiter{
			limiter:    rate.NewLimiter(limit, burst),
			perMinute:  perMinute,
			burstLimit: burst,
		}
		l.clients[ip] = client
	}
	client.lastSeen = now
	return client.limiter.Allow()
}
