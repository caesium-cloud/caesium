//go:build integration

package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	jobdefsvc "github.com/caesium-cloud/caesium/api/rest/service/jobdef"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
)

func (s *IntegrationTestSuite) TestContractEnforcementRejectsRemovedParamMappingOutputKey() {
	suffix := time.Now().UnixNano()
	producer := fmt.Sprintf("integration-contract-producer-%d", suffix)
	consumer := fmt.Sprintf("integration-contract-consumer-%d", suffix)

	dir := s.writeContractManifests(map[string]string{
		producer: contractProducerManifest(producer, []string{"customer_id", "row_count"}),
		consumer: contractConsumerManifest(consumer, producer, "reporting", "customer", "customer_id"),
	})
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	status, body := s.postContractApplyManifest(contractProducerManifest(producer, []string{"row_count"}))
	s.Equal(http.StatusConflict, status, string(body))
	s.Contains(string(body), "contract_breaking_change")
	s.Contains(string(body), "customer_id")
	s.Contains(string(body), consumer)
	s.Contains(string(body), "reporting")

	brokenDir := s.writeContractManifests(map[string]string{
		producer: contractProducerManifest(producer, []string{"row_count"}),
	})
	defer os.RemoveAll(brokenDir)
	output, err := s.runCLIExpectError("job", "apply", "--path", brokenDir, "--server", s.caesiumURL)
	s.Require().Error(err)
	s.Contains(output, "customer_id")
	s.Contains(output, consumer)
}

func (s *IntegrationTestSuite) TestContractEnforcementAllowsNonBreakingProducerApply() {
	suffix := time.Now().UnixNano()
	producer := fmt.Sprintf("integration-contract-happy-producer-%d", suffix)
	consumer := fmt.Sprintf("integration-contract-happy-consumer-%d", suffix)

	dir := s.writeContractManifests(map[string]string{
		producer: contractProducerManifest(producer, []string{"customer_id", "row_count"}),
		consumer: contractConsumerManifest(consumer, producer, "analytics", "customer", "customer_id"),
	})
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	status, body := s.postContractApplyManifest(contractProducerManifest(producer, []string{"customer_id", "row_count", "customer_segment"}))
	s.Equal(http.StatusOK, status, string(body))
	s.Require().NotNil(s.requireJobByAlias(producer))
}

func (s *IntegrationTestSuite) writeContractManifests(files map[string]string) string {
	s.T().Helper()

	dir, err := os.MkdirTemp("", "caesium-contract-*")
	s.Require().NoError(err)
	for name, contents := range files {
		path := filepath.Join(dir, name+".job.yaml")
		s.Require().NoError(os.WriteFile(path, []byte(strings.TrimSpace(s.injectEngine(contents))), 0o644))
	}
	return dir
}

func (s *IntegrationTestSuite) postContractApplyManifest(manifest string) (int, []byte) {
	s.T().Helper()

	def, err := schema.Parse([]byte(s.injectEngine(manifest)))
	s.Require().NoError(err)
	payload, err := json.Marshal(jobdefsvc.ApplyRequest{Definitions: []schema.Definition{*def}})
	s.Require().NoError(err)

	resp, err := s.doJSONRequest(http.MethodPost, s.caesiumURL+"/v1/jobdefs/apply", bytes.NewReader(payload))
	s.Require().NoError(err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	return resp.StatusCode, body
}

func contractProducerManifest(alias string, outputKeys []string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
  labels:
    team: producer-team
trigger:
  type: cron
  configuration:
    cron: "0 0 1 1 *"
steps:
  - name: export
    image: alpine:3.23
    command: ["sh", "-c", "echo contract producer"]
    outputSchema:
      type: object
      required: [%s]
      properties:
%s
`, alias, strings.Join(outputKeys, ", "), contractOutputSchemaProperties(outputKeys))
}

func contractOutputSchemaProperties(keys []string) string {
	var b strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&b, "        %s:\n          type: string\n", key)
	}
	return strings.TrimRight(b.String(), "\n")
}

func contractConsumerManifest(alias, producerAlias, team, paramName, outputKey string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
  labels:
    team: %s
trigger:
  type: event
  configuration:
    events:
      - type: run_completed
        source: caesium
        filter:
          job_alias: %s
    paramMapping:
      %s: "$.tasks[0].output.%s"
steps:
  - name: consume
    image: alpine:3.23
    command: ["sh", "-c", "echo contract consumer"]
`, alias, team, producerAlias, paramName, outputKey)
}
