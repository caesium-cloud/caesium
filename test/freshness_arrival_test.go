//go:build integration

package test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"
)

type arrivalDatasetState struct {
	Name           string
	Watermark      string
	AdvancedAt     sql.NullString
	WatermarkRunAt sql.NullString
	LastRunID      sql.NullString
}

func (s *IntegrationTestSuite) TestArrivalEventAdvancesSourceDatasetWatermark() {
	if s.engineType == "kubernetes" {
		s.T().Skipf("skipping direct dataset_states DB assertion: dqlite binds to POD_IP under CAESIUM_TEST_ENGINE=%s and is not port-forward-reachable; REST coverage can move to /v1/datasets when Stream E lands", s.engineType)
	}

	suffix := time.Now().UnixNano()
	alias := fmt.Sprintf("integration-arrival-%d", suffix)
	dataset := fmt.Sprintf("raw.arrival.%d", suffix)
	eventType := fmt.Sprintf("arrival.integration.%d", suffix)
	watermark := fmt.Sprintf("vendor/orders/%d.json", suffix)

	dir := s.writeJobManifest(arrivalManifest(alias, dataset, eventType))
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	catalogDB := s.openIntegrationCatalogDB()
	defer func() { s.Require().NoError(catalogDB.Close()) }()

	payload := fmt.Sprintf(`{
		"type":%q,
		"source":"ignored-by-arrival",
		"data":{"detail":{"kind":"orders","objects":[{"key":%q}]}}
	}`, eventType, watermark)

	first := s.postEvent(payload)
	s.NotEmpty(first.EventID)
	s.Equal(0, first.MatchedTriggers)
	s.Equal(0, first.RunsStarted)

	var firstState arrivalDatasetState
	var readErr error
	s.Require().Eventually(func() bool {
		firstState, readErr = readArrivalDatasetState(s.T().Context(), catalogDB, dataset)
		return readErr == nil && firstState.Watermark == watermark && firstState.AdvancedAt.Valid
	}, 30*time.Second, 500*time.Millisecond, "arrival event should advance source dataset state")
	s.Require().NoError(readErr)
	s.False(firstState.LastRunID.Valid, "external arrivals must not record a run id")

	second := s.postEvent(payload)
	s.NotEmpty(second.EventID)
	s.NotEqual(first.EventID, second.EventID)
	s.Equal(0, second.MatchedTriggers)
	s.Equal(0, second.RunsStarted)

	var secondState arrivalDatasetState
	s.Require().Eventually(func() bool {
		secondState, readErr = readArrivalDatasetState(s.T().Context(), catalogDB, dataset)
		return readErr == nil && secondState.Watermark == watermark
	}, 30*time.Second, 500*time.Millisecond, "duplicate arrival should remain readable")
	s.Require().NoError(readErr)
	s.Equal(firstState.AdvancedAt.String, secondState.AdvancedAt.String, "duplicate watermark must not advance advanced_at")
	s.Equal(firstState.WatermarkRunAt.String, secondState.WatermarkRunAt.String, "duplicate watermark must not move watermark_run_at")
	s.False(secondState.LastRunID.Valid, "external duplicate arrivals must not record a run id")
}

func readArrivalDatasetState(ctx context.Context, db *sql.DB, name string) (arrivalDatasetState, error) {
	var state arrivalDatasetState
	err := db.QueryRowContext(ctx, `
SELECT name, watermark, CAST(advanced_at AS TEXT), CAST(watermark_run_at AS TEXT), last_run_id
FROM dataset_states
WHERE namespace = '' AND name = ?
`, name).Scan(
		&state.Name,
		&state.Watermark,
		&state.AdvancedAt,
		&state.WatermarkRunAt,
		&state.LastRunID,
	)
	return state, err
}

func arrivalManifest(alias, dataset, eventType string) string {
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
  configuration: {cron: "0 0 31 2 *"}
steps:
  - name: consume
    image: debian:12-slim
    command: ["sh", "-c", "echo arrival"]
    datasets:
      consumes: [%s]
`, alias, dataset, eventType, dataset)
}
