//go:build integration

package test

import (
	"fmt"
	"time"
)

type namespaceLifecycleIncident struct {
	ID        string     `json:"id"`
	JobID     string     `json:"job_id"`
	RunID     string     `json:"run_id"`
	TaskName  string     `json:"task_name"`
	Namespace string     `json:"namespace"`
	Status    string     `json:"status"`
	ClosedAt  *time.Time `json:"closed_at"`
}

// TestIncidentNamespaceLifecycle verifies that success remediates incidents in
// the run's namespace, including when a job moves away and later moves back.
func (s *IntegrationTestSuite) TestIncidentNamespaceLifecycle() {
	s.requireAuthLane()

	alias := fmt.Sprintf("namespace-incident-%d", time.Now().UnixNano())
	definition := failingJobDefinition(alias, s.engineType)
	var jobID string
	runInNamespace := func(namespace string, exitCode int) *runResponse {
		definition.Metadata.Namespace = namespace
		marker := fmt.Sprintf("%s-%s-exit-%d", alias, namespace, exitCode)
		definition.Steps[0].Command = []string{"sh", "-c", fmt.Sprintf("echo %s; exit %d", marker, exitCode)}
		s.applyDefinition(definition)
		job := s.requireJobByAlias(alias)
		if jobID == "" {
			jobID = job.ID
		}
		s.Require().Equal(jobID, job.ID, "moving a namespace must preserve the job")

		run := s.awaitRun(jobID, s.triggerRun(jobID), runTimeout)
		wantStatus := "succeeded"
		if exitCode != 0 {
			wantStatus = "failed"
		}
		s.Require().Equal(wantStatus, run.Status)
		s.Require().Equal(namespace, run.Namespace)
		s.assertJobNamespace(jobID, namespace, namespace)

		var observed resourceRunObservation
		s.getJSON(fmt.Sprintf("/v1/jobs/%s/runs/%s", jobID, run.ID), &observed)
		s.Require().Len(observed.Tasks, 1)
		task := observed.Tasks[0]
		s.Require().NotEmpty(task.RuntimeID, "the task must have a real runtime")
		s.Require().NotNil(task.ExitCode, "the runtime exit must be persisted")
		s.Require().Equal(exitCode, *task.ExitCode)
		s.Require().Contains(s.taskLog(jobID, run.ID, task.ID), marker,
			"an image-pull or startup failure must not stand in for the requested exit")
		return run
	}

	marketingFailure := runInNamespace("marketing", 1)
	marketing := s.awaitNamespaceLifecycleIncident(jobID, "marketing")
	s.Require().Equal(marketingFailure.ID, marketing.RunID)
	s.Require().Nil(marketing.ClosedAt)

	// A second real incident provides an observable processing fence: closing
	// finance after its success proves the subscriber consumed that success,
	// while the original marketing incident must still be open.
	financeFailure := runInNamespace("finance", 1)
	finance := s.awaitNamespaceLifecycleIncident(jobID, "finance")
	s.Require().NotEqual(marketing.ID, finance.ID, "dedupe must include namespace")
	s.Require().Equal(financeFailure.ID, finance.RunID)
	financeSuccess := runInNamespace("finance", 0)
	s.Require().NotEqual(financeFailure.ID, financeSuccess.ID)
	s.awaitIncidentStatus(finance.ID, "closed", 60*time.Second)
	closedFinance := s.namespaceLifecycleIncident(finance.ID)
	s.Require().Equal("closed", closedFinance.Status)
	s.Require().Equal("finance", closedFinance.Namespace)
	s.Require().Equal(financeFailure.ID, closedFinance.RunID)
	s.Require().NotNil(closedFinance.ClosedAt)

	stillMarketing := s.namespaceLifecycleIncident(marketing.ID)
	s.Require().Equal("marketing", stillMarketing.Namespace)
	s.Require().Equal(marketingFailure.ID, stillMarketing.RunID)
	s.Require().Contains([]string{"open", "triaging", "awaiting_approval"}, stillMarketing.Status,
		"finance success must not remediate an incident owned by marketing")
	s.Require().Nil(stillMarketing.ClosedAt)
	var historical runResponse
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/runs/%s", jobID, marketingFailure.ID), &historical)
	s.Require().Equal("marketing", historical.Namespace)
	s.Require().Equal("failed", historical.Status)

	marketingSuccess := runInNamespace("marketing", 0)
	s.Require().NotEqual(marketingFailure.ID, marketingSuccess.ID)
	s.awaitIncidentStatus(marketing.ID, "closed", 60*time.Second)
	closedMarketing := s.namespaceLifecycleIncident(marketing.ID)
	s.Require().Equal("closed", closedMarketing.Status)
	s.Require().Equal("marketing", closedMarketing.Namespace)
	s.Require().Equal(marketingFailure.ID, closedMarketing.RunID)
	s.Require().NotNil(closedMarketing.ClosedAt)
	s.Require().Equal("closed", s.namespaceLifecycleIncident(finance.ID).Status)
}

func (s *IntegrationTestSuite) awaitNamespaceLifecycleIncident(jobID, namespace string) namespaceLifecycleIncident {
	s.T().Helper()
	var found namespaceLifecycleIncident
	s.Require().Eventually(func() bool {
		var list struct {
			Incidents []namespaceLifecycleIncident `json:"incidents"`
		}
		if err := s.tryGetJSON("/v1/incidents?job_id="+jobID, &list); err != nil {
			return false
		}
		for _, incident := range list.Incidents {
			if incident.JobID == jobID && incident.Namespace == namespace && incident.TaskName == "gate" {
				found = incident
				return true
			}
		}
		return false
	}, 60*time.Second, 200*time.Millisecond, "no real failure incident appeared in namespace %s", namespace)
	return found
}

func (s *IntegrationTestSuite) namespaceLifecycleIncident(id string) namespaceLifecycleIncident {
	s.T().Helper()
	var detail struct {
		Incident namespaceLifecycleIncident `json:"incident"`
	}
	s.getJSON("/v1/incidents/"+id, &detail)
	s.Require().Equal(id, detail.Incident.ID)
	return detail.Incident
}
