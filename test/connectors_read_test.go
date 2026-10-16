//go:build integration

package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/connector"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// These wire types deliberately decode the public HTTP response. No test calls
// the read service or writes observations/relationships into the catalog.
type connectorReadReference struct {
	ConnectionID string            `json:"connection_id"`
	Coordinates  map[string]string `json:"coordinates"`
}

type connectorReadExecution struct {
	ID                 string                 `json:"id"`
	Reference          connectorReadReference `json:"reference"`
	Availability       string                 `json:"availability"`
	Completeness       string                 `json:"completeness"`
	ObservedAt         time.Time              `json:"observed_at"`
	SnapshotObservedAt time.Time              `json:"snapshot_observed_at"`
	Metadata           struct {
		WorkflowID string `json:"workflow_id"`
		RunID      string `json:"run_id"`
		Terminal   bool   `json:"terminal"`
	} `json:"metadata"`
}

type connectorReadPage struct {
	Executions []connectorReadExecution `json:"executions"`
	Cursor     string                   `json:"cursor"`
	Incomplete bool                     `json:"incomplete"`
}

type connectorReadConnections struct {
	Ready       bool   `json:"ready"`
	Fingerprint string `json:"fingerprint"`
	Connections []struct {
		ID           string   `json:"id"`
		Provider     string   `json:"provider"`
		Capabilities []string `json:"capabilities"`
	} `json:"connections"`
}

type connectorReadRelations struct {
	Relationships []struct {
		Type      string                 `json:"type"`
		ID        string                 `json:"id"`
		Reference connectorReadReference `json:"reference"`
	} `json:"relationships"`
	Correlations []models.ExternalExecutionCorrelation `json:"correlations"`
}

func connectorReadHTTP(t *testing.T, base, key, method, path string, in, out any) (int, []byte) {
	t.Helper()
	var body []byte
	var err error
	if in != nil {
		body, err = json.Marshal(in)
		require.NoError(t, err)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, strings.TrimRight(base, "/")+path, bytes.NewReader(body))
	require.NoError(t, err)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	require.NoError(t, err)
	if out != nil {
		require.Equal(t, http.StatusOK, resp.StatusCode, "%s: %s", path, raw)
		require.NoError(t, json.Unmarshal(raw, out), "%s: %s", path, raw)
	}
	return resp.StatusCode, raw
}

func connectorFixtureControlURL() string {
	if base := os.Getenv("CAESIUM_CONNECTORS_FIXTURE_CONTROL_URL"); base != "" {
		return base
	}
	return "http://127.0.0.1:7240"
}

func connectorProviderRPCs(t *testing.T, methodSuffix ...string) int {
	t.Helper()
	var events []map[string]any
	connectorReadHTTP(t, connectorFixtureControlURL(), "", http.MethodGet, "/evidence", nil, &events)
	n := 0
	for _, event := range events {
		if event["kind"] == "provider_rpc" {
			method, ok := event["method"].(string)
			require.True(t, ok)
			require.NotEmpty(t, method)
			if len(methodSuffix) == 0 || strings.HasSuffix(method, methodSuffix[0]) {
				n++
			}
		}
	}
	return n
}

func (s *IntegrationTestSuite) connectorRead(path string, out any) {
	s.T().Helper()
	connectorReadHTTP(s.T(), s.caesiumURL, s.authAPIKey, http.MethodGet, path, nil, out)
}

func (s *IntegrationTestSuite) connectorRows(sql string) [][]any {
	s.T().Helper()
	var page struct {
		Rows      [][]any `json:"rows"`
		RowCount  int     `json:"row_count"`
		ReadOnly  bool    `json:"read_only"`
		Truncated bool    `json:"truncated"`
	}
	connectorReadHTTP(s.T(), s.caesiumURL, s.authAPIKey, http.MethodPost, "/v1/database/query", map[string]any{"sql": sql, "limit": 1000}, &page)
	s.Require().True(page.ReadOnly)
	s.Require().False(page.Truncated, "a truncated diagnostic cannot prove the write budget")
	s.Require().Equal(len(page.Rows), page.RowCount)
	return page.Rows
}

func (s *IntegrationTestSuite) connectorHostRestart() map[string]any {
	s.T().Helper()
	dir := filepath.Join(os.Getenv("CAESIUM_CONNECTORS_ARTIFACTS"), "control")
	id := uuid.NewString()
	body, err := json.Marshal(map[string]string{"id": id, "action": "restart"})
	s.Require().NoError(err)
	s.Require().NoError(os.WriteFile(filepath.Join(dir, "request.tmp"), body, 0600))
	s.Require().NoError(os.Rename(filepath.Join(dir, "request.tmp"), filepath.Join(dir, "request.json")))
	var response map[string]any
	s.Require().Eventually(func() bool {
		raw, e := os.ReadFile(filepath.Join(dir, "response.json"))
		return e == nil && json.Unmarshal(raw, &response) == nil && response["id"] == id
	}, 90*time.Second, 100*time.Millisecond, "owned server restart controller did not acknowledge this request")
	s.Require().Equal("ok", response["status"])
	s.Require().NotEmpty(response["started_before"])
	s.Require().NotEqual(response["started_before"], response["started_after"], "restart proof requires a new observed container start time")
	s.Require().Eventually(func() bool {
		req, e := http.NewRequestWithContext(s.T().Context(), http.MethodGet, s.caesiumURL+"/health/ready", nil)
		if e != nil {
			return false
		}
		resp, e := (&http.Client{Timeout: time.Second}).Do(req)
		if e != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 90*time.Second, 200*time.Millisecond)
	return response
}

// TestConnectorsRead is selected by the manual real-provider qualification.
// A selected run fails for missing fixture/auth/control prerequisites; normal
// integration lanes remain independent of the optional Temporal server.
func (s *IntegrationTestSuite) TestConnectorsRead() {
	if os.Getenv("CAESIUM_CONNECTORS_READ_FIXTURE") != "true" {
		s.T().Skip("requires selected real connector HTTP fixture")
	}
	s.Require().Equal("true", os.Getenv("CAESIUM_CONNECTORS_FIXTURE"))
	s.Require().Equal("true", os.Getenv("CAESIUM_CONNECTORS_OWNED_SERVER"), "process restart requires this invocation's immutable owned server ID")
	s.Require().NotEmpty(s.authAPIKey)
	artifacts := os.Getenv("CAESIUM_CONNECTORS_ARTIFACTS")
	s.Require().NotEmpty(artifacts)
	s.Require().Len(os.Getenv("CAESIUM_CONNECTORS_CANDIDATE_SHA"), 40)
	var ready map[string]any
	connectorReadHTTP(s.T(), connectorFixtureControlURL(), "", http.MethodGet, "/health", nil, &ready)
	s.Require().Equal(true, ready["ready"])
	var startup struct {
		CandidateSHA              string    `json:"candidate_sha"`
		ContainerID               string    `json:"container_id"`
		ImageID                   string    `json:"image_id"`
		ExitCode                  int       `json:"exit_code"`
		WaitExitCode              int       `json:"wait_exit_code"`
		OOMKilled                 bool      `json:"oom_killed"`
		StartedAt                 time.Time `json:"started_at"`
		FinishedAt                time.Time `json:"finished_at"`
		ProviderRPCBefore         *int      `json:"provider_rpc_before"`
		ProviderRPCAfter          *int      `json:"provider_rpc_after"`
		LogCanaryAbsent           bool      `json:"log_canary_absent"`
		StrictParserErrorObserved bool      `json:"strict_parser_error_observed"`
		SDK                       string    `json:"sdk"`
		CLI                       string    `json:"cli"`
		Server                    string    `json:"server"`
	}
	startupRaw, err := os.ReadFile(filepath.Join(artifacts, "invalid-startup-evidence.json"))
	s.Require().NoError(err, "selected HTTP qualification requires real invalid-enabled-startup evidence")
	s.Require().NoError(json.Unmarshal(startupRaw, &startup))
	s.Require().Equal(os.Getenv("CAESIUM_CONNECTORS_CANDIDATE_SHA"), startup.CandidateSHA)
	s.Require().Len(startup.ContainerID, 64)
	var serverImages []struct {
		ID string `json:"Id"`
	}
	imageRaw, err := os.ReadFile(filepath.Join(artifacts, "server-image.json"))
	s.Require().NoError(err)
	s.Require().NoError(json.Unmarshal(imageRaw, &serverImages))
	s.Require().Len(serverImages, 1)
	s.Require().NotEmpty(serverImages[0].ID)
	s.Require().Equal(serverImages[0].ID, startup.ImageID, "invalid startup must use the actual candidate server image")
	s.Require().Equal(1, startup.ExitCode)
	s.Require().Equal(1, startup.WaitExitCode)
	s.Require().False(startup.OOMKilled)
	s.Require().False(startup.StartedAt.IsZero())
	s.Require().True(startup.FinishedAt.After(startup.StartedAt))
	s.Require().NotNil(startup.ProviderRPCBefore)
	s.Require().NotNil(startup.ProviderRPCAfter)
	s.Require().Equal(*startup.ProviderRPCBefore, *startup.ProviderRPCAfter)
	s.Require().True(startup.LogCanaryAbsent)
	s.Require().True(startup.StrictParserErrorObserved)
	s.Require().Equal("1.49.0", startup.SDK)
	s.Require().Equal("1.9.1", startup.CLI)
	s.Require().Equal("1.32.0", startup.Server)
	s.Require().Equal(ready["sdk"], startup.SDK)
	s.Require().Equal(ready["cli"], startup.CLI)
	s.Require().Equal(ready["server"], startup.Server)
	startupLog, err := os.ReadFile(filepath.Join(artifacts, "invalid-startup.log"))
	s.Require().NoError(err)
	s.Require().Contains(string(startupLog), "inlineToken")
	s.Require().NotContains(string(startupLog), "fixture-invalid-inline-credential-canary")
	var connections connectorReadConnections
	s.connectorRead("/v1/connectors", &connections)
	s.Require().True(connections.Ready)
	s.Require().Len(connections.Fingerprint, 64)
	s.Require().Len(connections.Connections, 1)
	s.Require().Equal("fixture", connections.Connections[0].ID)
	s.Require().Equal("temporal", connections.Connections[0].Provider)
	for _, capability := range []string{"discovery", "inspection", "history", "relationships"} {
		s.Require().Contains(connections.Connections[0].Capabilities, capability)
	}
	var features map[string]any
	s.connectorRead("/v1/system/features", &features)
	s.Require().Equal(true, features["connectors_enabled"])
	s.Require().Equal(true, features["connectors_ready"])
	refs := make([]fixtureRef, 6)
	for i := range refs {
		refs[i] = s.fixtureStart(fmt.Sprintf("http-only-%d", i), "external_only", "", "", false, false, false, false)
	}
	beforeList := s.connectorRows("SELECT * FROM external_executions ORDER BY opaque_id")
	visibility := s.createAPIKeyCLI("--role", "viewer", "--description", "connector fixture Visibility readiness")
	pending := make(map[string]bool, len(refs))
	for _, ref := range refs {
		pending[ref.RunID] = true
	}
	visibilityCursor, visibilityReads := "", 0
	// Await these exact new executions, not a page populated by older fixture
	// workflows. A separate principal bounds setup to 40 reads and preserves the
	// tested principal's 60/minute budget for the actual pagination assertions.
	s.Require().Eventually(func() bool {
		if visibilityReads >= 40 {
			return false
		}
		visibilityReads++
		var page connectorReadPage
		connectorReadHTTP(s.T(), s.caesiumURL, visibility.Plaintext, http.MethodGet, "/v1/connectors/fixture/executions?workflow_type=external_only&page_size=100&cursor="+url.QueryEscape(visibilityCursor), nil, &page)
		for _, execution := range page.Executions {
			delete(pending, execution.Metadata.RunID)
		}
		visibilityCursor = page.Cursor
		return len(pending) == 0
	}, 20*time.Second, 500*time.Millisecond, "all six real workflows must enter Visibility before unchanged pagination is tested")
	listPath := "/v1/connectors/fixture/executions?workflow_type=external_only&page_size=2"
	var first connectorReadPage
	s.connectorRead(listPath, &first)
	s.Require().Len(first.Executions, 2)
	s.Require().NotEmpty(first.Cursor)
	seen := map[string]connectorReadExecution{}
	cursor, pages := "", 0
	for {
		path := listPath
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		var page connectorReadPage
		s.connectorRead(path, &page)
		s.Require().LessOrEqual(len(page.Executions), 2)
		for _, execution := range page.Executions {
			s.Require().NotEmpty(execution.ID)
			s.Require().Equal("fixture", execution.Reference.ConnectionID)
			s.Require().NotContains(seen, execution.Metadata.RunID, "unchanged paginated discovery returned a duplicate run")
			seen[execution.Metadata.RunID] = execution
		}
		pages++
		s.Require().Less(pages, 30, "bounded pagination must terminate")
		if page.Cursor == "" {
			break
		}
		s.Require().NotEqual(cursor, page.Cursor)
		cursor = page.Cursor
	}
	for _, ref := range refs {
		s.Require().Contains(seen, ref.RunID, "real external-only workflow must be discoverable")
	}
	s.Require().Equal(beforeList, s.connectorRows("SELECT * FROM external_executions ORDER BY opaque_id"), "list pages must insert no catalog identities")
	status, _ := connectorReadHTTP(s.T(), s.caesiumURL, s.authAPIKey, http.MethodGet, "/v1/connectors/fixture/executions?workflow_type=publication&page_size=2&cursor="+url.QueryEscape(first.Cursor), nil, nil)
	s.Require().Equal(http.StatusBadRequest, status, "cursor must bind to the original filter")
	external := seen[refs[0].RunID]
	path := "/v1/connectors/fixture/executions/" + url.PathEscape(external.ID)
	var detail connectorReadExecution
	s.connectorRead(path, &detail)
	s.Require().Equal(refs[0].WorkflowID, detail.Metadata.WorkflowID)
	s.Require().Equal(refs[0].RunID, detail.Metadata.RunID)
	s.Require().Equal("fixture", detail.Reference.ConnectionID)
	s.Require().Equal("available", detail.Availability)
	s.Require().False(detail.ObservedAt.IsZero())
	s.Require().False(detail.SnapshotObservedAt.IsZero())
	beforePoll := s.connectorRows("SELECT * FROM external_execution_snapshots ORDER BY id")
	beforeIdentities := s.connectorRows("SELECT * FROM external_executions ORDER BY opaque_id")
	for range 3 {
		var unchanged connectorReadExecution
		s.connectorRead(path, &unchanged)
		s.Require().Equal(detail.SnapshotObservedAt, unchanged.SnapshotObservedAt)
		s.Require().False(unchanged.ObservedAt.Before(detail.ObservedAt))
	}
	s.Require().Equal(beforePoll, s.connectorRows("SELECT * FROM external_execution_snapshots ORDER BY id"), "unchanged public polling must add zero snapshot writes")
	s.Require().Equal(beforeIdentities, s.connectorRows("SELECT * FROM external_executions ORDER BY opaque_id"), "unchanged public polling must add zero identity updates")
	var history struct {
		Events []struct {
			ID int64 `json:"event_id"`
		} `json:"events"`
		Cursor string `json:"cursor"`
	}
	events := map[int64]bool{}
	cursor = ""
	for page := 0; ; page++ {
		s.Require().Less(page, 30)
		history.Events = nil
		s.connectorRead(path+"/history?page_size=1&cursor="+url.QueryEscape(cursor), &history)
		for _, event := range history.Events {
			s.Require().Positive(event.ID)
			s.Require().False(events[event.ID], "history pagination duplicated an event")
			events[event.ID] = true
		}
		if history.Cursor == "" {
			break
		}
		cursor = history.Cursor
		history.Cursor = ""
	}
	s.Require().NotEmpty(events)
	s.Require().Equal(beforePoll, s.connectorRows("SELECT * FROM external_execution_snapshots ORDER BY id"), "history pages must not persist observations")
	var relations connectorReadRelations
	s.connectorRead(path+"/relationships", &relations)
	s.Require().Empty(relations.Correlations, "external-only workflows have no invented local run")
	// Denied principals are authenticated against the real server. The existing
	// agent credential mint is only credential setup; no expected application row
	// is seeded. A scope refusal (403 rather than 401) proves authentication.
	viewer := s.createAPIKeyCLI("--role", "viewer", "--description", "connector public read viewer")
	scoped := s.createAPIKeyCLI("--role", "viewer", "--scope-jobs", "connector-fixture-publish", "--description", "connector denied scoped viewer")
	s.Require().NoError(env.Process())
	agent := s.mintAgentSessionToken(uuid.NewString(), "connector-fixture-publish")
	for _, key := range []string{scoped.Plaintext, agent} {
		before := connectorProviderRPCs(s.T())
		for _, deniedPath := range []string{"/v1/connectors", listPath, path, path + "/history", path + "/relationships"} {
			code, raw := connectorReadHTTP(s.T(), s.caesiumURL, key, http.MethodGet, deniedPath, nil, nil)
			s.Require().Equal(http.StatusForbidden, code, "%s: %s", deniedPath, raw)
			if key == agent {
				s.Require().Contains(string(raw), "agent session token")
			}
			s.Require().NotContains(string(raw), refs[0].WorkflowID)
		}
		s.Require().Equal(before, connectorProviderRPCs(s.T()), "denial must precede provider RPC")
	}
	status, _ = connectorReadHTTP(s.T(), s.caesiumURL, "", http.MethodGet, path, nil, nil)
	s.Require().Equal(http.StatusUnauthorized, status)
	connectorReadHTTP(s.T(), s.caesiumURL, viewer.Plaintext, http.MethodGet, path, nil, &detail)
	beforeOutage := connectorProviderRPCs(s.T())
	connectorReadHTTP(s.T(), connectorFixtureControlURL(), "", http.MethodPost, "/fault", map[string]bool{"outage": true}, &ready)
	s.T().Cleanup(func() {
		connectorReadHTTP(s.T(), connectorFixtureControlURL(), "", http.MethodPost, "/fault", map[string]bool{"outage": false}, nil)
	})
	var stale connectorReadExecution
	s.connectorRead(path, &stale)
	s.Require().Equal("stale", stale.Availability)
	s.Require().Equal(detail.Metadata, stale.Metadata)
	s.Require().Equal(detail.SnapshotObservedAt, stale.SnapshotObservedAt)
	s.Require().Greater(connectorProviderRPCs(s.T()), beforeOutage, "real connector path interruption must activate")
	connectorReadHTTP(s.T(), connectorFixtureControlURL(), "", http.MethodPost, "/fault", map[string]bool{"outage": false}, &ready)
	s.connectorRead(path, &detail)
	s.Require().Equal("available", detail.Availability)
	restart := s.connectorHostRestart()
	s.connectorRead(path, &detail)
	s.Require().Equal(external.ID, detail.ID)
	s.Require().Equal(refs[0].RunID, detail.Metadata.RunID)
	s.Require().Equal("available", detail.Availability)
	// Operations/verified links protect their execution snapshots. The retention
	// ceiling applies only to unreferenced identities, including when Actions ran
	// first in this same live server. Retention must preserve protected evidence.
	const protectedSnapshots = "SELECT s.* FROM external_execution_snapshots s JOIN external_executions e ON e.opaque_id = s.opaque_id WHERE s.connection_id = 'fixture' AND e.referenced = 1 ORDER BY s.id"
	const unreferencedSnapshots = "SELECT s.id FROM external_execution_snapshots s JOIN external_executions e ON e.opaque_id = s.opaque_id WHERE s.connection_id = 'fixture' AND e.referenced = 0 ORDER BY s.id"
	protectedBefore := s.connectorRows(protectedSnapshots)
	if len(s.connectorRows("SELECT id FROM connector_operations WHERE connection_id = 'fixture'")) > 0 {
		s.Require().NotEmpty(protectedBefore, "real operations must retain protected observation evidence")
	}
	// Each insertion is caused by a direct public read of a distinct real
	// workflow. The fixture's small configured cap makes eviction observable.
	for _, ref := range refs {
		s.connectorRead("/v1/connectors/fixture/executions/"+url.PathEscape(seen[ref.RunID].ID), &detail)
	}
	rows := s.connectorRows(unreferencedSnapshots)
	s.Require().LessOrEqual(len(rows), 4, "configured unreferenced snapshot cap must hold through HTTP")
	s.Require().NotEmpty(rows)
	time.Sleep(11 * time.Second)
	s.connectorRead(path, &detail)
	rows = s.connectorRows(unreferencedSnapshots)
	s.Require().Empty(rows, "age retention must evict old unreferenced public snapshots; unchanged evidence cannot recreate them")
	s.Require().Equal(protectedBefore, s.connectorRows(protectedSnapshots), "count/age retention must preserve operation and relation snapshot evidence")
	// Advance real provider evidence after expiry, then read the closed workflow
	// repeatedly. Inspection must never dispatch an automatic status Query.
	closedViewer := s.createAPIKeyCLI("--role", "viewer", "--description", "closed connector workflow witness")
	queriesBefore := connectorProviderRPCs(s.T(), "/QueryWorkflow")
	s.fixtureSignal(refs[0], "fixture_finish", true)
	s.Require().Eventually(func() bool {
		connectorReadHTTP(s.T(), s.caesiumURL, closedViewer.Plaintext, http.MethodGet, path, nil, &detail)
		return detail.Metadata.Terminal
	}, 10*time.Second, time.Second)
	advanced := s.connectorRows("SELECT * FROM external_execution_snapshots ORDER BY id")
	s.Require().NotEmpty(advanced, "new real terminal evidence must persist after retention expiry")
	s.Require().LessOrEqual(len(s.connectorRows(unreferencedSnapshots)), 4, "polls through genuine closing evidence must preserve the configured unreferenced retention cap")
	terminalRef, err := connector.NewExecutionReference("fixture", map[string]string{"workflow_id": refs[0].WorkflowID, "run_id": refs[0].RunID})
	s.Require().NoError(err)
	terminalSQL := fmt.Sprintf("SELECT COUNT(*) FROM external_execution_snapshots WHERE opaque_id = '%s' AND terminal = 1", terminalRef.OpaqueID())
	s.Require().Equal([][]any{{float64(1)}}, s.connectorRows(terminalSQL), "this exact run's real terminal observation must be durable")
	for range 3 {
		connectorReadHTTP(s.T(), s.caesiumURL, closedViewer.Plaintext, http.MethodGet, path, nil, &detail)
		s.Require().True(detail.Metadata.Terminal)
	}
	s.Require().Equal(advanced, s.connectorRows("SELECT * FROM external_execution_snapshots ORDER BY id"))
	closedQueries := connectorProviderRPCs(s.T(), "/QueryWorkflow") - queriesBefore
	s.Require().Zero(closedQueries, "closed-workflow inspection must not automatically dispatch Queries")
	// Authenticated startup with the gate disabled ignores even a missing config
	// file. The sibling is owned by this invocation and has no connector routes.
	gateOff := os.Getenv("CAESIUM_CONNECTORS_GATE_OFF_URL")
	s.Require().NotEmpty(gateOff)
	status, _ = connectorReadHTTP(s.T(), gateOff, "", http.MethodGet, "/health/ready", nil, nil)
	s.Require().Equal(http.StatusOK, status)
	status, raw := connectorReadHTTP(s.T(), gateOff, "", http.MethodGet, "/v1/connectors", nil, nil)
	s.Require().Equal(http.StatusNotFound, status, string(raw))
	connectorReadHTTP(s.T(), gateOff, "", http.MethodGet, "/v1/system/features", nil, &features)
	s.Require().Equal(false, features["connectors_enabled"])
	s.Require().NoError(os.WriteFile(filepath.Join(artifacts, "read-http-evidence.json"), mustConnectorJSON(s.T(), map[string]any{"candidate_sha": os.Getenv("CAESIUM_CONNECTORS_CANDIDATE_SHA"), "external_workflows": refs, "list_pages": pages, "connections": connections, "restart": restart, "snapshot_rows_after_retention": len(rows), "snapshot_rows_after_terminal_advance": len(advanced), "closed_workflow_query_rpc_delta": closedQueries, "unchanged_snapshot_writes": 0, "unchanged_identity_updates": 0}), 0600))
	s.connectorReadCorrelation(viewer, scoped)
	// A fresh principal gets its own finite read window. Budget refusal must
	// precede provider work, including for cheap connection-list reads.
	budget := s.createAPIKeyCLI("--role", "viewer", "--description", "connector read budget witness")
	beforeBudget := connectorProviderRPCs(s.T())
	for range 60 {
		code, body := connectorReadHTTP(s.T(), s.caesiumURL, budget.Plaintext, http.MethodGet, "/v1/connectors", nil, nil)
		s.Require().Equal(http.StatusOK, code, string(body))
	}
	status, _ = connectorReadHTTP(s.T(), s.caesiumURL, budget.Plaintext, http.MethodGet, path, nil, nil)
	s.Require().Equal(http.StatusTooManyRequests, status)
	s.Require().Equal(beforeBudget, connectorProviderRPCs(s.T()), "budget refusal must precede RPC")
}

func mustConnectorJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.MarshalIndent(value, "", "  ")
	require.NoError(t, err)
	return append(raw, '\n')
}

func (s *IntegrationTestSuite) connectorReadCorrelation(viewer, scoped createdAPIKey) {
	s.T().Helper()
	for _, alias := range []string{"connector-fixture-publish", "connector-fixture-notify"} {
		manifest := fmt.Sprintf("apiVersion: v1\nkind: Job\nmetadata:\n  alias: %s\n  concurrency:\n    maxRuns: 1\n    strategy: queue\ntrigger:\n  type: cron\n  configuration:\n    cron: '0 0 1 1 *'\nsteps:\n  - name: work\n    image: alpine:3.23\n    command: [sh, -c, 'sleep 8; echo connector-http']\n", alias)
		dir := s.writeJobManifest(manifest)
		defer os.RemoveAll(dir)
		s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	}
	job := s.requireJobByAlias("connector-fixture-publish")
	second := s.requireJobByAlias("connector-fixture-notify")
	runner := s.createAPIKeyCLI("--role", "runner", "--scope-jobs", "connector-fixture-publish,connector-fixture-notify", "--description", "public connector correlation fixture")
	s.Require().Equal(http.StatusOK, s.fixtureRequest(http.MethodPost, "/configure", map[string]string{"runner_key": runner.Plaintext}, nil))
	reader := s.createAPIKeyCLI("--role", "viewer", "--description", "connector queued correlation reads")
	holder := s.postRunStart(job.ID, "http-correlation-holder", `{"params":{}}`)
	s.Require().Equal(http.StatusAccepted, holder.status)
	ref := s.fixtureStart("http-correlation", "publication", job.ID, second.ID, true, true, false, true)
	// A1's stateless handle can address an actual execution before eventually
	// consistent Visibility discovers it. Constructing it from the real start
	// response creates no catalog rows; all observations and links below still
	// travel through public HTTP. Discovery is qualified separately above.
	reference, err := connector.NewExecutionReference("fixture", map[string]string{"workflow_id": ref.WorkflowID, "run_id": ref.RunID})
	s.Require().NoError(err)
	path := "/v1/connectors/fixture/executions/" + url.PathEscape(connector.ReferenceID(reference))
	relationshipReads := 0
	readRelations := func(out *connectorReadRelations) {
		relationshipReads++
		s.Require().LessOrEqual(relationshipReads, 58, "correlation polling must stay within the fresh principal's real 60/minute budget")
		connectorReadHTTP(s.T(), s.caesiumURL, reader.Plaintext, http.MethodGet, path+"/relationships", nil, out)
	}
	var queued connectorReadRelations
	s.Require().Eventually(func() bool {
		readRelations(&queued)
		return len(queued.Correlations) == 1 && queued.Correlations[0].Outcome == "queued"
	}, 7*time.Second, time.Second, "public relation must expose the genuine queued admission")
	s.Require().NotNil(queued.Correlations[0].QueueID)
	s.Require().Nil(queued.Correlations[0].RunID)
	var promoted connectorReadRelations
	s.Require().Eventually(func() bool {
		readRelations(&promoted)
		return len(promoted.Correlations) == 1 && promoted.Correlations[0].RunID != nil
	}, 30*time.Second, time.Second)
	link := promoted.Correlations[0]
	s.Require().Equal(queued.Correlations[0].ID, link.ID)
	s.Require().Equal(queued.Correlations[0].QueueID, link.QueueID)
	s.Require().Equal(job.ID, link.JobID.String())
	s.Require().Equal("delegation", link.RelationType)
	s.Require().True(strings.HasPrefix(link.IdempotencyKey, ref.RunID+"/"))
	var reverse struct {
		Executions []models.ExternalExecutionCorrelation `json:"executions"`
	}
	reversePath := fmt.Sprintf("/v1/jobs/%s/runs/%s/external-executions", job.ID, link.RunID)
	connectorReadHTTP(s.T(), s.caesiumURL, viewer.Plaintext, http.MethodGet, reversePath, nil, &reverse)
	s.Require().Len(reverse.Executions, 1)
	s.Require().Equal(link.ID, reverse.Executions[0].ID)
	connectorReadHTTP(s.T(), s.caesiumURL, scoped.Plaintext, http.MethodGet, reversePath, nil, &reverse)
	s.Require().Empty(reverse.Executions, "local job permission must not disclose unrestricted external coordinates")
	s.Require().Eventually(func() bool {
		readRelations(&promoted)
		for _, relation := range promoted.Relationships {
			if relation.Type == "child" && relation.Reference.Coordinates["run_id"] != "" {
				return true
			}
		}
		return false
	}, 20*time.Second, time.Second, "actual child execution must retain its distinct provider identity")
	before := s.connectorRows("SELECT * FROM external_execution_correlations ORDER BY id")
	readRelations(&promoted)
	s.Require().Equal(before, s.connectorRows("SELECT * FROM external_execution_correlations ORDER BY id"), "repeat reads must retain one durable correlation")
	s.connectorHostRestart()
	connectorReadHTTP(s.T(), s.caesiumURL, viewer.Plaintext, http.MethodGet, reversePath, nil, &reverse)
	s.Require().Len(reverse.Executions, 1)
	s.Require().Equal(link.ID, reverse.Executions[0].ID, "public reverse link must survive an actual Caesium process restart")
	retirement := s.connectorReadRetiredCorrelation(viewer, scoped, reference, link, reversePath)
	s.Require().NoError(os.WriteFile(filepath.Join(os.Getenv("CAESIUM_CONNECTORS_ARTIFACTS"), "read-correlation-http-evidence.json"), mustConnectorJSON(s.T(), map[string]any{"candidate_sha": os.Getenv("CAESIUM_CONNECTORS_CANDIDATE_SHA"), "workflow": ref, "queued": queued, "promoted": promoted, "reverse": reverse, "relationship_http_reads": relationshipReads, "retired_connection": retirement}), 0600))
}

// Configuration changes go through the same file parser, guarded activation,
// and actual process restart as an operator's change. The runner never writes
// correlation or configuration rows into the database.
func (s *IntegrationTestSuite) connectorReadReconfigure(viewer createdAPIKey, raw []byte) map[string]any {
	s.T().Helper()
	var before, after connectorReadConnections
	connectorReadHTTP(s.T(), s.caesiumURL, viewer.Plaintext, http.MethodGet, "/v1/connectors", nil, &before)
	cfg, err := connector.Parse(raw, nil)
	s.Require().NoError(err)
	fingerprint, err := connector.Fingerprint(cfg)
	s.Require().NoError(err)
	artifacts := os.Getenv("CAESIUM_CONNECTORS_ARTIFACTS")
	s.Require().NoError(os.WriteFile(filepath.Join(artifacts, "control", "previous-fingerprint"), []byte(before.Fingerprint), 0600))
	// Truncate the mounted file rather than replacing its bind-mounted inode.
	s.Require().NoError(os.WriteFile(filepath.Join(artifacts, "http-connections.yaml"), raw, 0644))
	restart := s.connectorHostRestart()
	connectorReadHTTP(s.T(), s.caesiumURL, viewer.Plaintext, http.MethodGet, "/v1/connectors", nil, &after)
	s.Require().True(after.Ready)
	s.Require().Equal(fingerprint, after.Fingerprint)
	s.Require().NotEqual(before.Fingerprint, after.Fingerprint)
	return map[string]any{"previous_fingerprint": before.Fingerprint, "fingerprint": after.Fingerprint, "restart": restart}
}

func (s *IntegrationTestSuite) connectorReadRetiredCorrelation(viewer, scoped createdAPIKey, reference connector.ExecutionReference, link models.ExternalExecutionCorrelation, reversePath string) map[string]any {
	s.T().Helper()
	artifacts := os.Getenv("CAESIUM_CONNECTORS_ARTIFACTS")
	original, err := os.ReadFile(filepath.Join(artifacts, "http-connections.yaml"))
	s.Require().NoError(err)
	var document map[string]any
	s.Require().NoError(yaml.Unmarshal(original, &document))
	connections, ok := document["connections"].([]any)
	s.Require().True(ok)
	s.Require().Len(connections, 1)
	active, ok := connections[0].(map[string]any)
	s.Require().True(ok)
	retired := make(map[string]any, len(active))
	for key, value := range active {
		retired[key] = value
	}
	retired["id"] = "fixture-retired"
	document["connections"] = append(connections, retired)
	encode := func() []byte {
		raw, e := yaml.Marshal(document)
		s.Require().NoError(e)
		return raw
	}
	evidence := map[string]any{"enabled": s.connectorReadReconfigure(viewer, encode())}
	retiredRef, err := connector.NewExecutionReference("fixture-retired", reference.Coordinates)
	s.Require().NoError(err)
	retiredPath := "/v1/connectors/fixture-retired/executions/" + url.PathEscape(connector.ReferenceID(retiredRef))
	var relations connectorReadRelations
	connectorReadHTTP(s.T(), s.caesiumURL, viewer.Plaintext, http.MethodGet, retiredPath+"/relationships", nil, &relations)
	s.Require().Len(relations.Correlations, 1, "the second configured connection must observe genuine admission evidence over HTTP")
	retiredLink := relations.Correlations[0]
	s.Require().Equal(link.AdmissionID, retiredLink.AdmissionID)
	s.Require().Equal(link.RunID, retiredLink.RunID)
	s.Require().Equal("fixture-retired", retiredLink.ConnectionID)
	s.Require().NotEqual(link.ID, retiredLink.ID)
	evidence["correlation"] = retiredLink
	var reverse struct {
		Executions []models.ExternalExecutionCorrelation `json:"executions"`
	}
	assertLinks := func() {
		beforeRPC := connectorProviderRPCs(s.T())
		connectorReadHTTP(s.T(), s.caesiumURL, viewer.Plaintext, http.MethodGet, reversePath, nil, &reverse)
		var ids []uuid.UUID
		for _, row := range reverse.Executions {
			ids = append(ids, row.ID)
		}
		s.Require().ElementsMatch([]uuid.UUID{link.ID, retiredLink.ID}, ids, "retired correlations must remain visible alongside enabled connections")
		connectorReadHTTP(s.T(), s.caesiumURL, scoped.Plaintext, http.MethodGet, reversePath, nil, &reverse)
		s.Require().Empty(reverse.Executions, "retirement must preserve scope-safe reverse lookup")
		s.Require().Equal(beforeRPC, connectorProviderRPCs(s.T()), "reverse lookup must not dispatch provider RPCs")
	}
	assertLinks()
	for _, phase := range []string{"disabled", "removed"} {
		if phase == "disabled" {
			retired["enabled"] = false
		} else {
			document["connections"] = connections
		}
		evidence[phase] = s.connectorReadReconfigure(viewer, encode())
		beforeRPC := connectorProviderRPCs(s.T())
		assertLinks()
		status, raw := connectorReadHTTP(s.T(), s.caesiumURL, viewer.Plaintext, http.MethodGet, retiredPath, nil, nil)
		s.Require().Equal(http.StatusNotFound, status, string(raw))
		missing := fmt.Sprintf("/v1/jobs/%s/runs/%s/external-executions", link.JobID, uuid.New())
		status, raw = connectorReadHTTP(s.T(), s.caesiumURL, viewer.Plaintext, http.MethodGet, missing, nil, nil)
		s.Require().Equal(http.StatusNotFound, status, string(raw))
		s.Require().Equal(beforeRPC, connectorProviderRPCs(s.T()), "retired provider and missing-run refusals must not dispatch RPCs")
	}
	// Removing the added connection restores the original parsed configuration.
	originalConfig, err := connector.Parse(original, nil)
	s.Require().NoError(err)
	originalFingerprint, err := connector.Fingerprint(originalConfig)
	s.Require().NoError(err)
	var final connectorReadConnections
	connectorReadHTTP(s.T(), s.caesiumURL, viewer.Plaintext, http.MethodGet, "/v1/connectors", nil, &final)
	s.Require().Equal(originalFingerprint, final.Fingerprint)
	evidence["reverse_rpc_delta"] = 0
	return evidence
}
