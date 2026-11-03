package incident

import (
	"context"
	"crypto/sha1" //nolint:gosec // git blob ids in a test fake
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/caesium-cloud/caesium/internal/incident/forge"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	pkglog "github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const (
	forgeTestToken  = "ghs_3kF9xQ2mZ7vB1nT8wR4yL6pD"
	testManifest    = "pipelines/orders.job.yaml"
	testAlias       = "orders-daily"
	testSiblingText = "alias: orders-hourly  # keep this comment"
)

var (
	syncedCommit = strings.Repeat("a", 40)
	headCommit   = strings.Repeat("b", 40)
)

// manifestFile is a multi-document manifest: a sibling job and comments that a
// patch of orders-daily must leave byte for byte.
const manifestFile = `# Owned by data-eng.
---
# The daily rollup.
apiVersion: v1
kind: Job
metadata:
  alias: orders-daily
trigger:
  type: cron
  configuration:
    cron: "0 6 * * *"
steps:
  - name: load
    image: busybox:1.36.1
    command: ["true"]
---
apiVersion: v1
kind: Job
metadata:
  alias: orders-hourly  # keep this comment
trigger:
  type: cron
  configuration:
    cron: "0 * * * *"
steps:
  - name: load
    image: busybox:1.36.1
    command: ["true"]
`

const patchedDefinition = `apiVersion: v1
kind: Job
metadata:
  alias: orders-daily
trigger:
  type: cron
  configuration:
    cron: "0 6 * * *"
steps:
  - name: load
    image: busybox:1.36.1
    command: ["sh", "-c", "exit 0"]
`

func patchJSON(t *testing.T, manifest string) json.RawMessage {
	t.Helper()
	def, err := schema.Parse([]byte(manifest))
	require.NoError(t, err)
	encoded, err := json.Marshal(def)
	require.NoError(t, err)
	return encoded
}

// fakeGitHub speaks the GitHub REST shapes the client uses, against an in-memory
// repository, so a test exercises URL building, headers and error mapping.
type fakeGitHub struct {
	mu       sync.Mutex
	t        *testing.T
	srv      *httptest.Server
	heads    map[string]string
	files    map[string][]byte // "<ref>\x00<path>"
	pulls    []map[string]any
	requests []string
	nextPull int
	// failOn answers the next request containing it with failStatus and
	// failMessage, then clears.
	failOn      string
	failStatus  int
	failMessage string
	redirectTo  string
	// badLocation answers every request with a redirect to this Location.
	badLocation string
	// hidePullsUntilOpen makes pull request lists come back empty until the route
	// tries to open one, as a lookup lagging behind the forge would.
	hidePullsUntilOpen bool
	// onRequest runs as a request arrives.
	onRequest func()
}

func newFakeGitHub(t *testing.T, content string) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{
		t:        t,
		heads:    map[string]string{"main": headCommit},
		files:    map[string][]byte{},
		nextPull: 40,
	}
	f.put(syncedCommit, testManifest, []byte(content))
	f.put(headCommit, testManifest, []byte(content))
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) put(ref, filePath string, content []byte) {
	f.files[ref+"\x00"+filePath] = content
}

func (f *fakeGitHub) get(ref, filePath string) ([]byte, bool) {
	content, ok := f.files[ref+"\x00"+filePath]
	return content, ok
}

func (f *fakeGitHub) requestCount(match string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, request := range f.requests {
		if strings.Contains(request, match) {
			count++
		}
	}
	return count
}

func (f *fakeGitHub) hits() int { return f.requestCount("") }

func (f *fakeGitHub) failNext(match string, status int, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failOn, f.failStatus, f.failMessage = match, status, message
}

func blobSHA(content []byte) string {
	sum := sha1.Sum(content) //nolint:gosec // git blob ids in a test fake
	return hex.EncodeToString(sum[:])
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	request := r.Method + " " + r.URL.EscapedPath()
	f.requests = append(f.requests, request)
	if f.onRequest != nil {
		f.onRequest()
	}
	if r.Header.Get("Authorization") != "Bearer "+forgeTestToken {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "Bad credentials"})
		return
	}
	if f.redirectTo != "" {
		http.Redirect(w, r, f.redirectTo, http.StatusFound)
		return
	}
	if f.badLocation != "" {
		// Set as sent, unlike http.Redirect, which would clean it.
		w.Header()["Location"] = []string{f.badLocation}
		w.WriteHeader(http.StatusFound)
		return
	}
	if f.failOn != "" && strings.Contains(request, f.failOn) {
		status, message := f.failStatus, f.failMessage
		f.failOn = ""
		writeJSON(w, status, map[string]any{"message": message})
		return
	}
	const root = "/repos/acme/jobs/"
	resource, ok := strings.CutPrefix(r.URL.Path, root)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
		return
	}
	switch {
	case strings.HasPrefix(resource, "git/ref/heads/") && r.Method == http.MethodGet:
		branch := strings.TrimPrefix(resource, "git/ref/heads/")
		sha, ok := f.heads[branch]
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ref": "refs/heads/" + branch, "object": map[string]any{"sha": sha}})
	case resource == "git/refs" && r.Method == http.MethodPost:
		var body struct{ Ref, SHA string }
		require.NoError(f.t, json.NewDecoder(r.Body).Decode(&body))
		branch := strings.TrimPrefix(body.Ref, "refs/heads/")
		if _, exists := f.heads[branch]; exists {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "Reference already exists"})
			return
		}
		f.heads[branch] = body.SHA
		for key, content := range f.files {
			if ref, filePath, _ := strings.Cut(key, "\x00"); ref == body.SHA {
				f.put(branch, filePath, content)
			}
		}
		writeJSON(w, http.StatusCreated, map[string]any{"ref": body.Ref})
	case strings.HasPrefix(resource, "contents/") && r.Method == http.MethodGet:
		content, ok := f.get(r.URL.Query().Get("ref"), strings.TrimPrefix(resource, "contents/"))
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"type": "file", "encoding": "base64", "sha": blobSHA(content),
			"content": base64.StdEncoding.EncodeToString(content),
		})
	case strings.HasPrefix(resource, "contents/") && r.Method == http.MethodPut:
		var body struct{ Message, Content, SHA, Branch string }
		require.NoError(f.t, json.NewDecoder(r.Body).Decode(&body))
		filePath := strings.TrimPrefix(resource, "contents/")
		current, ok := f.get(body.Branch, filePath)
		if !ok || blobSHA(current) != body.SHA {
			writeJSON(w, http.StatusConflict, map[string]any{"message": "stale blob"})
			return
		}
		decoded, err := base64.StdEncoding.DecodeString(body.Content)
		require.NoError(f.t, err)
		f.put(body.Branch, filePath, decoded)
		// The write is a new commit at the branch's head.
		commit := blobSHA([]byte(body.Branch + "\x00" + string(decoded)))
		f.heads[body.Branch] = commit
		f.put(commit, filePath, decoded)
		writeJSON(w, http.StatusOK, map[string]any{})
	case resource == "pulls" && r.Method == http.MethodGet:
		out := []map[string]any{}
		if f.hidePullsUntilOpen {
			writeJSON(w, http.StatusOK, out)
			return
		}
		query := r.URL.Query()
		for _, pull := range f.pulls {
			head := pull["head"].(map[string]any)
			ref := head["ref"].(string)
			if "acme:"+ref != query.Get("head") {
				continue
			}
			if state := query.Get("state"); state != "all" && (pull["state"] == "open") != (state == "open") {
				continue
			}
			// A pull request's head is its branch's head, unless the test pinned one.
			listed := maps.Clone(pull)
			listed["head"] = maps.Clone(head)
			if _, pinned := head["sha"]; !pinned {
				listed["head"].(map[string]any)["sha"] = f.heads[ref]
			}
			out = append(out, listed)
		}
		// Pages as the API does: per_page at a time, page counted from one.
		perPage, _ := strconv.Atoi(query.Get("per_page"))
		page, _ := strconv.Atoi(query.Get("page"))
		if perPage > 0 {
			start := min(max(page-1, 0)*perPage, len(out))
			out = out[start:min(start+perPage, len(out))]
		}
		writeJSON(w, http.StatusOK, out)
	case resource == "pulls" && r.Method == http.MethodPost:
		f.hidePullsUntilOpen = false
		var body struct{ Title, Body, Head, Base string }
		require.NoError(f.t, json.NewDecoder(r.Body).Decode(&body))
		for _, pull := range f.pulls {
			if pull["head"].(map[string]any)["ref"] == body.Head {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
					"message": "Validation Failed",
					"errors":  []map[string]string{{"message": "A pull request already exists for acme:" + body.Head}},
				})
				return
			}
		}
		f.nextPull++
		pull := map[string]any{
			"number": f.nextPull, "html_url": fmt.Sprintf("%s/acme/jobs/pull/%d", f.srv.URL, f.nextPull), "state": "open",
			"title": body.Title, "body": body.Body, "base": map[string]any{"ref": body.Base},
			"head": map[string]any{"ref": body.Head, "repo": map[string]any{"full_name": "acme/jobs"}},
		}
		f.pulls = append(f.pulls, pull)
		// The answer carries the branch's head as it is when the pull request opens.
		created := maps.Clone(pull)
		created["head"] = map[string]any{"ref": body.Head, "sha": f.heads[body.Head], "repo": map[string]any{"full_name": "acme/jobs"}}
		writeJSON(w, http.StatusCreated, created)
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
	}
}

func (f *fakeGitHub) repoURL() string { return f.srv.URL + "/acme/jobs.git" }

// routeFixture is a job with verified provenance, a source and a credential that
// all agree. A test breaks one of them.
type routeFixture struct {
	db     *gorm.DB
	ops    *fakeOps
	exec   *Executor
	inc    *models.Incident
	forge  *fakeGitHub
	config ProvenanceConfig
}

func (f *routeFixture) install() { f.exec.SetProvenanceRouter(NewProvenanceRouter(f.config)) }

func newRouteFixture(t *testing.T, content string) *routeFixture {
	t.Helper()
	enableAuthMode(t)
	db, store, ops, exec := newExecutorTest(t)
	inc, _, _ := seedJobIncident(t, db, store, "load", "orders-source")
	github := newFakeGitHub(t, content)
	require.NoError(t, db.Model(&models.Job{}).Where("id = ?", inc.JobID).Updates(map[string]any{
		"alias":                testAlias,
		"provenance_repo":      github.repoURL(),
		"provenance_ref":       "main",
		"provenance_commit":    syncedCommit,
		"provenance_path":      testManifest,
		"provenance_source_id": "orders-source",
	}).Error)
	fixture := &routeFixture{
		db: db, ops: ops, exec: exec, inc: inc, forge: github,
		config: ProvenanceConfig{
			GitSyncEnabled: true,
			Sources: env.GitSources{{
				URL: github.repoURL(), Ref: "main", SourceID: "orders-source", Path: "pipelines",
				Globs: []string{"*.job.yaml"},
			}},
			Credentials: env.GitWriteCredentials{{
				SourceID: "orders-source", Forge: env.GitForgeGitHub, APIBaseURL: github.srv.URL,
				Token: forgeTestToken, Namespaces: []string{models.DefaultNamespace},
			}},
			// cmd/start supplies the real check; these tests drive the route around it.
			SyncTenancy: func(context.Context, *schema.Definition, []string) (bool, error) { return true, nil },
		},
	}
	fixture.install()
	return fixture
}

func (f *routeFixture) updateJob(t *testing.T, column string, value any) {
	t.Helper()
	require.NoError(t, f.db.Model(&models.Job{}).Where("id = ?", f.inc.JobID).Update(column, value).Error)
}

func (f *routeFixture) propose(t *testing.T, definition json.RawMessage) *models.AgentAction {
	t.Helper()
	action, err := f.exec.Execute(context.Background(), ActionRequest{
		IncidentID: f.inc.ID,
		Type:       ActionTypeApplyJobdefPatch,
		Params:     ActionParams{Definition: definition},
	})
	require.NoError(t, err)
	return action
}

// approveAndExecute drives one proposal through approval.
func (f *routeFixture) approveAndExecute(t *testing.T, definition json.RawMessage) (*models.AgentAction, map[string]any) {
	t.Helper()
	action := f.propose(t, definition)
	approveAction(t, f.db, action.ID, "operator@example.com", "")
	executed, err := f.exec.ExecuteApproved(context.Background(), action.ID)
	require.NoError(t, err)
	return executed, patchResult(t, executed)
}

func patchResult(t *testing.T, action *models.AgentAction) map[string]any {
	t.Helper()
	var result map[string]any
	require.NoError(t, json.Unmarshal(action.Result, &result))
	return result
}

func requireDegraded(t *testing.T, f *routeFixture, result map[string]any, code string) {
	t.Helper()
	require.Equal(t, routeEscalate, result["route"], "result: %v", result)
	require.Equal(t, code, result["degrade"], "result: %v", result)
	require.Equal(t, false, result["applied"])
	require.NotEmpty(t, result["reason"])
	require.NotEmpty(t, result["diff"], "a degrade carries the rendered diff")
	require.Len(t, f.ops.escalate, 1)
	require.Contains(t, f.ops.escalate[0].summary, "alias", "the escalation carries the diff")
	for _, call := range f.ops.applyPatch {
		require.True(t, call.dryRun, "a job with provenance is never applied directly")
	}
}

func openPull(t *testing.T, github *fakeGitHub) map[string]any {
	t.Helper()
	require.Len(t, github.pulls, 1)
	return github.pulls[0]
}

// TestGitPRRouteOpensAPullRequest is the live path: the patch lands as a pull
// request from a deterministic branch, the file changes only in the target
// document, and the result records the URL.
func TestGitPRRouteOpensAPullRequest(t *testing.T) {
	f := newRouteFixture(t, manifestFile)

	executed, result := f.approveAndExecute(t, patchJSON(t, patchedDefinition))

	require.Equal(t, models.AgentActionStatusExecuted, executed.Status)
	require.Equal(t, routeGitPR, result["route"])
	require.Equal(t, false, result["applied"])
	branch := forge.BranchForAction(executed.ID)
	require.Equal(t, branch, result["pr_branch"])
	require.Equal(t, "acme/jobs", result["repo"])
	require.Equal(t, "main", result["base"])
	require.Equal(t, testManifest, result["path"])
	require.NotEmpty(t, result["diff"])
	require.Equal(t, "operator@example.com", result["approved_by"])
	require.Empty(t, f.ops.escalate, "a pull request is not an escalation")

	pull := openPull(t, f.forge)
	require.Equal(t, result["pr_url"], pull["html_url"])
	require.EqualValues(t, pull["number"], result["pr_number"])
	require.Equal(t, branch, pull["head"].(map[string]any)["ref"])
	require.Equal(t, "main", pull["base"].(map[string]any)["ref"])
	require.Contains(t, pull["title"], testAlias)
	require.Contains(t, pull["body"], f.inc.ID.String())

	written, ok := f.forge.get(branch, testManifest)
	require.True(t, ok)
	text := string(written)
	require.Contains(t, text, "# Owned by data-eng.", "the file header stays")
	require.Contains(t, text, "# The daily rollup.", "the document's header comment stays")
	require.Contains(t, text, testSiblingText, "a sibling job stays byte for byte")
	require.Contains(t, text, "exit 0", "the patch is in the file")
	original, _ := f.forge.get(headCommit, testManifest)
	require.Equal(t, manifestFile, string(original), "the base branch is never written")
	require.Equal(t, 1, f.forge.requestCount("PUT /repos/acme/jobs/contents/"+testManifest), "one write")

	defs, err := decodeManifest(written)
	require.NoError(t, err)
	require.Len(t, defs, 2)
	require.Equal(t, testAlias, defs[0].Metadata.Alias)
	require.Equal(t, []string{"sh", "-c", "exit 0"}, defs[0].Steps[0].Command)
	require.Equal(t, "orders-hourly", defs[1].Metadata.Alias)
}

// TestGitPRRouteUsesTheSourceURLNotTheJobs: job data only has to agree with the
// source; the repository the token writes to is the configured one, whatever
// form the job's URL takes.
func TestGitPRRouteUsesTheSourceURLNotTheJobs(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	f.updateJob(t, "provenance_repo", f.forge.srv.URL+"/acme/jobs")

	_, result := f.approveAndExecute(t, patchJSON(t, patchedDefinition))
	require.Equal(t, routeGitPR, result["route"])
}

func TestGitPRRouteResolvesATokenRefAtUse(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	f.config.Credentials[0].Token = ""
	f.config.Credentials[0].TokenRef = "secret://env/FORGE_TOKEN"
	resolver := &countingResolver{value: forgeTestToken}
	f.config.Resolver = resolver
	f.install()

	action := f.propose(t, patchJSON(t, patchedDefinition))
	require.Zero(t, resolver.calls, "a preview touches no secret")
	approveAction(t, f.db, action.ID, "operator@example.com", "")
	executed, err := f.exec.ExecuteApproved(context.Background(), action.ID)
	require.NoError(t, err)
	require.Equal(t, routeGitPR, patchResult(t, executed)["route"])
	require.Equal(t, 1, resolver.calls)
	require.Equal(t, "secret://env/FORGE_TOKEN", resolver.ref)
}

type countingResolver struct {
	value string
	err   error
	calls int
	ref   string
}

func (r *countingResolver) Resolve(_ context.Context, ref string) (string, error) {
	r.calls++
	r.ref = ref
	return r.value, r.err
}

func TestGitPRRouteDegradesWhenTheTokenCannotBeResolved(t *testing.T) {
	for name, resolver := range map[string]SecretResolver{
		"resolver error": &countingResolver{err: errors.New("vault sealed")},
		"empty secret":   &countingResolver{value: "  "},
		"no resolver":    nil,
	} {
		t.Run(name, func(t *testing.T) {
			f := newRouteFixture(t, manifestFile)
			f.config.Credentials[0].Token = ""
			f.config.Credentials[0].TokenRef = "secret://env/FORGE_TOKEN"
			f.config.Resolver = resolver
			f.install()

			_, result := f.approveAndExecute(t, patchJSON(t, patchedDefinition))
			requireDegraded(t, f, result, degradeCredentialUnavailable)
			require.NotContains(t, result["reason"], "vault sealed")
			require.Zero(t, f.forge.hits())
		})
	}
}

// TestGitPRRouteRefusesWhatProvenanceCannotProve: each case breaks one link of
// the trust chain, and the server neither contacts the forge nor applies.
func TestGitPRRouteRefusesWhatProvenanceCannotProve(t *testing.T) {
	cases := map[string]struct {
		code    string
		corrupt func(t *testing.T, f *routeFixture)
	}{
		"no credential at all": {degradeNoCredential, func(_ *testing.T, f *routeFixture) {
			f.config.Credentials = nil
		}},
		"credential for another source": {degradeNoCredential, func(_ *testing.T, f *routeFixture) {
			f.config.Credentials[0].SourceID = "other-source"
		}},
		"credential for another repo": {degradeNoCredential, func(_ *testing.T, f *routeFixture) {
			f.config.Credentials[0].SourceID = ""
			f.config.Credentials[0].Repo = f.forge.srv.URL + "/acme/other.git"
		}},
		"credential for another namespace": {degradeNoCredential, func(_ *testing.T, f *routeFixture) {
			f.config.Credentials[0].Namespaces = []string{"marketing"}
		}},
		"source sync disabled": {degradeUntrustedProvenance, func(_ *testing.T, f *routeFixture) {
			f.config.GitSyncEnabled = false
		}},
		"no configured source": {degradeUntrustedProvenance, func(_ *testing.T, f *routeFixture) {
			f.config.Sources = nil
		}},
		"asserted source id": {degradeUntrustedProvenance, func(t *testing.T, f *routeFixture) {
			f.config.Credentials[0].SourceID = ""
			f.config.Credentials[0].Repo = f.forge.repoURL()
			f.updateJob(t, "provenance_source_id", "asserted")
		}},
		"asserted repository": {degradeUntrustedProvenance, func(t *testing.T, f *routeFixture) {
			f.updateJob(t, "provenance_repo", f.forge.srv.URL+"/victim/secrets.git")
		}},
		"asserted ref": {degradeUntrustedProvenance, func(t *testing.T, f *routeFixture) {
			f.updateJob(t, "provenance_ref", "release")
		}},
		"source excludes the namespace": {degradeUntrustedProvenance, func(_ *testing.T, f *routeFixture) {
			f.config.Sources[0].Namespaces = []string{"marketing"}
		}},
		"tag source": {degradeInvalidProvenance, func(t *testing.T, f *routeFixture) {
			f.config.Sources[0].Ref = "refs/tags/v1"
			f.updateJob(t, "provenance_ref", "refs/tags/v1")
		}},
		"ref injection": {degradeInvalidProvenance, func(t *testing.T, f *routeFixture) {
			f.config.Sources[0].Ref = "main..evil"
			f.updateJob(t, "provenance_ref", "main..evil")
		}},
		"path traversal": {degradeInvalidProvenance, func(t *testing.T, f *routeFixture) {
			f.updateJob(t, "provenance_path", "pipelines/../../secrets.job.yaml")
		}},
		"path traversal on an open source": {degradeInvalidProvenance, func(t *testing.T, f *routeFixture) {
			f.config.Sources[0].Path, f.config.Sources[0].Globs = "", nil
			f.updateJob(t, "provenance_path", "pipelines/../../secrets.job.yaml")
		}},
		"absolute path": {degradeInvalidProvenance, func(t *testing.T, f *routeFixture) {
			f.updateJob(t, "provenance_path", "/etc/orders.job.yaml")
		}},
		"path outside the source": {degradeInvalidProvenance, func(t *testing.T, f *routeFixture) {
			f.updateJob(t, "provenance_path", "elsewhere/orders.job.yaml")
		}},
		"path outside the globs": {degradeInvalidProvenance, func(t *testing.T, f *routeFixture) {
			f.updateJob(t, "provenance_path", "pipelines/orders.yaml")
		}},
		"no commit": {degradeInvalidProvenance, func(t *testing.T, f *routeFixture) {
			f.updateJob(t, "provenance_commit", "")
		}},
		"malformed commit": {degradeInvalidProvenance, func(t *testing.T, f *routeFixture) {
			f.updateJob(t, "provenance_commit", "main")
		}},
		"unnamed job against a named source": {degradeUntrustedProvenance, func(t *testing.T, f *routeFixture) {
			f.config.Credentials[0].SourceID = ""
			f.config.Credentials[0].Repo = f.forge.repoURL()
			f.updateJob(t, "provenance_source_id", "")
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRouteFixture(t, manifestFile)
			tc.corrupt(t, f)
			f.install()

			executed, result := f.approveAndExecute(t, patchJSON(t, patchedDefinition))
			require.Equal(t, models.AgentActionStatusExecuted, executed.Status)
			requireDegraded(t, f, result, tc.code)
			require.Zero(t, f.forge.hits(), "an unproven job never reaches the forge")
		})
	}
}

// A tenant must not learn which repositories hold a write grant from the refusal.
func TestNoCredentialReasonDoesNotRevealAGrant(t *testing.T) {
	reason := func(mutate func(*routeFixture)) any {
		f := newRouteFixture(t, manifestFile)
		mutate(f)
		f.install()
		_, result := f.approveAndExecute(t, patchJSON(t, patchedDefinition))
		return result["reason"]
	}
	require.Equal(t,
		reason(func(f *routeFixture) { f.config.Credentials = nil }),
		reason(func(f *routeFixture) { f.config.Credentials[0].Namespaces = []string{"marketing"} }))
}

func TestUnnamedSourceMatchesOnlyAJobWithNoSourceID(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	f.config.Sources[0].SourceID = ""
	f.config.Credentials[0].SourceID = ""
	f.config.Credentials[0].Repo = f.forge.repoURL()
	f.updateJob(t, "provenance_source_id", "")
	f.install()

	_, result := f.approveAndExecute(t, patchJSON(t, patchedDefinition))
	require.Equal(t, routeGitPR, result["route"])
}

func TestGitPRRouteWithoutARouterDegrades(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	f.exec.SetProvenanceRouter(nil)

	_, result := f.approveAndExecute(t, patchJSON(t, patchedDefinition))
	requireDegraded(t, f, result, degradeNoCredential)
}

// TestDirectRouteIsUnchangedForAJobWithNoProvenance: no forge, no escalation.
func TestDirectRouteIsUnchangedForAJobWithNoProvenance(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	for _, column := range []string{"provenance_repo", "provenance_ref", "provenance_commit", "provenance_path", "provenance_source_id"} {
		f.updateJob(t, column, "")
	}
	f.ops.patchDiff = json.RawMessage(`{"alias":"orders-daily","empty":false,"diff":"the rendered diff"}`)
	_, result := f.approveAndExecute(t, patchJSON(t, patchedDefinition))
	require.Equal(t, routeDirect, result["route"])
	require.Equal(t, true, result["applied"])
	require.Equal(t, map[string]any{"alias": testAlias, "empty": false, "diff": "the rendered diff"}, result["diff"],
		"the result's `diff` stays an object whose `empty` a reader can test")
	require.Zero(t, f.forge.hits())
	require.Empty(t, f.ops.escalate)
	require.False(t, f.ops.applyPatch[len(f.ops.applyPatch)-1].dryRun)
}

// The direct route's result follows the same rule as the preview: with no diff to
// record, it records the whole proposal.
func TestDirectRouteRecordsTheProposalWhenNoDiffExists(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	for _, column := range []string{"provenance_repo", "provenance_ref", "provenance_commit", "provenance_path", "provenance_source_id"} {
		f.updateJob(t, column, "")
	}
	_, result := f.approveAndExecute(t, patchJSON(t, patchedDefinition))
	require.Equal(t, routeDirect, result["route"])
	text := resultDiff(t, result)
	require.Contains(t, text, "no diff could be produced")
	require.Contains(t, text, "exit 0")
}

// A forge outage must not become a direct server-side edit: git sync would
// revert it, and the human who approved a pull request would get something else.
func TestForgeOutageNeverFallsBackToADirectApply(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	f.forge.srv.Close()

	_, result := f.approveAndExecute(t, patchJSON(t, patchedDefinition))
	requireDegraded(t, f, result, degradeForgeError)
}

func TestForgeServerErrorDegradesBeforeAnyBranchExists(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	f.forge.failNext("GET /repos/acme/jobs/git/ref/heads/main", http.StatusBadGateway, "upstream down")

	_, result := f.approveAndExecute(t, patchJSON(t, patchedDefinition))
	requireDegraded(t, f, result, degradeForgeError)
	require.Contains(t, result["reason"], "status 502")
	require.NotContains(t, result, "pr_branch")
}

// A forge's error text can quote the file, as a pre-receive hook's output does, so
// the reason, the result and the escalation say which call failed and its status.
func TestForgeErrorTextStaysOffTheIncident(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	f.forge.failNext("PUT /repos/acme/jobs/contents/", http.StatusUnprocessableEntity, "hook declined: DB_PASSWORD=quoted-from-the-file")

	executed, result := f.approveAndExecute(t, patchJSON(t, patchedDefinition))
	requireDegraded(t, f, result, degradeManifestChanged)
	require.Contains(t, result["reason"], "status 422")
	require.NotContains(t, string(executed.Result), "quoted-from-the-file")
	require.NotContains(t, f.ops.escalate[0].summary, "quoted-from-the-file")
}

// A transport error can quote the forge too: Go's client fails on a Location it
// cannot parse before CheckRedirect runs, and quotes the whole header. The reason
// is the call and a fixed category.
func TestForgeTransportErrorTextStaysOffTheIncident(t *testing.T) {
	const marker = "finance-webhook-secret"
	f := newRouteFixture(t, manifestFile)
	f.forge.badLocation = "/" + marker + "%zz"
	logged := lastLogSequence()

	executed, result := f.approveAndExecute(t, patchJSON(t, patchedDefinition))
	requireDegraded(t, f, result, degradeForgeError)
	require.Equal(t, "forge: request: transport error", result["reason"])
	require.NotContains(t, string(executed.Result), marker)
	require.NotContains(t, f.ops.escalate[0].summary, marker)
	var rows []models.AgentAction
	require.NoError(t, f.db.Find(&rows).Error)
	for i := range rows {
		require.NotContains(t, string(rows[i].Result), marker)
	}
	entries := logsSince(logged)
	require.NotEmpty(t, entries, "the degrade is logged")
	for _, entry := range entries {
		require.NotContains(t, entry.Message+string(entry.Fields), marker)
	}
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

// The route refuses, before any forge call, a patch the sync that imports the
// merged pull request would refuse, and when that cannot be checked. The reason
// is fixed text.
func TestSyncTenancyIsCheckedBeforeAnyForgeCall(t *testing.T) {
	refused := errors.New("trigger tenancy conflict: shared with finance-hook-owner")
	for name, check := range map[string]SyncTenancyCheck{
		"refused": func(context.Context, *schema.Definition, []string) (bool, error) { return false, nil },
		"unknown": func(context.Context, *schema.Definition, []string) (bool, error) { return false, refused },
		"missing": nil,
	} {
		t.Run(name, func(t *testing.T) {
			f := newRouteFixture(t, manifestFile)
			f.config.SyncTenancy = check
			f.install()

			executed, result := f.approveAndExecute(t, patchJSON(t, patchedDefinition))
			requireDegraded(t, f, result, degradeTriggerTenancy)
			require.Zero(t, f.forge.hits(), "no forge call")
			require.NotContains(t, result, "pr_branch")
			require.NotContains(t, string(executed.Result), "finance-hook-owner")
			require.NotContains(t, f.ops.escalate[0].summary, "finance-hook-owner")
		})
	}
}

// The check gets the source's allowlist as configured, nil for an unrestricted
// source, and the definition the pull request would write.
func TestSyncTenancyGetsTheSourcesScope(t *testing.T) {
	for name, namespaces := range map[string][]string{
		"unrestricted": nil,
		"allowlisted":  {models.DefaultNamespace, "finance"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRouteFixture(t, manifestFile)
			f.config.Sources[0].Namespaces = namespaces
			var (
				calls int
				got   []string
				def   *schema.Definition
			)
			f.config.SyncTenancy = func(_ context.Context, d *schema.Definition, sourceNamespaces []string) (bool, error) {
				calls++
				got, def = sourceNamespaces, d
				return true, nil
			}
			f.install()

			_, result := f.approveAndExecute(t, patchJSON(t, patchedDefinition))
			require.Equal(t, routeGitPR, result["route"], "result: %v", result)
			require.Equal(t, 1, calls)
			require.Equal(t, namespaces, got)
			require.Equal(t, namespaces == nil, got == nil, "nil means unrestricted")
			require.Equal(t, testAlias, def.Metadata.Alias)
			require.Equal(t, []string{"sh", "-c", "exit 0"}, def.Steps[0].Command, "the patched definition")
		})
	}
}

// A refusal of the file's syntax names the rule, not the line that broke it.
func TestManifestRefusalQuotesNoLineOfTheFile(t *testing.T) {
	for name, line := range map[string]string{
		"directive":           "%quoted-from-the-file\n",
		"content on a marker": "--- quoted-from-the-file\n",
		"document end":        "... quoted-from-the-file\n",
	} {
		t.Run(name, func(t *testing.T) {
			f := newRouteFixture(t, line+manifestFile)
			executed, result := f.approveAndExecute(t, patchJSON(t, patchedDefinition))
			requireDegraded(t, f, result, degradeManifestUnsafe)
			require.NotContains(t, string(executed.Result), "quoted-from-the-file")
			require.NotContains(t, f.ops.escalate[0].summary, "quoted-from-the-file")
		})
	}
}

// TestForgeErrorCarryingTheTokenIsScrubbed reads every place a token could land.
func TestForgeErrorCarryingTheTokenIsScrubbed(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	f.forge.failNext("POST /repos/acme/jobs/pulls", http.StatusInternalServerError, "denied for "+forgeTestToken)

	executed, result := f.approveAndExecute(t, patchJSON(t, patchedDefinition))
	requireDegraded(t, f, result, degradeForgeError)
	require.Contains(t, result["reason"], "status 500")

	require.NotContains(t, string(executed.Result), forgeTestToken)
	require.NotContains(t, f.ops.escalate[0].summary, forgeTestToken)

	var audits []models.AuditLog
	require.NoError(t, f.db.Find(&audits).Error)
	require.NotEmpty(t, audits)
	for i := range audits {
		require.NotContains(t, string(audits[i].Metadata), forgeTestToken)
	}
	var rows []models.AgentAction
	require.NoError(t, f.db.Find(&rows).Error)
	for i := range rows {
		require.NotContains(t, string(rows[i].Result), forgeTestToken)
		require.NotContains(t, string(rows[i].Params), forgeTestToken)
	}
	var persisted models.Incident
	require.NoError(t, f.db.First(&persisted, "id = ?", f.inc.ID).Error)
	require.NotContains(t, string(persisted.Evidence), forgeTestToken)
	for _, pull := range f.forge.pulls {
		require.NotContains(t, pull["body"], forgeTestToken)
	}
}

func TestRedirectingForgeDegradesAndTheTargetSeesNothing(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	var stolen []string
	thief := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		stolen = append(stolen, r.Header.Get("Authorization"))
	}))
	t.Cleanup(thief.Close)
	f.forge.redirectTo = thief.URL + "/capture"

	_, result := f.approveAndExecute(t, patchJSON(t, patchedDefinition))
	requireDegraded(t, f, result, degradeForgeError)
	require.Empty(t, stolen)
}

// TestRepeatedExecutionOpensOnePullRequest: the claim makes a second dispatch
// unreachable today; the route is still idempotent on its own, so a redrive
// cannot open a second pull request.
func TestRepeatedExecutionOpensOnePullRequest(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	executed, first := f.approveAndExecute(t, patchJSON(t, patchedDefinition))
	require.Equal(t, routeGitPR, first["route"])

	require.NoError(t, f.db.Model(&models.AgentAction{}).Where("id = ?", executed.ID).
		Update("status", models.AgentActionStatusApproved).Error)
	again, err := f.exec.ExecuteApproved(context.Background(), executed.ID)
	require.NoError(t, err)

	second := patchResult(t, again)
	require.Equal(t, routeGitPR, second["route"])
	require.Equal(t, first["pr_url"], second["pr_url"])
	require.Equal(t, true, second["pr_existing"])
	require.Len(t, f.forge.pulls, 1)
	require.Equal(t, 1, f.forge.requestCount("POST /repos/acme/jobs/pulls"))
}

// A repeat is held to the proposal's commit too: the first outcome replaced the
// preview, and a sync can land between the two attempts.
func TestRetryKeepsTheProposalsBaseline(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	f.forge.failNext("POST /repos/acme/jobs/git/refs", http.StatusBadGateway, "try later")

	executed, first := f.approveAndExecute(t, patchJSON(t, patchedDefinition))
	requireDegraded(t, f, first, degradeForgeError)
	f.syncToHead(t, strings.Replace(manifestFile, "0 6 * * *", "0 7 * * *", 1))

	require.NoError(t, f.db.Model(&models.AgentAction{}).Where("id = ?", executed.ID).
		Update("status", models.AgentActionStatusApproved).Error)
	retried, err := f.exec.ExecuteApproved(context.Background(), executed.ID)
	require.NoError(t, err)
	second := patchResult(t, retried)
	require.Equal(t, routeEscalate, second["route"], "result: %v", second)
	require.Equal(t, degradeManifestChanged, second["degrade"])
	require.Zero(t, f.forge.requestCount("PUT "))
	require.Empty(t, f.forge.pulls)
}

// TestRetryConvergesAfterThePullRequestCallFails: the branch and the commit stay
// from the first attempt, the result says so, and a retry finishes without a
// second write.
func TestRetryConvergesAfterThePullRequestCallFails(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	f.forge.failNext("POST /repos/acme/jobs/pulls", http.StatusBadGateway, "try later")

	executed, first := f.approveAndExecute(t, patchJSON(t, patchedDefinition))
	requireDegraded(t, f, first, degradeForgeError)
	branch := forge.BranchForAction(executed.ID)
	require.Equal(t, branch, first["pr_branch"], "the result names what the failure left behind")
	require.Empty(t, f.forge.pulls)

	require.NoError(t, f.db.Model(&models.AgentAction{}).Where("id = ?", executed.ID).
		Update("status", models.AgentActionStatusApproved).Error)
	retried, err := f.exec.ExecuteApproved(context.Background(), executed.ID)
	require.NoError(t, err)

	second := patchResult(t, retried)
	require.Equal(t, routeGitPR, second["route"])
	require.NotContains(t, second, "pr_existing")
	require.Len(t, f.forge.pulls, 1)
	require.Equal(t, 1, f.forge.requestCount("PUT /repos/acme/jobs/contents/"), "the commit is not repeated")
}

// A branch left with a file write missing is finished by the retry.
func TestRetryConvergesAfterTheWriteFails(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	f.forge.failNext("PUT /repos/acme/jobs/contents/", http.StatusBadGateway, "try later")

	executed, first := f.approveAndExecute(t, patchJSON(t, patchedDefinition))
	requireDegraded(t, f, first, degradeForgeError)
	require.Equal(t, forge.BranchForAction(executed.ID), first["pr_branch"])

	require.NoError(t, f.db.Model(&models.AgentAction{}).Where("id = ?", executed.ID).
		Update("status", models.AgentActionStatusApproved).Error)
	retried, err := f.exec.ExecuteApproved(context.Background(), executed.ID)
	require.NoError(t, err)
	require.Equal(t, routeGitPR, patchResult(t, retried)["route"])
	written, _ := f.forge.get(forge.BranchForAction(executed.ID), testManifest)
	require.Contains(t, string(written), "exit 0")
}

// The pull request exists although the lookup did not see it (a race, or a
// lagging read): the 422 is a lookup, not a failure, and the pull request it
// finds is held to the approved change like any other.
func TestAnExistingPullRequestAtOpenIsALookup(t *testing.T) {
	for name, tc := range map[string]struct {
		base string
		// headFile pins the pull request's head to a commit holding this file;
		// empty means the branch's head, which the route has just written.
		headFile string
		code     string
	}{
		"the approved change": {base: "main"},
		"another base":        {base: "release", code: degradePullRequestMismatch},
		"another edit":        {base: "main", headFile: manifestFile + "# another edit\n", code: degradePullRequestMismatch},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRouteFixture(t, manifestFile)
			action := f.propose(t, patchJSON(t, patchedDefinition))
			approveAction(t, f.db, action.ID, "operator@example.com", "")
			branch := forge.BranchForAction(action.ID)
			head := map[string]any{"ref": branch, "repo": map[string]any{"full_name": "acme/jobs"}}
			if tc.headFile != "" {
				pinned := strings.Repeat("c", 40)
				f.forge.put(pinned, testManifest, []byte(tc.headFile))
				head["sha"] = pinned
			}
			f.forge.pulls = append(f.forge.pulls, map[string]any{
				"number": 9, "html_url": "https://forge.example/9", "state": "open",
				"base": map[string]any{"ref": tc.base}, "head": head,
			})
			// The first lookup answers with an empty list.
			f.forge.hidePullsUntilOpen = true

			executed, err := f.exec.ExecuteApproved(context.Background(), action.ID)
			require.NoError(t, err)
			result := patchResult(t, executed)
			require.Equal(t, 1, f.forge.requestCount("POST /repos/acme/jobs/pulls"), "the route tried to open one")
			if tc.code == "" {
				require.Equal(t, routeGitPR, result["route"], "result: %v", result)
				require.Equal(t, "https://forge.example/9", result["pr_url"])
				require.Equal(t, true, result["pr_existing"])
				return
			}
			requireDegraded(t, f, result, tc.code)
			requirePullRequestRefused(t, f, executed, result, branch)
		})
	}
}

// requirePullRequestRefused checks a degrade over a pull request that is not the
// approved change: fixed text naming the branch, and no trace of that pull request.
func requirePullRequestRefused(t *testing.T, f *routeFixture, executed *models.AgentAction, result map[string]any, branch string) {
	t.Helper()
	require.Equal(t, branch, result["pr_branch"])
	require.Contains(t, result["reason"], branch)
	require.NotContains(t, result, "pr_url")
	require.NotContains(t, string(executed.Result), "forge.example/9")
	require.NotContains(t, string(executed.Result), "another edit")
	require.NotContains(t, f.ops.escalate[0].summary, "another edit")
}

// TestAnExistingPullRequestIsHeldToTheApprovedChange: the preview names the
// branch, so anyone who can push to the repository can open a pull request from
// it while approval is pending. One that targets another base, or whose manifest
// is not the approved replacement, is not this action's pull request.
func TestAnExistingPullRequestIsHeldToTheApprovedChange(t *testing.T) {
	approvedFile := func(t *testing.T) []byte {
		t.Helper()
		rendered, err := renderManifest(patchJSON(t, patchedDefinition))
		require.NoError(t, err)
		spliced, err := spliceManifest([]byte(manifestFile), rendered, testAlias, models.DefaultNamespace)
		require.NoError(t, err)
		return spliced.File
	}
	for name, tc := range map[string]struct {
		base string
		file func(t *testing.T) []byte
		sha  string
		code string
	}{
		"the approved change": {base: "main", file: approvedFile},
		"another edit": {base: "main", file: func(*testing.T) []byte {
			return []byte(strings.Replace(manifestFile, `["true"]`, `["sh", "-c", "another edit"]`, 1))
		}},
		"the approved change plus a byte": {base: "main", file: func(t *testing.T) []byte {
			return append(approvedFile(t), '\n')
		}},
		"another base":       {base: "release", file: approvedFile},
		"no manifest":        {base: "main"},
		"no head commit":     {base: "main", file: approvedFile, sha: "not-a-commit"},
		"the unpatched file": {base: "main", file: func(*testing.T) []byte { return []byte(manifestFile) }},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRouteFixture(t, manifestFile)
			action := f.propose(t, patchJSON(t, patchedDefinition))
			branch := forge.BranchForAction(action.ID)
			sha := tc.sha
			if sha == "" {
				sha = strings.Repeat("d", 40)
			}
			if tc.file != nil {
				f.forge.put(sha, testManifest, tc.file(t))
			}
			f.forge.pulls = append(f.forge.pulls, map[string]any{
				"number": 9, "html_url": "https://forge.example/9", "state": "open",
				"base": map[string]any{"ref": tc.base},
				"head": map[string]any{"ref": branch, "sha": sha, "repo": map[string]any{"full_name": "acme/jobs"}},
			})
			approveAction(t, f.db, action.ID, "operator@example.com", "")

			executed, err := f.exec.ExecuteApproved(context.Background(), action.ID)
			require.NoError(t, err)
			result := patchResult(t, executed)
			require.Zero(t, f.forge.requestCount("PUT "), "the route writes nothing over a pull request")
			require.Zero(t, f.forge.requestCount("POST /repos/acme/jobs/pulls"))
			if name == "the approved change" {
				require.Equal(t, routeGitPR, result["route"], "result: %v", result)
				require.Equal(t, "https://forge.example/9", result["pr_url"])
				require.Equal(t, true, result["pr_existing"])
				return
			}
			requireDegraded(t, f, result, degradePullRequestMismatch)
			requirePullRequestRefused(t, f, executed, result, branch)
		})
	}
}

// approvedManifest is the file the route writes for patchedDefinition.
func approvedManifest(t *testing.T) []byte {
	t.Helper()
	rendered, err := renderManifest(patchJSON(t, patchedDefinition))
	require.NoError(t, err)
	spliced, err := spliceManifest([]byte(manifestFile), rendered, testAlias, models.DefaultNamespace)
	require.NoError(t, err)
	return spliced.File
}

// TestANewPullRequestIsHeldToTheApprovedChange: someone pushes to the branch
// between the route's write and its pull request, so the pull request GitHub
// opens has another head. It is not this action's change.
func TestANewPullRequestIsHeldToTheApprovedChange(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	action := f.propose(t, patchJSON(t, patchedDefinition))
	branch := forge.BranchForAction(action.ID)
	pushed := strings.Repeat("e", 40)
	f.forge.onRequest = func() {
		// Runs under the fake's lock, as the request arrives.
		if f.forge.requests[len(f.forge.requests)-1] != "POST /repos/acme/jobs/pulls" {
			return
		}
		other := []byte(manifestFile + "# another edit\n")
		f.forge.put(pushed, testManifest, other)
		f.forge.put(branch, testManifest, other)
		f.forge.heads[branch] = pushed
	}
	approveAction(t, f.db, action.ID, "operator@example.com", "")

	executed, err := f.exec.ExecuteApproved(context.Background(), action.ID)
	require.NoError(t, err)
	result := patchResult(t, executed)
	require.Equal(t, 1, f.forge.requestCount("PUT /repos/acme/jobs/contents/"), "the route wrote the approved change")
	require.Len(t, f.forge.pulls, 1, "and opened a pull request")
	requireDegraded(t, f, result, degradePullRequestMismatch)
	require.Equal(t, branch, result["pr_branch"])
	require.Contains(t, result["reason"], branch)
	require.NotContains(t, result, "pr_url")
	require.NotContains(t, string(executed.Result), "another edit")
}

// TestPullRequestStateDecidesAcceptance: the lookup asks for every state, and the
// forge reports a merged pull request and a rejected one alike as closed. An open
// one holding the approved change is accepted. A merged one is the change already
// landed only if the base holds it now: an `ours` merge marks the pull request
// merged and keeps the old manifest. A closed, unmerged one was rejected. On a
// refusal the route degrades, reopens nothing and opens no second pull request on
// the branch. Both lookups, the first and the one after a 422 at open, are held
// to this.
func TestPullRequestStateDecidesAcceptance(t *testing.T) {
	merged := "2026-11-01T12:00:00Z"
	for _, lookup := range []string{"first lookup", "after a 422"} {
		for name, tc := range map[string]struct {
			state    string
			mergedAt any
			// landed puts the approved manifest at the base's head before the
			// lookup that finds the pull request.
			landed bool
			code   string
		}{
			"open":                    {state: "open"},
			"merged and landed":       {state: "closed", mergedAt: merged, landed: true},
			"merged without the edit": {state: "closed", mergedAt: merged, code: degradePullRequestMergedWithoutChange},
			"closed unmerged":         {state: "closed", code: degradePullRequestClosed},
		} {
			t.Run(lookup+"/"+name, func(t *testing.T) {
				f := newRouteFixture(t, manifestFile)
				action := f.propose(t, patchJSON(t, patchedDefinition))
				branch := forge.BranchForAction(action.ID)
				sha := strings.Repeat("d", 40)
				approved := approvedManifest(t)
				f.forge.put(sha, testManifest, approved)
				f.forge.pulls = append(f.forge.pulls, map[string]any{
					"number": 9, "html_url": "https://forge.example/9", "state": tc.state, "merged_at": tc.mergedAt,
					"base": map[string]any{"ref": "main"},
					"head": map[string]any{"ref": branch, "sha": sha, "repo": map[string]any{"full_name": "acme/jobs"}},
				})
				land := func() {
					// The merge commit, holding the approved manifest.
					mergeCommit := strings.Repeat("f", 40)
					f.forge.put(mergeCommit, testManifest, approved)
					f.forge.heads["main"] = mergeCommit
				}
				if lookup == "after a 422" {
					f.forge.hidePullsUntilOpen = true
					if tc.landed {
						// It lands after the route checked the base, before it opens.
						f.forge.onRequest = func() {
							if f.forge.requests[len(f.forge.requests)-1] == "POST /repos/acme/jobs/pulls" {
								land()
							}
						}
					}
				} else if tc.landed {
					land()
				}
				approveAction(t, f.db, action.ID, "operator@example.com", "")

				executed, err := f.exec.ExecuteApproved(context.Background(), action.ID)
				require.NoError(t, err)
				result := patchResult(t, executed)
				require.Len(t, f.forge.pulls, 1, "no second pull request on the branch")
				require.Zero(t, f.forge.requestCount("PATCH "), "nothing is reopened")
				wantPosts := 0
				if lookup == "after a 422" {
					wantPosts = 1
				}
				require.Equal(t, wantPosts, f.forge.requestCount("POST /repos/acme/jobs/pulls"))
				if tc.code != "" {
					requireDegraded(t, f, result, tc.code)
					require.Equal(t, branch, result["pr_branch"])
					require.Contains(t, result["reason"], branch)
					require.NotContains(t, result, "pr_url")
					return
				}
				require.Equal(t, routeGitPR, result["route"], "result: %v", result)
				require.Equal(t, "https://forge.example/9", result["pr_url"])
				require.Equal(t, true, result["pr_existing"])
				require.Equal(t, tc.state, result["pr_state"])
				if tc.mergedAt != nil {
					require.True(t, tc.landed)
					require.Equal(t, true, result["pr_merged"], "a merged pull request says so")
				} else {
					require.NotContains(t, result, "pr_merged")
				}
				require.Empty(t, f.ops.escalate)
			})
		}
	}
}

// A branch with more pull requests than the lookup reads is never chosen from: the
// route escalates with fixed text, and writes and opens nothing.
func TestPullRequestLookupPastItsBoundEscalates(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	action := f.propose(t, patchJSON(t, patchedDefinition))
	branch := forge.BranchForAction(action.ID)
	for i := range 10*100 + 1 {
		f.forge.pulls = append(f.forge.pulls, map[string]any{
			"number": i + 1, "html_url": fmt.Sprintf("https://forge.example/%d", i+1), "state": "closed", "merged_at": nil,
			"base": map[string]any{"ref": "main"},
			"head": map[string]any{"ref": branch, "sha": strings.Repeat("d", 40), "repo": map[string]any{"full_name": "acme/jobs"}},
		})
	}
	approveAction(t, f.db, action.ID, "operator@example.com", "")

	executed, err := f.exec.ExecuteApproved(context.Background(), action.ID)
	require.NoError(t, err)
	result := patchResult(t, executed)
	requireDegraded(t, f, result, degradeForgeError)
	require.Equal(t, "forge: list pull requests: too many pull requests", result["reason"])
	require.Zero(t, f.forge.requestCount("PUT "))
	require.Zero(t, f.forge.requestCount("POST /repos/acme/jobs/pulls"))
	require.NotContains(t, string(executed.Result), "forge.example")
}

// An empty confined render on a pull request someone else opened must not claim
// the change is there: it degrades before any result names the pull request.
func TestEmptyRenderDoesNotVouchForAnotherPullRequest(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	f.ops.patchDiff = json.RawMessage(`{"alias":"orders-daily","empty":true}`)
	action := f.propose(t, patchJSON(t, patchedDefinition))
	branch := forge.BranchForAction(action.ID)
	sha := strings.Repeat("d", 40)
	f.forge.put(sha, testManifest, []byte(manifestFile+"# another edit\n"))
	f.forge.pulls = append(f.forge.pulls, map[string]any{
		"number": 9, "html_url": "https://forge.example/9", "state": "open",
		"base": map[string]any{"ref": "main"},
		"head": map[string]any{"ref": branch, "sha": sha, "repo": map[string]any{"full_name": "acme/jobs"}},
	})
	approveAction(t, f.db, action.ID, "operator@example.com", "")

	executed, err := f.exec.ExecuteApproved(context.Background(), action.ID)
	require.NoError(t, err)
	result := patchResult(t, executed)
	require.Equal(t, routeEscalate, result["route"], "result: %v", result)
	require.Equal(t, degradePullRequestMismatch, result["degrade"])
	require.NotContains(t, result, "pr_url")
	require.NotContains(t, result["diff"].(map[string]any)["diff"], "in the pull request")
	require.NotContains(t, string(executed.Result), "forge.example/9")
}

// sync moves the job to headCommit, as a git sync does between a proposal and its
// approval. A commit that changed the manifest changes it at the head too.
func (f *routeFixture) syncToHead(t *testing.T, manifest string) {
	t.Helper()
	f.updateJob(t, "provenance_commit", headCommit)
	f.forge.put(headCommit, testManifest, []byte(manifest))
}

func (f *routeFixture) otherRepoURL() string { return f.forge.srv.URL + "/acme/other.git" }

// TestSyncBeforeApprovalIsNotReverted is the proposal's baseline guard. Git sync
// imports a schedule change between the proposal and the approval, so the job,
// the head and the file at the job's commit all agree on it. The proposal was a
// whole document written before it. Writing that document would revert the
// change, while the preview the human approved describes the older file.
func TestSyncBeforeApprovalIsNotReverted(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	action := f.propose(t, patchJSON(t, patchedDefinition))
	f.syncToHead(t, strings.Replace(manifestFile, "0 6 * * *", "0 7 * * *", 1))
	approveAction(t, f.db, action.ID, "operator@example.com", "")

	executed, err := f.exec.ExecuteApproved(context.Background(), action.ID)
	require.NoError(t, err)
	result := patchResult(t, executed)
	requireDegraded(t, f, result, degradeManifestChanged)
	require.Contains(t, result["reason"], syncedCommit, "the reason names the commit the patch was proposed against")
	require.Contains(t, f.ops.escalate[0].summary, syncedCommit)
	require.Zero(t, f.forge.requestCount("POST /repos/acme/jobs/git/refs"), "no branch is created")
	require.Zero(t, f.forge.requestCount("PUT "))
	require.Empty(t, f.forge.pulls)
}

// A new commit that leaves this manifest alone is no reason to refuse: the file
// is the one the human reviewed, and every sync of a busy repository stamps its
// jobs with a new commit. The preview, the result and the pull request all name
// the proposal's commit.
func TestSyncThatLeavesTheManifestAloneStillOpensAPullRequest(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	action := f.propose(t, patchJSON(t, patchedDefinition))
	f.syncToHead(t, manifestFile)
	approveAction(t, f.db, action.ID, "operator@example.com", "")

	executed, err := f.exec.ExecuteApproved(context.Background(), action.ID)
	require.NoError(t, err)
	result := patchResult(t, executed)
	require.Equal(t, routeGitPR, result["route"], "result: %v", result)
	require.Equal(t, syncedCommit, result["baseline"].(map[string]any)["commit"])
	body := openPull(t, f.forge)["body"].(string)
	require.Contains(t, body, "baseline commit: "+syncedCommit)
	require.NotContains(t, body, headCommit)
}

// A proposal with no baseline cannot prove it is not stale, so it never reaches
// the forge, whatever made it miss one.
func TestProposalWithoutABaselineFailsClosed(t *testing.T) {
	for name, stored := range map[string]any{
		"no result":      nil,
		"no baseline":    `{"preview":true}`,
		"empty commit":   `{"preview":true,"baseline":{"commit":""}}`,
		"invalid commit": `{"preview":true,"baseline":{"commit":"main"}}`,
		"unreadable":     `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newRouteFixture(t, manifestFile)
			action := f.propose(t, patchJSON(t, patchedDefinition))
			require.NoError(t, f.db.Model(&models.AgentAction{}).Where("id = ?", action.ID).Update("result", stored).Error)
			approveAction(t, f.db, action.ID, "operator@example.com", "")

			executed, err := f.exec.ExecuteApproved(context.Background(), action.ID)
			require.NoError(t, err)
			requireDegraded(t, f, patchResult(t, executed), degradeNoBaseline)
			require.Zero(t, f.forge.hits(), "nothing reaches the forge")
		})
	}
}

// The baseline is the provenance the human reviewed, not only its commit.
func TestBaselineAlsoHoldsTheManifestPathAndSource(t *testing.T) {
	const moved = "pipelines/moved.job.yaml"
	for name, change := range map[string]struct {
		column string
		value  func(*routeFixture) string
	}{
		"path":       {"provenance_path", func(*routeFixture) string { return moved }},
		"source id":  {"provenance_source_id", func(*routeFixture) string { return "orders-source-2" }},
		"repository": {"provenance_repo", (*routeFixture).otherRepoURL},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRouteFixture(t, manifestFile)
			f.config.Sources = append(f.config.Sources, f.config.Sources[0], f.config.Sources[0])
			f.config.Sources[1].SourceID = "orders-source-2"
			f.config.Sources[2].URL = f.otherRepoURL()
			f.config.Credentials[0].SourceID = ""
			f.install()
			f.forge.put(syncedCommit, moved, []byte(manifestFile))
			f.forge.put(headCommit, moved, []byte(manifestFile))
			action := f.propose(t, patchJSON(t, patchedDefinition))
			f.updateJob(t, change.column, change.value(f))
			approveAction(t, f.db, action.ID, "operator@example.com", "")

			executed, err := f.exec.ExecuteApproved(context.Background(), action.ID)
			require.NoError(t, err)
			result := patchResult(t, executed)
			requireDegraded(t, f, result, degradeManifestChanged)
			require.Contains(t, result["reason"], "since this patch was proposed")
			require.Zero(t, f.forge.hits())
		})
	}
}

// TestNewerManifestIsNotOverwritten: the file moved since the job was synced, so
// the patch was reviewed against something else. Nothing is written.
func TestNewerManifestIsNotOverwritten(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	f.forge.put(headCommit, testManifest, []byte(strings.Replace(manifestFile, "0 6 * * *", "0 7 * * *", 1)))

	_, result := f.approveAndExecute(t, patchJSON(t, patchedDefinition))
	requireDegraded(t, f, result, degradeManifestChanged)
	require.Zero(t, f.forge.requestCount("POST /repos/acme/jobs/git/refs"), "no branch is created")
	require.Zero(t, f.forge.requestCount("PUT "))
	require.Empty(t, f.forge.pulls)
}

func TestManifestMissingAtTheSyncedCommitDegrades(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	delete(f.forge.files, syncedCommit+"\x00"+testManifest)

	_, result := f.approveAndExecute(t, patchJSON(t, patchedDefinition))
	requireDegraded(t, f, result, degradeManifestChanged)
}

// TestStaleBlobAtWriteDoesNotOverwrite: the file moved between the read and the
// write, so the forge refuses the blob sha and the action says so.
func TestStaleBlobAtWriteDoesNotOverwrite(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	f.forge.failNext("PUT /repos/acme/jobs/contents/", http.StatusConflict, "does not match")

	executed, result := f.approveAndExecute(t, patchJSON(t, patchedDefinition))
	requireDegraded(t, f, result, degradeManifestChanged)
	require.Equal(t, forge.BranchForAction(executed.ID), result["pr_branch"])
	require.Empty(t, f.forge.pulls)
}

// A branch holding someone else's commit is not overwritten by the retry.
func TestForeignChangeOnTheBranchIsNotOverwritten(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	action := f.propose(t, patchJSON(t, patchedDefinition))
	approveAction(t, f.db, action.ID, "operator@example.com", "")
	branch := forge.BranchForAction(action.ID)
	f.forge.heads[branch] = headCommit
	f.forge.put(branch, testManifest, []byte(manifestFile+"# edited by hand\n"))

	executed, err := f.exec.ExecuteApproved(context.Background(), action.ID)
	require.NoError(t, err)
	requireDegraded(t, f, patchResult(t, executed), degradeManifestChanged)
	require.Zero(t, f.forge.requestCount("PUT "))
}

func TestManifestProofIsRequired(t *testing.T) {
	cases := map[string]struct {
		content string
		code    string
		reason  string
	}{
		"the file does not declare the job": {
			strings.ReplaceAll(manifestFile, "alias: orders-daily", "alias: orders-weekly"), degradeManifestUnsafe, "does not declare this job",
		},
		"the file declares it in another namespace": {
			strings.Replace(manifestFile, "alias: orders-daily", "alias: orders-daily\n  namespace: finance", 1), degradeManifestUnsafe, "another namespace",
		},
		"the file declares it twice": {
			strings.ReplaceAll(manifestFile, "alias: orders-hourly  # keep this comment", "alias: orders-daily"), degradeManifestUnsafe, "more than once",
		},
		"the file is not a manifest": {"not: [a, job\n", degradeManifestUnsafe, "does not parse"},
		"the file uses a directive":  {"%YAML 1.2\n" + manifestFile, degradeManifestUnsafe, "unsupported document syntax"},
		"the file ends a document":   {manifestFile + "...\n", degradeManifestUnsafe, "unsupported document syntax"},
		"a marker carries content":   {manifestFile + "--- {apiVersion: v1}\n", degradeManifestUnsafe, "unsupported document syntax"},
		"the patch changes nothing":  {patchedDefinition, degradeManifestUnchanged, "already declares"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRouteFixture(t, tc.content)
			_, result := f.approveAndExecute(t, patchJSON(t, patchedDefinition))
			requireDegraded(t, f, result, tc.code)
			require.Contains(t, result["reason"], tc.reason)
			require.Zero(t, f.forge.requestCount("POST /repos/acme/jobs/git/refs"))
			require.Zero(t, f.forge.requestCount("PUT "))
		})
	}
}

// TestFileRemediationIsAlsoProtected: the job's stored block may have drifted
// from the file (a --force apply). The policy in the repository is what governs
// after the merge, so a patch may not change it there either.
func TestFileRemediationIsAlsoProtected(t *testing.T) {
	governed := strings.Replace(manifestFile, "alias: orders-daily", `alias: orders-daily
  remediation:
    profile: quoted-from-the-file
    classes: [unknown]`, 1)
	f := newRouteFixture(t, governed)
	action := f.propose(t, patchJSON(t, patchedDefinition))
	approveAction(t, f.db, action.ID, "operator@example.com", "")

	executed, err := f.exec.ExecuteApproved(context.Background(), action.ID)
	require.ErrorIs(t, err, ErrPatchAltersRemediation)
	require.Equal(t, models.AgentActionStatusFailed, executed.Status)
	require.NotContains(t, err.Error(), "quoted-from-the-file", "the refusal quotes no block from the file")
	require.NotContains(t, string(executed.Result), "quoted-from-the-file")
	require.Empty(t, f.ops.escalate, "a policy edit is refused, not escalated")
	require.Zero(t, f.forge.requestCount("PUT "))
	require.Empty(t, f.forge.pulls)
}

// The stored policy check runs ahead of the router, on the Git-PR route as on
// the others.
func TestStoredRemediationRefusalPrecedesTheRoute(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	require.NoError(t, f.db.Model(&models.Job{}).Where("id = ?", f.inc.JobID).
		Update("remediation", `{"profile":"triage-only","classes":["unknown"]}`).Error)
	action := f.propose(t, patchJSON(t, patchedDefinition))
	approveAction(t, f.db, action.ID, "operator@example.com", "")

	_, err := f.exec.ExecuteApproved(context.Background(), action.ID)
	require.ErrorIs(t, err, ErrPatchAltersRemediation)
	require.Zero(t, f.forge.hits())
}

func TestGitPRRouteIsRefusedWithoutAnAuthMode(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	action := f.propose(t, patchJSON(t, patchedDefinition))
	approveAction(t, f.db, action.ID, "operator@example.com", "")

	t.Setenv("CAESIUM_AUTH_MODE", "none")
	require.NoError(t, env.Process())
	executed, err := f.exec.ExecuteApproved(context.Background(), action.ID)
	require.ErrorIs(t, err, ErrAuthModeNone)
	require.Equal(t, models.AgentActionStatusFailed, executed.Status)
	require.Zero(t, f.forge.hits())
}

func TestJobMovedOutIsRefusedBeforeTheRoute(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	action := f.propose(t, patchJSON(t, patchedDefinition))
	approveAction(t, f.db, action.ID, "operator@example.com", "")
	f.updateJob(t, "namespace", "finance")

	_, err := f.exec.ExecuteApproved(context.Background(), action.ID)
	require.ErrorIs(t, err, ErrNamespaceMoved)
	require.Zero(t, f.forge.hits())
}

// The approving request can go away part-way through the forge calls. The route
// stops, the human is told, and the row still reaches a terminal state.
func TestCancelledRouteEndsTruthfully(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	action := f.propose(t, patchJSON(t, patchedDefinition))
	approveAction(t, f.db, action.ID, "operator@example.com", "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.forge.onRequest = cancel

	executed, err := f.exec.ExecuteApproved(ctx, action.ID)
	require.NoError(t, err)
	requireDegraded(t, f, patchResult(t, executed), degradeForgeError)

	var row models.AgentAction
	require.NoError(t, f.db.First(&row, "id = ?", action.ID).Error)
	require.Equal(t, models.AgentActionStatusExecuted, row.Status)
}

// TestPreviewShowsTheApproverTheRoute: the proposal records what approving will
// do, without touching the forge or a secret. The Console's approval card renders
// the string `diff`.
func TestPreviewShowsTheApproverTheRoute(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	action := f.propose(t, patchJSON(t, patchedDefinition))
	require.Equal(t, models.AgentActionStatusProposed, action.Status)

	var row models.AgentAction
	require.NoError(t, f.db.First(&row, "id = ?", action.ID).Error)
	preview := patchResult(t, &row)
	require.Equal(t, true, preview["preview"])
	require.Equal(t, routeGitPR, preview["route"])
	require.Equal(t, "acme/jobs", preview["repo"])
	require.Equal(t, "main", preview["base"])
	require.Equal(t, testManifest, preview["path"])
	require.Equal(t, forge.BranchForAction(action.ID), preview["pr_branch"])
	require.Contains(t, preview["diff"], "pull request against acme/jobs")
	require.Contains(t, preview["diff"], "changed since commit "+syncedCommit)
	require.Equal(t, map[string]any{
		"source_id": "orders-source", "repo": repoIdentity(f.forge.repoURL()), "ref": "main",
		"commit": syncedCommit, "path": testManifest,
	}, preview["baseline"])
	require.Zero(t, f.forge.hits(), "a preview makes no forge call")
	require.Len(t, f.ops.applyPatch, 1)
	require.True(t, f.ops.applyPatch[0].dryRun)
}

func TestPreviewShowsADegrade(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	f.config.Credentials = nil
	f.install()
	action := f.propose(t, patchJSON(t, patchedDefinition))

	preview := patchResult(t, action)
	require.Equal(t, routeEscalate, preview["route"])
	require.Equal(t, degradeNoCredential, preview["degrade"])
	require.Contains(t, preview["diff"], "escalate")
	require.NotContains(t, preview, "pr_branch")
}

// A degraded preview records the baseline too: a credential added before the
// approval sends the patch to the forge, held to the commit the human reviewed.
func TestDegradedPreviewStillRecordsTheBaseline(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	credentials := f.config.Credentials
	f.config.Credentials = nil
	f.install()
	action := f.propose(t, patchJSON(t, patchedDefinition))
	f.config.Credentials = credentials
	f.install()
	f.syncToHead(t, strings.Replace(manifestFile, "0 6 * * *", "0 7 * * *", 1))
	approveAction(t, f.db, action.ID, "operator@example.com", "")

	executed, err := f.exec.ExecuteApproved(context.Background(), action.ID)
	require.NoError(t, err)
	requireDegraded(t, f, patchResult(t, executed), degradeManifestChanged)
}

func TestPreviewShowsTheDirectRoute(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	for _, column := range []string{"provenance_repo", "provenance_ref", "provenance_commit", "provenance_path", "provenance_source_id"} {
		f.updateJob(t, column, "")
	}
	require.Equal(t, routeDirect, patchResult(t, f.propose(t, patchJSON(t, patchedDefinition)))["route"])
}

// A proposal naming another job gets no preview: its provenance is not the
// incident's to show, and the boundary refuses it at dispatch anyway.
func TestPreviewRespectsTheIncidentBoundary(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	other, _, _ := seedJobIncident(t, f.db, NewStore(f.db), "other", "other-source")
	action, err := f.exec.Execute(context.Background(), ActionRequest{
		IncidentID: f.inc.ID,
		Type:       ActionTypeApplyJobdefPatch,
		Params:     ActionParams{Definition: patchJSON(t, patchedDefinition), JobID: &other.JobID},
	})
	require.NoError(t, err)
	require.Nil(t, action.Result)
	require.Empty(t, f.ops.applyPatch)
}

func TestPreviewRecordsNothingForARefusedPatch(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	require.NoError(t, f.db.Model(&models.Job{}).Where("id = ?", f.inc.JobID).
		Update("remediation", `{"profile":"triage-only","classes":["unknown"]}`).Error)
	require.Nil(t, f.propose(t, patchJSON(t, patchedDefinition)).Result)
	require.Empty(t, f.ops.applyPatch)
}

// TestPullRequestBodyCarriesTheEvidence is the plan's body contract: the incident
// id, the violated assertion, the observed/bound/baseline triple and the diff.
func TestPullRequestBodyCarriesTheEvidence(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	require.NoError(t, f.db.Model(&models.Incident{}).Where("id = ?", f.inc.ID).Updates(map[string]any{
		"class": string(ClassDataQualityHold),
		"evidence": `{"dataset_namespace":"warehouse","dataset":"orders","reason":"rowCount breach",
			"violations":[{"metric":"rowCount","kind":"delta_from_baseline","observed":3,"bound":100,"baseline":{"median":1000}}]}`,
	}).Error)

	_, result := f.approveAndExecute(t, patchJSON(t, patchedDefinition))
	require.Equal(t, routeGitPR, result["route"])
	body := openPull(t, f.forge)["body"].(string)
	require.Contains(t, body, f.inc.ID.String())
	require.Contains(t, body, "rowCount")
	require.Contains(t, body, `"observed": 3`)
	require.Contains(t, body, `"bound": 100`)
	require.Contains(t, body, `"median": 1000`)
	require.Contains(t, body, "alias", "the rendered diff")
}

// richManifest holds the fields the reduced JobSpec leaves out, in one document.
const richManifest = `apiVersion: v1
kind: Job
metadata:
  alias: orders-daily
trigger:
  type: cron
  configuration:
    cron: "0 6 * * *"
steps:
  - name: load
    image: busybox:1.36.1
    command: ["true"]
    env:
      REGION: eu
      STORE: secret://env/STORE_URL
    mounts:
      - type: bind
        source: /data
        target: /in
    next: [publish]
  - name: publish
    image: busybox:1.36.1
    command: ["true"]
`

// richEdits are edits only a comparison of the whole documents can see, alone and
// with one the reduced JobSpec does see.
var richEdits = []struct {
	name     string
	edit     func(string) string
	expected []string
}{
	{
		name:     "env only",
		edit:     func(m string) string { return strings.Replace(m, "REGION: eu", "REGION: us", 1) },
		expected: []string{"-", "REGION: eu", "+", "REGION: us"},
	},
	{
		name:     "mount only",
		edit:     func(m string) string { return strings.Replace(m, "target: /in", "target: /elsewhere", 1) },
		expected: []string{"target: /in", "target: /elsewhere"},
	},
	{
		name: "mixed",
		edit: func(m string) string {
			edited := strings.Replace(m, `command: ["true"]`, `command: ["sh", "-c", "exit 0"]`, 1)
			return strings.Replace(edited, "target: /in", "target: /elsewhere", 1)
		},
		expected: []string{"exit 0", "target: /in", "target: /elsewhere"},
	},
	{
		name:     "edge",
		edit:     func(m string) string { return strings.Replace(m, "    next: [publish]\n", "", 1) },
		expected: []string{"next:", "publish"},
	},
}

// resultDiff is the text of an executed result's `diff`.
func resultDiff(t *testing.T, result map[string]any) string {
	t.Helper()
	diff, ok := result["diff"].(map[string]any)
	require.True(t, ok, "result: %v", result)
	return diff["diff"].(string)
}

// diffSection cuts the description's rendered diff out of the rest of the body.
func diffSection(t *testing.T, body string) string {
	t.Helper()
	_, section, found := strings.Cut(body, "### Rendered diff")
	require.True(t, found, "body: %s", body)
	return section
}

// TestPullRequestBodyShowsEveryEdit: the description quotes the change the commit
// makes, read from the file and its replacement, so an edit outside the reduced
// JobSpec cannot read as "no changes".
func TestPullRequestBodyShowsEveryEdit(t *testing.T) {
	for _, tc := range richEdits {
		t.Run(tc.name, func(t *testing.T) {
			f := newRouteFixture(t, richManifest)
			// The file is the description's source, so a render that reports no change
			// must not suppress it.
			f.ops.patchDiff = json.RawMessage(`{"alias":"orders-daily","empty":true}`)

			_, result := f.approveAndExecute(t, patchJSON(t, tc.edit(richManifest)))
			require.Equal(t, routeGitPR, result["route"], "result: %v", result)
			body := diffSection(t, openPull(t, f.forge)["body"].(string))
			require.NotContains(t, body, "no changes")
			require.Contains(t, body, "--- current\n+++ proposed\n@@ ")
			for _, expected := range tc.expected {
				require.Contains(t, body, expected)
			}
		})
	}
}

// The diff reads the manifest's own document, so a difference between the file
// and the server's copy shows too.
func TestPullRequestBodyComparesTheFileNotTheServersCopy(t *testing.T) {
	f := newRouteFixture(t, strings.Replace(richManifest, "REGION: eu", "REGION: only-in-the-file", 1))
	f.ops.patchDiff = json.RawMessage(`{"alias":"orders-daily","empty":false,"diff":"from the server's copy"}`)

	_, result := f.approveAndExecute(t, patchJSON(t, richManifest))
	require.Equal(t, routeGitPR, result["route"], "result: %v", result)
	body := openPull(t, f.forge)["body"].(string)
	require.Contains(t, body, "-        REGION: only-in-the-file")
	require.NotContains(t, body, "from the server's copy")
}

// The result carries the confined render, never the file's own text: the file can
// hold what the incident's readers may not read. cmd/start drives the real render.
func TestPullRequestResultCarriesNothingFromTheFile(t *testing.T) {
	f := newRouteFixture(t, strings.Replace(richManifest, "REGION: eu", "REGION: only-in-the-file", 1))
	f.ops.patchDiff = json.RawMessage(`{"alias":"orders-daily","empty":false,"diff":"from the server's copy"}`)

	executed, result := f.approveAndExecute(t, patchJSON(t, richManifest))
	require.Equal(t, routeGitPR, result["route"], "result: %v", result)
	require.Equal(t, map[string]any{"alias": testAlias, "empty": false, "diff": "from the server's copy"}, result["diff"])
	require.NotContains(t, string(executed.Result), "only-in-the-file")
}

// A pull request is a change even when the server's copy shows none, so its result
// does not say "no changes": it says where the change is.
func TestEmptyRenderOnASuccessfulRouteNamesThePullRequest(t *testing.T) {
	f := newRouteFixture(t, richManifest)
	f.ops.patchDiff = json.RawMessage(`{"alias":"orders-daily","empty":true}`)

	_, result := f.approveAndExecute(t, patchJSON(t, strings.Replace(richManifest, "REGION: eu", "REGION: us", 1)))
	require.Equal(t, routeGitPR, result["route"], "result: %v", result)
	diff := result["diff"].(map[string]any)
	require.Equal(t, false, diff["empty"])
	require.NotContains(t, diff["diff"], "no changes")
	require.Contains(t, diff["diff"], "in the pull request")
	require.Contains(t, diff["diff"], result["pr_url"])
}

// So does the repeat that finds the pull request an earlier run opened.
func TestEmptyRenderOnAnExistingPullRequestNamesIt(t *testing.T) {
	f := newRouteFixture(t, richManifest)
	f.ops.patchDiff = json.RawMessage(`{"alias":"orders-daily","empty":true}`)
	executed, first := f.approveAndExecute(t, patchJSON(t, strings.Replace(richManifest, "REGION: eu", "REGION: us", 1)))
	require.Equal(t, routeGitPR, first["route"])

	require.NoError(t, f.db.Model(&models.AgentAction{}).Where("id = ?", executed.ID).
		Update("status", models.AgentActionStatusApproved).Error)
	again, err := f.exec.ExecuteApproved(context.Background(), executed.ID)
	require.NoError(t, err)
	second := patchResult(t, again)
	require.Equal(t, true, second["pr_existing"])
	require.Contains(t, second["diff"].(map[string]any)["diff"], first["pr_url"])
}

// The preview of a job a file owns cannot read the file, so a match with the
// server's copy is not a claim about the file.
func TestPreviewOfAGitJobDoesNotClaimTheFileMatches(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	f.ops.patchDiff = json.RawMessage(`{"alias":"orders-daily","empty":true}`)
	preview := patchResult(t, f.propose(t, patchJSON(t, patchedDefinition)))
	require.Contains(t, preview["diff"], "no changes to the server's copy")
	require.NotContains(t, preview["diff"], "identical")
}

// A source URL can hold inline credentials, and a baseline is stored on the action.
func TestBaselineHoldsNoURLCredentials(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	withCredentials := strings.Replace(f.forge.repoURL(), "http://", "http://reader:inline-url-secret@", 1)
	f.updateJob(t, "provenance_repo", withCredentials)
	f.config.Sources[0].URL = withCredentials
	f.install()

	action := f.propose(t, patchJSON(t, patchedDefinition))
	require.NotContains(t, string(action.Result), "inline-url-secret")
	require.Equal(t, routeGitPR, patchResult(t, action)["route"])
	approveAction(t, f.db, action.ID, "operator@example.com", "")
	executed, err := f.exec.ExecuteApproved(context.Background(), action.ID)
	require.NoError(t, err)
	require.NotContains(t, string(executed.Result), "inline-url-secret")
	require.Equal(t, routeGitPR, patchResult(t, executed)["route"], "the identity matches across the URL's forms")
}

// The description carries a credential value only as a mask, and a reference as
// itself. A multi-line value is the hard case: no line of it may appear, even
// when the changed line sits too far below its key for a hunk to show the key.
func TestPullRequestBodyMasksCredentialValues(t *testing.T) {
	const multiline = "      DB_PASSWORD: |\n" +
		"        short-line-one\n        short-line-two\n        short-line-three\n" +
		"        short-line-four\n        short-line-five\n        short-line-six\n"
	file := strings.Replace(richManifest, "      REGION: eu\n", multiline+"      REGION: eu\n", 1)
	proposed := strings.Replace(file, "short-line-five", "short-line-changed", 1)
	f := newRouteFixture(t, file)

	_, result := f.approveAndExecute(t, patchJSON(t, proposed))
	require.Equal(t, routeGitPR, result["route"], "result: %v", result)
	body := openPull(t, f.forge)["body"].(string)
	require.NotContains(t, body, "short-line")
	require.Regexp(t, `(?m)^-\s+DB_PASSWORD: .\[REDACTED\] [0-9a-f]{8}`, body, "the masked line shows the change")
	require.Regexp(t, `(?m)^\+\s+DB_PASSWORD: .\[REDACTED\] [0-9a-f]{8}`, body)
	require.Contains(t, body, "STORE: secret://env/STORE_URL", "a reference names a secret without holding it")
	require.Contains(t, resultDiff(t, result), "short-line-changed", "the result carries the render's text")
	require.NotContains(t, resultDiff(t, result), "short-line-five", "and nothing the file alone holds")
}

func TestPatchDiffTextOnlySaysNoChangesWhenTheRenderDoes(t *testing.T) {
	definition := patchJSON(t, patchedDefinition)
	for name, rendered := range map[string]string{
		"no diff":           `{"alias":"x","empty":false}`,
		"reduced diff only": `{"alias":"x","empty":false,"updates":[{"alias":"x","diff":"part of it"}]}`,
		"not json":          `oops`,
		"nothing":           ``,
		"wrong type":        `{"empty":"yes"}`,
	} {
		t.Run(name, func(t *testing.T) {
			text := patchDiffText(json.RawMessage(rendered), definition)
			require.NotContains(t, text, "no changes")
			require.Contains(t, text, "no diff could be produced")
			require.Contains(t, text, "exit 0", "the whole proposal shows instead")
		})
	}
	require.Equal(t, noPatchChanges, patchDiffText(json.RawMessage(`{"empty":true}`), definition))
	require.Equal(t, "the diff", patchDiffText(json.RawMessage(`{"empty":false,"diff":"the diff"}`), definition))
	require.Equal(t, "the diff", patchDiffText(json.RawMessage(`{"empty":true,"diff":"the diff"}`), definition), "text beats the flag")

	require.True(t, newPatchDiff("x", json.RawMessage(`{"empty":true}`), definition).Empty)
	require.False(t, newPatchDiff("x", json.RawMessage(`{"empty":false,"diff":"d"}`), definition).Empty)
	require.False(t, newPatchDiff("x", json.RawMessage(`{"empty":false}`), definition).Empty, "no diff is not no change")
}

// With no diff to show, the approver reads the whole proposal, not a claim.
func TestPreviewShowsTheProposalWhenNoDiffExists(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	preview := patchResult(t, f.propose(t, patchJSON(t, patchedDefinition)))
	require.Contains(t, preview["diff"], "no diff could be produced")
	require.Contains(t, preview["diff"], "exit 0")
	require.NotContains(t, preview["diff"], "no changes")
}

// Every surface that carries a diff bounds it and says it did.
func TestDiffIsBoundedOnEverySurface(t *testing.T) {
	huge, err := json.Marshal(strings.Repeat("+ line\n", maxDiffBytes))
	require.NoError(t, err)
	const trailer = "truncated; the incident holds the full text"

	f := newRouteFixture(t, manifestFile)
	f.config.Credentials = nil
	f.install()
	f.ops.patchDiff = json.RawMessage(`{"alias":"orders-daily","empty":false,"diff":` + string(huge) + `,"outbound_diff":` + string(huge) + `}`)
	action := f.propose(t, patchJSON(t, patchedDefinition))
	preview := patchResult(t, action)["diff"].(string)
	require.Less(t, len(preview), maxDiffBytes+len(trailer)+8)
	require.True(t, strings.HasSuffix(preview, trailer))

	approveAction(t, f.db, action.ID, "operator@example.com", "")
	executed, err := f.exec.ExecuteApproved(context.Background(), action.ID)
	require.NoError(t, err)
	result := resultDiff(t, patchResult(t, executed))
	require.Less(t, len(result), maxDiffBytes+len(trailer)+8)
	require.True(t, strings.HasSuffix(result, trailer))
	require.Len(t, f.ops.escalate, 1)
	require.Less(t, len(f.ops.escalate[0].summary), maxDiffBytes+1024)
	require.True(t, strings.HasSuffix(f.ops.escalate[0].summary, trailer))
}

// The escalation leaves the incident, so it carries the render's masked form, and
// the action row keeps the complete one.
func TestEscalationCarriesTheMaskedDiff(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	f.config.Credentials = nil
	f.install()
	f.ops.patchDiff = json.RawMessage(`{"alias":"orders-daily","empty":false,"diff":"+ complete-text","outbound_diff":"+ masked-text"}`)

	_, result := f.approveAndExecute(t, patchJSON(t, patchedDefinition))
	require.Equal(t, routeEscalate, result["route"])
	require.Contains(t, f.ops.escalate[0].summary, "masked-text")
	require.NotContains(t, f.ops.escalate[0].summary, "complete-text")
	require.Equal(t, "+ complete-text", resultDiff(t, result))
	require.NotContains(t, string(mustJSON(t, result)), "masked-text", "the masked form stays off the result")
}

// With no masked diff from the render, the escalation falls back to the proposal,
// masked: never to the complete text.
func TestEscalationWithoutAMaskedDiffMasksTheProposal(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	f.config.Credentials = nil
	f.install()
	f.ops.patchDiff = json.RawMessage(`{"alias":"orders-daily","empty":false}`)
	proposed := strings.Replace(richManifest, "REGION: eu", "DB_PASSWORD: |\n        short-line-one\n        short-line-two", 1)

	_, result := f.approveAndExecute(t, patchJSON(t, proposed))
	require.Equal(t, routeEscalate, result["route"])
	summary := f.ops.escalate[0].summary
	require.Contains(t, summary, "no diff could be produced")
	require.NotContains(t, summary, "short-line")
	require.Contains(t, summary, "STORE: secret://env/STORE_URL")
	require.Contains(t, resultDiff(t, result), "short-line-one")
}

func TestOutboundTextWithholdsAProposalItCannotMask(t *testing.T) {
	require.Equal(t, "no diff could be produced; the proposed definition is on the incident's action",
		outboundProposalText([]byte("password: [unterminated")))
	text := outboundDocumentDiff([]byte("a: [unterminated"), []byte("DB_PASSWORD: short-value\n"))
	require.Contains(t, text, "no diff could be produced")
	require.NotContains(t, text, "short-value")
	require.Equal(t, noPatchChanges, outboundDocumentDiff([]byte("a: 1"), []byte("a: 1")))
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return encoded
}

var fenceLine = regexp.MustCompile("^`{3,}$")

// outsideFences returns the body's text that a forge would render as Markdown.
func outsideFences(body string) string {
	var rendered []string
	fence := ""
	for line := range strings.SplitSeq(body, "\n") {
		switch {
		case fence == "" && fenceLine.MatchString(line):
			fence = line
		case fence != "" && line == fence:
			fence = ""
		case fence == "":
			rendered = append(rendered, line)
		}
	}
	return strings.Join(rendered, "\n")
}

// TestPullRequestBodyIsSafeForUntrustedValues: job and data values may mention a
// team, link an issue, or close the fence. None of that may render.
func TestPullRequestBodyIsSafeForUntrustedValues(t *testing.T) {
	f := newRouteFixture(t, manifestFile)
	hostile := "@acme/owners\n```\n# Injected #1 [x](http://evil.example) ![i](http://evil.example/i.png)"
	encoded, err := json.Marshal(hostile)
	require.NoError(t, err)
	require.NoError(t, f.db.Model(&models.Incident{}).Where("id = ?", f.inc.ID).Updates(map[string]any{
		"class":    string(ClassDataQualityHold),
		"evidence": `{"dataset":` + string(encoded) + `,"reason":` + string(encoded) + `,"violations":[{"metric":` + string(encoded) + `}]}`,
	}).Error)
	definition, err := schema.Parse([]byte(patchedDefinition))
	require.NoError(t, err)
	definition.Steps[0].Command = []string{"sh", "-c", "echo keep; " + hostile}
	patch, err := json.Marshal(definition)
	require.NoError(t, err)

	_, result := f.approveAndExecute(t, patch)
	require.Equal(t, routeGitPR, result["route"])

	pull := openPull(t, f.forge)
	body := pull["body"].(string)
	require.Contains(t, body, "@acme/owners", "the value is shown")
	require.Contains(t, body, "echo keep", "the value reaches the diff")
	rendered := outsideFences(body)
	for _, marker := range []string{"@", "Injected", "http://", "#1", "![", "evil"} {
		require.NotContains(t, rendered, marker)
	}
	require.NotContains(t, pull["title"], "@")
}

func TestPatchTitleCannotMentionOrLink(t *testing.T) {
	require.Equal(t, "Patch job acme-owners-1 for incident 00000000", patchTitle("@acme/owners #1", uuid.Nil))
	require.NotContains(t, patchCommitMessage("@acme [x](y)", uuid.Nil, uuid.Nil), "@")
}

func TestFencedTextCannotCloseItsFence(t *testing.T) {
	block := fenced("a ```` b")
	require.True(t, strings.HasPrefix(block, "`````\n"))
	require.True(t, strings.HasSuffix(block, "\n`````"))
}

func TestBoundedSectionTruncatesAndScrubs(t *testing.T) {
	section := boundedSection(strings.Repeat("é", 100), 51)
	require.True(t, strings.HasSuffix(section, "truncated; the incident holds the full text"))
	require.NotContains(t, section, "�")
	require.Contains(t, boundedSection("key "+forgeTestToken, 1000), Redacted)
}

func TestSpliceKeepsSiblingBytes(t *testing.T) {
	rendered, err := renderManifest(patchJSON(t, patchedDefinition))
	require.NoError(t, err)
	spliced, err := spliceManifest([]byte(manifestFile), rendered, testAlias, models.DefaultNamespace)
	require.NoError(t, err)

	sibling := manifestFile[strings.LastIndex(manifestFile, "---\n"):]
	require.True(t, strings.HasSuffix(string(spliced.File), sibling))
	require.True(t, strings.HasPrefix(string(spliced.File), "# Owned by data-eng.\n---\n# The daily rollup.\n"))
	require.Contains(t, string(spliced.Original), "alias: orders-daily")
	require.Contains(t, string(spliced.Original), `command: ["true"]`, "the job's document as the file held it")
	require.NotContains(t, string(spliced.Original), "orders-hourly")
}

func TestSplicePatchesASingleDocumentFile(t *testing.T) {
	file := strings.TrimPrefix(manifestFile[:strings.LastIndex(manifestFile, "---\n")], "# Owned by data-eng.\n---\n")
	rendered, err := renderManifest(patchJSON(t, patchedDefinition))
	require.NoError(t, err)
	spliced, err := spliceManifest([]byte(file), rendered, testAlias, models.DefaultNamespace)
	require.NoError(t, err)
	defs, err := decodeManifest(spliced.File)
	require.NoError(t, err)
	require.Len(t, defs, 1)
	require.Equal(t, []string{"sh", "-c", "exit 0"}, defs[0].Steps[0].Command)
}

func TestSpliceHandlesCRLFMarkers(t *testing.T) {
	file := strings.ReplaceAll(manifestFile, "\n", "\r\n")
	rendered, err := renderManifest(patchJSON(t, patchedDefinition))
	require.NoError(t, err)
	spliced, err := spliceManifest([]byte(file), rendered, testAlias, models.DefaultNamespace)
	require.NoError(t, err)
	require.Contains(t, string(spliced.File), "alias: orders-hourly  # keep this comment\r\n")
}

// TestSpliceAcceptsANamespaceWrittenOutOrDefaulted: a manifest that omits the
// namespace declares `default`.
func TestSpliceAcceptsANamespaceWrittenOutOrDefaulted(t *testing.T) {
	explicit := strings.Replace(manifestFile, "alias: orders-daily", "alias: orders-daily\n  namespace: default", 1)
	rendered, err := renderManifest(patchJSON(t, patchedDefinition))
	require.NoError(t, err)
	_, err = spliceManifest([]byte(explicit), rendered, testAlias, models.DefaultNamespace)
	require.NoError(t, err)
}

func TestRenderManifestRefusesAnInvalidDefinition(t *testing.T) {
	_, err := renderManifest(json.RawMessage(`{"apiVersion":"v1"}`))
	require.Error(t, err)
	_, err = renderManifest(json.RawMessage(`[`))
	require.Error(t, err)
}

// TestRenderedExamplesRoundTrip runs every shipped example through JSON (the
// proposal shape), the renderer and the real parser.
func TestRenderedExamplesRoundTrip(t *testing.T) {
	paths, err := filepath.Glob("../../docs/examples/*.job.yaml")
	require.NoError(t, err)
	require.NotEmpty(t, paths)
	for _, file := range paths {
		raw, err := os.ReadFile(file)
		require.NoError(t, err)
		defs, err := decodeManifest(raw)
		require.NoError(t, err, file)
		for i := range defs {
			encoded, err := json.Marshal(&defs[i])
			require.NoError(t, err, file)
			rendered, err := renderManifest(encoded)
			require.NoError(t, err, file)
			require.True(t, sameJob(&defs[i], rendered.Definition), file)
		}
	}
}

func TestSourceCovers(t *testing.T) {
	cases := []struct {
		source env.GitSourceConfig
		path   string
		want   bool
	}{
		{env.GitSourceConfig{}, "a.yaml", true},
		{env.GitSourceConfig{}, "a.txt", false},
		{env.GitSourceConfig{Path: "/jobs/"}, "jobs/a.yml", true},
		{env.GitSourceConfig{Path: "jobs"}, "jobsuffix/a.yaml", false},
		{env.GitSourceConfig{Path: "."}, "a.yaml", true},
		{env.GitSourceConfig{Globs: []string{"**/*.job.yaml"}}, "x/y/a.job.yaml", true},
		{env.GitSourceConfig{Path: "jobs", Globs: []string{"*.job.yaml"}}, "jobs/a.job.yaml", true},
		{env.GitSourceConfig{Path: "jobs", Globs: []string{"*.job.yaml"}}, "jobs/x/a.job.yaml", false},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, sourceCovers(&tc.source, tc.path), "%+v %s", tc.source, tc.path)
	}
}

func TestSyncBranch(t *testing.T) {
	branch, err := syncBranch("")
	require.NoError(t, err)
	require.Equal(t, "main", branch, "git sync reads an empty ref as main")
	branch, err = syncBranch("refs/heads/release/1")
	require.NoError(t, err)
	require.Equal(t, "release/1", branch)
	_, err = syncBranch("refs/tags/v1")
	require.Error(t, err)
}

func TestSameRepoIgnoresTheURLForm(t *testing.T) {
	require.True(t, sameRepo("https://user:pw@github.com/a/b.git", "git@github.com:a/b.git"))
	require.False(t, sameRepo("https://github.com/a/b", "https://github.com/a/c"))
	require.False(t, sameRepo("https://github.com/a/b", "https://gitlab.com/a/b"))
	require.False(t, sameRepo("not a url", "not a url"))
}
