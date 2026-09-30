package faults

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func upstream(t *testing.T, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"11111111-2222-3333-4444-555555555555","status":"running"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func start(t *testing.T, base string) *Interposer {
	t.Helper()
	in, err := NewInterposer(base)
	if err != nil {
		t.Fatalf("interposer: %v", err)
	}
	if err := in.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = in.Close(context.Background()) })
	return in
}

// send performs one request and always closes the body, so a faulted response
// cannot be mistaken for a leaked connection.
func send(t *testing.T, client *http.Client, method, url, body string) (int, string, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(raw), nil
}

func TestPassThroughDeliversTheResponse(t *testing.T) {
	var hits atomic.Int64
	in := start(t, upstream(t, &hits).URL)

	status, body, err := send(t, &http.Client{Timeout: 10 * time.Second}, http.MethodPost, in.BaseURL()+"/v1/jobs/x/run", "{}")
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if status != http.StatusAccepted || !strings.Contains(body, "11111111") {
		t.Fatalf("pass-through corrupted the response: %d %s", status, body)
	}
	ops := in.Operations()
	if len(ops) != 1 || ops[0].PossiblyCommitted {
		t.Fatalf("a delivered response must not be possibly committed: %+v", ops)
	}
}

// The core of "commit before response loss": the upstream really ran and
// returned 202, and only then did the client lose the answer.
func TestDroppedResponseIsRecordedAsPossiblyCommitted(t *testing.T) {
	var hits atomic.Int64
	in := start(t, upstream(t, &hits).URL)
	in.SetPolicy(Policy{Mode: ModeDropResponse, Method: http.MethodPost, PathContains: "/run", Once: true})

	client := &http.Client{Timeout: 10 * time.Second}
	if _, _, err := send(t, client, http.MethodPost, in.BaseURL()+"/v1/jobs/x/run", "{}"); err == nil {
		t.Fatal("client received a response although it was dropped")
	}

	if hits.Load() != 1 {
		t.Fatalf("upstream was invoked %d times, want exactly 1", hits.Load())
	}
	possibly := in.PossiblyCommitted()
	if len(possibly) != 1 {
		t.Fatalf("expected one possibly committed operation, got %+v", in.Operations())
	}
	op := possibly[0]
	if op.UpstreamStatus != http.StatusAccepted {
		t.Fatalf("controller-observed upstream status lost: %+v", op)
	}
	if !strings.Contains(op.UpstreamBody, "11111111") {
		t.Fatalf("controller-observed upstream body lost: %+v", op)
	}
	if op.Applied != ModeDropResponse {
		t.Fatalf("applied fault not recorded: %+v", op)
	}
	note := ReconcileNote(op)
	if !strings.Contains(note, "POSSIBLY COMMITTED") || !strings.Contains(note, "do not treat the client outcome as a rejection") {
		t.Fatalf("reconcile note does not forbid reading the timeout as a rejection: %q", note)
	}
}

func TestDelayedResponseOutlastsAClientDeadline(t *testing.T) {
	var hits atomic.Int64
	in := start(t, upstream(t, &hits).URL)
	in.SetPolicy(Policy{Mode: ModeDelayResponse, Method: http.MethodPost, PathContains: "/run", Delay: 2 * time.Second})

	client := &http.Client{Timeout: 300 * time.Millisecond}
	if _, _, err := send(t, client, http.MethodPost, in.BaseURL()+"/v1/jobs/x/run", "{}"); err == nil {
		t.Fatal("client did not hit its deadline although the response was delayed")
	}

	deadline := time.Now().Add(10 * time.Second)
	for hits.Load() != 1 || len(in.PossiblyCommitted()) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("delayed operation never settled: hits=%d ops=%+v", hits.Load(), in.Operations())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if in.PossiblyCommitted()[0].UpstreamStatus != http.StatusAccepted {
		t.Fatalf("delayed operation lost its upstream observation: %+v", in.PossiblyCommitted()[0])
	}
}

func TestPolicyOnlyMatchesTheSelectedOperation(t *testing.T) {
	var hits atomic.Int64
	in := start(t, upstream(t, &hits).URL)
	in.SetPolicy(Policy{Mode: ModeDropResponse, Method: http.MethodPost, PathContains: "/run"})

	if _, _, err := send(t, &http.Client{Timeout: 10 * time.Second}, http.MethodGet, in.BaseURL()+"/v1/jobs", ""); err != nil {
		t.Fatalf("unmatched GET was faulted: %v", err)
	}
	if len(in.PossiblyCommitted()) != 0 {
		t.Fatalf("unmatched operation was recorded as possibly committed: %+v", in.Operations())
	}
}

func TestOncePolicyAppliesToTheFirstMatchOnly(t *testing.T) {
	var hits atomic.Int64
	in := start(t, upstream(t, &hits).URL)
	in.SetPolicy(Policy{Mode: ModeDropResponse, Method: http.MethodPost, PathContains: "/run", Once: true})

	client := &http.Client{Timeout: 10 * time.Second}
	if _, _, err := send(t, client, http.MethodPost, in.BaseURL()+"/v1/jobs/x/run", "{}"); err == nil {
		t.Fatal("first matching operation was not dropped")
	}
	if _, _, err := send(t, client, http.MethodPost, in.BaseURL()+"/v1/jobs/x/run", "{}"); err != nil {
		t.Fatalf("second operation was dropped although Once was set: %v", err)
	}
	if n := len(in.PossiblyCommitted()); n != 1 {
		t.Fatalf("expected exactly one faulted operation, got %d", n)
	}
}

func TestReconcileNoteIsEmptyForSettledOperations(t *testing.T) {
	if ReconcileNote(Operation{ID: "op", PossiblyCommitted: false}) != "" {
		t.Fatal("a settled operation must not carry a possibly-committed note")
	}
}

func TestNewInterposerRejectsBadUpstream(t *testing.T) {
	if _, err := NewInterposer("not-a-url"); err == nil {
		t.Fatal("an upstream without a scheme and host must be rejected")
	}
}
