//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// --------------------------------------------------------------------------
// Retry re-entry executes the recipe frozen on the task_runs row
// --------------------------------------------------------------------------

// TestRetryAfterApplyExecutesRegisteredCommand pins the cross-lane contract
// from issue #354: a run RE-ENTERED by `run retry` executes the command the run
// was registered with, not whatever `job apply` has since landed in the catalog.
//
// This scenario is deliberately lane-INDEPENDENT and is run in both the default
// (local) lane and CAESIUM_EXECUTION_MODE=distributed, because the bug it
// covers was precisely the two lanes disagreeing: the distributed worker
// executed parseTaskCommand(taskRun.Command) off the frozen row while the local
// executor rebuilt its runner from a live catalog read, so the same retry ran
// the OLD command on one lane and the NEW one on the other. Branching the
// assertion per lane would let that divergence pass again.
//
// The observation is the container's own output, read back through
// GET /v1/jobs/:id/runs/:run_id/logs — the retry reset clears log_text, so what
// is streamed afterwards is the retried attempt's stdout, not the first run's.
func (s *IntegrationTestSuite) TestRetryAfterApplyExecutesRegisteredCommand() {
	alias := fmt.Sprintf("integration-retry-frozen-cmd-%d", time.Now().UnixNano())

	manifest := func(marker, exitCode string) string {
		return fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: seed
    image: alpine:3.23
    command: ["sh", "-c", "echo seeded"]
    next: subject
  - name: subject
    image: alpine:3.23
    command: ["sh", "-c", "echo RECIPE=%s; exit %s"]
    dependsOn: seed
`, alias, marker, exitCode)
	}

	// v1: `subject` announces itself and fails, so the run lands in a terminal
	// state a retry can re-open.
	dirV1 := s.writeJobManifest(manifest("V1", "1"))
	defer os.RemoveAll(dirV1)
	s.runCLI("job", "apply", "--path", dirV1, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)
	subjectID := s.jobTaskIDByName(job.ID, "subject")

	runID := s.triggerRun(job.ID)
	firstRun := s.awaitRun(job.ID, runID, runTimeout)
	s.Require().Equal("failed", firstRun.Status, "the v1 `subject` command exits 1, so the run must fail")
	s.Require().Equal("succeeded", s.taskStatusesByName(job.ID, firstRun)["seed"])

	// v2: the same step, a command that would SUCCEED. Applying it changes the
	// catalog; it must not change what the already-registered run executes.
	dirV2 := s.writeJobManifest(manifest("V2", "0"))
	defer os.RemoveAll(dirV2)
	s.runCLI("job", "apply", "--path", dirV2, "--server", s.caesiumURL)

	// Guard the fixture: if the apply were a no-op the assertions below would
	// pass for the wrong reason.
	s.Require().Contains(s.jobTaskCommand(job.ID, "subject"), "RECIPE=V2",
		"the second apply did not reach the catalog, so this scenario proves nothing")

	resp, err := s.doJSONRequest(http.MethodPost,
		fmt.Sprintf("%s/v1/jobs/%s/runs/%s/retry", s.caesiumURL, job.ID, runID), nil)
	s.Require().NoError(err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	s.Require().Equal(http.StatusAccepted, resp.StatusCode, "retry should return 202: %s", body)
	var reopened runResponse
	s.Require().NoError(json.Unmarshal(body, &reopened))
	s.Require().NotEqual("failed", reopened.Status,
		"the retry response must show the run re-opened, or the wait below races the previous terminal state: %s", body)

	retryRun := s.awaitRun(job.ID, runID, runTimeout)

	// The row the run was registered with still carries the v1 command …
	s.Equal([]string{"sh", "-c", "echo RECIPE=V1; exit 1"}, s.runTaskCommand(job.ID, runID, subjectID),
		"the retry reset must not rewrite the frozen recipe columns")

	// … the container that ran printed it …
	logs := s.taskLogs(job.ID, runID, subjectID)
	s.Contains(logs, "RECIPE=V1",
		"the retried attempt executed a command the run was never registered with: %s", logs)
	s.NotContains(logs, "RECIPE=V2",
		"the retried attempt picked up the applied definition instead of replaying the run: %s", logs)

	// … and it therefore still failed, exactly as the registered run would.
	s.Equal("failed", retryRun.Status,
		"a retry that succeeded here ran the applied v2 command (exit 0) rather than the registered v1 one")
	s.Equal("failed", s.taskStatusesByName(job.ID, retryRun)["subject"])
	s.Equal("succeeded", s.taskStatusesByName(job.ID, retryRun)["seed"],
		"a retry must preserve the already-succeeded predecessor")
}

// TestRetryValidatesAgainstTheRegisteredOutputSchema proves the freeze covers a
// field BEYOND `command`: the output schema and its enforcement mode, which
// RegisterTasks pins onto the row and the distributed worker validates from
// (runtimeExecutor.runSchemaValidation). The local lane used to read both live,
// so a retry after a `job apply` that relaxed the contract passed on one lane
// and failed on the other.
//
// The step's command never changes here - only `outputSchema` and
// `metadata.schemaValidation` - so a pass can only come from the frozen
// validation inputs, not from the command freeze the sibling scenario covers.
// Like that scenario this is deliberately lane-INDEPENDENT and runs in both.
func (s *IntegrationTestSuite) TestRetryValidatesAgainstTheRegisteredOutputSchema() {
	alias := fmt.Sprintf("integration-retry-frozen-schema-%d", time.Now().UnixNano())

	// v1: schemaValidation=fail plus a schema demanding an integer `rows`. The
	// step emits a string, so the task fails validation and the run is
	// retryable. The command is byte-identical in both manifests.
	const command = `["sh", "-c", "echo '##caesium::output {\"rows\": \"not-a-number\"}'"]`

	manifestV1 := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  schemaValidation: fail
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: emit
    image: alpine:3.23
    command: %s
    outputSchema:
      type: object
      required: [rows]
      properties:
        rows:
          type: integer
`, alias, command)

	// v2: the contract is relaxed away entirely - enforcement disabled AND the
	// schema dropped. A NEW run would pass; this registered one must not.
	manifestV2 := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: emit
    image: alpine:3.23
    command: %s
`, alias, command)

	dirV1 := s.writeJobManifest(manifestV1)
	defer os.RemoveAll(dirV1)
	s.runCLI("job", "apply", "--path", dirV1, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	runID := s.triggerRun(job.ID)
	firstRun := s.awaitRun(job.ID, runID, runTimeout)
	s.Require().Equal("failed", firstRun.Status,
		"fail-mode validation of a string against an integer schema must fail the run: %s", firstRun.Error)

	dirV2 := s.writeJobManifest(manifestV2)
	defer os.RemoveAll(dirV2)
	s.runCLI("job", "apply", "--path", dirV2, "--server", s.caesiumURL)

	// Fixture guard: without a real catalog change this scenario proves nothing.
	s.Require().Empty(s.jobTaskOutputSchema(job.ID, "emit"),
		"the second apply did not drop the outputSchema from the catalog")

	resp, err := s.doJSONRequest(http.MethodPost,
		fmt.Sprintf("%s/v1/jobs/%s/runs/%s/retry", s.caesiumURL, job.ID, runID), nil)
	s.Require().NoError(err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	s.Require().Equal(http.StatusAccepted, resp.StatusCode, "retry should return 202: %s", body)
	var reopened runResponse
	s.Require().NoError(json.Unmarshal(body, &reopened))
	s.Require().NotEqual("failed", reopened.Status,
		"the retry response must show the run re-opened, or the wait below races the previous terminal state: %s", body)

	retryRun := s.awaitRun(job.ID, runID, runTimeout)

	s.Equal("failed", retryRun.Status,
		"the retry validated against the RELAXED live definition instead of the schema frozen when the run was registered")
	s.Equal("failed", s.taskStatusesByName(job.ID, retryRun)["emit"])
}

// jobTaskOutputSchema returns the CATALOG outputSchema for a step.
func (s *IntegrationTestSuite) jobTaskOutputSchema(jobID, name string) string {
	s.T().Helper()

	var tasks []struct {
		Name         string          `json:"Name"`
		OutputSchema json.RawMessage `json:"output_schema"`
	}
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/tasks", jobID), &tasks)

	for _, t := range tasks {
		if t.Name != name {
			continue
		}
		trimmed := strings.TrimSpace(string(t.OutputSchema))
		if trimmed == "null" {
			return ""
		}
		return trimmed
	}

	s.T().Fatalf("task %q not found on job %s", name, jobID)
	return ""
}

// jobTaskCommand returns the CATALOG command for a step, as the job's task /
// atom rows currently hold it.
func (s *IntegrationTestSuite) jobTaskCommand(jobID, name string) string {
	s.T().Helper()

	var tasks []struct {
		Name   string `json:"Name"`
		AtomID string `json:"AtomID"`
	}
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/tasks", jobID), &tasks)

	for _, t := range tasks {
		if t.Name != name {
			continue
		}
		var atomModel struct {
			Command string `json:"command"`
		}
		s.getJSON(fmt.Sprintf("/v1/atoms/%s", t.AtomID), &atomModel)
		return atomModel.Command
	}

	s.T().Fatalf("task %q not found on job %s", name, jobID)
	return ""
}

// runTaskCommand returns the command FROZEN on a run's task row.
func (s *IntegrationTestSuite) runTaskCommand(jobID, runID, taskID string) []string {
	s.T().Helper()

	var detail struct {
		Tasks []struct {
			ID      string   `json:"id"`
			Command []string `json:"command"`
		} `json:"tasks"`
	}
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/runs/%s", jobID, runID), &detail)

	for _, t := range detail.Tasks {
		if t.ID == taskID {
			return t.Command
		}
	}

	raw, _ := json.Marshal(detail)
	s.T().Fatalf("task %s not present on run %s: %s", taskID, runID, raw)
	return nil
}

// taskLogs streams one unfanned task's persisted log snapshot.
func (s *IntegrationTestSuite) taskLogs(jobID, runID, taskID string) string {
	s.T().Helper()

	resp, err := s.doJSONRequest(http.MethodGet,
		fmt.Sprintf("%s/v1/jobs/%s/runs/%s/logs?task_id=%s", s.caesiumURL, jobID, runID, taskID), nil)
	s.Require().NoError(err)
	text, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	s.Require().NoError(readErr)
	s.Require().Equal(http.StatusOK, resp.StatusCode, "logs for task %s: %s", taskID, text)

	return string(text)
}
