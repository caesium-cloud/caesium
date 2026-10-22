//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
)

// Namespace-aware DAG snapshots (identity-and-access B7) and canonical job
// aliases (W6-α).

// b7Apply applies def as the admin with commit as its provenance, so each
// snapshot carries a distinct git_commit the reads can be asserted on.
func (s *IntegrationTestSuite) b7Apply(def schema.Definition, commit string) {
	s.T().Helper()
	status, body := s.jobdefRequest("/v1/jobdefs/apply", s.authAPIKey, map[string]any{
		"definitions": []schema.Definition{def},
		"provenance": map[string]string{
			"source_id": "b7-integration", "repo": "https://example.invalid/b7.git",
			"ref": "refs/heads/main", "commit": commit, "path": "jobs/" + def.Metadata.Alias + ".job.yaml",
		},
	})
	s.Require().Equal(http.StatusOK, status, body)
}

func (s *IntegrationTestSuite) b7History(jobID, token string) []string {
	s.T().Helper()
	body := s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/jobs/"+jobID+"/topology/history", token, "")
	var history topologyHistoryResponse
	s.Require().NoError(json.Unmarshal([]byte(body), &history), body)
	commits := make([]string, 0, len(history.Snapshots))
	for _, snap := range history.Snapshots {
		commits = append(commits, snap.GitCommit)
	}
	return commits
}

func (s *IntegrationTestSuite) b7Latest(jobID, token, query string) (int, topologySnapshot) {
	s.T().Helper()
	path := "/v1/jobs/" + jobID + "/topology"
	if query != "" {
		path += "?" + query
	}
	status, body := s.nsRequest(http.MethodGet, path, token, "", nil)
	var snap topologySnapshot
	if status == http.StatusOK {
		s.Require().NoError(json.Unmarshal([]byte(body), &snap), body)
	}
	return status, snap
}

func (s *IntegrationTestSuite) b7Blame(jobID, token, query string) (int, blameRESTResponse, string) {
	s.T().Helper()
	path := "/v1/jobs/" + jobID + "/blame"
	if query != "" {
		path += "?" + query
	}
	status, body := s.nsRequest(http.MethodGet, path, token, "", nil)
	var res blameRESTResponse
	if status == http.StatusOK {
		s.Require().NoError(json.Unmarshal([]byte(body), &res), body)
	}
	return status, res, body
}

// errorMessage decodes an echo error body's message.
func errorMessage(s *IntegrationTestSuite, body string) string {
	s.T().Helper()
	var out struct {
		Message string `json:"message"`
	}
	s.Require().NoError(json.Unmarshal([]byte(body), &out), body)
	return out.Message
}

func blameCommits(res blameRESTResponse) map[string]string {
	out := make(map[string]string, len(res.Tasks)+len(res.Edges))
	for _, task := range res.Tasks {
		out[task.Element.Name] = task.IntroducingCommit
	}
	for _, edge := range res.Edges {
		out[edge.Element.From+"->"+edge.Element.To] = edge.IntroducingCommit
	}
	return out
}

type b7Viewers struct {
	mkt, fin, both, cluster string
}

func (s *IntegrationTestSuite) b7MintViewers() b7Viewers {
	s.T().Helper()
	return b7Viewers{
		mkt:     s.mintNamespacedKey("viewer", "marketing").Plaintext,
		fin:     s.mintNamespacedKey("viewer", "finance").Plaintext,
		both:    s.mintNamespacedKey("viewer", "marketing", "finance").Plaintext,
		cluster: s.mintNamespacedKey("viewer").Plaintext,
	}
}

// b7MovedJobBack moves the job of alias back to marketing with one more step.
func (s *IntegrationTestSuite) b7MovedJobBack(alias, commit string) {
	s.T().Helper()
	back := s.namespacedJobDefinition(alias, "marketing")
	back.Steps = append(back.Steps, schema.Step{Name: "extra", Image: back.Steps[0].Image, Engine: back.Steps[0].Engine, Command: []string{"sh", "-c", "echo extra"}})
	s.b7Apply(back, commit)
}

// TestAuthDagSnapshotsFollowNamespaceMoves applies a job in marketing (c1),
// moves it unchanged to finance (c2) and back to marketing with one more step
// (c3). Each move writes a snapshot owned by the namespace it was written in,
// and blame, topology and topology history show a confined viewer only the
// snapshots of the namespaces it reads: finance sees only c2, and its blame
// starts at the move; marketing sees c1 and c3 but never c2; a commit only an
// unreadable snapshot carries is a 404 like a missing one; the viewer of the
// side the job is not in is refused the job outright (403). A cluster viewer
// sees every snapshot.
func (s *IntegrationTestSuite) TestAuthDagSnapshotsFollowNamespaceMoves() {
	s.requireAuthLane()
	alias := fmt.Sprintf("b7-moved-%d", time.Now().UnixNano())
	s.b9Cleanup(alias)
	c1, c2, c3 := alias+"-c1", alias+"-c2", alias+"-c3"
	v := s.b7MintViewers()
	mktViewer, finViewer, bothViewer, clusterViewer := v.mkt, v.fin, v.both, v.cluster

	s.b7Apply(s.namespacedJobDefinition(alias, "marketing"), c1)
	job := s.requireJobByAlias(alias)
	s.b7Apply(s.namespacedJobDefinition(alias, "finance"), c2)
	jobURL := "/v1/jobs/" + job.ID

	// After the pure move: the move wrote a finance snapshot.
	s.Equal([]string{c2, c1}, s.b7History(job.ID, clusterViewer), "a move without a topology change writes a snapshot")
	s.Equal([]string{c2, c1}, s.b7History(job.ID, bothViewer))
	s.Equal([]string{c2}, s.b7History(job.ID, finViewer))
	status, latest := s.b7Latest(job.ID, finViewer, "")
	s.Require().Equal(http.StatusOK, status)
	s.Equal(c2, latest.GitCommit)
	status, _ = s.b7Latest(job.ID, finViewer, "commit="+c1)
	s.Equal(http.StatusNotFound, status, "the marketing commit is not finance's")
	status, res, body := s.b7Blame(job.ID, finViewer, "")
	s.Require().Equal(http.StatusOK, status, body)
	s.Equal(map[string]string{"ok": c2}, blameCommits(res), "finance's blame starts at the move")
	s.requireBodyFree(body, "finance blame", c1)
	for _, query := range []string{"from=" + c1, "to=" + c1} {
		status, _, body = s.b7Blame(job.ID, finViewer, query)
		s.Equal(http.StatusNotFound, status, "%s: %s", query, body)
	}
	for _, path := range []string{"/blame", "/topology", "/topology/history"} {
		s.requireNSStatus(http.StatusForbidden, http.MethodGet, jobURL+path, mktViewer, "")
	}

	// Back to marketing with an added step.
	s.b7MovedJobBack(alias, c3)

	s.Equal([]string{c3, c2, c1}, s.b7History(job.ID, clusterViewer))
	s.Equal([]string{c3, c1}, s.b7History(job.ID, mktViewer), "marketing never sees the finance snapshot")
	s.Equal([]string{c3, c2, c1}, s.b7History(job.ID, bothViewer))
	status, res, body = s.b7Blame(job.ID, mktViewer, "")
	s.Require().Equal(http.StatusOK, status, body)
	s.Equal(map[string]string{"ok": c1, "extra": c3, "ok->extra": c3}, blameCommits(res))
	s.requireBodyFree(body, "marketing blame", c2)
	status, _ = s.b7Latest(job.ID, mktViewer, "commit="+c2)
	s.Equal(http.StatusNotFound, status)
	for _, query := range []string{"from=" + c2, "to=" + c2} {
		status, _, body = s.b7Blame(job.ID, mktViewer, query)
		s.Equal(http.StatusNotFound, status, "%s: %s", query, body)
	}
	status, latest = s.b7Latest(job.ID, mktViewer, "")
	s.Require().Equal(http.StatusOK, status)
	s.Equal(c3, latest.GitCommit)
	for _, path := range []string{"/blame", "/topology", "/topology/history"} {
		s.requireNSStatus(http.StatusForbidden, http.MethodGet, jobURL+path, finViewer, "")
	}
	status, res, body = s.b7Blame(job.ID, clusterViewer, "")
	s.Require().Equal(http.StatusOK, status, body)
	s.Equal(map[string]string{"ok": c1, "extra": c3, "ok->extra": c3}, blameCommits(res))
}

// TestAuthDagSnapshotsLegacyRowsClusterOnly: a snapshot written before
// snapshots recorded a namespace (produced here by clearing the column in the
// catalog) cannot be attributed to an owner, so it is shown only to
// unconfined callers. A reader of marketing and finance still sees every
// namespaced snapshot of both, and a marketing reader's blame starts at its
// first readable snapshot.
func (s *IntegrationTestSuite) TestAuthDagSnapshotsLegacyRowsClusterOnly() {
	s.requireAuthLane()
	alias := fmt.Sprintf("b7-legacy-%d", time.Now().UnixNano())
	s.b9Cleanup(alias)
	c1, c2, c3 := alias+"-c1", alias+"-c2", alias+"-c3"
	v := s.b7MintViewers()
	mktViewer, bothViewer, clusterViewer := v.mkt, v.both, v.cluster
	s.b7Apply(s.namespacedJobDefinition(alias, "marketing"), c1)
	job := s.requireJobByAlias(alias)
	s.b7Apply(s.namespacedJobDefinition(alias, "finance"), c2)
	s.b7MovedJobBack(alias, c3)
	s.Equal([]string{c3, c1}, s.b7History(job.ID, mktViewer))

	catalog := s.openIntegrationCatalogDB()
	defer func() { s.NoError(catalog.Close()) }()
	result, err := catalog.ExecContext(s.T().Context(), "UPDATE dag_snapshots SET namespace = NULL WHERE job_id = ? AND git_commit = ?", job.ID, c1)
	s.Require().NoError(err)
	affected, err := result.RowsAffected()
	s.Require().NoError(err)
	s.Require().Equal(int64(1), affected)

	s.Equal([]string{c3}, s.b7History(job.ID, mktViewer))
	s.Equal([]string{c3, c2}, s.b7History(job.ID, bothViewer), "a reader of both namespaces still sees no legacy row")
	s.Equal([]string{c3, c2, c1}, s.b7History(job.ID, clusterViewer))
	s.Equal([]string{c3, c2, c1}, s.b7History(job.ID, s.authAPIKey))
	status, res, body := s.b7Blame(job.ID, mktViewer, "")
	s.Require().Equal(http.StatusOK, status, body)
	s.Equal(map[string]string{"ok": c3, "extra": c3, "ok->extra": c3}, blameCommits(res))
	s.requireBodyFree(body, "marketing blame over a legacy row", c1, c2)
	status, _ = s.b7Latest(job.ID, mktViewer, "commit="+c1)
	s.Equal(http.StatusNotFound, status)
	status, legacy := s.b7Latest(job.ID, clusterViewer, "commit="+c1)
	s.Require().Equal(http.StatusOK, status)
	s.Equal(c1, legacy.GitCommit)
}

// TestJobApplyRejectsNonCanonicalAlias: an alias with leading or trailing
// whitespace is refused for every writer with an error naming it quoted,
// through `caesium job apply`, the raw apply route and the legacy create
// route, and no job is created.
func (s *IntegrationTestSuite) TestJobApplyRejectsNonCanonicalAlias() {
	if onAuthLane() {
		s.T().Skip("the no-auth lane drives this scenario")
	}
	base := fmt.Sprintf("alias-canonical-%d", time.Now().UnixNano())
	alias := base + " "
	quoted := fmt.Sprintf("metadata.alias %q must not have leading or trailing whitespace", alias)

	def := s.namespacedJobDefinition(base, "")
	manifest := strings.Join([]string{
		"apiVersion: v1",
		"kind: Job",
		"metadata:",
		fmt.Sprintf("  alias: %q", alias),
		"trigger:",
		"  type: cron",
		"  configuration:",
		`    cron: "0 0 31 2 *"`,
		"steps:",
		"  - name: ok",
		"    image: " + def.Steps[0].Image,
		`    command: ["true"]`,
		"",
	}, "\n")
	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	out, err := s.runCLIRaw("job", "apply", "--path", dir, "--server", s.caesiumURL)
	s.Require().Error(err, "caesium job apply must refuse the alias:\n%s", out)
	s.Contains(out, quoted)

	def.Metadata.Alias = alias
	status, body := s.jobdefRequest("/v1/jobdefs/apply", "", map[string]any{"definitions": []schema.Definition{def}})
	s.Equal(http.StatusBadRequest, status, body)
	s.Contains(errorMessage(s, body), quoted)

	legacy, err := json.Marshal(map[string]any{
		"alias":   alias,
		"trigger": map[string]any{"type": "cron", "configuration": map[string]any{"cron": "0 0 31 2 *"}},
		"tasks":   []any{map[string]any{"atom": map[string]any{"engine": s.engineOrDocker(), "image": def.Steps[0].Image, "command": []string{"true"}}}},
	})
	s.Require().NoError(err)
	status, body = s.jobdefRequest("/v1/jobs", "", json.RawMessage(legacy))
	s.Equal(http.StatusBadRequest, status, body)
	s.Contains(errorMessage(s, body), quoted)

	s.Nil(s.findJobByAlias(alias), "no job was created")
	s.Nil(s.findJobByAlias(base))
}
