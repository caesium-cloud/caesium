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
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	dqliteclient "github.com/canonical/go-dqlite/v3/client"
	dqlitedriver "github.com/canonical/go-dqlite/v3/driver"
)

type webhookReceiptResponse struct {
	ReceiptID            string `json:"receipt_id"`
	Path                 string `json:"path"`
	Source               string `json:"source"`
	EventMatchedTriggers int    `json:"event_matched_triggers"`
	EventRunsStarted     int    `json:"event_runs_started"`
	HTTPTriggersAccepted int    `json:"http_triggers_accepted"`
	HTTPRunsStarted      int    `json:"http_runs_started"`
}

type persistedWebhookEvent struct {
	ReceiptID            string
	Path                 string
	Source               string
	Status               string
	EventMatchedTriggers int
	EventRunsStarted     int
	HTTPTriggersAccepted int
	HTTPRunsStarted      int
}

func (s *IntegrationTestSuite) TestEventAndTriggerCLIWithWebhookReceiptLog() {
	suffix := time.Now().UnixNano()
	alias := fmt.Sprintf("integration-event-cli-%d", suffix)
	eventType := fmt.Sprintf("cli.created.%d", suffix)
	source := fmt.Sprintf("cli-source-%d", suffix)
	dir := s.writeJobManifest(eventCLIManifest(alias, eventType, source))
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	payload := fmt.Sprintf(`{"env":"w3-alpha","item":{"id":"item-%d"}}`, suffix)
	stdout, stderr, err := s.runCLISeparate(
		"event", "push",
		"--type", eventType,
		"--source", source,
		"--data", payload,
		"--server", s.caesiumURL,
		"--api-key", s.eventIngestAPIKey,
	)
	s.Require().NoError(err, "caesium event push failed:\nstdout=%s\nstderr=%s", stdout, stderr)
	s.Require().True(json.Valid([]byte(stdout)), "caesium event push stdout was not clean JSON:\n%s", stdout)
	s.NotContains(stdout, "warning:", "operator warnings must stay off stdout")
	s.Contains(strings.ToLower(stderr), "warning", "--api-key secrecy warning should land on stderr")

	var pushed eventIngestResponse
	s.Require().NoError(json.Unmarshal([]byte(stdout), &pushed))
	s.Require().NotEmpty(pushed.EventID)
	s.Equal(1, pushed.MatchedTriggers)
	s.Equal(1, pushed.RunsStarted)

	var runID string
	var lastListOut string
	var lastListErrOut string
	s.Require().Eventually(func() bool {
		listOut, listErrOut, listErr := s.runCLISeparate(
			"trigger", "events", alias,
			"--type", eventType,
			"--source", source,
			"--server", s.caesiumURL,
			"--api-key", s.manualTriggerAPIKey,
		)
		lastListOut = listOut
		lastListErrOut = listErrOut
		if listErr != nil || !json.Valid([]byte(listOut)) {
			return false
		}

		var events []triggerEventResponse
		if err := json.Unmarshal([]byte(listOut), &events); err != nil {
			return false
		}
		for _, evt := range events {
			if evt.EventID == pushed.EventID && evt.TriggerID != "" && len(evt.RunsStarted) == 1 {
				runID = evt.RunsStarted[0]
				return true
			}
		}
		return false
	}, 30*time.Second, 500*time.Millisecond, "trigger events CLI should list the matched event")
	s.Require().True(json.Valid([]byte(lastListOut)), "caesium trigger events stdout was not clean JSON:\n%s", lastListOut)
	s.NotContains(lastListOut, "warning:", "operator warnings must stay off stdout")
	s.Contains(strings.ToLower(lastListErrOut), "warning", "--api-key secrecy warning should land on stderr")

	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Equal("succeeded", run.Status)
	s.Equal(fmt.Sprintf("item-%d", suffix), run.Params["item_id"])

	webhookAlias := fmt.Sprintf("integration-webhook-log-%d", suffix)
	hookPath := fmt.Sprintf("/receipt-log/%d", suffix)
	webhookJob := s.createJobWithTriggerConfig(
		webhookAlias,
		nil,
		models.TriggerTypeHTTP,
		map[string]any{"path": hookPath},
	)
	s.Require().NotNil(webhookJob)

	req, err := http.NewRequestWithContext(
		s.T().Context(),
		http.MethodPost,
		fmt.Sprintf("%v/v1/hooks%s", s.caesiumURL, hookPath),
		bytes.NewBufferString(`{"delivery":"receipt-log"}`),
	)
	s.Require().NoError(err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Caesium-Event-Source", "integration-webhook-log")

	resp, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	s.Require().Equal(http.StatusAccepted, resp.StatusCode, string(body))
	s.Require().True(json.Valid(body), "webhook receipt response was not clean JSON:\n%s", body)

	var receipt webhookReceiptResponse
	s.Require().NoError(json.Unmarshal(body, &receipt))
	s.Require().NotEmpty(receipt.ReceiptID)
	s.Equal(strings.Trim(hookPath, "/"), receipt.Path)
	s.Equal("integration-webhook-log", receipt.Source)
	s.Equal(1, receipt.HTTPTriggersAccepted)
	s.Equal(1, receipt.HTTPRunsStarted)

	catalogDB := s.openIntegrationCatalogDB()
	defer func() { s.Require().NoError(catalogDB.Close()) }()

	var persisted persistedWebhookEvent
	var readErr error
	// webhook_events has no read surface yet, so this verifies persistence at the DB level.
	s.Require().Eventually(func() bool {
		persisted, readErr = readPersistedWebhookEvent(s.T().Context(), catalogDB, receipt.ReceiptID)
		return readErr == nil
	}, 30*time.Second, 500*time.Millisecond, "webhook_events should persist the receipt row")
	s.Require().NoError(readErr)
	s.Equal(receipt.ReceiptID, persisted.ReceiptID)
	s.Equal(receipt.Path, persisted.Path)
	s.Equal(receipt.Source, persisted.Source)
	s.Equal("accepted", persisted.Status)
	s.Equal(receipt.EventMatchedTriggers, persisted.EventMatchedTriggers)
	s.Equal(receipt.EventRunsStarted, persisted.EventRunsStarted)
	s.Equal(receipt.HTTPTriggersAccepted, persisted.HTTPTriggersAccepted)
	s.Equal(receipt.HTTPRunsStarted, persisted.HTTPRunsStarted)
}

func (s *IntegrationTestSuite) openIntegrationCatalogDB() *sql.DB {
	nodeAddress := strings.TrimSpace(os.Getenv("CAESIUM_NODE_ADDRESS"))
	if nodeAddress == "" {
		nodeAddress = "127.0.0.1:9001"
	}

	store := dqliteclient.NewInmemNodeStore()
	s.Require().NoError(store.Set(s.T().Context(), []dqliteclient.NodeInfo{{
		ID:      1,
		Address: nodeAddress,
		Role:    dqliteclient.Voter,
	}}))
	drv, err := dqlitedriver.New(store)
	s.Require().NoError(err)
	connector, err := drv.OpenConnector("caesium")
	s.Require().NoError(err)

	return sql.OpenDB(connector)
}

func readPersistedWebhookEvent(ctx context.Context, db *sql.DB, receiptID string) (persistedWebhookEvent, error) {
	var evt persistedWebhookEvent
	err := db.QueryRowContext(ctx, `
SELECT id, path, source, status, event_matched_triggers, event_runs_started, http_triggers_accepted, http_runs_started
FROM webhook_events
WHERE id = ?
`, receiptID).Scan(
		&evt.ReceiptID,
		&evt.Path,
		&evt.Source,
		&evt.Status,
		&evt.EventMatchedTriggers,
		&evt.EventRunsStarted,
		&evt.HTTPTriggersAccepted,
		&evt.HTTPRunsStarted,
	)
	return evt, err
}

func eventCLIManifest(alias, eventType, source string) string {
	return fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: event
  configuration:
    events:
      - type: %s
        source: %s
        filter:
          env: w3-alpha
    paramMapping:
      item_id: $.item.id
steps:
  - name: record
    image: debian:12-slim
    command: ["sh", "-c", "echo event-cli"]
`, alias, eventType, source)
}
