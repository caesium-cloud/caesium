//go:build integration

package robustness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/test/robustness/cluster"
)

func TestDurableSnapshotCannotCertifyUnavailableSQL(t *testing.T) {
	const runID = "11111111-1111-1111-1111-111111111111"
	const jobID = "22222222-2222-2222-2222-222222222222"
	if failure := os.Getenv("CAESIUM_TEST_SNAPSHOT_FAILURE"); failure != "" {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(cluster.Run{ID: runID, JobID: jobID, Status: "succeeded"})
				return
			}
			var request struct {
				SQL string `json:"sql"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if failure == "lease" || strings.Contains(request.SQL, "task_runs") {
				http.Error(w, "SQL view unavailable", http.StatusServiceUnavailable)
				return
			}
			_, _ = fmt.Fprintf(w, `{"rows":[[%q,"owner",1,"future"]]}`, runID)
		}))
		defer server.Close()
		fe := &faultEnv{httpAPI: &cluster.HTTP{Client: server.Client()}}
		fingerprintDurableRun(t, context.Background(), fe, server.URL, jobID, runID)
		t.Fatal("unavailable SQL produced a certifiable snapshot")
		return
	}
	for _, failure := range []string{"lease", "recipes"} {
		t.Run(failure, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestDurableSnapshotCannotCertifyUnavailableSQL$", "-test.count=1")
			cmd.Env = append(os.Environ(), "CAESIUM_TEST_SNAPSHOT_FAILURE="+failure)
			out, err := cmd.CombinedOutput()
			want := "inconclusive: snapshot lease"
			if failure == "recipes" {
				want = "inconclusive: snapshot durable task recipes"
			}
			if err == nil || !strings.Contains(string(out), want) || strings.Contains(string(out), "produced a certifiable snapshot") {
				t.Fatalf("unavailable %s did not fail closed: %v\n%s", failure, err, out)
			}
		})
	}
}
