package secret

import (
	"fmt"
	"net/url"
	"strings"
)

// Reference represents a parsed secret:// URI.
type Reference struct {
	Raw      string
	URL      *url.URL
	Provider string
	Path     string
	Segments []string
	Query    url.Values
}

// Parse converts a secret:// URI into a structured reference.
func Parse(ref string) (*Reference, error) {
	u, err := url.Parse(ref)
	if err != nil {
		return nil, fmt.Errorf("parse secret reference %q: %w", ref, err)
	}
	if u.Scheme != scheme {
		return nil, fmt.Errorf("invalid secret scheme %q", u.Scheme)
	}
	provider := strings.ToLower(strings.TrimSpace(u.Host))
	if provider == "" {
		return nil, fmt.Errorf("secret reference %q missing provider", ref)
	}
	path := strings.TrimPrefix(u.Path, "/")
	segments := []string{}
	if path != "" {
		segments = strings.Split(path, "/")
	}
	return &Reference{
			Raw:      ref,
			URL:      u,
			Provider: provider,
			Path:     path,
			Segments: segments,
			Query:    u.Query(),
		},
		nil
}
