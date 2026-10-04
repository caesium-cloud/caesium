// Package workloadcatalog reads the shared load and performance workload schema.
package workloadcatalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Catalog struct {
	SchemaVersion int               `json:"schema_version"`
	Description   string            `json:"description"`
	Tiers         map[string]string `json:"tiers"`
	Workloads     []Entry           `json:"workloads"`
}
type Entry struct {
	Name               string         `json:"name"`
	Tier               string         `json:"tier"`
	Description        string         `json:"description"`
	Requires           Requires       `json:"requires"`
	Driver             map[string]any `json:"driver"`
	Expect             Expectations   `json:"expect"`
	SustainedRationale string         `json:"sustained_rationale"`
}
type Requires struct {
	Engine    string   `json:"engine"`
	ServerEnv []string `json:"server_env"`
	Reason    string   `json:"reason"`
}

// Pointer fields preserve omitted values independently of explicit false/zero.
type Expectations struct {
	ExitCode                 *float64          `json:"exit_code,omitempty"`
	AccountingIdentity       *bool             `json:"accounting_identity,omitempty"`
	MinOffered               *float64          `json:"min_offered,omitempty"`
	MinAdmitted              *float64          `json:"min_admitted,omitempty"`
	MinSucceeded             *float64          `json:"min_succeeded,omitempty"`
	MaxUnreconciled          *float64          `json:"max_unreconciled,omitempty"`
	MinOverloadSignal        *float64          `json:"min_overload_signal,omitempty"`
	SustainedVerdict         *string           `json:"sustained_verdict,omitempty"`
	MaxBacklogFinal          *float64          `json:"max_backlog_final,omitempty"`
	RequireDrainComplete     *bool             `json:"require_drain_complete,omitempty"`
	MaxQueueDepthFinal       *float64          `json:"max_queue_depth_final,omitempty"`
	MinQueuedOrSkipped       *float64          `json:"min_queued_or_skipped,omitempty"`
	MinCacheHitRatio         *float64          `json:"min_cache_hit_ratio,omitempty"`
	MaxCacheHitRatio         *float64          `json:"max_cache_hit_ratio,omitempty"`
	MinAPIReadsOK            *float64          `json:"min_api_reads_ok,omitempty"`
	MinSubscriberEvents      *float64          `json:"min_subscriber_events,omitempty"`
	MinSubscriberCoverage    *float64          `json:"min_subscriber_coverage,omitempty"`
	RequireLifecycleOK       []string          `json:"require_lifecycle_ok,omitempty"`
	RequireUnavailableReason map[string]string `json:"require_unavailable_reason,omitempty"`
	MaxDurationSeconds       *float64          `json:"max_duration_seconds,omitempty"`
	declared                 int
}

func (e *Expectations) UnmarshalJSON(data []byte) error {
	type plain Expectations
	var decoded plain
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&decoded); err != nil {
		return fmt.Errorf("expectations: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for key, raw := range fields {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("expectation %q cannot be null", key)
		}
		// encoding/json allows null into a string; refuse it inside the two
		// nested forms as well, rather than accepting an empty assertion name.
		switch key {
		case "require_lifecycle_ok":
			var names []*string
			if err := json.Unmarshal(raw, &names); err != nil {
				return err
			}
			for _, name := range names {
				if name == nil {
					return fmt.Errorf("expectation %q contains null", key)
				}
			}
		case "require_unavailable_reason":
			var reasons map[string]*string
			if err := json.Unmarshal(raw, &reasons); err != nil {
				return err
			}
			for _, reason := range reasons {
				if reason == nil {
					return fmt.Errorf("expectation %q contains null", key)
				}
			}
		}
	}
	decoded.declared = len(fields)
	*e = Expectations(decoded)
	return nil
}

func Load(path string) (*Catalog, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("read workload catalog: %w", err)
	}
	var c Catalog
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse workload catalog %s: %w", path, err)
	}
	if c.SchemaVersion != 1 {
		return nil, fmt.Errorf("workload catalog %s has schema_version %d, this driver understands 1", path, c.SchemaVersion)
	}
	if len(c.Workloads) == 0 {
		return nil, fmt.Errorf("workload catalog %s declares no workloads", path)
	}
	seen := make(map[string]bool)
	for _, entry := range c.Workloads {
		if strings.TrimSpace(entry.Name) == "" {
			return nil, fmt.Errorf("workload catalog %s has an entry without a name", path)
		}
		if seen[entry.Name] {
			return nil, fmt.Errorf("workload catalog %s declares %q twice", path, entry.Name)
		}
		seen[entry.Name] = true
		if len(entry.Driver) == 0 {
			return nil, fmt.Errorf("workload %q declares no driver flags", entry.Name)
		}
		if entry.Expect.declared == 0 {
			return nil, fmt.Errorf("workload %q declares no expectations, so running it could not fail", entry.Name)
		}
	}
	return &c, nil
}
