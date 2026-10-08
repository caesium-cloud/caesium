//go:build integration

package test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/stretchr/testify/require"
)

func TestReplaySSEBacklogConsumesBeyondBuffer(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	body := &replaySSEObservedBody{ReadCloser: reader}
	ctx, cancel := context.WithCancel(t.Context())
	capture := newReplaySSECapture(ctx, cancel, body)
	t.Cleanup(func() { require.NoError(t, capture.Close()) })

	var frames strings.Builder
	for sequence := 1; sequence <= 133; sequence++ {
		fmt.Fprintf(&frames, "event: incident_escalated\ndata: {\"sequence\":%d,\"payload\":{\"incident_id\":\"target-%d\"}}\n\n", sequence, sequence)
	}
	written := make(chan error, 1)
	go func() {
		_, err := io.WriteString(writer, frames.String())
		written <- err
	}()
	// Establish the original failure boundary: the reader is already blocked
	// behind a full 128-event buffer before observation begins.
	require.Eventually(t, func() bool { return len(capture.events) == 128 }, 2*time.Second, time.Millisecond)
	events, err := collectReplaySSEWithin(t, capture, 100*time.Millisecond)
	require.NoError(t, err)
	require.Len(t, events, 133)
	for i, evt := range events {
		require.Equal(t, uint64(i+1), evt.Sequence)
		require.Equal(t, event.TypeIncidentEscalated, evt.Type)
	}
	require.JSONEq(t, `{"incident_id":"target-133"}`, string(events[132].Payload))
	select {
	case err := <-written:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("backlog writer did not finish")
	}
	requireReplaySSEClosed(t, capture, body)
}

func TestReplaySSEReaderRefusesIncompleteObservation(t *testing.T) {
	readFailure := errors.New("injected read failure")
	cases := []struct {
		name string
		body io.ReadCloser
		want error
	}{
		{"empty EOF", io.NopCloser(strings.NewReader("")), io.EOF},
		{"EOF after valid frame", io.NopCloser(strings.NewReader("event: incident_escalated\ndata: {\"sequence\":133}\n\n")), io.EOF},
		{"unfinished frame", io.NopCloser(strings.NewReader("event: incident_escalated\ndata: {\"sequence\":133}\n")), io.ErrUnexpectedEOF},
		{"malformed JSON", io.NopCloser(strings.NewReader("event: incident_escalated\ndata: {broken}\n\n")), nil},
		{"missing type", io.NopCloser(strings.NewReader("data: {}\n\n")), nil},
		{"oversized frame", io.NopCloser(strings.NewReader("data: " + strings.Repeat("x", 128*1024) + "\n\n")), nil},
		{"read error", io.NopCloser(&replaySSEErrorReader{err: readFailure}), readFailure},
		{"read error after valid frame", io.NopCloser(io.MultiReader(strings.NewReader("event: incident_escalated\ndata: {\"sequence\":133}\n\n"), &replaySSEErrorReader{err: readFailure})), readFailure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := &replaySSEObservedBody{ReadCloser: tc.body}
			ctx, cancel := context.WithCancel(t.Context())
			capture := newReplaySSECapture(ctx, cancel, body)
			_, err := collectReplaySSEWithin(t, capture, time.Second)
			require.Error(t, err, "partial evidence must not become a successful absence observation")
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
			}
			requireReplaySSEClosed(t, capture, body)
		})
	}
}

func TestReplaySSECaptureCancellationJoinsReader(t *testing.T) {
	for _, blocked := range []string{"read", "send"} {
		t.Run(blocked, func(t *testing.T) {
			var rawBody io.ReadCloser
			if blocked == "read" {
				reader, writer := io.Pipe()
				defer writer.Close()
				rawBody = reader
			} else {
				rawBody = io.NopCloser(strings.NewReader(strings.Repeat("event: incident_escalated\ndata: {}\n\n", 133)))
			}
			body := &replaySSEObservedBody{ReadCloser: rawBody, readStarted: make(chan struct{})}
			ctx, cancel := context.WithCancel(t.Context())
			capture := newReplaySSECapture(ctx, cancel, body)
			select {
			case <-body.readStarted:
			case <-time.After(2 * time.Second):
				t.Fatal("reader did not start")
			}
			if blocked == "send" {
				require.Eventually(t, func() bool { return len(capture.events) == 128 }, 2*time.Second, time.Millisecond)
			}
			cancel()
			_, err := collectReplaySSEWithin(t, capture, time.Second)
			require.ErrorIs(t, err, context.Canceled)
			require.NoError(t, capture.Close()) // repeated cleanup must be safe
			requireReplaySSEClosed(t, capture, body)
		})
	}
}

func TestReplaySSECaptureCloseErrorIsNotSuccessfulEvidence(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	closeFailure := errors.New("injected close failure")
	body := &replaySSEObservedBody{ReadCloser: reader, closeErr: closeFailure}
	ctx, cancel := context.WithCancel(t.Context())
	capture := newReplaySSECapture(ctx, cancel, body)
	_, err := collectReplaySSEWithin(t, capture, time.Millisecond)
	require.ErrorIs(t, err, closeFailure)
	requireReplaySSEClosed(t, capture, body)
}

func TestReplaySSEDrainSurfacesReaderFailure(t *testing.T) {
	body := &replaySSEObservedBody{ReadCloser: io.NopCloser(strings.NewReader("data: malformed\n\n"))}
	ctx, cancel := context.WithCancel(t.Context())
	capture := newReplaySSECapture(ctx, cancel, body)
	select {
	case <-capture.done:
	case <-time.After(2 * time.Second):
		t.Fatal("reader did not report malformed data")
	}
	var observed error
	capture.check = func(err error) { observed = err }
	require.Empty(t, capture.Drain())
	require.Error(t, observed)
	require.Error(t, capture.Close())
	requireReplaySSEClosed(t, capture, body)
}

func TestReplaySSEReaderDecodesMultilineData(t *testing.T) {
	frames := ": ping\n\nevent: incident_escalated\nid: 133\ndata: {\"sequence\":133,\ndata: \"payload\":{\"incident_id\":\"target\"}}\n\n"
	events := make(chan event.Event, 1)
	err := readReplaySSE(t.Context(), strings.NewReader(frames), events)
	require.ErrorIs(t, err, io.EOF)
	select {
	case evt := <-events:
		require.Equal(t, uint64(133), evt.Sequence)
		require.Equal(t, event.TypeIncidentEscalated, evt.Type)
		require.JSONEq(t, `{"incident_id":"target"}`, string(evt.Payload))
	default:
		t.Fatal("multiline SSE event was not decoded")
	}
}

// These controls exercise the helper without an HTTP listener, database,
// daemon, or product internals. They do not substitute for a live auth lane.
func collectReplaySSEWithin(t *testing.T, capture *replaySSECapture, window time.Duration) ([]event.Event, error) {
	t.Helper()
	type result struct {
		events []event.Event
		err    error
	}
	finished := make(chan result, 1)
	go func() {
		events, err := capture.CollectFor(window)
		finished <- result{events, err}
	}()
	select {
	case result := <-finished:
		return result.events, result.err
	case <-time.After(3 * time.Second):
		capture.cancel()
		_ = capture.Close()
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			t.Fatal("SSE observation did not join after reader cleanup")
		}
		t.Fatal("SSE observation exceeded its bound; reader and collector joined during cleanup")
		return nil, nil
	}
}

func requireReplaySSEClosed(t *testing.T, capture *replaySSECapture, body *replaySSEObservedBody) {
	t.Helper()
	select {
	case <-capture.done:
	default:
		t.Fatal("SSE reader was not joined")
	}
	require.Equal(t, int32(1), body.closes.Load(), "body must be closed exactly once")
}

type replaySSEObservedBody struct {
	io.ReadCloser
	closes      atomic.Int32
	closeErr    error
	readStarted chan struct{}
	readOnce    sync.Once
}

func (b *replaySSEObservedBody) Read(p []byte) (int, error) {
	b.readOnce.Do(func() {
		if b.readStarted != nil {
			close(b.readStarted)
		}
	})
	return b.ReadCloser.Read(p)
}

func (b *replaySSEObservedBody) Close() error {
	b.closes.Add(1)
	return errors.Join(b.ReadCloser.Close(), b.closeErr)
}

type replaySSEErrorReader struct{ err error }

func (r *replaySSEErrorReader) Read([]byte) (int, error) { return 0, r.err }
