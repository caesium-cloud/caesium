//go:build integration

package test

import (
	"bytes"
	"context"
	"database/sql"
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

func (s *IntegrationTestSuite) TestContractEnforcementAllowBreakingAckLifecycle() {
	suffix := time.Now().UnixNano()
	dataset := fmt.Sprintf("lake.contract_customers_%d", suffix)
	producer := fmt.Sprintf("integration-contract-ack-producer-%d", suffix)
	consumer := fmt.Sprintf("integration-contract-ack-consumer-%d", suffix)
	reason := "planned customer_id migration"

	initialDir := s.writeContractManifests(map[string]string{
		producer: contractDatasetProducerManifest(producer, dataset, []string{"customer_id", "row_count"}, 1),
		consumer: contractDatasetConsumerManifest(consumer, dataset, "reporting"),
	})
	defer os.RemoveAll(initialDir)
	s.runCLI("job", "apply", "--path", initialDir, "--server", s.caesiumURL)

	consumerJob := s.requireJobByAlias(consumer)
	s.Require().NotNil(consumerJob)

	var notifications *replayWebhookCapture
	if s.engineType == "podman" || s.engineType == "kubernetes" {
		s.T().Logf("skipping contract notification webhook assertion under CAESIUM_TEST_ENGINE=%s; webhook host reachability is covered on the docker lane", s.engineType)
	} else {
		notifications = newReplayWebhookCapture(s.T().Context())
		defer notifications.Close()
		channelID := s.createWebhookNotificationChannel("contract-break-"+fmt.Sprint(suffix), notifications.URL())
		s.createNotificationPolicy("contract-break-"+fmt.Sprint(suffix), channelID, []string{"contract_break_declared"}, consumerJob.ID)
	}

	breakingDir := s.writeContractManifests(map[string]string{
		producer: contractDatasetProducerManifest(producer, dataset, []string{"row_count"}, 2),
	})
	defer os.RemoveAll(breakingDir)

	stdout, stderr, err := s.runCLISeparate(
		"job", "apply",
		"--path", breakingDir,
		"--server", s.caesiumURL,
		"--allow-breaking", "dataset="+dataset,
		"--reason", reason,
	)
	s.Require().NoError(err, stderr)
	s.Contains(stdout, "Applied 1 job definition")
	s.Contains(stderr, "contract break declared")
	s.Contains(stderr, dataset)
	s.Contains(stderr, "customer_id")
	s.requireContractAckRecorded(dataset, reason)
	s.requireContractBreakNotification(notifications, producer, dataset, "customer_id", reason)

	stdout, stderr, err = s.runCLISeparate("job", "apply", "--path", breakingDir, "--server", s.caesiumURL)
	s.Require().NoError(err, stderr)
	s.Contains(stdout, "Applied 1 job definition")
	s.Contains(stderr, "deprecation window")
	s.Contains(stderr, dataset)

	consumerDir := s.writeContractManifests(map[string]string{
		consumer: contractDatasetConsumerManifest(consumer, dataset, "reporting"),
	})
	defer os.RemoveAll(consumerDir)

	stdout, stderr, err = s.runCLISeparate("job", "apply", "--path", consumerDir, "--server", s.caesiumURL)
	s.Require().NoError(err, stderr)
	s.Contains(stdout, "Applied 1 job definition")
	s.Contains(stderr, "consuming a deprecated contract")
	s.Contains(stderr, dataset)

	time.Sleep(6 * time.Second)
	output, err := s.runCLIExpectError("job", "apply", "--path", consumerDir, "--server", s.caesiumURL)
	s.Require().Error(err)
	s.Contains(output, "contract_breaking_change")
	s.Contains(output, dataset)
	s.Contains(output, "customer_id")
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

func contractDatasetProducerManifest(alias, dataset string, outputKeys []string, version int) string {
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
    command: ["sh", "-c", "echo contract dataset producer"]
    outputSchema:
      type: object
      required: [%s]
      properties:
%s
    datasets:
      produces:
        - name: %s
          schemaFrom: output
          version: %d
`, alias, strings.Join(outputKeys, ", "), contractOutputSchemaProperties(outputKeys), dataset, version)
}

func contractDatasetConsumerManifest(alias, dataset, team string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
  labels:
    team: %s
trigger:
  type: cron
  configuration:
    cron: "0 0 1 1 *"
steps:
  - name: consume
    image: alpine:3.23
    command: ["sh", "-c", "echo contract dataset consumer"]
    datasets:
      consumes:
        - name: %s
          schema:
            type: object
            required: [customer_id]
            properties:
              customer_id:
                type: string
`, alias, team, dataset)
}

func (s *IntegrationTestSuite) requireContractAckRecorded(dataset, reason string) {
	s.T().Helper()
	if s.engineType == "kubernetes" {
		s.T().Logf("skipping direct contract_acks DB assertion under CAESIUM_TEST_ENGINE=%s; dqlite is not port-forward-reachable", s.engineType)
		return
	}

	catalogDB := s.openIntegrationCatalogDB()
	defer func() { s.Require().NoError(catalogDB.Close()) }()

	var actor, digest string
	var readErr error
	s.Require().Eventually(func() bool {
		actor, digest, readErr = readContractAck(s.T().Context(), catalogDB, dataset, reason)
		return readErr == nil
	}, 10*time.Second, 250*time.Millisecond, "contract_acks should record the intentional break")
	s.Require().NoError(readErr)
	s.Equal("anonymous", actor)
	s.NotEmpty(digest)
}

func readContractAck(ctx context.Context, db *sql.DB, dataset, reason string) (string, string, error) {
	var actor, digest string
	err := db.QueryRowContext(ctx, `
SELECT actor, edge_set_digest
FROM contract_acks
WHERE dataset = ? AND reason = ?
ORDER BY created_at DESC
LIMIT 1
`, dataset, reason).Scan(&actor, &digest)
	return actor, digest, err
}

func (s *IntegrationTestSuite) requireContractBreakNotification(c *replayWebhookCapture, producer, dataset, key, reason string) {
	s.T().Helper()
	if c == nil {
		return
	}
	s.Require().Eventually(func() bool {
		return c.Count() > 0
	}, 30*time.Second, 250*time.Millisecond, "contract_break_declared notification should be delivered")
	body := strings.Join(c.Drain(), "\n")
	s.Contains(body, "contract_break_declared")
	s.Contains(body, producer)
	s.Contains(body, dataset)
	s.Contains(body, key)
	s.Contains(body, reason)
}
