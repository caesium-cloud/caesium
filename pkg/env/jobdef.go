package env

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// GitSources represents a collection of Git sync source configurations parsed from
// the CAESIUM_JOBDEF_GIT_SOURCES environment variable. The value must be JSON
// encoded (array of objects matching GitSourceConfig).
type GitSources []GitSourceConfig

// Decode implements envconfig.Decoder.
func (g *GitSources) Decode(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		*g = nil
		return nil
	}

	var sources []GitSourceConfig
	if err := json.Unmarshal([]byte(value), &sources); err != nil {
		return fmt.Errorf("decode git sources: %w", err)
	}

	*g = sources
	return nil
}

// GitSourceConfig describes a Git repository used to source job definitions.
type GitSourceConfig struct {
	URL      string        `json:"url"`
	Ref      string        `json:"ref,omitempty"`
	Path     string        `json:"path,omitempty"`
	Globs    []string      `json:"globs,omitempty"`
	SourceID string        `json:"source_id,omitempty"`
	LocalDir string        `json:"local_dir,omitempty"`
	Interval string        `json:"interval,omitempty"`
	Once     *bool         `json:"once,omitempty"`
	Auth     *GitBasicAuth `json:"auth,omitempty"`
	SSH      *GitSSHAuth   `json:"ssh,omitempty"`
}

// GitBasicAuth captures HTTPS credential configuration for a Git source.
type GitBasicAuth struct {
	Username    string `json:"username,omitempty"`
	Password    string `json:"password,omitempty"`
	UsernameRef string `json:"username_ref,omitempty"`
	PasswordRef string `json:"password_ref,omitempty"`
}

// GitSSHAuth captures SSH credential configuration for a Git source.
type GitSSHAuth struct {
	Username        string   `json:"username,omitempty"`
	UsernameRef     string   `json:"username_ref,omitempty"`
	PrivateKey      string   `json:"private_key,omitempty"`
	PrivateKeyRef   string   `json:"private_key_ref,omitempty"`
	Passphrase      string   `json:"passphrase,omitempty"`
	PassphraseRef   string   `json:"passphrase_ref,omitempty"`
	KnownHosts      string   `json:"known_hosts,omitempty"`
	KnownHostsRef   string   `json:"known_hosts_ref,omitempty"`
	KnownHostsPath  string   `json:"known_hosts_path,omitempty"`
	KnownHostsPaths []string `json:"known_hosts_paths,omitempty"`
}

// IntervalDuration resolves the per-source interval, falling back to the
// provided default when unset.
func (c GitSourceConfig) IntervalDuration(defaultInterval time.Duration) (time.Duration, error) {
	trimmed := strings.TrimSpace(c.Interval)
	if trimmed == "" {
		return defaultInterval, nil
	}
	duration, err := time.ParseDuration(trimmed)
	if err != nil {
		return 0, fmt.Errorf("parse interval %q: %w", c.Interval, err)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("interval must be positive")
	}
	return duration, nil
}

// OnceValue resolves whether the watcher should execute only once for this
// source, preferring the per-source value when provided.
func (c GitSourceConfig) OnceValue(defaultOnce bool) bool {
	if c.Once == nil {
		return defaultOnce
	}
	return *c.Once
}

// IsZero reports whether the configuration lacks the minimal required fields.
func (c GitSourceConfig) IsZero() bool {
	return strings.TrimSpace(c.URL) == ""
}
