package recorder

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SSEEvent is one delivery observed on the public `/v1/events` stream.
//
// Every delivery is retained, including duplicates and out-of-order arrivals:
// DT-EVENT-01 makes both legal, so de-duplicating here would destroy the
// evidence the history comparison needs. ConnGen identifies which connection
// delivered it, so a reconnection's results can be compared as a set against
// the first connection's rather than assumed to continue a global counter.
type SSEEvent struct {
	ConnGen  int             `json:"conn_gen"`
	At       time.Time       `json:"at"`
	RawID    string          `json:"raw_id,omitempty"`
	Sequence uint64          `json:"sequence"`
	Type     string          `json:"type"`
	RunID    string          `json:"run_id,omitempty"`
	JobID    string          `json:"job_id,omitempty"`
	TaskID   string          `json:"task_id,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
}

// SSEConnection records one subscription attempt, including its resume cursor
// and how it ended. A stream that failed to open is evidence of nothing, so the
// error is retained rather than collapsed into "no events". DecodeFailures
// counts frames whose JSON payload could not be decoded: those are recorder
// loss, not legal at-least-once delivery gaps.
type SSEConnection struct {
	Gen            int       `json:"gen"`
	StartedAt      time.Time `json:"started_at"`
	EndedAt        time.Time `json:"ended_at,omitempty"`
	LastEventID    string    `json:"last_event_id,omitempty"`
	Status         int       `json:"status,omitempty"`
	Delivered      int       `json:"delivered"`
	DecodeFailures int       `json:"decode_failures,omitempty"`
	Err            string    `json:"error,omitempty"`
}

// SSESubscriber subscribes to the existing public `/v1/events` surface. It adds
// no product API: it is an ordinary HTTP client of the shipped endpoint.
type SSESubscriber struct {
	BaseURL   string
	RunID     string
	ManualKey string
	Client    *http.Client

	mu     sync.Mutex
	gen    int
	events []SSEEvent
	conns  []SSEConnection
}

func NewSSESubscriber(baseURL, runID, manualKey string) *SSESubscriber {
	return &SSESubscriber{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		RunID:     runID,
		ManualKey: manualKey,
		// No client timeout: an SSE stream is long lived and is bounded by ctx.
		Client: &http.Client{},
	}
}

func (s *SSESubscriber) URL() string {
	if s.RunID == "" {
		return s.BaseURL + "/v1/events"
	}
	return s.BaseURL + "/v1/events?run_id=" + s.RunID
}

// Events returns every retained delivery, duplicates included.
func (s *SSESubscriber) Events() []SSEEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SSEEvent, len(s.events))
	copy(out, s.events)
	return out
}

// Connections returns the record of every subscription attempt.
func (s *SSESubscriber) Connections() []SSEConnection {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SSEConnection, len(s.conns))
	copy(out, s.conns)
	return out
}

// EventsFromGen returns only the deliveries observed on connection generation g.
func (s *SSESubscriber) EventsFromGen(g int) []SSEEvent {
	var out []SSEEvent
	for _, ev := range s.Events() {
		if ev.ConnGen == g {
			out = append(out, ev)
		}
	}
	return out
}

// HighestSequence is a resume cursor, not a completeness claim: per-run
// sequences are sparse, so it says only "this is the largest id seen".
func (s *SSESubscriber) HighestSequence() uint64 {
	var max uint64
	for _, ev := range s.Events() {
		if ev.Sequence > max {
			max = ev.Sequence
		}
	}
	return max
}

// Connect opens one subscription and blocks until the stream ends or ctx is
// done. lastEventID resumes with the documented `Last-Event-ID` header; an
// empty value starts from the beginning of the store's retained scope. It
// returns the generation number of this connection.
func (s *SSESubscriber) Connect(ctx context.Context, lastEventID string) (int, error) {
	s.mu.Lock()
	s.gen++
	gen := s.gen
	idx := len(s.conns)
	s.conns = append(s.conns, SSEConnection{Gen: gen, StartedAt: time.Now().UTC(), LastEventID: lastEventID})
	s.mu.Unlock()

	finish := func(status, delivered int, err error) {
		s.mu.Lock()
		s.conns[idx].EndedAt = time.Now().UTC()
		s.conns[idx].Status = status
		s.conns[idx].Delivered = delivered
		if err != nil {
			s.conns[idx].Err = err.Error()
		}
		s.mu.Unlock()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL(), nil)
	if err != nil {
		finish(0, 0, err)
		return gen, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	if strings.TrimSpace(s.ManualKey) != "" {
		req.Header.Set("X-Caesium-Manual-Trigger-Key", s.ManualKey)
	}

	resp, err := s.Client.Do(req)
	if err != nil {
		finish(0, 0, err)
		return gen, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		err := fmt.Errorf("/v1/events status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		finish(resp.StatusCode, 0, err)
		return gen, err
	}
	// Record that the stream is live before the parse loop so a still-open
	// subscription can be distinguished from one that never reached HTTP 200.
	s.mu.Lock()
	s.conns[idx].Status = resp.StatusCode
	s.mu.Unlock()

	delivered := 0
	perr := ParseSSE(resp.Body, func(f SSEFrame) {
		ev := SSEEvent{
			ConnGen:  gen,
			At:       time.Now().UTC(),
			RawID:    f.ID,
			Sequence: f.Sequence(),
			Type:     f.Event,
			Data:     json.RawMessage(f.Data),
		}
		var payload struct {
			Sequence uint64 `json:"sequence"`
			Type     string `json:"type"`
			RunID    string `json:"run_id"`
			JobID    string `json:"job_id"`
			TaskID   string `json:"task_id"`
		}
		s.mu.Lock()
		if err := json.Unmarshal([]byte(f.Data), &payload); err == nil {
			if ev.Sequence == 0 {
				ev.Sequence = payload.Sequence
			}
			if ev.Type == "" {
				ev.Type = payload.Type
			}
			ev.RunID = payload.RunID
			ev.JobID = payload.JobID
			ev.TaskID = payload.TaskID
		} else {
			s.conns[idx].DecodeFailures++
		}
		s.events = append(s.events, ev)
		s.mu.Unlock()
		delivered++
	})
	finish(resp.StatusCode, delivered, perr)
	return gen, perr
}

// SSEFrame is one parsed server-sent-event frame.
type SSEFrame struct {
	ID    string
	Event string
	Data  string
}

// Sequence parses the frame's `id:` as the event-store sequence. A frame with
// no parseable id yields 0, which the caller must treat as "unidentified",
// never as sequence zero.
func (f SSEFrame) Sequence() uint64 {
	n, err := strconv.ParseUint(strings.TrimSpace(f.ID), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// ParseSSE reads text/event-stream frames and calls emit for each one that
// carries data. Comment lines (`: ping`) are skipped, as the specification
// requires, but they are not treated as deliveries.
func ParseSSE(r io.Reader, emit func(SSEFrame)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var frame SSEFrame
	var data []string
	flush := func() {
		if len(data) == 0 {
			frame = SSEFrame{}
			return
		}
		frame.Data = strings.Join(data, "\n")
		emit(frame)
		frame = SSEFrame{}
		data = nil
	}
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, ":"):
			// comment / keep-alive
		case strings.HasPrefix(line, "id:"):
			frame.ID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		case strings.HasPrefix(line, "event:"):
			frame.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	flush()
	return scanner.Err()
}
