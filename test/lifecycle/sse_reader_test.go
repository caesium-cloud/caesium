//go:build integration

package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const testSSEFrame = `data: {"sequence":7,"type":"run_started"}`

func TestReadEventBacklogCompleteFrameAndIdle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintln(w, testSSEFrame)
		_, _ = fmt.Fprintln(w)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()

	got, err := readEventBacklogWithLimits(context.Background(), &client{base: server.URL}, "run", 0, 20*time.Millisecond, time.Second)
	if err != nil || len(got) != 1 || got[0].Sequence != 7 || got[0].Type != "run_started" {
		t.Fatalf("backlog = %+v, %v", got, err)
	}
}

func TestReadEventBacklogRejectsPartialFrameAtIdleAndEOF(t *testing.T) {
	for _, tc := range []struct {
		name   string
		finish bool
	}{
		{name: "idle"},
		{name: "eof", finish: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintln(w, testSSEFrame)
				w.(http.Flusher).Flush()
				if !tc.finish {
					<-r.Context().Done()
				}
			}))
			defer server.Close()

			_, err := readEventBacklogWithLimits(context.Background(), &client{base: server.URL}, "run", 0, 20*time.Millisecond, time.Second)
			if err == nil {
				t.Fatal("partial frame was accepted")
			}
		})
	}
}

func TestReadEventBacklogReturnsParentAndHardLimitErrors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		hardLimit time.Duration
		cancel    bool
		want      error
	}{
		{name: "parent cancel", hardLimit: time.Second, cancel: true, want: context.Canceled},
		{name: "hard limit", hardLimit: 40 * time.Millisecond, want: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				close(started)
				flusher := w.(http.Flusher)
				ticker := time.NewTicker(5 * time.Millisecond)
				defer ticker.Stop()
				for {
					_, _ = fmt.Fprintln(w, ": ping")
					flusher.Flush()
					select {
					case <-r.Context().Done():
						return
					case <-ticker.C:
					}
				}
			}))
			defer server.Close()

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				_, err := readEventBacklogWithLimits(ctx, &client{base: server.URL}, "run", 0, time.Second, tc.hardLimit)
				done <- err
			}()
			<-started
			if tc.cancel {
				cancel()
			} else {
				defer cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, tc.want) {
					t.Fatalf("read error = %v, want %v", err, tc.want)
				}
			case <-time.After(time.Second):
				t.Fatal("stream reader did not stop")
			}
		})
	}
}

func TestReadEventBacklogPropagatesScannerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, testSSEFrame)
		w.(http.Flusher).Flush()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close() // End the chunked response without its terminal chunk.
		}
	}))
	defer server.Close()

	_, err := readEventBacklogWithLimits(context.Background(), &client{base: server.URL}, "run", 0, time.Second, 2*time.Second)
	if err == nil {
		t.Fatal("scanner read failure was treated as a complete stream")
	}
}

func TestReadEventBacklogAcceptsEOFAfterCompletedFrame(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintln(w, testSSEFrame)
		_, _ = fmt.Fprintln(w)
	}))
	defer server.Close()

	got, err := readEventBacklogWithLimits(context.Background(), &client{base: server.URL}, "run", 0, time.Second, time.Second)
	if err != nil || len(got) != 1 || got[0].Sequence != 7 {
		t.Fatalf("backlog = %+v, %v", got, err)
	}
}
