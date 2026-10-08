package workloadcatalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, raw string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

const valid = `{"schema_version":1,"workloads":[{"name":"smoke","driver":{"jobs":1},"expect":{"exit_code":0}}]}`

func TestLoadPreservesUnionAndDefersDriverValidation(t *testing.T) {
	raw := strings.Replace(valid, `"schema_version":1`, `"schema_version":1,"description":"catalog","tiers":{"smoke":"default"},"future_metadata":true`, 1)
	raw = strings.Replace(raw, `"name":"smoke"`, `"name":"smoke","description":"entry","requires":{"engine":"docker","server_env":["X=true"],"reason":"gate"},"sustained_rationale":"bounded"`, 1)
	// Invalid flag values must remain available for selected-entry precedence.
	raw = strings.Replace(raw, `"jobs":1`, `"jobs":1,"unknown-flag":{"nested":true}`, 1)
	c, err := Load(fixture(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	e := c.Workloads[0]
	if c.Description != "catalog" || c.Tiers["smoke"] != "default" || e.Requires.Engine != "docker" || e.Requires.ServerEnv[0] != "X=true" || e.Requires.Reason != "gate" || e.SustainedRationale != "bounded" || e.Driver["jobs"] != float64(1) {
		t.Fatalf("schema union lost: %+v %+v", c, e)
	}
}

func TestAllExpectationKeysPreserveExplicitValues(t *testing.T) {
	raw := `{"exit_code":0,"accounting_identity":false,"min_offered":0,"min_admitted":0,"min_succeeded":0,"max_unreconciled":0,"min_overload_signal":0,"sustained_verdict":"sustained","max_backlog_final":0,"require_drain_complete":false,"max_queue_depth_final":0,"min_queued_or_skipped":0,"min_cache_hit_ratio":0,"max_cache_hit_ratio":0,"min_api_reads_ok":0,"min_subscriber_events":0,"min_subscriber_coverage":0,"require_lifecycle_ok":["claim"],"require_unavailable_reason":{"dispatch":"disabled"},"max_duration_seconds":0}`
	var e Expectations
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		t.Fatal(err)
	}
	if e.declared != 20 || e.AccountingIdentity == nil || *e.AccountingIdentity || e.RequireDrainComplete == nil || *e.RequireDrainComplete || e.ExitCode == nil || *e.ExitCode != 0 || e.MinOffered == nil || *e.MinOffered != 0 || e.RequireLifecycleOK[0] != "claim" || e.RequireUnavailableReason["dispatch"] != "disabled" {
		t.Fatalf("explicit zero/false/nested values lost: %+v", e)
	}
	encoded, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var before, after map[string]any
	if err := json.Unmarshal([]byte(raw), &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &after); err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("keys lost: %s", encoded)
	}
	for key, want := range before {
		w, _ := json.Marshal(want)
		g, _ := json.Marshal(after[key])
		if string(w) != string(g) {
			t.Fatalf("%s: %s != %s", key, g, w)
		}
	}
}

func TestLoadRejectsInvalidAndUnselectedExpectations(t *testing.T) {
	for _, raw := range []string{
		`{}`, `{"schema_version":2,"workloads":[]}`,
		strings.Replace(valid, `"smoke"`, `" "`, 1),
		strings.Replace(valid, `"driver":{"jobs":1}`, `"driver":{}`, 1),
		strings.Replace(valid, `"expect":{"exit_code":0}`, `"expect":{}`, 1),
		strings.Replace(valid, `"expect":{"exit_code":0}`, `"expect":null`, 1),
		strings.Replace(valid, `"expect":{"exit_code":0}`, `"expect":{"unknown":1}`, 1),
		strings.Replace(valid, `"expect":{"exit_code":0}`, `"expect":{"exit_code":null}`, 1),
		strings.Replace(valid, `"expect":{"exit_code":0}`, `"expect":{"exit_code":"0"}`, 1),
		strings.Replace(valid, `"expect":{"exit_code":0}`, `"expect":{"accounting_identity":0}`, 1),
		strings.Replace(valid, `"expect":{"exit_code":0}`, `"expect":{"sustained_verdict":true}`, 1),
		strings.Replace(valid, `"expect":{"exit_code":0}`, `"expect":{"require_lifecycle_ok":[null]}`, 1),
		strings.Replace(valid, `"expect":{"exit_code":0}`, `"expect":{"require_lifecycle_ok":[1]}`, 1),
		strings.Replace(valid, `"expect":{"exit_code":0}`, `"expect":{"require_unavailable_reason":{"claim":null}}`, 1),
		strings.Replace(valid, `"expect":{"exit_code":0}`, `"expect":{"require_unavailable_reason":{"claim":1}}`, 1),
		strings.Replace(valid, `"expect":{"exit_code":0}`, `"expect":{"require_unavailable_reason":[]}`, 1),
		strings.TrimSuffix(valid, `]}`) + `,{"name":"unselected","driver":{"unused":"bad-value"},"expect":{"min_offered":"bad"}}]}`,
		strings.TrimSuffix(valid, `]}`) + `,{"name":"smoke","driver":{"jobs":1},"expect":{"exit_code":0}}]}`,
	} {
		if _, err := Load(fixture(t, raw)); err == nil {
			t.Fatalf("accepted invalid catalog: %s", raw)
		}
	}
}

func TestCurrentCatalogLoads(t *testing.T) {
	c, err := Load(filepath.Join("..", "..", "performance", "workloads.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Workloads) == 0 || len(c.Tiers) == 0 {
		t.Fatalf("empty actual catalog: %+v", c)
	}
}
