package http

import (
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
)

type Config struct {
	Path            string            `json:"path,omitempty"`
	Secret          string            `json:"secret,omitempty"`
	SignatureScheme string            `json:"signatureScheme,omitempty"`
	SignatureHeader string            `json:"signatureHeader,omitempty"`
	TimestampHeader string            `json:"timestampHeader,omitempty"`
	MaxTimestampAge string            `json:"maxTimestampAge,omitempty"`
	ParamMapping    map[string]string `json:"paramMapping,omitempty"`
	DefaultParams   map[string]string `json:"defaultParams,omitempty"`
}

const defaultMaxTimestampAge = 5 * time.Minute

// maxTimestampAgeDuration returns the configured max timestamp age or the default (5m).
func (c Config) maxTimestampAgeDuration() time.Duration {
	if c.MaxTimestampAge == "" {
		return defaultMaxTimestampAge
	}
	d, err := time.ParseDuration(c.MaxTimestampAge)
	if err != nil || d <= 0 {
		return defaultMaxTimestampAge
	}
	return d
}

func (c Config) withDefaults() Config {
	if c.ParamMapping == nil {
		c.ParamMapping = map[string]string{}
	}
	if c.DefaultParams == nil {
		c.DefaultParams = map[string]string{}
	}
	c.Path = normalizePath(c.Path)
	c.Secret = strings.TrimSpace(c.Secret)
	c.SignatureScheme = strings.ToLower(strings.TrimSpace(c.SignatureScheme))
	c.SignatureHeader = strings.TrimSpace(c.SignatureHeader)
	return c
}

func (c Config) mergedParams(params map[string]string) map[string]string {
	merged := make(map[string]string, len(c.DefaultParams)+len(params))
	for k, v := range c.DefaultParams {
		merged[k] = v
	}
	for k, v := range params {
		merged[k] = v
	}
	return merged
}

func normalizePath(path string) string {
	return models.NormalizedTriggerPath(path)
}
