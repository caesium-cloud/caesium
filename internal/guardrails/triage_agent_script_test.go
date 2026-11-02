package guardrails_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// runTriageAgent runs build/triage-agent.sh against a fake agent API serving
// bundle, returning the paths the script posted to and its output.
func runTriageAgent(t *testing.T, bundle string) ([]string, string, error) {
	t.Helper()
	for _, tool := range []string{"sh", "jq", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}

	var (
		mu     sync.Mutex
		posted []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(bundle))
		case strings.HasSuffix(r.URL.Path, "/notes"):
			mu.Lock()
			posted = append(posted, r.URL.Path)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"id":"n","type":"note","status":"executed"}`))
		default:
			mu.Lock()
			posted = append(posted, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"action":{"id":"a","type":"skip_task"},"disposition":"awaiting_approval"}`))
		}
	}))
	t.Cleanup(server.Close)

	cmd := exec.CommandContext(t.Context(), "sh", "../../build/triage-agent.sh")
	cmd.Env = append(cmd.Environ(),
		"CAESIUM_API_URL="+server.URL,
		"CAESIUM_INCIDENT_ID=inc-1",
		"CAESIUM_AGENT_TOKEN=token",
	)
	out, err := cmd.CombinedOutput()
	mu.Lock()
	defer mu.Unlock()
	return posted, string(out), err
}

func triageBundle(t *testing.T, tasks []map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"incident":       map[string]any{"id": "inc-1", "job_id": "job-1", "status": "open", "task_name": "load"},
		"classification": map[string]any{"class": "data_quality_hold"},
		"failure":        map[string]any{"log_tail_scrubbed": false},
		"job":            map[string]any{"alias": "orders", "tasks": tasks, "edges": []any{}},
		"run_history":    []any{},
		"lineage_impact": map[string]any{"allowed_jobs": []any{}, "frozen": true},
		"generated_at":   "2026-11-02T00:00:00Z",
	})
	require.NoError(t, err)
	return string(raw)
}

func TestTriageAgentAcceptsABundleWithNoDAGAndProposesNoTaskAction(t *testing.T) {
	posted, out, err := runTriageAgent(t, triageBundle(t, []map[string]any{}))
	require.NoError(t, err, out)
	require.Equal(t, []string{"/v1/agent/incidents/inc-1/notes"}, posted, "a job with no DAG has no task to act on")
	require.Contains(t, out, "proposing no task action")
}

func TestTriageAgentProposesATaskActionWhenTheJobHasTasks(t *testing.T) {
	posted, out, err := runTriageAgent(t, triageBundle(t, []map[string]any{{"name": "load"}}))
	require.NoError(t, err, out)
	require.Equal(t, []string{"/v1/agent/incidents/inc-1/notes", "/v1/agent/incidents/inc-1/actions"}, posted)
}

func TestTriageAgentStillRejectsAMalformedBundle(t *testing.T) {
	_, out, err := runTriageAgent(t, `{"incident":{"id":"inc-1"}}`)
	require.Error(t, err)
	require.Contains(t, out, "bundle shape assertion failed")
}
