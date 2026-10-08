//go:build integration

package performance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/caesium-cloud/caesium/test/internal/workloadcatalog"
)

func typedExpectations(t *testing.T, raw string) workloadcatalog.Expectations {
	t.Helper()
	var e workloadcatalog.Expectations
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		t.Fatal(err)
	}
	return e
}
func TestTypedExpectationPresenceKeepsFalseZeroAndEmptyCollections(t *testing.T) {
	for _, raw := range []string{`{"accounting_identity":false}`, `{"require_drain_complete":false}`, `{"require_lifecycle_ok":[]}`, `{"require_unavailable_reason":{}}`, `{"exit_code":0}`} {
		assertExpectations(t, catalogEntry{Name: "presence", Expect: typedExpectations(t, raw)}, driverResult{})
	}
}
func TestTypedExpectationsCoverAllTwentyFieldsAndNestedForms(t *testing.T) {
	raw := `{"exit_code":0,"accounting_identity":true,"min_offered":0,"min_admitted":0,"min_succeeded":0,"max_unreconciled":0,"min_overload_signal":0,"sustained_verdict":"sustained","max_backlog_final":0,"require_drain_complete":true,"max_queue_depth_final":0,"min_queued_or_skipped":0,"min_cache_hit_ratio":0,"max_cache_hit_ratio":0,"min_api_reads_ok":0,"min_subscriber_events":0,"min_subscriber_coverage":0,"require_lifecycle_ok":["claim"],"require_unavailable_reason":{"dispatch":"disabled"},"max_duration_seconds":0}`
	var report map[string]any
	if err := json.Unmarshal([]byte(`{"accounting":{"offered_equals_dropped_plus_attempted":true,"admitted_equals_settled":true,"offered":0,"admitted":0,"dropped":0,"queued_or_skipped":0,"rejected":0,"transport_uncertain":0,"unreconciled":0},"counts":{"succeeded":0},"throughput":{"verdict":"sustained"},"backlog":{"final":0},"drain":{"all_admitted_reconciled":true,"queue_status":"ok","queue_depth_final":0},"cache":{"status":"ok","hit_ratio":0},"api_reads":{"status":"ok","ok":0},"subscribers":{"events_received":0,"coverage_ratio":0},"lifecycle":{"intervals":{"claim":{"status":"ok"},"dispatch":{"status":"unavailable","unavailable_reasons":{"disabled":1}}}}}`), &report); err != nil {
		t.Fatal(err)
	}
	assertExpectations(t, catalogEntry{Name: "twenty", Expect: typedExpectations(t, raw)}, driverResult{report: report})
}
func TestPerformanceCatalogLoadsTypedExpectationsBeforeSelection(t *testing.T) {
	t.Chdir(t.TempDir())
	raw := `{"schema_version":1,"workloads":[{"name":"smoke","tier":"smoke","driver":{"jobs":1},"expect":{"accounting_identity":false,"require_lifecycle_ok":[]}},{"name":"other","driver":{"unused":[false]},"expect":{"exit_code":0}}]}`
	if err := os.WriteFile(catalogFile, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	c := loadCatalog(t)
	if c.Workloads[0].Expect.AccountingIdentity == nil || *c.Workloads[0].Expect.AccountingIdentity || c.Workloads[0].Expect.RequireLifecycleOK == nil {
		t.Fatalf("presence lost: %+v", c)
	}
	bad := `{"schema_version":1,"workloads":[{"name":"smoke","driver":{"jobs":1},"expect":{"exit_code":0}},{"name":"unselected","driver":{"jobs":1},"expect":{"require_unavailable_reason":{"claim":1}}}]}`
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte(bad), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := workloadcatalog.Load(path); err == nil {
		t.Fatal("malformed unselected expectation accepted")
	}
}
