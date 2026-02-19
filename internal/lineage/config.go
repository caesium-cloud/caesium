package lineage

import (
	"fmt"
	"strings"
	"time"
)

type Config struct {
	Enabled   bool
	Transport string
	URL       string
	Namespace string
	Headers   string
	FilePath  string
	Timeout   time.Duration
}

func BuildTransport(cfg Config) (Transport, error) {
	transportType := strings.ToLower(strings.TrimSpace(cfg.Transport))

	switch transportType {
	case "http":
		url := strings.TrimSpace(cfg.URL)
		if url == "" {
			return nil, fmt.Errorf("lineage: CAESIUM_OPEN_LINEAGE_URL is required for HTTP transport")
		}
		return NewHTTPTransport(HTTPTransportConfig{
			URL:     url,
			Headers: parseHeaders(cfg.Headers),
			Timeout: cfg.Timeout,
		}), nil

	case "console":
		return NewConsoleTransport(), nil

	case "file":
		return NewFileTransport(cfg.FilePath)

	default:
		return nil, fmt.Errorf("lineage: unsupported transport type %q", transportType)
	}
}

func parseHeaders(raw string) map[string]string {
	headers := make(map[string]string)
	if raw == "" {
		return headers
	}
	for _, pair := range strings.Split(raw, ",") {
		parts := strings.SplitN(strings.TrimSpace(pair), "=", 2)
		if len(parts) == 2 {
			headers[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return headers
}
