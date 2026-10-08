//go:build integration

package robustness

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/bodylimit"
	"github.com/caesium-cloud/caesium/test/robustness/cluster"
)

func incompleteObservationBodies(raw string) []struct {
	name  string
	body  io.ReadCloser
	cause error
} {
	cause := errors.New("read-only response incomplete")
	return []struct {
		name  string
		body  io.ReadCloser
		cause error
	}{
		{"valid JSON read error", &qualificationBody{data: []byte(raw), err: cause}, cause},
		{"exact cap plus one", io.NopCloser(strings.NewReader(raw + strings.Repeat(" ", (4<<20)+1-len(raw)))), bodylimit.ErrTooLarge},
	}
}

func TestSoakQueueDepthRejectsIncompleteEvidence(t *testing.T) {
	const raw = `[{"id":"queued-row"}]`
	member := cluster.Member{Name: "member", IP: "192.0.2.10"}
	for _, tc := range incompleteObservationBodies(raw) {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			h := &cluster.HTTP{Client: &http.Client{Transport: qualificationTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Method != http.MethodGet || req.URL.Path != "/v1/jobs/job/queue" || req.Header.Get("Idempotency-Key") != "" {
					t.Error("queue observation became a mutation or changed route")
				}
				body := tc.body
				if calls > 1 {
					body = io.NopCloser(strings.NewReader(raw))
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, Request: req}, nil
			})}}
			sr := &soakRunner{fe: &faultEnv{httpAPI: h}}
			depth, err := sr.queueDepth(t.Context(), member, "job")
			if depth != 0 || !errors.Is(err, tc.cause) || calls != 1 || strings.Contains(err.Error(), "committed") {
				t.Fatalf("depth=%d err=%v calls=%d", depth, err, calls)
			}
			// The caller may observe again, but it cannot use the first body's
			// valid-looking pending row as successful queue evidence.
			depth, err = sr.queueDepth(t.Context(), member, "job")
			if err != nil || depth != 1 || calls != 2 {
				t.Fatalf("complete observation depth=%d err=%v calls=%d", depth, err, calls)
			}
		})
	}
}

func TestSoakWaitTerminalRetriesIncompleteEvidence(t *testing.T) {
	const (
		jobID = "7c39392d-d1bf-43fc-b803-fce7d606141f"
		runID = "148d4542-8df5-4bf2-8ff4-d542af055498"
		stale = `{"id":"unreadable-run","status":"succeeded"}`
	)
	for _, tc := range incompleteObservationBodies(stale) {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			member := cluster.Member{Name: "member", IP: "192.0.2.10"}
			h := &cluster.HTTP{Client: &http.Client{Transport: qualificationTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Method != http.MethodGet || req.URL.Path != "/v1/jobs/"+jobID+"/runs/"+runID {
					t.Error("terminal observation changed its read-only route")
				}
				body := tc.body
				if calls > 1 {
					body = io.NopCloser(strings.NewReader(`{"id":"` + runID + `","status":"succeeded"}`))
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, Request: req}, nil
			})}}
			sr := &soakRunner{fe: &faultEnv{httpAPI: h, topo: cluster.Topology{Members: []cluster.Member{member}}}}
			got, err := sr.waitTerminal(t.Context(), jobID, runID, 10*time.Second)
			if err != nil || got.ID != runID || got.Status != "succeeded" || calls != 2 {
				t.Fatalf("terminal=%+v err=%v calls=%d", got, err, calls)
			}
		})
	}
}

type cancelObservationBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelObservationBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

func TestSoakWaitTerminalUnreadableReasonSurvivesCancellation(t *testing.T) {
	const (
		jobID = "7c39392d-d1bf-43fc-b803-fce7d606141f"
		runID = "148d4542-8df5-4bf2-8ff4-d542af055498"
	)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cause := errors.New("terminal body incomplete")
	body := &cancelObservationBody{ReadCloser: &qualificationBody{data: []byte(`{"id":"` + runID + `","status":"succeeded"}`), err: cause}, cancel: cancel}
	calls := 0
	h := &cluster.HTTP{Client: &http.Client{Transport: qualificationTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, Request: req}, nil
	})}}
	sr := &soakRunner{fe: &faultEnv{httpAPI: h, topo: cluster.Topology{Members: []cluster.Member{{Name: "member", IP: "192.0.2.10"}}}}}
	got, err := sr.waitTerminal(ctx, jobID, runID, time.Second)
	if err == nil || got.ID != "" || got.Status != "" || calls != 1 || !strings.Contains(err.Error(), cause.Error()) {
		t.Fatalf("terminal=%+v err=%v calls=%d", got, err, calls)
	}
}
