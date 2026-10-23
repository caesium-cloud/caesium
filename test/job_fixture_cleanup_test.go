//go:build integration

package test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cleanupJobAfterTest owns only the job returned to its caller. Callers use
// manual triggers or cron schedules outside the test window: pausing a job
// does not fence admission from a cron snapshot already in flight.
func (s *IntegrationTestSuite) cleanupJobAfterTest(jobID, triggerID string) *jobFixtureCleanup {
	t := s.T()
	t.Helper()
	fixture := &jobFixtureCleanup{
		baseURL: s.caesiumURL, authKey: s.authAPIKey,
		jobID: jobID, triggerID: triggerID, client: *http.DefaultClient,
	}
	if fixture.client.Timeout == 0 || fixture.client.Timeout > 10*time.Second {
		fixture.client.Timeout = 10 * time.Second
	}
	// testing cancels the child context before Cleanup, and testify restores
	// Suite.T to its parent first. Capture both the context values and T here;
	// cleanup must never call a suite request or assertion helper.
	ctx := context.WithoutCancel(t.Context())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(ctx, runTimeout+30*time.Second)
		defer cancel()
		if err := fixture.cleanup(ctx); err != nil {
			t.Errorf("clean up owned job %s: %v", jobID, err)
		}
	})
	return fixture
}

// Register after creating the backfill so LIFO cleanup joins its driver before
// the earlier job cleanup drains runs, even if a later test assertion fails.
func (s *IntegrationTestSuite) cleanupBackfillAfterTest(fixture *jobFixtureCleanup, backfillID string) {
	t := s.T()
	t.Helper()
	fixture.blockedReason = fmt.Sprintf("backfill %s has not joined", backfillID)
	ctx := context.WithoutCancel(t.Context())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(ctx, runTimeout+30*time.Second)
		defer cancel()
		if err := fixture.cancelBackfill(ctx, backfillID); err != nil {
			fixture.blockedReason = fmt.Sprintf("backfill %s terminal proof failed: %v", backfillID, err)
			t.Errorf("clean up owned backfill %s: %v", backfillID, err)
			return
		}
		fixture.blockedReason = ""
	})
}

type jobFixtureCleanup struct {
	baseURL       string
	authKey       string
	jobID         string
	triggerID     string
	client        http.Client
	blockedReason string
}

func (f jobFixtureCleanup) request(ctx context.Context, method, path string) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, f.baseURL+path, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	if f.authKey != "" {
		req.Header.Set("Authorization", "Bearer "+f.authKey)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("read %s %s: %w", method, path, err)
	}
	return resp.StatusCode, resp.Header, body, nil
}

func (f *jobFixtureCleanup) cleanup(ctx context.Context) error {
	if f.blockedReason != "" {
		return fmt.Errorf("retain owned job without cleanup mutations: %s", f.blockedReason)
	}
	path := "/v1/jobs/" + f.jobID
	status, _, body, err := f.request(ctx, http.MethodPut, path+"/pause")
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK:
		var paused struct {
			ID        string `json:"id"`
			TriggerID string `json:"trigger_id"`
			Paused    bool   `json:"paused"`
		}
		if err := json.Unmarshal(body, &paused); err != nil {
			return fmt.Errorf("decode paused job: %w", err)
		}
		if paused.ID != f.jobID || !paused.Paused {
			return fmt.Errorf("pause did not confirm owned job %s is paused: %s", f.jobID, body)
		}
		if f.triggerID == "" {
			f.triggerID = paused.TriggerID
		}
		if err := f.drainRuns(ctx); err != nil {
			return err
		}
	case http.StatusNotFound:
		// Some tests explicitly delete their fixture. Deletion is idempotent.
	default:
		return fmt.Errorf("pause job: HTTP %d: %s", status, body)
	}
	status, _, body, err = f.request(ctx, http.MethodDelete, path)
	if err != nil {
		return err
	}
	if status != http.StatusAccepted {
		return fmt.Errorf("delete job: HTTP %d: %s", status, body)
	}
	status, _, body, err = f.request(ctx, http.MethodGet, path)
	if err != nil {
		return err
	}
	if status != http.StatusNotFound {
		return fmt.Errorf("deleted job still resolves: HTTP %d: %s", status, body)
	}
	if f.triggerID == "" {
		return nil
	}
	// A shared trigger belongs to the remaining job. DELETE itself removes
	// only orphan triggers, so check ownership before asserting its absence.
	status, _, body, err = f.request(ctx, http.MethodGet, "/v1/jobs?trigger_id="+f.triggerID+"&limit=1")
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("list trigger owners: HTTP %d: %s", status, body)
	}
	var owners []jobSummary
	if err := json.Unmarshal(body, &owners); err != nil {
		return fmt.Errorf("decode trigger owners: %w", err)
	}
	if len(owners) > 0 {
		return nil
	}
	return f.requireTriggerAbsent(ctx)
}

func (f jobFixtureCleanup) drainRuns(ctx context.Context) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	var active []string
	for {
		active = nil
		for offset := 0; ; {
			path := fmt.Sprintf("/v1/jobs/%s/runs?limit=100&offset=%d", f.jobID, offset)
			status, header, body, err := f.request(ctx, http.MethodGet, path)
			if err != nil {
				return fmt.Errorf("drain admitted runs (last active %v): %w", active, err)
			}
			if status != http.StatusOK {
				return fmt.Errorf("list admitted runs: HTTP %d: %s", status, body)
			}
			var runs []runResponse
			if err := json.Unmarshal(body, &runs); err != nil {
				return fmt.Errorf("decode admitted runs: %w", err)
			}
			for _, run := range runs {
				switch run.Status {
				case "succeeded", "failed", "cancelled", "skipped":
				case "running":
					active = append(active, run.ID)
				default:
					return fmt.Errorf("run %s has unknown status %q", run.ID, run.Status)
				}
			}
			next := header.Get("X-Caesium-Next-Offset")
			if next == "" {
				break
			}
			parsed, err := strconv.Atoi(next)
			if err != nil || parsed <= offset {
				return fmt.Errorf("invalid run continuation %q after offset %d", next, offset)
			}
			offset = parsed
		}
		if len(active) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for admitted runs %v: %w", active, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (f jobFixtureCleanup) cancelBackfill(ctx context.Context, backfillID string) error {
	path := fmt.Sprintf("/v1/jobs/%s/backfills/%s", f.jobID, backfillID)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	cancelRequested := false
	for {
		status, _, body, err := f.request(ctx, http.MethodGet, path)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("get owned backfill: HTTP %d: %s", status, body)
		}
		var backfill backfillResponse
		if err := json.Unmarshal(body, &backfill); err != nil {
			return fmt.Errorf("decode owned backfill: %w", err)
		}
		if backfill.ID != backfillID || backfill.JobID != f.jobID {
			return fmt.Errorf("backfill identity changed: %s", body)
		}
		switch backfill.Status {
		case "succeeded", "failed", "cancelled":
			// The driver writes terminal state only after stopping admission,
			// joining its children, and flushing progress (or before any child).
			if backfill.CompletedAt == nil {
				return fmt.Errorf("terminal backfill has no completion timestamp: %s", body)
			}
			return nil
		case "running":
		default:
			return fmt.Errorf("backfill has unknown status %q", backfill.Status)
		}
		if !cancelRequested {
			status, _, body, err = f.request(ctx, http.MethodPut, path+"/cancel")
			if err != nil {
				return err
			}
			if status != http.StatusOK && status != http.StatusConflict {
				return fmt.Errorf("cancel owned backfill: HTTP %d: %s", status, body)
			}
			// A 409 can race completion. Neither it nor the 200 cancellation
			// response proves the driver joined; require a fresh terminal GET.
			cancelRequested = true
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for owned backfill to stop admitting: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (f jobFixtureCleanup) requireTriggerAbsent(ctx context.Context) error {
	// Trigger GET currently maps a missing database row to HTTP 500 on
	// unauthenticated servers. A successful, ordered collection census proves
	// absence without treating an error response as evidence of deletion.
	for offset := 0; ; offset += 100 {
		path := fmt.Sprintf("/v1/triggers?limit=100&offset=%d&order_by=id", offset)
		status, _, body, err := f.request(ctx, http.MethodGet, path)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("list triggers: HTTP %d: %s", status, body)
		}
		var triggers []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(body, &triggers); err != nil {
			return fmt.Errorf("decode triggers: %w", err)
		}
		for _, trigger := range triggers {
			if trigger.ID == f.triggerID {
				return fmt.Errorf("orphan trigger %s still exists after job deletion", f.triggerID)
			}
		}
		if len(triggers) < 100 {
			return nil
		}
	}
}

// This top-level live test preserves the existing suite's shard membership.
// The child must clean up before s.Run returns, after both context cancellation
// and testify's restoration of Suite.T. The unchanged createJob fails the GET
// assertion below with HTTP 200; the helper alone cannot make it pass.
func TestCreateJobFixtureCleanupUsesChildLifetime(t *testing.T) {
	s := &IntegrationTestSuite{}
	s.SetT(t)
	s.SetupSuite()
	for _, explicitDelete := range []bool{false, true} {
		var jobID, triggerID string
		var childContext context.Context
		s.Run(fmt.Sprintf("explicit-delete-%t", explicitDelete), func() {
			created := s.createJob(fmt.Sprintf("fixture-cleanup-%d", time.Now().UnixNano()), nil)
			jobID, triggerID = created.ID.String(), created.TriggerID.String()
			childContext = s.T().Context()
			if explicitDelete {
				resp, err := s.doRequest(http.MethodDelete, s.caesiumURL+"/v1/jobs/"+jobID, nil)
				require.NoError(s.T(), err)
				defer resp.Body.Close()
				require.Equal(s.T(), http.StatusAccepted, resp.StatusCode)
			} else {
				// Admit real work, then leave draining to the registered cleanup.
				s.startRunREST(jobID, "normal", nil)
			}
		})
		require.NotEmpty(t, jobID)
		require.Same(t, t, s.T(), "testify must restore the parent T before cleanup")
		require.ErrorIs(t, childContext.Err(), context.Canceled)
		// A parent fallback also keeps the intentionally failing old-source
		// probe isolated; it runs only after the durable-state assertions.
		s.cleanupJobAfterTest(jobID, triggerID)
		resp, err := s.doRequest(http.MethodGet, s.caesiumURL+"/v1/jobs/"+jobID, nil)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, resp.Body.Close())
		require.NoError(t, err)
		require.Equal(t, http.StatusNotFound, resp.StatusCode, "child-owned job survived cleanup: %s", body)
		fixture := jobFixtureCleanup{baseURL: s.caesiumURL, authKey: s.authAPIKey, triggerID: triggerID, client: *http.DefaultClient}
		require.NoError(t, fixture.requireTriggerAbsent(t.Context()))
	}
}

func TestJobFixtureCleanupDrainsPaginatedRuns(t *testing.T) {
	var olderRunReads atomic.Int32
	var triggerPageReads atomic.Int32
	var deleted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer cleanup-token", r.Header.Get("Authorization"))
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/v1/jobs/owned/pause":
			_, _ = io.WriteString(w, `{"id":"owned","trigger_id":"owned-trigger","paused":true}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/owned/runs":
			if r.URL.Query().Get("offset") == "0" {
				w.Header().Set("X-Caesium-Next-Offset", "100")
				runs := make([]runResponse, 100)
				for i := range runs {
					runs[i] = runResponse{ID: fmt.Sprintf("newer-%d", i), Status: "succeeded"}
				}
				_ = json.NewEncoder(w).Encode(runs)
			} else {
				assert.Equal(t, "100", r.URL.Query().Get("offset"))
				status := "running"
				if olderRunReads.Add(1) > 1 {
					status = "succeeded"
				}
				_, _ = fmt.Fprintf(w, `[{"id":"older","status":%q}]`, status)
			}
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/jobs/owned":
			assert.Greater(t, olderRunReads.Load(), int32(1), "must drain active work on the later page before delete")
			deleted.Store(true)
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/owned":
			assert.True(t, deleted.Load())
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs":
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/triggers":
			triggerPageReads.Add(1)
			assert.Equal(t, "id", r.URL.Query().Get("order_by"))
			if r.URL.Query().Get("offset") == "0" {
				triggers := make([]map[string]string, 100)
				for i := range triggers {
					triggers[i] = map[string]string{"id": fmt.Sprintf("foreign-%d", i)}
				}
				_ = json.NewEncoder(w).Encode(triggers)
			} else {
				assert.Equal(t, "100", r.URL.Query().Get("offset"))
				_, _ = io.WriteString(w, `[]`)
			}
		default:
			t.Errorf("unexpected cleanup request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	fixture := jobFixtureCleanup{baseURL: server.URL, jobID: "owned", authKey: "cleanup-token", client: *server.Client()}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, fixture.cleanup(ctx))
	require.True(t, deleted.Load())
	require.Equal(t, int32(2), triggerPageReads.Load(), "trigger absence requires the complete collection")
}

func TestJobFixtureCleanupReportsFailureBeforeDelete(t *testing.T) {
	for _, mode := range []string{"http-error", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			var deleted atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					deleted.Store(true)
				}
				if mode == "http-error" {
					http.Error(w, "pause failed", http.StatusInternalServerError)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/pause") {
					_, _ = io.WriteString(w, `{"id":"owned","paused":true}`)
					return
				}
				<-r.Context().Done()
			}))
			defer server.Close()
			fixture := jobFixtureCleanup{baseURL: server.URL, jobID: "owned", client: *server.Client()}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			err := fixture.cleanup(ctx)
			require.Error(t, err)
			if mode == "http-error" {
				require.Contains(t, err.Error(), "HTTP 500: pause failed")
			} else {
				require.ErrorIs(t, err, context.DeadlineExceeded)
			}
			require.False(t, deleted.Load(), "failed cleanup must not hide undrained work by deleting its job")
		})
	}
}

func TestBackfillFixtureCleanupCancelsBeforeJobDrain(t *testing.T) {
	var backfillReads, cancelRequests atomic.Int32
	var joined, deleted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer backfill-token", r.Header.Get("Authorization"))
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/owned/backfills/backfill":
			if backfillReads.Add(1) < 3 {
				_, _ = io.WriteString(w, `{"id":"backfill","job_id":"owned","status":"running"}`)
			} else {
				joined.Store(true)
				_, _ = io.WriteString(w, `{"id":"backfill","job_id":"owned","status":"cancelled","completed_at":"2026-01-01T00:00:00Z"}`)
			}
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/backfills/backfill/cancel"):
			cancelRequests.Add(1)
			_, _ = io.WriteString(w, `{"id":"backfill","job_id":"owned","status":"running"}`)
		case r.Method == http.MethodPut && r.URL.Path == "/v1/jobs/owned/pause":
			assert.True(t, joined.Load(), "backfill cleanup must join before job cleanup starts")
			_, _ = io.WriteString(w, `{"id":"owned","paused":true}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/owned/runs":
			assert.True(t, joined.Load(), "job run census must follow backfill terminal state")
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/jobs/owned":
			assert.True(t, joined.Load())
			deleted.Store(true)
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/owned":
			assert.True(t, deleted.Load())
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected cleanup request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	s := &IntegrationTestSuite{caesiumURL: server.URL, authAPIKey: "backfill-token"}
	s.SetT(t)
	var childContext context.Context
	s.Run("leave-after-registering-owned-cleanups", func() {
		childContext = s.T().Context()
		fixture := s.cleanupJobAfterTest("owned", "")
		s.cleanupBackfillAfterTest(fixture, "backfill")
	})
	require.Same(t, t, s.T())
	require.ErrorIs(t, childContext.Err(), context.Canceled)
	require.True(t, joined.Load())
	require.True(t, deleted.Load())
	require.Equal(t, int32(1), cancelRequests.Load(), "request cancellation once, then poll the owned terminal record")
	require.Equal(t, int32(3), backfillReads.Load(), "a running cancellation reply is not a completed join")
}

func TestBackfillFixtureCleanupRetainsJobAfterCancellationFailure(t *testing.T) {
	const childServer = "CAESIUM_TEST_BACKFILL_CLEANUP_FAILURE_SERVER"
	if serverURL := os.Getenv(childServer); serverURL != "" {
		s := &IntegrationTestSuite{caesiumURL: serverURL}
		s.SetT(t)
		fixture := s.cleanupJobAfterTest("owned", "")
		s.cleanupBackfillAfterTest(fixture, "backfill")
		return
	}
	var cancelRequests, jobRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/owned/backfills/backfill":
			_, _ = io.WriteString(w, `{"id":"backfill","job_id":"owned","status":"running"}`)
		case r.Method == http.MethodPut && r.URL.Path == "/v1/jobs/owned/backfills/backfill/cancel":
			cancelRequests.Add(1)
			http.Error(w, "cancel failed", http.StatusInternalServerError)
		default:
			jobRequests.Add(1)
			http.Error(w, "job cleanup ran before backfill joined", http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestBackfillFixtureCleanupRetainsJobAfterCancellationFailure$", "-test.count=1")
	command.Env = append(os.Environ(), childServer+"="+server.URL)
	output, err := command.CombinedOutput()
	var exited *exec.ExitError
	require.ErrorAs(t, err, &exited, "cleanup errors must fail the owning test: %s", output)
	require.Equal(t, 1, exited.ExitCode(), "cleanup failure must not panic: %s", output)
	require.Contains(t, string(output), "cancel owned backfill: HTTP 500: cancel failed")
	require.Contains(t, string(output), "retain owned job without cleanup mutations")
	require.Equal(t, int32(1), cancelRequests.Load())
	require.Zero(t, jobRequests.Load(), "failed terminal proof must prevent job pause, run census and DELETE")
}
