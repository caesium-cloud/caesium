package start

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	incidentsvc "github.com/caesium-cloud/caesium/api/rest/service/incident"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/incident"
	internaljobdef "github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	pkglog "github.com/caesium-cloud/caesium/pkg/log"
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
	// location, when set, answers every request with a redirect to it, as sent.
	location string
	// pulls is what the pull request list returns.
	pulls []any
	// opened counts pull requests opened.
	opened int
	// requests counts every request.
	requests int
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
	f.requests++
	reply := func(status int, body any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	if f.location != "" {
		w.Header()["Location"] = []string{f.location}
		w.WriteHeader(http.StatusFound)
		return
	}
	resource := strings.TrimPrefix(r.URL.Path, "/repos/acme/jobs/")
	switch {
	case resource == "pulls" && r.Method == http.MethodGet:
		reply(http.StatusOK, append([]any{}, f.pulls...))
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
		f.opened++
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
	return r.routedWith(t, stub, nil)
}

// routedWith is routed for a source confined to sourceNamespaces (nil,
// unrestricted), with the real sync tenancy check.
func (r *previewRig) routedWith(t *testing.T, stub *stubForge, sourceNamespaces []string) *recordingBus {
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
			Namespaces: sourceNamespaces,
		}},
		Credentials: env.GitWriteCredentials{{
			SourceID: "orders", Forge: env.GitForgeGitHub, APIBaseURL: stub.srv.URL,
			Token: "ghs_stubforgetoken", Namespaces: []string{"marketing"},
		}},
		SyncTenancy: newSyncTenancyCheck(r.db),
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

// incidentSurfaces collects everything a reader of the incident, or of its
// escalation, can read: the stored result, the incident read confined and not,
// and the published and persisted events, which notifications render from.
func (r *previewRig) incidentSurfaces(t *testing.T, bus *recordingBus, executed *models.AgentAction) map[string]string {
	t.Helper()
	surfaces := map[string]string{"stored result": string(executed.Result)}
	for _, confined := range []bool{true, false} {
		detail, err := incidentsvc.NewWithDatabase(context.Background(), r.db).GetForReader(r.inc.ID, confined)
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
	require.NoError(t, r.db.Find(&persisted).Error)
	require.NotEmpty(t, persisted)
	for i := range persisted {
		surfaces["persisted events"] += string(persisted[i].Payload)
	}
	return surfaces
}

// TestTransportErrorFromTheForgeStaysOffTheIncident is Codex's round 3 finding
// through the real client and executor. The forge answers with a Location header
// Go's client cannot parse, and the client's error quotes it whole before
// CheckRedirect runs. Only the call and a fixed category may reach the incident,
// its events and the server log.
func TestTransportErrorFromTheForgeStaysOffTheIncident(t *testing.T) {
	const marker = "finance-webhook-secret"
	rig := newPreviewRig(t, patchedJob)
	stub := newStubForge(t, patchedJob)
	stub.location = "/" + marker + "%zz"
	bus := rig.routed(t, stub)
	logged := lastLogSequence()

	executed := rig.approve(t, strings.Replace(patchedJob, "REGION: eu", "REGION: us", 1))

	var result struct {
		Route   string `json:"route"`
		Degrade string `json:"degrade"`
		Reason  string `json:"reason"`
	}
	require.NoError(t, json.Unmarshal(executed.Result, &result), string(executed.Result))
	require.Equal(t, "escalate", result.Route, string(executed.Result))
	require.Equal(t, "forge_error", result.Degrade)
	require.Equal(t, "forge: request: transport error", result.Reason)

	surfaces := rig.incidentSurfaces(t, bus, executed)
	entries := logsSince(logged)
	require.NotEmpty(t, entries, "the degrade is logged")
	for _, entry := range entries {
		surfaces["server log"] += entry.Message + string(entry.Fields)
	}
	for name, text := range surfaces {
		require.NotContains(t, text, marker, name)
	}
	require.Contains(t, surfaces["published events"], "forge: request: transport error", "the escalation says what failed")
}

// TestPullRequestSomeoneElseOpenedIsNotRecordedAsTheRoutes: the preview names the
// branch, so a maintainer can open a pull request from it with another edit while
// approval is pending. The route checks the manifest at that pull request's head
// against the approved replacement, and escalates instead of claiming it.
func TestPullRequestSomeoneElseOpenedIsNotRecordedAsTheRoutes(t *testing.T) {
	rig := newPreviewRig(t, patchedJob)
	stub := newStubForge(t, patchedJob)
	bus := rig.routed(t, stub)
	action, err := rig.exec.Execute(context.Background(), incident.ActionRequest{
		IncidentID: rig.inc.ID,
		Type:       incident.ActionTypeApplyJobdefPatch,
		Params:     patchParams(t, strings.Replace(patchedJob, "REGION: eu", "REGION: us", 1)),
	})
	require.NoError(t, err)
	branch := "caesium/patch-" + action.ID.String()
	// The stub serves the unpatched file at every ref, so the head holds another
	// change than the approved one.
	stub.pulls = []any{map[string]any{
		"number": 9, "html_url": stub.srv.URL + "/acme/jobs/pull/9", "state": "open",
		"base": map[string]any{"ref": "main"},
		"head": map[string]any{"ref": branch, "sha": forgeHeadCommit, "repo": map[string]any{"full_name": "acme/jobs"}},
	}}
	now := time.Now().UTC()
	require.NoError(t, rig.db.Model(&models.ApprovalRequest{}).Where("action_id = ?", action.ID).Updates(map[string]any{
		"decision": models.ApprovalDecisionApproved, "decider": "operator@example.com", "decided_at": now, "updated_at": now,
	}).Error)
	require.NoError(t, rig.db.Model(&models.AgentAction{}).Where("id = ?", action.ID).
		Update("status", models.AgentActionStatusApproved).Error)
	executed, err := rig.exec.ExecuteApproved(context.Background(), action.ID)
	require.NoError(t, err)

	var result struct {
		Route    string `json:"route"`
		Degrade  string `json:"degrade"`
		Reason   string `json:"reason"`
		PRBranch string `json:"pr_branch"`
		PRURL    string `json:"pr_url"`
	}
	require.NoError(t, json.Unmarshal(executed.Result, &result), string(executed.Result))
	require.Equal(t, "escalate", result.Route, string(executed.Result))
	require.Equal(t, "pull_request_mismatch", result.Degrade)
	require.Equal(t, branch, result.PRBranch)
	require.Contains(t, result.Reason, branch)
	require.Empty(t, result.PRURL)
	require.Zero(t, stub.opened, "the route opens no second pull request")
	require.Empty(t, stub.written, "the route writes nothing over it")
	for name, text := range rig.incidentSurfaces(t, bus, executed) {
		require.NotContains(t, text, "/acme/jobs/pull/9", name)
	}
}

// TestGitRouteRefusesATriggerSyncWouldRefuse is Greptile's round 3 finding
// through the real surface. The proposal moves marketing's HTTP trigger onto the
// webhook path a finance job serves. The sync that imports the merged pull
// request refuses that for a source confined to marketing, and for an
// unrestricted source under a policy file it would fail the whole source's sync.
// So the route escalates before any forge call. An unrestricted source without a
// policy file imports it, so the route opens the pull request.
func TestGitRouteRefusesATriggerSyncWouldRefuse(t *testing.T) {
	for name, tc := range map[string]struct {
		sourceNamespaces []string
		policyFile       bool
		refused          bool
	}{
		"allowlisted source":                      {sourceNamespaces: []string{"marketing"}, refused: true},
		"unrestricted source under a policy file": {policyFile: true, refused: true},
		"unrestricted source with no policy file": {},
	} {
		t.Run(name, func(t *testing.T) {
			const other = "finance-hook-owner"
			stored := strings.NewReplacer("type: cron", "type: http", `cron: "0 6 * * *"`, "path: /hooks/marketing").Replace(patchedJob)
			rig := newPreviewRig(t, stored)
			def, err := schema.Parse([]byte(strings.NewReplacer(
				"alias: orders-daily", "alias: "+other,
				"namespace: marketing", "namespace: finance",
				"path: /hooks/marketing", "path: /hooks/finance",
			).Replace(stored)))
			require.NoError(t, err)
			_, err = internaljobdef.NewImporter(rig.db).Apply(context.Background(), def)
			require.NoError(t, err)
			if tc.policyFile {
				withPolicyFile(t, "apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  marketing: {}\n  finance: {}\n")
			}
			stub := newStubForge(t, stored)
			bus := rig.routedWith(t, stub, tc.sourceNamespaces)

			executed := rig.approve(t, strings.Replace(stored, "path: /hooks/marketing", "path: /hooks/finance", 1))
			var result struct {
				Route   string `json:"route"`
				Degrade string `json:"degrade"`
				Reason  string `json:"reason"`
			}
			require.NoError(t, json.Unmarshal(executed.Result, &result), string(executed.Result))
			if !tc.refused {
				require.Equal(t, "git_pr", result.Route, string(executed.Result))
				require.Equal(t, 1, stub.opened)
				require.Contains(t, stub.written, "path: /hooks/finance")
				return
			}
			require.Equal(t, "escalate", result.Route, string(executed.Result))
			require.Equal(t, "trigger_tenancy", result.Degrade)
			require.Zero(t, stub.requests, "no forge call")
			for surface, text := range rig.incidentSurfaces(t, bus, executed) {
				require.NotContains(t, text, other, surface)
			}
			require.NotContains(t, result.Reason, "finance")
		})
	}
}

// withPolicyFile loads an access policy file for the test, as the server does
// at start, so the trigger tenancy invariant is enforced.
func withPolicyFile(t *testing.T, content string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	holder, err := policy.LoadFile(t.Context(), policy.HolderOptions{Path: path})
	require.NoError(t, err)
	previous := policy.ProcessHolder()
	t.Cleanup(func() { policy.SetProcessHolder(previous) })
	policy.SetProcessHolder(holder)
}

func lastLogSequence() uint64 {
	entries := pkglog.Buffer().Snapshot()
	if len(entries) == 0 {
		return 0
	}
	return entries[len(entries)-1].Sequence
}

func logsSince(sequence uint64) []pkglog.LogEntry {
	var out []pkglog.LogEntry
	for _, entry := range pkglog.Buffer().Snapshot() {
		if entry.Sequence > sequence {
			out = append(out, entry)
		}
	}
	return out
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

	for name, text := range rig.incidentSurfaces(t, bus, executed) {
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
