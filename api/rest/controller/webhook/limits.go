package webhook

import (
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/pkg/env"
)

var webhookRateLimiters = authmw.NewIPRateLimiters(15*time.Minute, webhookRateLimitConfig)

func webhookRateLimitConfig() (int, int) {
	vars := env.Variables()
	return vars.WebhookRateLimitPerMinute, vars.WebhookRateLimitBurst
}
