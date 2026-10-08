package faults

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type evidenceTransport func(*http.Request) (*http.Response, error)

func (f evidenceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type evidenceBody struct {
	reader io.Reader
	fault  error
	closed bool
}

func (b *evidenceBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	if b.fault != nil {
		err = b.fault
		b.fault = nil
	}
	return n, err
}
func (b *evidenceBody) Close() error { b.closed = true; return nil }

func TestInterposerRejectsIncompleteRequestBeforeSubmission(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		fault     error
		status    int
	}{
		{"valid JSON with read error", `{"action":"start"}`, errors.New("body fault"), http.StatusBadGateway},
		{"overflow", strings.Repeat("x", (8<<20)+1), nil, http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i, err := NewInterposer("http://upstream.invalid")
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			i.client = &http.Client{Transport: evidenceTransport(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("must not submit") })}
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "http://proxy/runs", nil)
			req.Body = &evidenceBody{reader: strings.NewReader(tc.raw), fault: tc.fault}
			rec := httptest.NewRecorder()
			i.handle(rec, req)
			ops := i.Operations()
			if calls != 0 || rec.Code != tc.status || len(ops) != 1 || ops[0].PossiblyCommitted || ops[0].UpstreamErr == "" {
				t.Fatalf("calls=%d status=%d ops=%+v", calls, rec.Code, ops)
			}
		})
	}
}

func TestInterposerRetainsAcceptedButIncompleteResponseEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		fault     error
		uncertain bool
	}{
		{"valid JSON with read error", `{"id":"accepted"}`, errors.New("body fault"), true},
		{"overflow", strings.Repeat("x", (8<<20)+1), nil, true},
		{"exact cap", strings.Repeat("x", 8<<20), nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i, err := NewInterposer("http://upstream.invalid")
			if err != nil {
				t.Fatal(err)
			}
			body := &evidenceBody{reader: strings.NewReader(tc.raw), fault: tc.fault}
			i.client = &http.Client{Transport: evidenceTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusAccepted, Header: make(http.Header), Body: body}, nil
			})}
			rec := httptest.NewRecorder()
			i.handle(rec, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "http://proxy/runs", strings.NewReader(`{}`)))
			ops := i.Operations()
			if len(ops) != 1 {
				t.Fatalf("ops=%+v", ops)
			}
			op := ops[0]
			wantStatus := http.StatusAccepted
			if tc.uncertain {
				wantStatus = http.StatusBadGateway
			}
			wantBody := tc.raw
			if len(wantBody) > 8<<20 {
				wantBody = wantBody[:8<<20]
			}
			if !body.closed || rec.Code != wantStatus || op.UpstreamStatus != http.StatusAccepted || op.UpstreamBody != wantBody || op.PossiblyCommitted != tc.uncertain || (op.UpstreamErr != "") != tc.uncertain {
				t.Fatalf("closed=%v status=%d upstream=%d bodylen=%d uncertain=%v err=%q", body.closed, rec.Code, op.UpstreamStatus, len(op.UpstreamBody), op.PossiblyCommitted, op.UpstreamErr)
			}
			if tc.uncertain && !strings.Contains(ReconcileNote(op), "do not retry") {
				t.Fatal("uncertain operation lacks reconciliation rule")
			}
		})
	}
}
