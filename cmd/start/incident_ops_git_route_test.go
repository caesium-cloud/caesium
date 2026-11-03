package start

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	incidentsvc "github.com/caesium-cloud/caesium/api/rest/service/incident"
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/incident"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const (
	forgeManifestPath = "pipelines/orders.job.yaml"
	forgeSyncedCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	forgeHeadCommit   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// stubForge answers the GitHub calls the route makes, so these scenarios run the
// real render, executor, router and forge client together. The router's own edge
// cases are in internal/incident against a fuller fake.
type stubForge struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	file    []byte
	body    string
	message string
	written string
	pullURL string
}

func newStubForge(t *testing.T, file string) *stubForge {
	t.Helper()
	stub := &stubForge{t: t, file: []byte(file)}
	stub.srv = httptest.NewServer(http.HandlerFunc(stub.serve))
	t.Cleanup(stub.srv.Close)
	return stub
}

func (f *stubForge) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(status int, body any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	resource := strings.TrimPrefix(r.URL.Path, "/repos/acme/jobs/")
	switch {
	case resource == "pulls" && r.Method == http.MethodGet:
		reply(http.StatusOK, []any{})
	case resource == "git/ref/heads/main":
		reply(http.StatusOK, map[string]any{"ref": "refs/heads/main", "object": map[string]any{"sha": forgeHeadCommit}})
	case strings.HasPrefix(resource, "contents/") && r.Method == http.MethodGet:
		sum := sha256.Sum256(f.file)
		reply(http.StatusOK, map[string]any{
			"type": "file", "encoding": "base64", "sha": hex.EncodeToString(sum[:20]),
			"content": base64.StdEncoding.EncodeToString(f.file),
		})
	case resource == "git/refs":
		reply(http.StatusCreated, map[string]any{})
	case strings.HasPrefix(resource, "contents/") && r.Method == http.MethodPut:
		var put struct{ Message, Content string }
		require.NoError(f.t, json.NewDecoder(r.Body).Decode(&put))
		decoded, err := base64.StdEncoding.DecodeString(put.Content)
		require.NoError(f.t, err)
		f.message, f.written = put.Message, string(decoded)
		reply(http.StatusOK, map[string]any{})
	case resource == "pulls" && r.Method == http.MethodPost:
		var pull struct{ Title, Body, Head, Base string }
		require.NoError(f.t, json.NewDecoder(r.Body).Decode(&pull))
		f.body = pull.Body
		f.pullURL = f.srv.URL + "/acme/jobs/pull/7"
		reply(http.StatusCreated, map[string]any{
			"number": 7, "html_url": f.pullURL, "state": "open",
			"head": map[string]any{"ref": pull.Head, "repo": map[string]any{"full_name": "acme/jobs"}},
		})
	default:
		reply(http.StatusNotFound, map[string]any{"message": "Not Found"})
	}
}

// routed gives the rig's job verified provenance and a router that reaches stub,
// and records every event.
func (r *previewRig) routed(t *testing.T, stub *stubForge) *recordingBus {
	t.Helper()
	repo := stub.srv.URL + "/acme/jobs.git"
	require.NoError(t, r.db.Model(&models.Job{}).Where("id = ?", r.job.ID).Updates(map[string]any{
		"provenance_repo":      repo,
		"provenance_ref":       "main",
		"provenance_commit":    forgeSyncedCommit,
		"provenance_path":      forgeManifestPath,
		"provenance_source_id": "orders",
	}).Error)
	r.exec.SetProvenanceRouter(incident.NewProvenanceRouter(incident.ProvenanceConfig{
		GitSyncEnabled: true,
		Sources: env.GitSources{{
			URL: repo, Ref: "main", SourceID: "orders", Path: "pipelines", Globs: []string{"*.job.yaml"},
		}},
		Credentials: env.GitWriteCredentials{{
			SourceID: "orders", Forge: env.GitForgeGitHub, APIBaseURL: stub.srv.URL,
			Token: "ghs_stubforgetoken", Namespaces: []string{"marketing"},
		}},
	}))
	bus := &recordingBus{}
	r.ops.bus = bus
	r.exec.SetEventSink(bus, event.NewStore(r.db))
	return bus
}

// approve proposes a patch and carries it through approval.
func (r *previewRig) approve(t *testing.T, manifest string) *models.AgentAction {
	t.Helper()
	action, err := r.exec.Execute(context.Background(), incident.ActionRequest{
		IncidentID: r.inc.ID,
		Type:       incident.ActionTypeApplyJobdefPatch,
		Params:     patchParams(t, manifest),
	})
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, r.db.Model(&models.ApprovalRequest{}).Where("action_id = ?", action.ID).Updates(map[string]any{
		"decision": models.ApprovalDecisionApproved, "decider": "operator@example.com", "decided_at": now, "updated_at": now,
	}).Error)
	require.NoError(t, r.db.Model(&models.AgentAction{}).Where("id = ?", action.ID).
		Update("status", models.AgentActionStatusApproved).Error)
	executed, err := r.exec.ExecuteApproved(context.Background(), action.ID)
	require.NoError(t, err)
	return executed
}

// TestPullRequestKeepsASharedTriggersSecretFromTheIncident is Codex's round 2
// finding through the real surface. Marketing's job shares an HTTP trigger with a
// finance job, so the confined render withholds its webhook secret from marketing's
// readers. The proposal replaces the trigger, so the file's old secret is on a
// removed line of the repository's diff. A value only the file holds is there too.
// Neither may reach anything a reader of the incident can read.
func TestPullRequestKeepsASharedTriggersSecretFromTheIncident(t *testing.T) {
	const (
		secret   = "finance-webhook-secret"
		fileOnly = "value-only-the-file-holds"
	)
	stored := strings.NewReplacer(
		"type: cron", "type: http",
		`cron: "0 6 * * *"`, "path: /hooks/shared\n    secret: "+secret,
	).Replace(patchedJob)
	file := strings.Replace(stored, "REGION: eu", "REGION: "+fileOnly, 1)
	rig := newPreviewRig(t, stored)
	require.NoError(t, rig.db.Create(&models.Job{
		ID: uuid.New(), Alias: "fin", Namespace: "finance", TriggerID: rig.job.TriggerID,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}).Error)
	stub := newStubForge(t, file)
	bus := rig.routed(t, stub)

	proposed := strings.NewReplacer(
		"path: /hooks/shared", "path: /hooks/marketing",
		"secret: "+secret, "secret: secret://env/HOOK",
	).Replace(stored)
	executed := rig.approve(t, proposed)

	var result struct {
		Route string `json:"route"`
		PRURL string `json:"pr_url"`
		Diff  struct {
			Empty bool   `json:"empty"`
			Diff  string `json:"diff"`
		} `json:"diff"`
	}
	require.NoError(t, json.Unmarshal(executed.Result, &result), string(executed.Result))
	require.Equal(t, "git_pr", result.Route, string(executed.Result))
	require.NotEmpty(t, result.PRURL)
	require.False(t, result.Diff.Empty)
	require.Contains(t, result.Diff.Diff, "/hooks/marketing", "the result shows the proposal against the confined copy")

	surfaces := map[string]string{"stored result": string(executed.Result)}

	for _, confined := range []bool{true, false} {
		detail, err := incidentsvc.NewWithDatabase(context.Background(), rig.db).GetForReader(rig.inc.ID, confined)
		require.NoError(t, err)
		encoded, err := json.Marshal(detail)
		require.NoError(t, err)
		surfaces["incident read"] += string(encoded)
	}

	require.NotEmpty(t, bus.published)
	for _, published := range bus.published {
		surfaces["published events"] += string(published.Payload)
	}
	var persisted []models.ExecutionEvent
	require.NoError(t, rig.db.Find(&persisted).Error)
	require.NotEmpty(t, persisted)
	for i := range persisted {
		surfaces["persisted events"] += string(persisted[i].Payload)
	}

	for name, text := range surfaces {
		require.NotContains(t, text, secret, name)
		require.NotContains(t, text, fileOnly, name)
	}

	// The description goes to readers of the file, masked: the old secret shows only
	// as a digest, so the change still reads as one.
	require.NotContains(t, stub.body, secret)
	require.Regexp(t, `(?m)^-\s+secret: .\[REDACTED\] [0-9a-f]{8}`, stub.body)
	require.Contains(t, stub.body, "secret: secret://env/HOOK")
	require.Contains(t, stub.body, fileOnly, "the description compares the file itself")
	require.NotContains(t, stub.message, secret)
	require.NotContains(t, stub.message, fileOnly)
	require.NotContains(t, stub.written, secret, "the commit replaces the secret")
}

// TestPullRequestWithAnEmptyRenderDoesNotSayNoChanges: the file moved ahead of the
// server, so the proposal matches the server's copy and the render is empty, while
// the commit does change the file.
func TestPullRequestWithAnEmptyRenderDoesNotSayNoChanges(t *testing.T) {
	const fileOnly = "value-only-the-file-holds"
	rig := newPreviewRig(t, patchedJob)
	stub := newStubForge(t, strings.Replace(patchedJob, "REGION: eu", "REGION: "+fileOnly, 1))
	rig.routed(t, stub)

	preview := rig.preview(t, patchedJob)
	require.Contains(t, preview["diff"], "no changes to the server's copy")
	require.NotContains(t, preview["diff"], "identical")

	executed := rig.approve(t, patchedJob)
	var result struct {
		Route string `json:"route"`
		PRURL string `json:"pr_url"`
		Diff  struct {
			Empty bool   `json:"empty"`
			Diff  string `json:"diff"`
		} `json:"diff"`
	}
	require.NoError(t, json.Unmarshal(executed.Result, &result), string(executed.Result))
	require.Equal(t, "git_pr", result.Route, string(executed.Result))
	require.False(t, result.Diff.Empty, "a pull request is a change")
	require.NotContains(t, result.Diff.Diff, "no changes")
	require.Contains(t, result.Diff.Diff, result.PRURL, "the result says where the change is")
	require.NotContains(t, string(executed.Result), fileOnly)
	require.Contains(t, stub.body, fileOnly, "the description shows the change")
}
