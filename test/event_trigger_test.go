//go:build integration

package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
)

type eventIngestResponse struct {
	EventID         string `json:"event_id"`
	MatchedTriggers int    `json:"matched_triggers"`
	RunsStarted     int    `json:"runs_started"`
}

type triggerEventResponse struct {
	EventID     string   `json:"event_id"`
	TriggerID   string   `json:"trigger_id"`
	Type        string   `json:"type"`
	Source      string   `json:"source"`
	RunsStarted []string `json:"runs_started"`
}

type ingestedEventResponse struct {
	ID     string          `json:"id"`
	Type   string          `json:"type"`
	Source string          `json:"source"`
	Data   json.RawMessage `json:"data"`
}

func (s *IntegrationTestSuite) TestEventIngestRoutesEventTriggerJob() {
	alias := fmt.Sprintf("integration-event-trigger-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(eventTriggerManifest(alias))
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	triggerID := stringFromMap(s.jobDetailByAlias(alias), "trigger_id")
	s.Require().NotEmpty(triggerID)

	matching := s.postEvent(`{
		"type":"order.created",
		"source":"integration",
		"data":{"env":"w2-alpha","order":{"id":"ord-123"}}
	}`)
	s.Equal(1, matching.MatchedTriggers)
	s.Equal(1, matching.RunsStarted)

	runID := s.requireTriggerEventRun(triggerID, matching.EventID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Equal("succeeded", run.Status)
	s.Equal("ord-123", run.Params["order_id"])

	beforeRuns := len(s.fetchRuns(job.ID))
	nonMatching := s.postEvent(`{
		"type":"order.updated",
		"source":"integration",
		"data":{"env":"w2-alpha","order":{"id":"ord-456"}}
	}`)
	s.NotEmpty(nonMatching.EventID)
	s.Equal(0, nonMatching.MatchedTriggers)
	s.Equal(0, nonMatching.RunsStarted)

	time.Sleep(2 * time.Second)
	s.Equal(beforeRuns, len(s.fetchRuns(job.ID)), "non-matching event should not start a run")
}

func (s *IntegrationTestSuite) TestEventIngestRequiresAPIKey() {
	payload := `{"type":"auth.probe","source":"integration","data":{}}`

	s.requireEventIngestStatus(payload, "wrong-key", true, http.StatusUnauthorized)
	s.requireEventIngestStatus(payload, "", false, http.StatusUnauthorized)
}

func (s *IntegrationTestSuite) TestListIngestedEventsFiltersAndBoundsPagination() {
	suffix := time.Now().UnixNano()
	eventType := fmt.Sprintf("audit.created.%d", suffix)
	source := fmt.Sprintf("integration-list-%d", suffix)

	first := s.postEvent(fmt.Sprintf(`{"type":%q,"source":%q,"data":{"seq":1}}`, eventType, source))
	second := s.postEvent(fmt.Sprintf(`{"type":%q,"source":%q,"data":{"seq":2}}`, eventType, source))

	events := s.listIngestedEvents(eventType, source, 100)
	s.Require().Len(events, 2)
	s.Contains(eventIDs(events), first.EventID)
	s.Contains(eventIDs(events), second.EventID)
	for _, evt := range events {
		s.Equal(eventType, evt.Type)
		s.Equal(source, evt.Source)
	}

	limited := s.listIngestedEvents(eventType, source, 1)
	s.Require().Len(limited, 1)

	overMax := s.listIngestedEvents(eventType, source, 9999)
	s.Require().Len(overMax, 2)
}

func (s *IntegrationTestSuite) TestWebhookPathFiresHTTPAndEventTriggers() {
	suffix := time.Now().UnixNano()
	path := fmt.Sprintf("/double-match/%d", suffix)
	source := fmt.Sprintf("webhook-double-%d", suffix)

	httpJob := s.createJobWithTriggerConfig(
		fmt.Sprintf("integration-webhook-http-%d", suffix),
		nil,
		models.TriggerTypeHTTP,
		map[string]any{"path": path},
	)
	s.Require().NotNil(httpJob)

	eventAlias := fmt.Sprintf("integration-webhook-event-%d", suffix)
	dir := s.writeJobManifest(webhookEventTriggerManifest(eventAlias, source))
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	eventJob := s.requireJobByAlias(eventAlias)
	s.Require().NotNil(eventJob)

	req, err := http.NewRequestWithContext(
		s.T().Context(),
		http.MethodPost,
		fmt.Sprintf("%v/v1/hooks%s", s.caesiumURL, path),
		bytes.NewBufferString(`{"payload":"double-match"}`),
	)
	s.Require().NoError(err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Caesium-Event-Source", source)

	resp, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	s.Require().Equal(http.StatusAccepted, resp.StatusCode, string(body))

	s.Require().Eventually(func() bool {
		return len(s.fetchRuns(httpJob.ID.String())) > 0
	}, 30*time.Second, 500*time.Millisecond, "HTTP trigger run should start")
	s.Require().Eventually(func() bool {
		return len(s.fetchRuns(eventJob.ID)) > 0
	}, 30*time.Second, 500*time.Millisecond, "event trigger run should start")
}

func (s *IntegrationTestSuite) postEvent(payload string) eventIngestResponse {
	s.T().Helper()

	resp, err := s.doEventIngestRequest(http.MethodPost, fmt.Sprintf("%v/v1/events", s.caesiumURL), bytes.NewBufferString(payload))
	s.Require().NoError(err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	s.Require().Equal(http.StatusAccepted, resp.StatusCode, string(body))

	var result eventIngestResponse
	s.Require().NoError(json.Unmarshal(body, &result))
	s.Require().NotEmpty(result.EventID)
	return result
}

func (s *IntegrationTestSuite) requireEventIngestStatus(payload, apiKey string, setAPIKey bool, status int) {
	s.T().Helper()

	resp, err := s.doEventIngestRequestWithKey(
		http.MethodPost,
		fmt.Sprintf("%v/v1/events", s.caesiumURL),
		bytes.NewBufferString(payload),
		apiKey,
		setAPIKey,
	)
	s.Require().NoError(err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	s.Require().Equal(status, resp.StatusCode, string(body))
}

func (s *IntegrationTestSuite) doEventIngestRequestWithKey(method, target string, body io.Reader, apiKey string, setAPIKey bool) (*http.Response, error) {
	s.T().Helper()

	req, err := http.NewRequestWithContext(s.T().Context(), method, target, body)
	if err != nil {
		return nil, err
	}
	if setAPIKey {
		req.Header.Set("X-Caesium-API-Key", apiKey)
	}
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}

func (s *IntegrationTestSuite) listIngestedEvents(eventType, source string, limit int) []ingestedEventResponse {
	s.T().Helper()

	query := url.Values{}
	query.Set("type", eventType)
	query.Set("source", source)
	query.Set("limit", fmt.Sprintf("%d", limit))

	var events []ingestedEventResponse
	s.getJSON("/v1/events/ingested?"+query.Encode(), &events)
	return events
}

func eventIDs(events []ingestedEventResponse) []string {
	ids := make([]string, 0, len(events))
	for _, evt := range events {
		ids = append(ids, evt.ID)
	}
	return ids
}

func (s *IntegrationTestSuite) requireTriggerEventRun(triggerID, eventID string) string {
	s.T().Helper()

	var runID string
	s.Require().Eventually(func() bool {
		var events []triggerEventResponse
		if err := s.tryGetJSON(fmt.Sprintf("/v1/triggers/%s/events?type=order.created&source=integration", triggerID), &events); err != nil {
			return false
		}
		for _, evt := range events {
			if evt.EventID != eventID {
				continue
			}
			if evt.TriggerID != triggerID || evt.Type != "order.created" || evt.Source != "integration" || len(evt.RunsStarted) != 1 {
				return false
			}
			runID = evt.RunsStarted[0]
			return runID != ""
		}
		return false
	}, 30*time.Second, 500*time.Millisecond)
	return runID
}

func eventTriggerManifest(alias string) string {
	return fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: event
  configuration:
    events:
      - type: order.created
        source: integration
        filter:
          env: w2-alpha
    paramMapping:
      order_id: $.order.id
steps:
  - name: record
    image: debian:12-slim
    command: ["sh", "-c", "echo event-trigger"]
`, alias)
}

func webhookEventTriggerManifest(alias, source string) string {
	return fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: event
  configuration:
    events:
      - type: webhook
        source: %s
steps:
  - name: record
    image: debian:12-slim
    command: ["sh", "-c", "echo webhook-event-trigger"]
`, alias, source)
}
