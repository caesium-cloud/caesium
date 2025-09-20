package config

import (
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	envBaseURL = "CAESIUM_BASE_URL"
	envHost    = "CAESIUM_HOST"
)

// Config captures runtime configuration for the console client.
type Config struct {
	BaseURL     *url.URL
	HTTPTimeout time.Duration
}

// Load reads configuration values from the environment and
// applies sane defaults if values are not provided.
func Load() (*Config, error) {
	baseURL := os.Getenv(envBaseURL)

	if baseURL == "" {
		host := strings.TrimSpace(os.Getenv(envHost))
		if host == "" {
			baseURL = "http://127.0.0.1:8080"
		} else {
			if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
				baseURL = host
			} else {
				baseURL = "http://" + host
			}

			if !strings.Contains(baseURL[strings.Index(baseURL, "://")+3:], ":") {
				baseURL = strings.TrimRight(baseURL, "/") + ":8080"
			}
		}
	}

	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		BaseURL:     u,
		HTTPTimeout: 10 * time.Second,
	}

	return cfg, nil
}
