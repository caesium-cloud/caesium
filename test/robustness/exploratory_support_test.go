//go:build integration

package robustness

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/caesium-cloud/caesium/test/robustness/cluster"
)

type startResponseRoundTripper struct {
	mu        sync.Mutex
	responses []string
	keys      []string
	bodies    []string
}

func (rt *startResponseRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	index := len(rt.keys)
	rt.keys = append(rt.keys, req.Header.Get("Idempotency-Key"))
	rt.bodies = append(rt.bodies, string(raw))
	response := rt.responses[min(index, len(rt.responses)-1)]
	return &http.Response{
		StatusCode: http.StatusAccepted,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(response)),
		Request:    req,
	}, nil
}

func newStartResponseRunner(responses ...string) (*soakRunner, *startResponseRoundTripper) {
	rt := &startResponseRoundTripper{responses: responses}
	sr := &soakRunner{
		fe:      &faultEnv{topo: cluster.Topology{Members: []cluster.Member{{Name: "member", IP: "192.0.2.10"}}}},
		client:  &http.Client{Transport: rt},
		faulted: map[string]string{},
	}
	return sr, rt
}

func TestSoakStartClassifiesAcceptedOutcomes(t *testing.T) {
	const (
		runID   = "e2a55b78-4f0e-4903-a9eb-36a3ff647959"
		queueID = "bcefa3b8-50a4-491e-8b4b-6d8cae604f17"
	)
	for _, tc := range []struct {
		name, body, outcome, runID, queueID string
		wantErr                             bool
	}{
		{name: "created valid identity", body: `{"outcome":"created","id":"` + runID + `"}`, outcome: "created", runID: runID},
		{name: "created missing identity", body: `{"outcome":"created"}`, outcome: "created", wantErr: true},
		{name: "created malformed identity", body: `{"outcome":"created","id":"bad"}`, outcome: "created", wantErr: true},
		{name: "queued valid queue", body: `{"outcome":"queued","queue_id":"` + queueID + `"}`, outcome: "queued", queueID: queueID},
		{name: "queued optional queue missing", body: `{"outcome":"queued"}`, outcome: "queued"},
		{name: "queued malformed queue", body: `{"outcome":"queued","queue_id":"bad"}`, outcome: "queued", wantErr: true},
		{name: "dropped valid queue", body: `{"outcome":"dropped","queue_id":"` + queueID + `"}`, outcome: "dropped", queueID: queueID},
		{name: "dropped optional queue missing", body: `{"outcome":"dropped"}`, outcome: "dropped"},
		{name: "dropped malformed queue", body: `{"outcome":"dropped","queue_id":"bad"}`, outcome: "dropped", wantErr: true},
		{name: "skipped concurrency", body: `{"outcome":"skipped"}`, outcome: "skipped"},
		{name: "skipped dataset hold", body: `{"outcome":"skipped","run_id":"` + runID + `"}`, outcome: "skipped", runID: runID},
		{name: "skipped malformed run", body: `{"outcome":"skipped","run_id":"bad"}`, outcome: "skipped", wantErr: true},
		{name: "missing outcome", body: `{}`, wantErr: true},
		{name: "unknown outcome", body: `{"outcome":"accepted"}`, outcome: "accepted", wantErr: true},
		{name: "non-JSON 202", body: `not JSON`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sr, _ := newStartResponseRunner(tc.body)
			got := sr.start(context.Background(), cluster.Member{IP: "192.0.2.10"}, "job-id", "same-key", nil, "")
			if got.HTTPStatus != http.StatusAccepted || got.Outcome != tc.outcome || got.RunID != tc.runID || got.QueueID != tc.queueID || (got.Err != "") != tc.wantErr || got.uncertain() != tc.wantErr {
				t.Fatalf("start outcome = %+v", got)
			}
		})
	}
}

func TestSoakStartUncertainOutcomeReplaysSameKeyAndStaysInconclusive(t *testing.T) {
	for _, body := range []string{`{"outcome":"future"}`, `{"outcome":"created","id":"bad"}`, `not JSON`} {
		t.Run(body, func(t *testing.T) {
			const key = "same-idempotency-key"
			sr, rt := newStartResponseRunner(body, body)
			member := cluster.Member{IP: "192.0.2.10"}
			entry := sr.startTracked(context.Background(), member, "job-id", key, map[string]string{"region": "west"}, "high", "test", nil, true)
			if err := sr.reconcileWithRetry(context.Background(), entry, 2, 0); err == nil {
				t.Fatal("repeated unknown outcome was treated as reconciled")
			}
			if entry.Reconciled || entry.RunID != "" || entry.Err == "" {
				t.Fatalf("unknown identity was not retained as inconclusive: %+v", entry)
			}
			rt.mu.Lock()
			defer rt.mu.Unlock()
			if len(rt.keys) != 3 {
				t.Fatalf("request keys = %v, want initial attempt plus two replays", rt.keys)
			}
			for _, got := range rt.keys {
				if got != key {
					t.Fatalf("replay key = %q, want %q", got, key)
				}
			}
			if rt.bodies[0] != rt.bodies[1] || rt.bodies[1] != rt.bodies[2] {
				t.Fatalf("replay request bodies differ: %q", rt.bodies)
			}
		})
	}
}
