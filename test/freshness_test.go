//go:build integration

package test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type persistedDatasetState struct {
	Status    string
	Reason    string
	Watermark string
	// Consumed is the raw consumed_watermarks JSON blob (empty when the column
	// is NULL — i.e. no run has recorded an input snapshot for this dataset).
	Consumed string
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

// TestFreshnessConsumedSnapshotTakenAtRunStart proves the consumed-input
// snapshot is the view the run ACTUALLY consumed. The input is an
// arrival-bound source dataset, so a single ingest POST advances its watermark
// in ~one round trip — which is what lets the second advance land reliably
// inside the consumer's run window without an expensive sleep. Crediting the
// consumer's output with that mid-run advance (the old completion-time read)
// would make a freshness comparison report the output as caught-up with an
// input the run never read.
func (s *IntegrationTestSuite) TestFreshnessConsumedSnapshotTakenAtRunStart() {
	if s.engineType == "kubernetes" {
		s.T().Skipf("freshness state DB assertions need direct dqlite access; covered on docker + podman lanes, not CAESIUM_TEST_ENGINE=%s", s.engineType)
	}

	// The consumer step must still be running when the second arrival lands.
	// The window it has to cover is one HTTP POST plus the capturer's async
	// advance; the guard below fails loudly if that ever stops holding.
	const consumerSleepSeconds = 30

	suffix := time.Now().UnixNano()
	inputDataset := fmt.Sprintf("integration.snapshot.in.%d", suffix)
	outputDataset := fmt.Sprintf("integration.snapshot.out.%d", suffix)
	alias := fmt.Sprintf("integration-snapshot-consumer-%d", suffix)
	eventType := fmt.Sprintf("snapshot.integration.%d", suffix)
	earlyWatermark := fmt.Sprintf("vendor/orders/%d-early.json", suffix)
	lateWatermark := fmt.Sprintf("vendor/orders/%d-late.json", suffix)

	dir := s.writeJobManifest(freshnessArrivalConsumerManifest(alias, inputDataset, outputDataset, eventType, consumerSleepSeconds))
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	consumer := s.requireJobByAlias(alias)
	s.Require().NotNil(consumer)

	catalogDB := s.openIntegrationCatalogDB()
	defer func() { s.Require().NoError(catalogDB.Close()) }()

	arrive := func(watermark string) {
		s.T().Helper()
		s.postEvent(fmt.Sprintf(`{
		"type":%q,
		"source":"ignored-by-arrival",
		"data":{"detail":{"kind":"orders","objects":[{"key":%q}]}}
	}`, eventType, watermark))
		s.Require().Eventually(func() bool {
			st, ok, err := readPersistedDatasetState(s.T().Context(), catalogDB, inputDataset)
			return err == nil && ok && st.Watermark == watermark
		}, 30*time.Second, 250*time.Millisecond, "arrival should advance the input dataset to %s", watermark)
	}

	// 1. The input's watermark as the consumer's run will begin.
	arrive(earlyWatermark)

	// 2. Start the slow consumer. The run row is already "running" when
	//    triggerRun returns — the run_started event is published just after that
	//    transaction commits, so give the capturer a moment to observe it.
	runID := s.triggerRun(consumer.ID)
	s.awaitRunStatus(consumer.ID, runID, runTimeout, "running")
	time.Sleep(3 * time.Second)

	// 3. Advance the input MID-RUN. This run never sees the late value.
	arrive(lateWatermark)

	// Guard: if the consumer already finished, the advance was not mid-run and
	// the scenario proves nothing — fail loudly rather than pass vacuously.
	s.Require().NotContains([]string{"succeeded", "failed", "cancelled"},
		s.fetchRun(consumer.ID, runID).Status,
		"consumer run reached a terminal status before the input advanced; the mid-run window was too short")

	s.Require().Equal("succeeded", s.awaitRun(consumer.ID, runID, runTimeout).Status)

	// 4. The produced dataset must record the START-time input view.
	var out persistedDatasetState
	s.Require().Eventually(func() bool {
		var ok bool
		var err error
		out, ok, err = readPersistedDatasetState(s.T().Context(), catalogDB, outputDataset)
		return err == nil && ok && strings.TrimSpace(out.Consumed) != ""
	}, 90*time.Second, 500*time.Millisecond, "consumer output should record a consumed-input snapshot")

	var consumed map[string]string
	s.Require().NoError(json.Unmarshal([]byte(out.Consumed), &consumed))
	s.Equal(earlyWatermark, consumed[inputDataset],
		"consumed snapshot must be the input view at the consumer run's START, not the mid-run advance: %v", consumed)
}

func readPersistedDatasetState(ctx context.Context, db *sql.DB, dataset string) (persistedDatasetState, bool, error) {
	var state persistedDatasetState
	var watermark, consumed sql.NullString
	err := db.QueryRowContext(ctx, `
SELECT status, reason, watermark, consumed_watermarks
FROM dataset_states
WHERE namespace = '' AND name = ?
`, dataset).Scan(&state.Status, &state.Reason, &watermark, &consumed)
	if err == sql.ErrNoRows {
		return persistedDatasetState{}, false, nil
	}
	if err != nil {
		return persistedDatasetState{}, false, err
	}
	state.Watermark = watermark.String
	state.Consumed = consumed.String
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

// freshnessArrivalConsumerManifest declares an arrival-bound external source
// (so an ingest POST advances its watermark without running anything) and a
// consumer step that stays running long enough for a second arrival to land
// mid-run.
func freshnessArrivalConsumerManifest(alias, inputDataset, outputDataset, eventType string, sleepSeconds int) string {
	return fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  datasets:
    sources:
      - name: %s
        expectedEvery: 24h
        external: true
        arrival:
          event:
            type: %s
            filter:
              detail.kind: orders
          watermark: "$.detail.objects[0].key"
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: consume
    image: alpine:3.23
    command: ["sh", "-c", "sleep %d; echo '##caesium::output {\"wm\":\"consumed\"}'"]
    datasets:
      consumes:
        - %s
      produces:
        - name: %s
          freshness: 10m
          watermark:
            key: wm
`, alias, inputDataset, eventType, sleepSeconds, inputDataset, outputDataset)
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
