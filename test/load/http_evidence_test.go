package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/bodylimit"
)

type evidenceTransport func(*http.Request) (*http.Response, error)

func (f evidenceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type incompleteEvidenceBody struct {
	data []byte
	err  error
}

func (b *incompleteEvidenceBody) Read(p []byte) (int, error) {
	if len(b.data) == 0 {
		return 0, b.err
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	if len(b.data) == 0 {
		return n, b.err
	}
	return n, nil
}
func (*incompleteEvidenceBody) Close() error { return nil }
func evidenceClient(body string, status int, failure error, calls *int) *client {
	return &client{base: "http://evidence.invalid", http: &http.Client{Transport: evidenceTransport(func(r *http.Request) (*http.Response, error) {
		*calls++
		var reader io.ReadCloser = io.NopCloser(strings.NewReader(body))
		if failure != nil {
			reader = &incompleteEvidenceBody{data: []byte(body), err: failure}
		}
		return &http.Response{StatusCode: status, Body: reader, Header: make(http.Header), Request: r}, nil
	})}}
}
func TestReadOnlyLoadSeamsRejectValidJSONWithReadError(t *testing.T) {
	failure := errors.New("incomplete body")
	for _, tc := range []struct {
		name, body string
		run        func(*client) error
	}{
		{"listJobs", "[]", func(c *client) error { _, err := c.listJobs(t.Context()); return err }},
		{"runStatus", `{"status":"succeeded"}`, func(c *client) error { _, err := c.getRunStatus(t.Context(), "job", "run"); return err }},
		{"run", `{"id":"run","status":"succeeded"}`, func(c *client) error { _, err := c.getRun(t.Context(), "job", "run"); return err }},
		{"census", "[]", func(c *client) error { _, err := c.listRuns(t.Context(), "job", 1); return err }},
		{"queue", "[]", func(c *client) error { _, err := c.queueDepth(t.Context(), "job"); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			err := tc.run(evidenceClient(tc.body, http.StatusOK, failure, &calls))
			if !errors.Is(err, failure) || calls != 1 {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
		})
	}
}
func TestLoadStartIncompleteEvidenceIsUncertainAndNeverRetried(t *testing.T) {
	failure := errors.New("incomplete body")
	id := "e2a55b78-4f0e-4903-a9eb-36a3ff647959"
	body := `{"id":"` + id + `"}`
	for _, status := range []int{http.StatusAccepted, http.StatusServiceUnavailable} {
		calls := 0
		h := &harness{client: evidenceClient(body, status, failure, &calls)}
		got, err := h.startRun(t.Context(), "job")
		var uncertain *uncertainStartError
		if got != id || !errors.As(err, &uncertain) || !errors.Is(err, failure) || uncertain.status != status || string(uncertain.raw) != body || calls != 1 {
			t.Fatalf("run=%q err=%v calls=%d", got, err, calls)
		}
	}
}
func TestCappedLoadEvidenceRejectsOverflowAndPreservesPartialStatus(t *testing.T) {
	const capBytes = 1 << 20
	failure := errors.New("incomplete body")
	for _, tc := range []struct {
		name, body    string
		failure, want error
	}{
		{"validJSONReadError", "{}", failure, failure},
		{"validJSONOverflow", "{}" + strings.Repeat(" ", capBytes-1), nil, bodylimit.ErrTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := evidenceClient(tc.body, http.StatusOK, tc.failure, &calls)
			if _, err := (&dockerStatsClient{http: c.http}).sample(t.Context(), "container"); !errors.Is(err, tc.want) {
				t.Fatal(err)
			}
			h := &harness{client: evidenceClient(tc.body, http.StatusAccepted, tc.failure, &calls)}
			lg := newLedger(1)
			h.offer(context.Background(), appliedJob{id: "job"}, 0, time.Now(), lg, make(chan reconcileItem, 1))
			a := lg.arrivals[0]
			if a.outcome != outcomeUncertain || a.statusCode != http.StatusAccepted || a.reason == "" {
				t.Fatalf("%+v", a)
			}
			if calls != 2 {
				t.Fatalf("blind retry: %d calls", calls)
			}
		})
	}
}

func TestUncertainOfferPossibleIdentityDoesNotCountAsAcknowledgedAdmission(t *testing.T) {
	const id = "e2a55b78-4f0e-4903-a9eb-36a3ff647959"
	calls := 0
	failure := errors.New("incomplete admission")
	h := &harness{client: evidenceClient(`{"id":"`+id+`"}`, http.StatusAccepted, failure, &calls)}
	lg := newLedger(1)
	h.offer(t.Context(), appliedJob{id: "job"}, 0, time.Now(), lg, make(chan reconcileItem, 1))
	a := lg.arrivals[0]
	if a.outcome != outcomeUncertain || a.runID != "" || !strings.Contains(a.reason, id) || a.statusCode != http.StatusAccepted || calls != 1 {
		t.Fatalf("%+v calls=%d", a, calls)
	}
	results := ledgerResults(lg)
	if results[0].runID != "" || results[0].status != outcomeUncertain {
		t.Fatalf("%+v", results)
	}
	report := buildReport(config{jobCount: 1}, results, metricSample{}, metricSample{}, nil, time.Second)
	if report.runsObserved != 0 || report.runsUncertain != 1 {
		t.Fatalf("observed=%d uncertain=%d", report.runsObserved, report.runsUncertain)
	}
}
