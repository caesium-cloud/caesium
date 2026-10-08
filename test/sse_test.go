//go:build integration

package test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func (s *IntegrationTestSuite) TestSSEStream() {
	// 1. Subscribe to SSE stream
	eventsURL := fmt.Sprintf("%v/v1/events", s.caesiumURL)
	req, err := http.NewRequestWithContext(s.T().Context(), http.MethodGet, eventsURL, nil)
	assert.Nil(s.T(), err)
	req.Header.Set("Accept", "text/event-stream")

	client := &http.Client{}
	//nolint:bodyclose // The SSE response body stays open for the lifetime of the streaming test and is closed on function exit.
	resp, err := client.Do(req)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), http.StatusOK, resp.StatusCode)

	// Ensure body is closed at the end
	defer resp.Body.Close()

	eventChan := make(chan event.Event, 100)
	errChan := make(chan error, 1)

	// Start reading events in a goroutine
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		scanner := bufio.NewScanner(resp.Body)
		var currentType event.Type
		var currentData []byte

		for scanner.Scan() {
			select {
			case <-ctx.Done():
				return
			default:
			}

			line := scanner.Bytes()
			if len(line) == 0 {
				if currentType != "" && len(currentData) > 0 {
					var evt event.Event
					if err := json.Unmarshal(currentData, &evt); err == nil {
						if evt.Type == "" {
							evt.Type = currentType
						}
						select {
						case eventChan <- evt:
						case <-ctx.Done():
							return
						}
					}
				}
				currentType = ""
				currentData = nil
				continue
			}

			if bytes.HasPrefix(line, []byte(":")) {
				continue
			}

			parts := bytes.SplitN(line, []byte(":"), 2)
			if len(parts) < 2 {
				continue
			}

			field := string(bytes.TrimSpace(parts[0]))
			value := bytes.TrimPrefix(parts[1], []byte(" "))

			switch field {
			case "event":
				currentType = event.Type(value)
			case "data":
				currentData = value
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case errChan <- err:
			default:
			}
		}
	}()

	// Give the subscription a moment to establish
	time.Sleep(1 * time.Second)

	// 2. Observe an unrelated real run event before creating the target job.
	unrelatedAlias := fmt.Sprintf("sse-unrelated-job-%d", time.Now().UnixNano())
	unrelatedJob := s.createJob(unrelatedAlias, nil)
	assert.NotNil(s.T(), unrelatedJob)
	unrelatedRunID := uuid.MustParse(s.triggerRun(unrelatedJob.ID.String()))
	unrelatedTimeout := time.After(20 * time.Second)
	unrelatedObserved := false
	for !unrelatedObserved {
		select {
		case evt := <-eventChan:
			if evt.Type == event.TypeRunStarted && evt.RunID == unrelatedRunID {
				unrelatedObserved = true
			}
		case err := <-errChan:
			s.T().Fatalf("SSE stream error before target run: %v", err)
		case <-unrelatedTimeout:
			s.T().Fatalf("unrelated run_started event %s was not observed", unrelatedRunID)
		}
	}
	assert.True(s.T(), unrelatedObserved, "the global SSE stream must deliver the unrelated run")

	// 3. Create and trigger the target run.
	alias := fmt.Sprintf("sse-test-job-%d", time.Now().UnixNano())
	job := s.createJob(alias, nil)
	assert.NotNil(s.T(), job)

	runID := uuid.MustParse(s.triggerRun(job.ID.String()))

	// 4. Verify events
	timeout := time.After(20 * time.Second)

	receivedEvents := make(map[event.Type]bool)
	expectedEvents := []event.Type{
		event.TypeJobCreated,
		event.TypeRunStarted,
		event.TypeTaskStarted,
		event.TypeTaskSucceeded,
		event.TypeRunCompleted,
	}
	expectedSet := make(map[event.Type]bool, len(expectedEvents))
	for _, t := range expectedEvents {
		expectedSet[t] = true
	}

	for len(receivedEvents) < len(expectedEvents) {
		select {
		case evt := <-eventChan:
			if !expectedSet[evt.Type] {
				continue
			}

			match := evt.JobID == job.ID
			if evt.Type != event.TypeJobCreated {
				match = evt.RunID == runID
			}

			if match {
				receivedEvents[evt.Type] = true
			}
		case err := <-errChan:
			s.T().Fatalf("SSE stream error: %v", err)
		case <-timeout:
			var missing []string
			for _, t := range expectedEvents {
				if !receivedEvents[t] {
					missing = append(missing, string(t))
				}
			}
			s.T().Fatalf("Timeout waiting for events. Received: %v, Missing: %s", receivedEvents, strings.Join(missing, ", "))
		}
	}

	for _, t := range expectedEvents {
		assert.True(s.T(), receivedEvents[t], "Expected event %s not received", t)
	}
}
