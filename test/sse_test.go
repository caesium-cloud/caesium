//go:build integration

package test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (s *IntegrationTestSuite) TestSSEStream() {
	trace := &sseHTTPTraceLog{}
	defer trace.dump(s.T())
	client := newSSEHTTPClient(s.T())
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithCancel(s.T().Context())
	defer cancel()
	//nolint:bodyclose // This factory returns a sender; create/trigger helpers close every response.
	sendJSON := s.sseJSONSender(client, trace)
	createJob := func(alias string) *models.Job {
		return s.createJobWithTriggerConfigUsingRequest(alias, nil, models.TriggerTypeCron, nil, sendJSON)
	}
	triggerRun := func(jobID string) string {
		target := fmt.Sprintf("%v/v1/jobs/%s/run", s.caesiumURL, jobID)
		resp, err := sendJSON(http.MethodPost, target, nil)
		s.Require().NoError(err, "POST %s while triggering SSE test run", target)
		defer resp.Body.Close()
		s.Require().Equal(http.StatusAccepted, resp.StatusCode)
		var run runResponse
		s.Require().NoError(json.NewDecoder(resp.Body).Decode(&run))
		s.Require().NotEmpty(run.ID)
		return run.ID
	}

	// 1. Subscribe to SSE stream
	eventsURL := fmt.Sprintf("%v/v1/events", s.caesiumURL)
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace.forRequest("subscribe")), http.MethodGet, eventsURL, nil)
	s.Require().NoError(err, "create GET %s subscription request", eventsURL)
	req.Header.Set("Accept", "text/event-stream")
	s.authorize(req)

	//nolint:bodyclose // The SSE response body stays open for the lifetime of the streaming test and is closed on function exit.
	resp, err := client.Do(req)
	s.Require().NoError(err, "GET %s while subscribing to execution events", eventsURL)

	// Ensure body is closed at the end
	defer resp.Body.Close()
	s.Require().Equal(http.StatusOK, resp.StatusCode, "GET %s while subscribing to execution events", eventsURL)
	s.Require().Contains(resp.Header.Get("Content-Type"), "text/event-stream", "GET %s must return an SSE stream", eventsURL)

	eventChan, errChan, stopReader := startSSEReader(ctx, resp.Body)
	defer stopReader()

	// Give the subscription a moment to establish
	time.Sleep(1 * time.Second)

	// 2. Observe an unrelated real run event before creating the target job.
	unrelatedAlias := fmt.Sprintf("sse-unrelated-job-%d", time.Now().UnixNano())
	unrelatedJob := createJob(unrelatedAlias)
	assert.NotNil(s.T(), unrelatedJob)
	unrelatedRunID := uuid.MustParse(triggerRun(unrelatedJob.ID.String()))
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
	job := createJob(alias)
	assert.NotNil(s.T(), job)

	runID := uuid.MustParse(triggerRun(job.ID.String()))

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

// Own the whole SSE test's connection pool. A new Client without a Transport
// still shares DefaultTransport with earlier integration tests.
func newSSEHTTPClient(t *testing.T) *http.Client {
	t.Helper()
	transport, ok := http.DefaultTransport.(*http.Transport)
	require.True(t, ok, "SSE test requires a clonable HTTP transport")
	return &http.Client{Transport: transport.Clone()}
}

func (s *IntegrationTestSuite) sseJSONSender(client *http.Client, trace *sseHTTPTraceLog) func(string, string, io.Reader) (*http.Response, error) {
	firstRequest := true
	return func(method, target string, body io.Reader) (*http.Response, error) {
		ctx := s.T().Context()
		if firstRequest {
			ctx = httptrace.WithClientTrace(ctx, trace.forRequest("create-unrelated"))
			firstRequest = false
		}
		req, err := http.NewRequestWithContext(ctx, method, target, body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		s.authorize(req)
		//nolint:bodyclose // Response body ownership is transferred to the caller.
		return client.Do(req)
	}
}

// Trace callbacks only buffer safe connection observations; the owning test
// logs them after its requests and reader have finished. No headers or bodies
// are retained, and no callback can log after test cleanup.
type sseHTTPTraceLog struct {
	mu      sync.Mutex
	records []string
}

func (l *sseHTTPTraceLog) record(label, observation string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, fmt.Sprintf("sse_http_trace time=%s request=%s %s", time.Now().UTC().Format(time.RFC3339Nano), label, observation))
}

func (l *sseHTTPTraceLog) forRequest(label string) *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			_, port, err := net.SplitHostPort(info.Conn.LocalAddr().String())
			if err != nil {
				port = "unknown"
			}
			l.record(label, fmt.Sprintf("local_port=%s reused=%t was_idle=%t idle_age=%s", port, info.Reused, info.WasIdle, info.IdleTime))
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			l.record(label, fmt.Sprintf("wrote_error=%v", info.Err))
		},
		GotFirstResponseByte: func() {
			l.record(label, "first_response_byte=true")
		},
	}
}

func (l *sseHTTPTraceLog) dump(t *testing.T) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, record := range l.records {
		t.Log(record)
	}
}

func startSSEReader(parent context.Context, body io.ReadCloser) (<-chan event.Event, <-chan error, func()) {
	ctx, cancel := context.WithCancel(parent)
	events := make(chan event.Event, 100)
	errors := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(body)
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
						case events <- evt:
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
			case errors <- err:
			default:
			}
		}
	}()
	return events, errors, func() {
		cancel()
		_ = body.Close() // Unblock Scan even when the server is quiet.
		<-done
	}
}

func TestSSEClientOwnsConnectionsAndJoinsReader(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	createdID := uuid.New()
	runID := uuid.New()
	postReceived := make(chan struct{})
	streamClosed := make(chan struct{})
	connections := make(chan string, 2)
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/prime" {
			w.Header().Set("X-Test-Connection", r.RemoteAddr)
			_, _ = io.WriteString(w, "primed")
			return
		}
		if r.Header.Get("Authorization") != "Bearer sse-test-token" {
			http.Error(w, "missing test authorization", http.StatusUnauthorized)
			return
		}
		select {
		case connections <- r.RemoteAddr:
		case <-r.Context().Done():
			return
		}
		switch r.URL.Path {
		case "/v1/events":
			defer close(streamClosed)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			http.NewResponseController(w).Flush()
			select {
			case <-postReceived:
				_, _ = fmt.Fprintf(w, "event: run_started\ndata: {\"run_id\":%q}\n\n", runID)
				http.NewResponseController(w).Flush()
			case <-r.Context().Done():
				return
			}
			<-r.Context().Done()
		case "/v1/jobs":
			if r.Method != http.MethodPost {
				http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
				return
			}
			if _, err := io.Copy(io.Discard, r.Body); err != nil {
				http.Error(w, "read request", http.StatusBadRequest)
				return
			}
			attempts.Add(1)
			close(postReceived)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(models.Job{ID: createdID})
		default:
			http.NotFound(w, r)
		}
	}))
	defer func() {
		cancel()
		server.CloseClientConnections()
		server.Close()
	}()
	prime := func() string {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/prime", nil)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		_, err = io.Copy(io.Discard, resp.Body)
		require.NoError(t, err)
		return resp.Header.Get("X-Test-Connection")
	}
	sharedConnection := prime()
	trace := &sseHTTPTraceLog{}
	defer trace.dump(t)
	client := newSSEHTTPClient(t)
	client.Timeout = 5 * time.Second // Bound only this regression's requests, including a stalled stream.
	defer client.CloseIdleConnections()
	s := &IntegrationTestSuite{caesiumURL: server.URL, engineType: "docker", authAPIKey: "sse-test-token"}
	s.SetT(t)
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace.forRequest("subscribe")), http.MethodGet, server.URL+"/v1/events", nil)
	require.NoError(t, err)
	s.authorize(req)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	events, errors, stopReader := startSSEReader(ctx, resp.Body)
	var stopOnce sync.Once
	stopReaderBounded := func() {
		stopOnce.Do(func() {
			done := make(chan struct{})
			go func() {
				stopReader()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("SSE reader cleanup did not finish")
			}
		})
	}
	defer stopReaderBounded()
	//nolint:bodyclose // The sender factory transfers response ownership to the job helper, which closes it.
	created := s.createJobWithTriggerConfigUsingRequest("isolated-sse", nil, models.TriggerTypeCron, nil, s.sseJSONSender(client, trace))
	require.Equal(t, createdID, created.ID)
	select {
	case received := <-events:
		require.Equal(t, event.TypeRunStarted, received.Type)
		require.Equal(t, runID, received.RunID)
	case err := <-errors:
		t.Fatalf("read real SSE event: %v", err)
	case <-ctx.Done():
		t.Fatal("regression deadline elapsed before receiving the real SSE event")
	}
	require.Equal(t, int32(1), attempts.Load())
	nextConnection := func() string {
		select {
		case connection := <-connections:
			return connection
		case <-ctx.Done():
			t.Fatal("regression deadline elapsed before observing request connection")
			return ""
		}
	}
	streamConnection, createConnection := nextConnection(), nextConnection()
	require.NotEqual(t, sharedConnection, streamConnection)
	require.NotEqual(t, sharedConnection, createConnection)
	require.NotEqual(t, streamConnection, createConnection, "the active stream must not block a concurrent POST")
	stopReaderBounded() // Returns only after the scanner goroutine has exited.
	select {
	case <-streamClosed:
	case <-ctx.Done():
		t.Fatal("regression deadline elapsed before server observed stream cleanup")
	}
	client.CloseIdleConnections()
	require.Equal(t, sharedConnection, prime(), "owned cleanup must leave the shared pool usable")
}
