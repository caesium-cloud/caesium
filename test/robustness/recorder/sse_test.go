package recorder

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseSSEKeepsIDsAndSkipsComments(t *testing.T) {
	stream := ": ping\n\n" +
		"id: 17\nevent: run_started\ndata: {\"sequence\":17,\"type\":\"run_started\"}\n\n" +
		": ping\n\n" +
		"id: 19\nevent: task_started\ndata: {\"sequence\":19,\"type\":\"task_started\"}\n\n"

	var frames []SSEFrame
	if err := ParseSSE(strings.NewReader(stream), func(f SSEFrame) { frames = append(frames, f) }); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("keep-alive comments were counted as deliveries: %+v", frames)
	}
	if frames[0].Sequence() != 17 || frames[1].Sequence() != 19 {
		t.Fatalf("event ids lost: %+v", frames)
	}
	if frames[0].Event != "run_started" {
		t.Fatalf("event name lost: %+v", frames[0])
	}
}

func TestParseSSEHandlesAFinalFrameWithoutTrailingBlankLine(t *testing.T) {
	var frames []SSEFrame
	if err := ParseSSE(strings.NewReader("id: 3\nevent: x\ndata: {}\n"), func(f SSEFrame) { frames = append(frames, f) }); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("a truncated stream lost its last frame: %+v", frames)
	}
}

func TestFrameWithoutIDIsUnidentifiedNotZero(t *testing.T) {
	f := SSEFrame{ID: "", Data: "{}"}
	if f.Sequence() != 0 {
		t.Fatal("an unparseable id must yield 0 so the caller treats it as unidentified")
	}
}

// Duplicates are legal under DT-EVENT-01 and must survive the subscriber.
func TestSubscriberRetainsDuplicates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Last-Event-ID") != "" {
			w.Header().Set("X-Resumed", r.Header.Get("Last-Event-ID"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(
			"id: 5\nevent: run_started\ndata: {\"sequence\":5,\"type\":\"run_started\",\"run_id\":\"r1\"}\n\n" +
				"id: 5\nevent: run_started\ndata: {\"sequence\":5,\"type\":\"run_started\",\"run_id\":\"r1\"}\n\n" +
				"id: 9\nevent: run_completed\ndata: {\"sequence\":9,\"type\":\"run_completed\",\"run_id\":\"r1\"}\n\n"))
	}))
	defer srv.Close()

	sub := NewSSESubscriber(srv.URL, "r1", "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	gen, err := sub.Connect(ctx, "")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	events := sub.Events()
	if len(events) != 3 {
		t.Fatalf("subscriber de-duplicated legal duplicate deliveries: %+v", events)
	}
	if events[0].RunID != "r1" || events[2].Type != "run_completed" {
		t.Fatalf("payload fields lost: %+v", events)
	}
	if sub.HighestSequence() != 9 {
		t.Fatalf("resume cursor wrong: %d", sub.HighestSequence())
	}
	if len(sub.EventsFromGen(gen)) != 3 {
		t.Fatal("connection generation was not recorded on deliveries")
	}
	conns := sub.Connections()
	if len(conns) != 1 || conns[0].Delivered != 3 {
		t.Fatalf("connection record wrong: %+v", conns)
	}
}

func TestSubscriberRecordsAFailedSubscription(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()

	sub := NewSSESubscriber(srv.URL, "r1", "")
	if _, err := sub.Connect(context.Background(), "42"); err == nil {
		t.Fatal("a 403 subscription must be an error, not an empty history")
	}
	conns := sub.Connections()
	if len(conns) != 1 || conns[0].Status != http.StatusForbidden || conns[0].LastEventID != "42" {
		t.Fatalf("failed subscription not retained with its cursor: %+v", conns)
	}
}

func TestSubscriberSendsTheResumeCursor(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("Last-Event-ID")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sub := NewSSESubscriber(srv.URL, "r1", "key")
	if _, err := sub.Connect(context.Background(), "123"); err != nil {
		t.Fatalf("connect: %v", err)
	}
	select {
	case v := <-got:
		if v != "123" {
			t.Fatalf("Last-Event-ID not sent: %q", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server never saw the request")
	}
}

func TestSubscriberURLFiltersByRun(t *testing.T) {
	if u := NewSSESubscriber("http://x:8080/", "r1", "").URL(); u != "http://x:8080/v1/events?run_id=r1" {
		t.Fatalf("unexpected subscription URL %q", u)
	}
	if u := NewSSESubscriber("http://x:8080", "", "").URL(); u != "http://x:8080/v1/events" {
		t.Fatalf("unexpected unfiltered URL %q", u)
	}
}

// A mixed valid/malformed stream must not drop the bad frame silently: the
// connection records a decode failure so history comparison can fail closed,
// even though a later valid event would otherwise make the run look complete.
func TestSubscriberRecordsPayloadDecodeFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(
			"id: 5\nevent: run_started\ndata: {\"sequence\":5,\"type\":\"run_started\",\"run_id\":\"r1\"}\n\n" +
				"id: 6\nevent: task_started\ndata: not-json\n\n" +
				"id: 9\nevent: run_completed\ndata: {\"sequence\":9,\"type\":\"run_completed\",\"run_id\":\"r1\"}\n\n"))
	}))
	defer srv.Close()

	sub := NewSSESubscriber(srv.URL, "r1", "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := sub.Connect(ctx, ""); err != nil {
		t.Fatalf("connect: %v", err)
	}

	events := sub.Events()
	if len(events) != 3 {
		t.Fatalf("malformed frame was dropped silently: %+v", events)
	}
	if events[0].RunID != "r1" || events[2].RunID != "r1" {
		t.Fatalf("valid payloads lost their run id: %+v", events)
	}
	if events[1].RunID != "" {
		t.Fatalf("undecodable payload should not invent a run id: %+v", events[1])
	}
	conns := sub.Connections()
	if len(conns) != 1 || conns[0].DecodeFailures != 1 || conns[0].Delivered != 3 {
		t.Fatalf("decode failure was not recorded on the connection: %+v", conns)
	}
	if conns[0].Status != http.StatusOK {
		t.Fatalf("a live 200 stream should record its status: %+v", conns)
	}
}

// requireObservingSSE in the hook subtest inspects Status while Connect is
// still blocked in the parse loop. Status used to be written only in finish(),
// so a live stream looked unestablished.
func TestSubscriberRecordsStatusWhileTheStreamIsOpen(t *testing.T) {
	started := make(chan struct{})
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(started)
		<-block
	}))
	defer srv.Close()
	defer close(block)

	sub := NewSSESubscriber(srv.URL, "r1", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _ = sub.Connect(ctx, "") }()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("server never accepted the subscription")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		conns := sub.Connections()
		if len(conns) == 1 && conns[0].Status == http.StatusOK && conns[0].EndedAt.IsZero() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("open stream did not record HTTP 200: %+v", conns)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
