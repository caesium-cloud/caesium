//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
)

// Agent sessions confined to their incident's namespace (identity-and-access
// design §7.4, plan item A8). Every scenario runs on the api-key auth lane,
// where a real failure opens the incident on the server.

type a8IncidentDetail struct {
	Incident struct {
		ID                  string          `json:"id"`
		Namespace           string          `json:"namespace"`
		AllowedJobs         json.RawMessage `json:"allowed_jobs"`
		AllowedJobsExternal int             `json:"allowed_jobs_external"`
	} `json:"incident"`
	Actions   []approvalAction  `json:"actions"`
	Approvals []approvalRequest `json:"approvals"`
}

type a8Bundle struct {
	Job struct {
		Alias          string            `json:"alias"`
		Tasks          []json.RawMessage `json:"tasks"`
		NamespaceMoved bool              `json:"namespace_moved"`
	} `json:"job"`
	RunHistory []struct {
		ID string `json:"id"`
	} `json:"run_history"`
	LineageImpact struct {
		AllowedJobs []string `json:"allowed_jobs"`
		External    int      `json:"external"`
	} `json:"lineage_impact"`
}

// a8Run runs a job and waits for it, returning the run id.
func (s *IntegrationTestSuite) a8Run(jobID, want string) string {
	s.T().Helper()
	runID := s.triggerRun(jobID)
	s.Require().Equal(want, s.awaitRun(jobID, runID, runTimeout).Status, "run %s of job %s", runID, jobID)
	return runID
}

// a8TaskRun returns the task-run id of a single-step run.
func (s *IntegrationTestSuite) a8TaskRun(f *nsRoutesFixture, runID string) string {
	s.T().Helper()
	var id string
	s.Require().NoError(f.catalogDB.QueryRowContext(s.T().Context(), `SELECT id FROM task_runs WHERE job_run_id = ? LIMIT 1`, runID).Scan(&id))
	return id
}

// a8Lineage records a lineage dataset row on a real task run (a precondition:
// the freeze walks the lineage graph the server reads).
func (s *IntegrationTestSuite) a8Lineage(f *nsRoutesFixture, taskRunID, direction, name string) {
	s.T().Helper()
	s.catalogExec(f, 1, `INSERT INTO lineage_datasets (id, task_run_id, namespace, name, direction, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), taskRunID, "a8-lineage", name, direction, time.Now().UTC())
}

// a8AgentToken mints an agent-session credential for the incident carrying
// the given allowlist, exactly as the session supervisor does.
func (s *IntegrationTestSuite) a8AgentToken(incidentID string, allowlist []string) string {
	s.T().Helper()
	conn := s.openIntegrationCatalogGorm()
	svc := iauth.NewService(conn, iauth.WithKeyHashSecret(env.Variables().AuthKeyHashSecret))
	key, err := svc.MintAgentSessionKey(uuid.MustParse(incidentID), allowlist, time.Hour)
	s.Require().NoError(err)
	return key.Plaintext
}

// a8LegacyAgentToken mints an agent-session credential the way the server did
// before agent sessions were namespace-confined: its claim carries no
// confinement mark.
func (s *IntegrationTestSuite) a8LegacyAgentToken(incidentID string, allowlist []string) string {
	s.T().Helper()
	conn := s.openIntegrationCatalogGorm()
	svc := iauth.NewService(conn, iauth.WithKeyHashSecret(env.Variables().AuthKeyHashSecret))
	expiry := time.Now().UTC().Add(time.Hour)
	key, err := svc.CreateKey(&iauth.CreateKeyRequest{
		Description: "a8 legacy agent session " + incidentID,
		Role:        iauth.AgentSessionKeyRole,
		Scope:       &models.KeyScope{Agent: &models.AgentClaim{IncidentID: uuid.MustParse(incidentID), Jobs: allowlist}},
		CreatedBy:   "incident-manager",
		ExpiresAt:   &expiry,
	})
	s.Require().NoError(err)
	return key.Plaintext
}

// a8MCP calls an MCP tool and returns the status and raw body.
func (s *IntegrationTestSuite) a8MCP(token, incidentID, tool string, arguments map[string]any) (int, string) {
	s.T().Helper()
	status, body := s.postAgentMCP(token, uuid.MustParse(incidentID), map[string]any{
		"jsonrpc": "2.0",
		"id":      tool,
		"method":  "tools/call",
		"params":  map[string]any{"name": tool, "arguments": arguments},
	})
	return status, string(body)
}

// a8AwaitIncidentForRun waits for the incident the server opens for runID.
func (s *IntegrationTestSuite) a8AwaitIncidentForRun(jobID, runID string) string {
	s.T().Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var list struct {
			Incidents []struct {
				ID    string `json:"id"`
				RunID string `json:"run_id"`
			} `json:"incidents"`
		}
		if err := s.tryGetJSON("/v1/incidents?job_id="+jobID, &list); err == nil {
			for _, inc := range list.Incidents {
				if inc.RunID == runID {
					return inc.ID
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	s.T().Fatalf("no incident opened for run %s of job %s", runID, jobID)
	return ""
}

func (s *IntegrationTestSuite) a8FailingDefinition(alias, namespace string) schema.Definition {
	def := s.namespacedJobDefinition(alias, namespace)
	def.Steps[0].Command = []string{"sh", "-c", "echo caesium a8 gate; exit 1"}
	def.Metadata.Remediation = &schema.MetadataRemediation{Profile: "triage-only", Classes: []string{"unknown"}}
	return def
}

// TestAuthAgentFreezeAndApprovalConfinedToIncidentNamespace: a marketing job
// whose lineage reaches a marketing and a finance consumer fails. The server
// freezes only the marketing jobs and counts the finance one; neither the
// incident detail, the bundle (REST and MCP) nor the context passthrough names
// or serves the finance job, even to a token whose claim still carries it. A
// marketing viewer reads the detail and a finance one cannot; a marketing
// operator approves the agent's patch, and the executor refuses the patch
// because it would move the job to finance.
func (s *IntegrationTestSuite) TestAuthAgentFreezeAndApprovalConfinedToIncidentNamespace() {
	s.requireAuthLane()
	f := s.newNamespacedRoutesFixture("a8freeze")
	defer s.closeNamespacedRoutesFixture(f)
	consumerAlias := fmt.Sprintf("a8-consumer-%d", f.stamp)
	consumer := s.addNamespacedJob(f, consumerAlias, "marketing")

	// Lineage: the marketing producer's trusted run writes D, which the
	// marketing consumer and the finance job read.
	producerRun := s.a8Run(f.mktJob.ID, "succeeded")
	consumerRun := s.a8Run(consumer.ID, "succeeded")
	financeRun := s.a8Run(f.finJob.ID, "succeeded")
	dataset := fmt.Sprintf("a8-d-%d", f.stamp)
	s.a8Lineage(f, s.a8TaskRun(f, producerRun), "output", dataset)
	for _, run := range []string{consumerRun, financeRun} {
		task := s.a8TaskRun(f, run)
		s.a8Lineage(f, task, "input", dataset)
		s.a8Lineage(f, task, "output", dataset+"-"+run[:8])
	}
	f.undo = append(f.undo, func() {
		s.catalogExec(f, -1, `DELETE FROM lineage_datasets WHERE namespace = 'a8-lineage' AND name LIKE ?`, dataset+"%")
	})

	// The producer now fails; the server opens the incident and freezes.
	s.applyDefinition(s.a8FailingDefinition(f.mktAlias, "marketing"))
	s.a8Run(f.mktJob.ID, "failed")
	incident := s.awaitIncidentForJobTask(f.mktJob.ID, "ok", 60*time.Second)

	var detail a8IncidentDetail
	s.Require().NoError(json.Unmarshal([]byte(s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/incidents/"+incident.ID, s.authAPIKey, "")), &detail))
	s.Equal("marketing", detail.Incident.Namespace)
	var frozen []string
	s.Require().NoError(json.Unmarshal(detail.Incident.AllowedJobs, &frozen))
	s.ElementsMatch([]string{f.mktAlias, consumerAlias}, frozen, "only marketing jobs are frozen")
	s.Equal(1, detail.Incident.AllowedJobsExternal, "the finance consumer is counted, not named")

	// A token whose claim still names the finance job (a pre-A8 freeze) is
	// served nothing about it.
	token := s.a8AgentToken(incident.ID, append(append([]string(nil), frozen...), f.finAlias))
	body := s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/agent/incidents/"+incident.ID+"/bundle", token, "")
	s.requireBodyFree(body, "REST bundle", f.finAlias, financeRun)
	var bundle a8Bundle
	s.Require().NoError(json.Unmarshal([]byte(body), &bundle))
	s.ElementsMatch([]string{f.mktAlias, consumerAlias}, bundle.LineageImpact.AllowedJobs)
	s.Equal(1, bundle.LineageImpact.External)
	status, mcpBody := s.a8MCP(token, incident.ID, "get_bundle", map[string]any{})
	s.Require().Equal(http.StatusOK, status, mcpBody)
	s.requireBodyFree(mcpBody, "MCP bundle", f.finAlias, financeRun)
	s.Contains(mcpBody, consumerAlias)

	s.requireNSStatus(http.StatusForbidden, http.MethodGet, "/v1/agent/incidents/"+incident.ID+"/context/history?job="+f.finAlias, token, "")
	body = s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/agent/incidents/"+incident.ID+"/context/history?job="+consumerAlias, token, "")
	s.Contains(body, consumerRun)
	status, mcpBody = s.a8MCP(token, incident.ID, "get_context", map[string]any{"kind": "history", "job": f.finAlias})
	s.Require().Equal(http.StatusOK, status, mcpBody)
	s.Contains(mcpBody, `"error"`, "the MCP history read of the finance job is refused")
	s.requireBodyFree(mcpBody, "MCP history", financeRun)

	// Humans: the incident's namespace decides.
	mktViewer := s.namespacedKey(f, "viewer", `["marketing"]`).Plaintext
	finViewer := s.namespacedKey(f, "viewer", `["finance"]`).Plaintext
	body = s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/incidents/"+incident.ID, mktViewer, "")
	s.requireBodyFree(body, "confined incident detail", f.finAlias)
	s.requireNSStatus(http.StatusForbidden, http.MethodGet, "/v1/incidents/"+incident.ID, finViewer, "")

	// A credential minted before sessions were namespace-confined (no mark on
	// its claim) still works, but what it writes is withheld from confined
	// readers; the cluster admin sees it.
	legacyToken := s.a8LegacyAgentToken(incident.ID, frozen)
	legacyNote := fmt.Sprintf("a8-legacy-note-%d", f.stamp)
	legacyMCPNote := fmt.Sprintf("a8-legacy-mcp-note-%d", f.stamp)
	confinedNote := fmt.Sprintf("a8-confined-note-%d", f.stamp)
	notes := "/v1/agent/incidents/" + incident.ID + "/notes"
	s.requireNSStatus(http.StatusCreated, http.MethodPost, notes, legacyToken, fmt.Sprintf(`{"text":%q}`, legacyNote))
	status, mcpBody = s.a8MCP(legacyToken, incident.ID, "add_note", map[string]any{"text": legacyMCPNote})
	s.Require().Equal(http.StatusOK, status, mcpBody)
	s.requireNSStatus(http.StatusCreated, http.MethodPost, notes, token, fmt.Sprintf(`{"text":%q}`, confinedNote))
	body = s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/incidents/"+incident.ID, mktViewer, "")
	s.requireBodyFree(body, "confined detail", legacyNote, legacyMCPNote)
	s.Contains(body, confinedNote)
	s.Contains(body, `"content_withheld":true`)
	body = s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/incidents/"+incident.ID, s.authAPIKey, "")
	s.Contains(body, legacyNote, "an unrestricted reader sees every row as stored")
	s.Contains(body, legacyMCPNote)
	body = s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/agent/incidents/"+incident.ID+"/bundle", token, "")
	s.requireBodyFree(body, "agent bundle notes", legacyNote, legacyMCPNote)
	s.Contains(body, confinedNote)

	// The agent proposes a patch that moves its job to finance; a marketing
	// operator approves it and the executor refuses the move.
	moved := s.a8FailingDefinition(f.mktAlias, "finance")
	patch, err := json.Marshal(moved)
	s.Require().NoError(err)
	status, proposed := s.postWithToken(fmt.Sprintf("%s/v1/agent/incidents/%s/actions", s.caesiumURL, incident.ID), token,
		map[string]any{"type": "apply_jobdef_patch", "params": map[string]any{"definition": json.RawMessage(patch)}})
	s.Require().Equal(http.StatusAccepted, status, string(proposed))
	var proposal struct {
		Action approvalAction `json:"action"`
	}
	s.Require().NoError(json.Unmarshal(proposed, &proposal))
	awaiting := s.awaitIncidentStatus(incident.ID, "awaiting_approval", 30*time.Second)
	var pending string
	for _, a := range awaiting.Approvals {
		if a.ActionID == proposal.Action.ID && a.Decision == "pending" {
			pending = a.ID
		}
	}
	s.Require().NotEmpty(pending, "no pending approval for the agent's patch")
	approve := "/v1/incidents/" + incident.ID + "/approvals/" + pending + "/approve"
	finOperator := s.namespacedKey(f, "operator", `["finance"]`).Plaintext
	mktOperator := s.namespacedKey(f, "operator", `["marketing"]`).Plaintext
	s.requireNSStatus(http.StatusForbidden, http.MethodPost, approve, finOperator, "")
	s.requireNSStatus(http.StatusOK, http.MethodPost, approve, mktOperator, "")
	executed := requireActionByID(s, s.incidentDetail(incident.ID), proposal.Action.ID)
	s.Equal("failed", executed.Status, "a patch that moves the job out of the incident's namespace is refused")
	s.Contains(string(executed.Result), "metadata.namespace")
	var namespace string
	s.Require().NoError(f.catalogDB.QueryRowContext(s.T().Context(), `SELECT namespace FROM jobs WHERE id = ?`, f.mktJob.ID).Scan(&namespace))
	s.Equal("marketing", namespace, "the job did not move")

	// A namespace-preserving patch that conflicts with a finance job (both
	// would produce one dataset) fails validation; the error recorded on the
	// marketing incident names neither the finance job nor the dataset.
	shared := fmt.Sprintf("a8-shared-dataset-%d", f.stamp)
	finProducer := fmt.Sprintf("a8-fin-producer-%d", f.stamp)
	finDef := s.namespacedJobDefinition(finProducer, "finance")
	finDef.Steps[0].Datasets = &schema.StepDatasets{Produces: []schema.ProducedDataset{{Name: shared}}}
	s.applyDefinition(finDef)
	f.jobs = append(f.jobs, s.requireJobByAlias(finProducer).ID)
	conflicting := s.a8FailingDefinition(f.mktAlias, "marketing")
	conflicting.Steps[0].Datasets = &schema.StepDatasets{Produces: []schema.ProducedDataset{{Name: shared}}}
	patch, err = json.Marshal(conflicting)
	s.Require().NoError(err)
	status, proposed = s.postWithToken(fmt.Sprintf("%s/v1/agent/incidents/%s/actions", s.caesiumURL, incident.ID), token,
		map[string]any{"type": "apply_jobdef_patch", "params": map[string]any{"definition": json.RawMessage(patch)}})
	s.Require().Equal(http.StatusAccepted, status, string(proposed))
	s.Require().NoError(json.Unmarshal(proposed, &proposal))
	awaiting = s.awaitIncidentStatus(incident.ID, "awaiting_approval", 30*time.Second)
	pending = ""
	for _, a := range awaiting.Approvals {
		if a.ActionID == proposal.Action.ID && a.Decision == "pending" {
			pending = a.ID
		}
	}
	s.Require().NotEmpty(pending, "no pending approval for the conflicting patch")
	s.requireNSStatus(http.StatusOK, http.MethodPost, "/v1/incidents/"+incident.ID+"/approvals/"+pending+"/approve", mktOperator, "")
	executed = requireActionByID(s, s.incidentDetail(incident.ID), proposal.Action.ID)
	s.Equal("failed", executed.Status, "the duplicate producer is still refused")
	s.requireBodyFree(string(executed.Result), "patch validation error", finProducer, shared)
	s.Contains(string(executed.Result), "another namespace")
	body = s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/incidents/"+incident.ID, mktViewer, "")
	s.requireBodyFree(body, "confined detail after the conflicting patch", finProducer)
}

// TestAuthAgentMovedJobBringsNoForeignHistory: a job fails in finance, moves
// to marketing and fails there. The marketing incident's agent reads the job's
// history and why baseline through REST and MCP: neither returns the
// finance-era run, which a cluster viewer still sees as the why baseline.
func (s *IntegrationTestSuite) TestAuthAgentMovedJobBringsNoForeignHistory() {
	s.requireAuthLane()
	f := s.newNamespacedRoutesFixture("a8move")
	defer s.closeNamespacedRoutesFixture(f)
	alias := fmt.Sprintf("a8-moved-%d", f.stamp)
	// The same failing, cached step in both namespaces, so the why baseline of
	// the marketing run is the finance-era run for an unrestricted caller.
	definition := func(namespace string) schema.Definition {
		def := s.namespacedJobDefinition(alias, namespace)
		def.Metadata.Cache = true
		def.Steps[0].Command = []string{"sh", "-c", "echo caesium a8 moved; exit 1"}
		return def
	}
	s.applyDefinition(definition("finance"))
	job := s.requireJobByAlias(alias)
	f.jobs = append(f.jobs, job.ID)
	financeRun := s.a8Run(job.ID, "failed")

	s.applyDefinition(definition("marketing"))
	marketingRun := s.a8Run(job.ID, "failed")
	incidentID := s.a8AwaitIncidentForRun(job.ID, marketingRun)
	incident := approvalIncident{ID: incidentID}
	s.Contains(s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/jobs/"+job.ID+"/runs/"+marketingRun+"/why?task=ok", s.authAPIKey, ""), financeRun,
		"an unrestricted caller still diffs against the finance-era run")

	token := s.a8AgentToken(incident.ID, []string{alias})
	for _, path := range []string{"/context/history", "/context/history?job=" + alias, "/context/why?task=ok", "/bundle"} {
		body := s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/agent/incidents/"+incident.ID+path, token, "")
		s.requireBodyFree(body, "REST "+path, financeRun)
		if path != "/context/why?task=ok" {
			s.Contains(body, marketingRun, path)
		}
	}
	for _, args := range []map[string]any{
		{"kind": "history"},
		{"kind": "history", "job": alias},
		{"kind": "why", "task": "ok"},
	} {
		status, body := s.a8MCP(token, incident.ID, "get_context", args)
		s.Require().Equal(http.StatusOK, status, body)
		s.requireBodyFree(body, fmt.Sprintf("MCP get_context %v", args), financeRun)
	}
}

// TestAuthAgentGitSyncedPatchDiffRedactsSharedTrigger: a git-synced marketing
// job whose trigger also serves a finance job gets an agent patch approved by
// a marketing operator. The patch is not applied but escalated with its diff,
// and that diff (on the marketing incident's timeline) withholds the shared
// trigger's stored configuration.
func (s *IntegrationTestSuite) TestAuthAgentGitSyncedPatchDiffRedactsSharedTrigger() {
	s.requireAuthLane()
	f := s.newNamespacedRoutesFixture("a8git")
	defer s.closeNamespacedRoutesFixture(f)
	s.applyDefinition(s.a8FailingDefinition(f.mktAlias, "marketing"))
	s.a8Run(f.mktJob.ID, "failed")
	incident := s.awaitIncidentForJobTask(f.mktJob.ID, "ok", 60*time.Second)

	// Preconditions: git provenance on the marketing job, and its trigger
	// (carrying a marker) shared with the finance job.
	marker := fmt.Sprintf("a8-shared-trigger-secret-%d", f.stamp)
	var mktTrigger, finTrigger, original string
	s.Require().NoError(f.catalogDB.QueryRowContext(s.T().Context(), `SELECT trigger_id FROM jobs WHERE id = ?`, f.mktJob.ID).Scan(&mktTrigger))
	s.Require().NoError(f.catalogDB.QueryRowContext(s.T().Context(), `SELECT trigger_id FROM jobs WHERE id = ?`, f.finJob.ID).Scan(&finTrigger))
	s.Require().NoError(f.catalogDB.QueryRowContext(s.T().Context(), `SELECT configuration FROM triggers WHERE id = ?`, mktTrigger).Scan(&original))
	s.catalogExec(f, 1, `UPDATE jobs SET provenance_source_id = ? WHERE id = ?`, "a8-git-source", f.mktJob.ID)
	s.catalogExec(f, 1, `UPDATE triggers SET configuration = ? WHERE id = ?`, fmt.Sprintf(`{"cron":"0 0 31 2 *","marker":%q}`, marker), mktTrigger)
	s.catalogExec(f, 1, `UPDATE jobs SET trigger_id = ? WHERE id = ?`, mktTrigger, f.finJob.ID)
	f.undo = append(f.undo, func() {
		s.catalogExec(f, 1, `UPDATE jobs SET trigger_id = ? WHERE id = ?`, finTrigger, f.finJob.ID)
		s.catalogExec(f, 1, `UPDATE triggers SET configuration = ? WHERE id = ?`, original, mktTrigger)
		s.catalogExec(f, 1, `UPDATE jobs SET provenance_source_id = NULL WHERE id = ?`, f.mktJob.ID)
	})

	token := s.a8AgentToken(incident.ID, []string{f.mktAlias})
	patch, err := json.Marshal(s.a8FailingDefinition(f.mktAlias, "marketing"))
	s.Require().NoError(err)
	status, proposed := s.postWithToken(fmt.Sprintf("%s/v1/agent/incidents/%s/actions", s.caesiumURL, incident.ID), token,
		map[string]any{"type": "apply_jobdef_patch", "params": map[string]any{"definition": json.RawMessage(patch)}})
	s.Require().Equal(http.StatusAccepted, status, string(proposed))
	var proposal struct {
		Action approvalAction `json:"action"`
	}
	s.Require().NoError(json.Unmarshal(proposed, &proposal))
	awaiting := s.awaitIncidentStatus(incident.ID, "awaiting_approval", 30*time.Second)
	var pending string
	for _, a := range awaiting.Approvals {
		if a.ActionID == proposal.Action.ID && a.Decision == "pending" {
			pending = a.ID
		}
	}
	s.Require().NotEmpty(pending, "no pending approval for the git-synced patch")
	mktOperator := s.namespacedKey(f, "operator", `["marketing"]`).Plaintext
	s.requireNSStatus(http.StatusOK, http.MethodPost, "/v1/incidents/"+incident.ID+"/approvals/"+pending+"/approve", mktOperator, "")

	executed := requireActionByID(s, s.incidentDetail(incident.ID), proposal.Action.ID)
	s.Contains(string(executed.Result), `"route":"escalate"`, "a git-synced job is escalated, not patched: %s", executed.Result)
	s.Contains(string(executed.Result), "another namespace", "the shared trigger's stored configuration is redacted from the diff")
	s.requireBodyFree(string(executed.Result), "escalated diff", marker)
	mktViewer := s.namespacedKey(f, "viewer", `["marketing"]`).Plaintext
	s.requireBodyFree(s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/incidents/"+incident.ID, mktViewer, ""), "confined detail", marker, f.finAlias)
}
