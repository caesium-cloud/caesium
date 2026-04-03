//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/caesium-cloud/caesium/pkg/jobdef"
)

func (s *IntegrationTestSuite) TestDataContractsPersistOnTasksAndDAG() {
	alias := fmt.Sprintf("integration-data-contracts-dag-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(dataContractsManifest(alias, jobdef.SchemaValidationWarn, "17"))
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	detail := s.jobDetailByAlias(alias)
	s.Equal(jobdef.SchemaValidationWarn, stringFromMap(detail, "schema_validation"))

	tasks := s.jobTasks(job.ID)
	s.Require().Len(tasks, 3)

	taskIDByName := make(map[string]string, len(tasks))
	for _, task := range tasks {
		taskIDByName[stringFromMap(task, "name")] = stringFromMap(task, "id")
	}

	extractTask := s.requireTaskByName(tasks, "extract")
	transformTask := s.requireTaskByName(tasks, "transform")
	notifyTask := s.requireTaskByName(tasks, "notify")

	extractOutputSchema := mapFromMap(extractTask, "output_schema")
	s.Equal("object", stringFromMap(extractOutputSchema, "type"))
	s.Contains(mapFromMap(extractOutputSchema, "properties"), "row_count")

	transformInputSchema := nestedMapFromAny(valueFromMap(transformTask, "input_schema"))
	s.Require().Contains(transformInputSchema, "extract")
	s.Equal([]string{"row_count"}, requiredKeys(transformInputSchema["extract"]))

	transformOutputSchema := mapFromMap(transformTask, "output_schema")
	s.Equal("object", stringFromMap(transformOutputSchema, "type"))
	s.Contains(mapFromMap(transformOutputSchema, "properties"), "rows_written")

	s.Empty(mapFromMap(notifyTask, "output_schema"))
	s.Empty(nestedMapFromAny(valueFromMap(notifyTask, "input_schema")))

	var dag struct {
		Nodes []struct {
			ID           string                    `json:"id"`
			OutputSchema map[string]any            `json:"output_schema"`
			InputSchema  map[string]map[string]any `json:"input_schema"`
		} `json:"nodes"`
		Edges []struct {
			From            string `json:"from"`
			To              string `json:"to"`
			ContractDefined bool   `json:"contract_defined"`
		} `json:"edges"`
	}
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/dag", job.ID), &dag)

	nodeByID := make(map[string]struct {
		OutputSchema map[string]any
		InputSchema  map[string]map[string]any
	}, len(dag.Nodes))
	for _, node := range dag.Nodes {
		nodeByID[node.ID] = struct {
			OutputSchema map[string]any
			InputSchema  map[string]map[string]any
		}{
			OutputSchema: node.OutputSchema,
			InputSchema:  node.InputSchema,
		}
	}

	s.Contains(nodeByID[taskIDByName["extract"]].OutputSchema, "properties")
	s.Contains(nodeByID[taskIDByName["transform"]].InputSchema, "extract")
	s.Contains(nodeByID[taskIDByName["transform"]].OutputSchema, "properties")
	s.Empty(nodeByID[taskIDByName["notify"]].InputSchema)

	edgeContractByNames := make(map[string]bool, len(dag.Edges))
	for _, edge := range dag.Edges {
		fromName := s.taskNameForID(tasks, edge.From)
		toName := s.taskNameForID(tasks, edge.To)
		edgeContractByNames[fromName+"->"+toName] = edge.ContractDefined
	}

	s.True(edgeContractByNames["extract->transform"])
	s.False(edgeContractByNames["transform->notify"])
}

func (s *IntegrationTestSuite) TestDataContractsWarnRecordsViolations() {
	alias := fmt.Sprintf("integration-data-contracts-warn-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(dataContractsManifest(alias, jobdef.SchemaValidationWarn, "unknown"))
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, 60*time.Second)

	s.Equal("succeeded", run.Status)

	transform := s.requireRunTaskByName(job.ID, run, "transform")
	s.Equal("succeeded", transform.Status)
	s.Equal("unknown", transform.Output["rows_written"])
	s.Require().NotEmpty(transform.SchemaViolations)
	s.Contains(transform.SchemaViolations[0].Message, "integer")
}

func (s *IntegrationTestSuite) TestDataContractsFailFailsRun() {
	alias := fmt.Sprintf("integration-data-contracts-fail-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(dataContractsManifest(alias, jobdef.SchemaValidationFail, "unknown"))
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, 60*time.Second)

	s.Equal("failed", run.Status)

	transform := s.requireRunTaskByName(job.ID, run, "transform")
	s.Equal("failed", transform.Status)
	s.Contains(transform.Error, "violates declared schema")
	s.Require().NotEmpty(transform.SchemaViolations)
	s.Contains(transform.SchemaViolations[0].Message, "integer")

	notify := s.requireRunTaskByName(job.ID, run, "notify")
	s.NotEqual("succeeded", notify.Status)
}

func (s *IntegrationTestSuite) TestDataContractsRejectIncompatibleConsumerSchema() {
	alias := fmt.Sprintf("integration-data-contracts-invalid-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(invalidDataContractsManifest(alias))
	defer os.RemoveAll(dir)

	output, err := s.runCLIExpectError("job", "apply", "--path", dir, "--server", s.caesiumURL)
	s.Require().Error(err)
	s.Contains(output, `requires key "missing_key"`)

	s.False(s.jobExists(alias))
}

func (s *IntegrationTestSuite) runCLIExpectError(args ...string) (string, error) {
	s.T().Helper()

	cmd := exec.CommandContext(s.T().Context(), s.cliPath, args...)
	cmd.Dir = s.projectRoot
	cmd.Env = os.Environ()
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func (s *IntegrationTestSuite) requireTaskByName(tasks []map[string]any, name string) map[string]any {
	s.T().Helper()

	for _, task := range tasks {
		if stringFromMap(task, "name") == name {
			return task
		}
	}

	s.T().Fatalf("task %s not found", name)
	return nil
}

func (s *IntegrationTestSuite) taskNameForID(tasks []map[string]any, id string) string {
	s.T().Helper()

	for _, task := range tasks {
		if stringFromMap(task, "id") == id {
			return stringFromMap(task, "name")
		}
	}

	return ""
}

func (s *IntegrationTestSuite) requireRunTaskByName(jobID string, run *runResponse, name string) runTaskResponse {
	s.T().Helper()

	nameByID := s.jobTaskNames(jobID)
	for _, task := range run.Tasks {
		if nameByID[task.ID] == name {
			return task
		}
	}

	s.T().Fatalf("run task %s not found", name)
	return runTaskResponse{}
}

func (s *IntegrationTestSuite) jobTaskNames(jobID string) map[string]string {
	s.T().Helper()

	tasks := s.jobTasks(jobID)
	names := make(map[string]string, len(tasks))
	for _, task := range tasks {
		names[stringFromMap(task, "id")] = stringFromMap(task, "name")
	}
	return names
}

func (s *IntegrationTestSuite) jobExists(alias string) bool {
	s.T().Helper()

	query := make([]jobSummary, 0)
	s.getJSON("/v1/jobs", &query)
	for _, job := range query {
		if job.Alias == alias {
			return true
		}
	}
	return false
}

func nestedMapFromAny(v any) map[string]map[string]any {
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	result := make(map[string]map[string]any)
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil
	}
	return result
}

func requiredKeys(schema map[string]any) []string {
	raw, ok := schema["required"]
	if !ok {
		return nil
	}
	values, ok := raw.([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if key, ok := value.(string); ok {
			result = append(result, key)
		}
	}
	return result
}

func dataContractsManifest(alias, schemaValidation, rowsWritten string) string {
	return fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  schemaValidation: %s
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: extract
    image: alpine:3.20
    outputSchema:
      type: object
      properties:
        row_count: { type: integer }
        source: { type: string }
      required: [row_count, source]
    command: ["sh", "-c", "echo '##caesium::output {\"row_count\": \"42\", \"source\": \"warehouse\"}'"]
    next: transform
  - name: transform
    image: alpine:3.20
    dependsOn: [extract]
    inputSchema:
      extract:
        required: [row_count]
        properties:
          row_count: { type: integer }
    outputSchema:
      type: object
      properties:
        rows_written: { type: integer }
      required: [rows_written]
    command: ["sh", "-c", "echo '##caesium::output {\"rows_written\": \"%s\"}'"]
    next: notify
  - name: notify
    image: alpine:3.20
    dependsOn: [transform]
    command: ["sh", "-c", "echo rows=$CAESIUM_OUTPUT_TRANSFORM_ROWS_WRITTEN"]
`, alias, schemaValidation, rowsWritten)
}

func invalidDataContractsManifest(alias string) string {
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
  - name: extract
    image: alpine:3.20
    outputSchema:
      type: object
      properties:
        row_count: { type: integer }
      required: [row_count]
    command: ["sh", "-c", "echo '##caesium::output {\"row_count\": \"42\"}'"]
    next: transform
  - name: transform
    image: alpine:3.20
    dependsOn: [extract]
    inputSchema:
      extract:
        required: [missing_key]
    command: ["sh", "-c", "echo invalid"]
`, alias)
}
