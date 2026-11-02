//go:build integration

package test

import (
	"encoding/json"
	"net/http"
	"time"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
)

// An incident outlives its job. DELETE /v1/jobs/:id removes the job and leaves
// its tasks and edges behind, and the agent's bundle read of an incident whose
// job was deleted used to 404 while the incident reads still showed it. The
// bundle is now served with the job's alias and no DAG. It runs on the agent
// lane, where a real failure opens the incident on the server.
func (s *IntegrationTestSuite) TestIncidentBundleSurvivesRESTJobDeletion() {
	s.requireAuthLane()
	f := s.newNamespacedRoutesFixture("deljob")
	defer s.closeNamespacedRoutesFixture(f)

	// Two steps, so the job has an edge as well as tasks.
	def := s.a8FailingDefinition(f.mktAlias, "marketing")
	tail := def.Steps[0]
	tail.Name = "tail"
	tail.Command = []string{"sh", "-c", "echo caesium deljob tail"}
	def.Steps = []schema.Step{def.Steps[0], tail}
	s.applyDefinition(def)

	s.a8Run(f.mktJob.ID, "failed")
	incident := s.awaitIncidentForJobTask(f.mktJob.ID, "ok", 60*time.Second)
	token := s.a8AgentToken(incident.ID, []string{f.mktAlias})
	bundlePath := "/v1/agent/incidents/" + incident.ID + "/bundle"

	var live a8Bundle
	s.Require().NoError(json.Unmarshal([]byte(s.requireNSStatus(http.StatusOK, http.MethodGet, bundlePath, token, "")), &live))
	s.Equal(f.mktAlias, live.Job.Alias)
	s.Len(live.Job.Tasks, 2, "the live job shows its DAG")
	s.Len(live.Job.Edges, 1)

	s.requireNSStatus(http.StatusAccepted, http.MethodDelete, "/v1/jobs/"+f.mktJob.ID, s.authAPIKey, "")

	// The incident is still shown, and so is its bundle, without a DAG.
	s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/incidents/"+incident.ID, s.authAPIKey, "")
	var deleted a8Bundle
	s.Require().NoError(json.Unmarshal([]byte(s.requireNSStatus(http.StatusOK, http.MethodGet, bundlePath, token, "")), &deleted))
	s.Equal(f.mktAlias, deleted.Job.Alias)
	s.Empty(deleted.Job.Tasks, "a deleted job has no DAG to act on")
	s.Empty(deleted.Job.Edges)
	s.False(deleted.Job.NamespaceMoved)

	// So is its history, by the alias the bundle gave the agent.
	historyPath := "/v1/agent/incidents/" + incident.ID + "/context/history?job=" + f.mktAlias
	var history struct {
		Runs []struct {
			ID string `json:"id"`
		} `json:"runs"`
	}
	s.Require().NoError(json.Unmarshal([]byte(s.requireNSStatus(http.StatusOK, http.MethodGet, historyPath, token, "")), &history))
	s.NotEmpty(history.Runs, "a deleted job's history outlives it")
}
