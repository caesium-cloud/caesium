package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

type EventType string

const (
	TypeJobCreated    EventType = "job_created"
	TypeJobDeleted    EventType = "job_deleted"
	TypeRunStarted    EventType = "run_started"
	TypeRunCompleted  EventType = "run_completed"
	TypeRunFailed     EventType = "run_failed"
	TypeTaskStarted   EventType = "task_started"
	TypeTaskSucceeded EventType = "task_succeeded"
	TypeTaskFailed    EventType = "task_failed"
	TypeTaskSkipped   EventType = "task_skipped"
	TypeLogChunk      EventType = "log_chunk"
)

type Event struct {
	Type      EventType       `json:"type"`
	JobID     uuid.UUID       `json:"job_id,omitempty"`
	RunID     uuid.UUID       `json:"run_id,omitempty"`
	TaskID    uuid.UUID       `json:"task_id,omitempty"`
	Timestamp time.Time       `json:"timestamp"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

type EventsService struct {
	client *Client
}

func (s *EventsService) Stream(ctx context.Context, jobID, runID string, types []EventType) (<-chan Event, error) {
	path := "/events"
	queries := []string{}
	if jobID != "" {
		queries = append(queries, fmt.Sprintf("job_id=%s", jobID))
	}
	if runID != "" {
		queries = append(queries, fmt.Sprintf("run_id=%s", runID))
	}
	if len(types) > 0 {
		tStrs := make([]string, len(types))
		for i, t := range types {
			tStrs[i] = string(t)
		}
		queries = append(queries, fmt.Sprintf("types=%s", strings.Join(tStrs, ",")))
	}

	url := s.client.resolve(path, queries...)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Connection", "keep-alive")

	resp, err := s.client.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("bad status: %s", resp.Status)
	}

	ch := make(chan Event, 100)

	go func() {
		defer resp.Body.Close()
		defer close(ch)

		scanner := bufio.NewScanner(resp.Body)
		var currentType EventType
		var currentData []byte

		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				if currentType != "" && len(currentData) > 0 {
					var evt Event
					if err := json.Unmarshal(currentData, &evt); err == nil {
						// Ensure type is set if not in payload (though payload usually matches)
						if evt.Type == "" {
							evt.Type = currentType
						}
						select {
						case ch <- evt:
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
				continue // Comment/Ping
			}

			parts := bytes.SplitN(line, []byte(":"), 2)
			if len(parts) < 2 {
				continue
			}

			field := string(bytes.TrimSpace(parts[0]))
			value := bytes.TrimPrefix(parts[1], []byte(" "))

			switch field {
			case "event":
				currentType = EventType(value)
			case "data":
				currentData = value
			}
		}
	}()

	return ch, nil
}
