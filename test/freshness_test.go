//go:build integration

package test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type persistedDatasetState struct {
	Status string
	Reason string
}

func (s *IntegrationTestSuite) TestFreshnessEvaluatorStateTransitions() {
	if s.engineType == "kubernetes" {
		s.T().Skipf("freshness state DB assertions need direct dqlite access; covered on docker + podman lanes, not CAESIUM_TEST_ENGINE=%s", s.engineType)
	}

	suffix := time.Now().UnixNano()
	rawDataset := fmt.Sprintf("integration.raw.%d", suffix)
	martDataset := fmt.Sprintf("integration.mart.%d", suffix)
	producerAlias := fmt.Sprintf("integration-freshness-producer-%d", suffix)
	consumerAlias := fmt.Sprintf("integration-freshness-consumer-%d", suffix)

	dir := s.writeJobManifest(freshnessProducerManifest(producerAlias, rawDataset, suffix))
	defer os.RemoveAll(dir)
	consumerPath := filepath.Join(dir, "consumer.job.yaml")
	s.Require().NoError(os.WriteFile(consumerPath, []byte(strings.TrimSpace(s.injectEngine(freshnessConsumerManifest(consumerAlias, rawDataset, martDataset)))), 0o644))

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	producer := s.requireJobByAlias(producerAlias)
	consumer := s.requireJobByAlias(consumerAlias)
	s.Require().NotNil(producer)
	s.Require().NotNil(consumer)

	catalogDB := s.openIntegrationCatalogDB()
	defer func() { s.Require().NoError(catalogDB.Close()) }()

	_, found, err := readPersistedDatasetState(s.T().Context(), catalogDB, rawDataset)
	s.Require().NoError(err)
	s.False(found, "produced dataset should start unknown before any run advances it")

	var martState persistedDatasetState
	s.Require().Eventually(func() bool {
		var ok bool
		var readErr error
		martState, ok, readErr = readPersistedDatasetState(s.T().Context(), catalogDB, martDataset)
		return readErr == nil && ok && martState.Status == "stale-upstream"
	}, 90*time.Second, 500*time.Millisecond, "consumer output should become stale-upstream while raw input has not arrived")

	consumerRunsBefore := len(s.fetchRuns(consumer.ID))
	s.Zero(consumerRunsBefore, "stale-upstream must not derive a consumer run before upstream arrival")

	runID := s.triggerRun(producer.ID)
	run := s.awaitRun(producer.ID, runID, runTimeout)
	s.Equal("succeeded", run.Status)

	var rawState persistedDatasetState
	s.Require().Eventually(func() bool {
		var ok bool
		var readErr error
		rawState, ok, readErr = readPersistedDatasetState(s.T().Context(), catalogDB, rawDataset)
		return readErr == nil && ok && rawState.Status == "fresh"
	}, 90*time.Second, 500*time.Millisecond, "producer output should become fresh after run completion advances its watermark")
}

func readPersistedDatasetState(ctx context.Context, db *sql.DB, dataset string) (persistedDatasetState, bool, error) {
	var state persistedDatasetState
	err := db.QueryRowContext(ctx, `
SELECT status, reason
FROM dataset_states
WHERE namespace = '' AND name = ?
`, dataset).Scan(&state.Status, &state.Reason)
	if err == sql.ErrNoRows {
		return persistedDatasetState{}, false, nil
	}
	if err != nil {
		return persistedDatasetState{}, false, err
	}
	return state, true, nil
}

func freshnessProducerManifest(alias, dataset string, watermark int64) string {
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
  - name: produce
    image: alpine:3.23
    command: ["sh", "-c", "echo '##caesium::output {\"wm\":\"%d\"}'"]
    datasets:
      produces:
        - name: %s
          freshness: 10m
          maxStaleness: 20m
          watermark:
            key: wm
`, alias, watermark, dataset)
}

func freshnessConsumerManifest(alias, inputDataset, outputDataset string) string {
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
  - name: consume
    image: alpine:3.23
    command: ["sh", "-c", "echo consuming"]
    datasets:
      consumes:
        - %s
      produces:
        - name: %s
          freshness: 1ns
          watermark:
            key: wm
`, alias, inputDataset, outputDataset)
}
