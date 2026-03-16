package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
)

type EventType string

const (
	TypeJobCreated       EventType = "job_created"
	TypeJobDeleted       EventType = "job_deleted"
	TypeJobPaused        EventType = "job_paused"
	TypeJobUnpaused      EventType = "job_unpaused"
	TypeRunStarted       EventType = "run_started"
	TypeRunCompleted     EventType = "run_completed"
	TypeRunFailed        EventType = "run_failed"
	TypeRunTerminal      EventType = "run_terminal"
	TypeTaskStarted      EventType = "task_started"
	TypeTaskSucceeded    EventType = "task_succeeded"
	TypeTaskFailed       EventType = "task_failed"
	TypeTaskSkipped      EventType = "task_skipped"
	TypeTaskRetrying     EventType = "task_retrying"
	TypeTaskReady        EventType = "task_ready"
	TypeTaskClaimed      EventType = "task_claimed"
	TypeTaskLeaseExpired EventType = "task_lease_expired"
	TypeLogChunk         EventType = "log_chunk"
)

type Event struct {
	Sequence  uint64          `json:"sequence,omitempty"`
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
	ch := make(chan Event, 100)

	go func() {
		defer close(ch)
		var lastEventID string
		for {
			if ctx.Err() != nil {
				return
			}

			resp, err := s.openStream(ctx, jobID, runID, types, lastEventID)
			if err != nil {
				log.Error("[console] failed to open event stream", "error", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Second):
					continue
				}
			}

			lastSeenID, streamErr := s.readStream(ctx, resp.Body, ch)
			_ = resp.Body.Close()
			if lastSeenID != "" {
				lastEventID = lastSeenID
			}
			if streamErr != nil && ctx.Err() == nil {
				log.Error("[console] event stream disconnected", "error", streamErr)
			}

			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()

	return ch, nil
}

func (s *EventsService) openStream(ctx context.Context, jobID, runID string, types []EventType, lastEventID string) (*http.Response, error) {
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
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}

	resp, err := s.client.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("bad status: %s", resp.Status)
	}
	return resp, nil
}

func (s *EventsService) readStream(ctx context.Context, body io.ReadCloser, ch chan<- Event) (string, error) {
	scanner := bufio.NewScanner(body)
	var (
		currentID   string
		currentType EventType
		currentData []byte
		lastSeenID  string
	)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			if currentType != "" && len(currentData) > 0 {
				var evt Event
				if err := json.Unmarshal(currentData, &evt); err != nil {
					log.Error("[console] failed to unmarshal event from stream", "error", err, "type", currentType)
				} else {
					if evt.Type == "" {
						evt.Type = currentType
					}
					if evt.Sequence == 0 && currentID != "" {
						if seq, err := strconv.ParseUint(currentID, 10, 64); err == nil {
							evt.Sequence = seq
						}
					}
					select {
					case ch <- evt:
					case <-ctx.Done():
						return lastSeenID, ctx.Err()
					}
					if currentID != "" {
						lastSeenID = currentID
					}
				}
			}
			currentID = ""
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
		case "id":
			currentID = string(value)
		case "event":
			currentType = EventType(value)
		case "data":
			currentData = value
		}
	}

	if err := scanner.Err(); err != nil {
		return lastSeenID, err
	}
	return lastSeenID, io.EOF
}
