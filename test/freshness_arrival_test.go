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
	Partition      string
	Watermark      string
	AdvancedAt     sql.NullString
	VerifiedAt     sql.NullString
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
		firstState, readErr = readArrivalDatasetState(s.T().Context(), catalogDB, dataset, "")
		return readErr == nil && firstState.Watermark == watermark && firstState.AdvancedAt.Valid
	}, 30*time.Second, 500*time.Millisecond, "arrival event should advance source dataset state")
	s.Require().NoError(readErr)
	s.Equal("", firstState.Partition, "an arrival with no partition path advances the whole-dataset row")
	s.False(firstState.VerifiedAt.Valid, "the first advance must not be recorded as a verify")
	s.False(firstState.LastRunID.Valid, "external arrivals must not record a run id")

	second := s.postEvent(payload)
	s.NotEmpty(second.EventID)
	s.NotEqual(first.EventID, second.EventID)
	s.Equal(0, second.MatchedTriggers)
	s.Equal(0, second.RunsStarted)

	var secondState arrivalDatasetState
	s.Require().Eventually(func() bool {
		secondState, readErr = readArrivalDatasetState(s.T().Context(), catalogDB, dataset, "")
		return readErr == nil && secondState.Watermark == watermark && secondState.VerifiedAt.Valid
	}, 30*time.Second, 500*time.Millisecond, "duplicate arrival should verify the source dataset")
	s.Require().NoError(readErr)
	s.Equal("", secondState.Partition)
	s.Equal(firstState.AdvancedAt.String, secondState.AdvancedAt.String, "duplicate watermark must not advance advanced_at")
	s.Equal(firstState.WatermarkRunAt.String, secondState.WatermarkRunAt.String, "duplicate watermark must not move watermark_run_at")
	s.False(secondState.LastRunID.Valid, "external duplicate arrivals must not record a run id")

	// A partition path on the same arrival binding shape records one watermark
	// per partition and leaves the whole-dataset row alone.
	partDataset := fmt.Sprintf("raw.arrival.part.%d", suffix)
	partAlias := fmt.Sprintf("integration-arrival-part-%d", suffix)
	partType := fmt.Sprintf("arrival.integration.part.%d", suffix)
	partDir := s.writeJobManifest(arrivalPartitionManifest(partAlias, partDataset, partType))
	defer os.RemoveAll(partDir)
	s.runCLI("job", "apply", "--path", partDir, "--server", s.caesiumURL)

	s.postEvent(fmt.Sprintf(`{
		"type":%q,
		"source":"ignored-by-arrival",
		"data":{"detail":{"kind":"orders","objects":[{"key":"ignored"}]}}
	}`, partType))
	var missed int
	s.Require().NoError(catalogDB.QueryRowContext(s.T().Context(), `
SELECT COUNT(*) FROM dataset_states WHERE namespace = '' AND name = ?
`, partDataset).Scan(&missed))
	s.Zero(missed, "an unresolved partition path must not write the dataset row or any partition")

	eastWatermark := fmt.Sprintf("vendor/orders/%d-east.json", suffix)
	westWatermark := fmt.Sprintf("vendor/orders/%d-west.json", suffix)
	partPayload := func(partition, key string) string {
		return fmt.Sprintf(`{
			"type":%q,
			"source":"ignored-by-arrival",
			"data":{"detail":{"kind":"orders","partition":%q,"objects":[{"key":%q}]}}
		}`, partType, partition, key)
	}
	s.postEvent(partPayload("east", eastWatermark))
	var east arrivalDatasetState
	s.Require().Eventually(func() bool {
		east, readErr = readArrivalDatasetState(s.T().Context(), catalogDB, partDataset, "east")
		return readErr == nil && east.Watermark == eastWatermark && east.AdvancedAt.Valid
	}, 30*time.Second, 500*time.Millisecond, "east partition should advance")
	s.Require().NoError(readErr)
	s.Equal("east", east.Partition)
	s.False(east.LastRunID.Valid, "partition arrivals must not record a run id")

	s.postEvent(partPayload("west", westWatermark))
	var west arrivalDatasetState
	s.Require().Eventually(func() bool {
		west, readErr = readArrivalDatasetState(s.T().Context(), catalogDB, partDataset, "west")
		return readErr == nil && west.Watermark == westWatermark && west.AdvancedAt.Valid
	}, 30*time.Second, 500*time.Millisecond, "west partition should advance")
	s.Require().NoError(readErr)

	s.postEvent(partPayload("east", eastWatermark))
	var eastVerified arrivalDatasetState
	s.Require().Eventually(func() bool {
		eastVerified, readErr = readArrivalDatasetState(s.T().Context(), catalogDB, partDataset, "east")
		return readErr == nil && eastVerified.VerifiedAt.Valid
	}, 30*time.Second, 500*time.Millisecond, "duplicate east watermark should verify that partition")
	s.Require().NoError(readErr)
	s.Equal(east.AdvancedAt.String, eastVerified.AdvancedAt.String, "verifying east must not advance it")
	s.Equal(east.WatermarkRunAt.String, eastVerified.WatermarkRunAt.String)
	s.False(eastVerified.LastRunID.Valid)

	var westAfter arrivalDatasetState
	westAfter, readErr = readArrivalDatasetState(s.T().Context(), catalogDB, partDataset, "west")
	s.Require().NoError(readErr)
	s.Equal(westWatermark, westAfter.Watermark)
	s.Equal(west.AdvancedAt.String, westAfter.AdvancedAt.String, "verifying east must not touch west")

	_, readErr = readArrivalDatasetState(s.T().Context(), catalogDB, partDataset, "")
	s.ErrorIs(readErr, sql.ErrNoRows, "partition watermarks must not create the whole-dataset row")

	var still arrivalDatasetState
	still, readErr = readArrivalDatasetState(s.T().Context(), catalogDB, dataset, "")
	s.Require().NoError(readErr)
	s.Equal(watermark, still.Watermark)
	s.Equal(firstState.AdvancedAt.String, still.AdvancedAt.String, "partition arrivals must not move another dataset")
	var datasetRows int
	s.Require().NoError(catalogDB.QueryRowContext(s.T().Context(), `
SELECT COUNT(*) FROM dataset_states WHERE namespace = '' AND name = ?
`, dataset).Scan(&datasetRows))
	s.Equal(1, datasetRows, "the unpartitioned dataset stays a single row")
}

func readArrivalDatasetState(ctx context.Context, db *sql.DB, name, partition string) (arrivalDatasetState, error) {
	var state arrivalDatasetState
	err := db.QueryRowContext(ctx, `
SELECT name, partition, watermark, CAST(advanced_at AS TEXT), CAST(verified_at AS TEXT), CAST(watermark_run_at AS TEXT), last_run_id
FROM dataset_states
WHERE namespace = '' AND name = ? AND partition = ?
`, name, partition).Scan(
		&state.Name,
		&state.Partition,
		&state.Watermark,
		&state.AdvancedAt,
		&state.VerifiedAt,
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

func arrivalPartitionManifest(alias, dataset, eventType string) string {
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
          partition: "$.detail.partition"
trigger:
  type: cron
  configuration: {cron: "0 0 31 2 *"}
steps:
  - name: consume
    image: alpine:3.23
    command: ["sh", "-c", "echo arrival-partition"]
    datasets:
      consumes: [%s]
`, alias, dataset, eventType, dataset)
}
