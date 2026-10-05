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
	data    []byte
	err     error
	closed  int
	onClose func()
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
func (b *incompleteEvidenceBody) Close() error {
	b.closed++
	if b.onClose != nil {
		b.onClose()
	}
	return nil
}
func evidenceClient(body string, status int, failure error, calls *int) *client {
	return &client{base: "http://evidence.invalid", http: &http.Client{Transport: evidenceTransport(func(r *http.Request) (*http.Response, error) {
		*calls++
		reader := io.NopCloser(strings.NewReader(body))
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

func TestRunStatusIncompleteSuccessRetriesOrHonorsCancellation(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		name := "retry complete read"
		if cancelled {
			name = "cancel after incomplete read"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			failure := errors.New("incomplete terminal observation")
			posts, gets := 0, 0
			var bodies []*incompleteEvidenceBody
			c := &client{base: "http://evidence.invalid", http: &http.Client{Transport: evidenceTransport(func(r *http.Request) (*http.Response, error) {
				reader := &incompleteEvidenceBody{err: io.EOF}
				status := http.StatusOK
				switch r.Method {
				case http.MethodPost:
					posts++
					status = http.StatusAccepted
					reader.data = []byte(`{"id":"e2a55b78-4f0e-4903-a9eb-36a3ff647959"}`)
				case http.MethodGet:
					gets++
					reader.data = []byte(`{"status":"succeeded"}`)
					if gets == 1 {
						reader.err = failure
						if cancelled {
							reader.onClose = cancel
						}
					}
				default:
					t.Fatal("unexpected request method")
				}
				bodies = append(bodies, reader)
				return &http.Response{StatusCode: status, Body: reader, Header: make(http.Header), Request: r}, nil
			})}}
			r := (&harness{client: c}).triggerAndWait(ctx, "alias", "job")
			if posts != 1 {
				t.Fatalf("blind admission retry: %d", posts)
			}
			if cancelled {
				if gets != 1 || r.status != "timeout" || !errors.Is(r.err, context.Canceled) {
					t.Fatalf("gets=%d result=%+v", gets, r)
				}
			} else if gets != 2 || r.status != "succeeded" || r.err != nil {
				t.Fatalf("gets=%d result=%+v", gets, r)
			}
			for _, body := range bodies {
				if body.closed != 1 {
					t.Fatalf("body closes=%d", body.closed)
				}
			}
		})
	}
}

func TestIncompleteCensusAndQueueCannotPublishSuccessfulObservation(t *testing.T) {
	failure := errors.New("incomplete observation")
	for _, seam := range []string{"census", "queue"} {
		t.Run(seam, func(t *testing.T) {
			body := `[{"id":"run","status":"succeeded"}]`
			if seam == "queue" {
				body = `[]`
			}
			calls := 0
			c := evidenceClient(body, http.StatusOK, failure, &calls)
			h, lg := &harness{client: c}, newLedger(1)
			if seam == "queue" {
				lg.noteQueueDepth(2) // A previous good observation must become stale.
				h.pollQueueOnce(t.Context(), []appliedJob{{id: "job"}}, lg)
				if lg.queueObserved.Load() || lg.queueDepth.Load() != 2 || lg.queueObsStatus != "unavailable" || lg.queueObsFailures != 1 || !strings.Contains(lg.queueObsReason, failure.Error()) {
					t.Fatalf("queue observed=%v status=%s reason=%s", lg.queueObserved.Load(), lg.queueObsStatus, lg.queueObsReason)
				}
			} else {
				lg.runBaselineOK, lg.runBaseline = true, map[string]map[string]bool{"job": {}}
				reconcile := make(chan reconcileItem, 1)
				h.collectCensus(t.Context(), []appliedJob{{id: "job"}}, lg, &openLoopResult{}, reconcile)
				if lg.censusStatus != "unavailable" || lg.censusExtra != 0 || lg.censusTerminal != 0 || len(reconcile) != 0 || !strings.Contains(lg.censusReason, failure.Error()) {
					t.Fatalf("census status=%s extra=%d terminal=%d reason=%s", lg.censusStatus, lg.censusExtra, lg.censusTerminal, lg.censusReason)
				}
			}
			if calls != 1 {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
}

func TestCensusReadErrorRetainsOnlyPreviouslyCompletePages(t *testing.T) {
	failure := errors.New("incomplete next page")
	calls := 0
	var bodies []*incompleteEvidenceBody
	c := &client{base: "http://evidence.invalid", http: &http.Client{Transport: evidenceTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		reader := &incompleteEvidenceBody{data: []byte(`[{"id":"first","status":"succeeded"}]`), err: io.EOF}
		header := make(http.Header)
		if calls == 1 {
			header.Set("X-Caesium-Next-Offset", "1")
		} else {
			if r.URL.Query().Get("offset") != "1" {
				t.Fatal("lost census pagination")
			}
			reader.data, reader.err = []byte(`[{"id":"unconfirmed","status":"succeeded"}]`), failure
		}
		bodies = append(bodies, reader)
		return &http.Response{StatusCode: http.StatusOK, Body: reader, Header: header, Request: r}, nil
	})}}
	runs, err := c.listRuns(t.Context(), "job", 2)
	if calls != 2 || !errors.Is(err, failure) || !strings.Contains(err.Error(), "HTTP 200") || !strings.Contains(err.Error(), "unconfirmed") || len(runs) != 1 || runs[0].ID != "first" {
		t.Fatalf("calls=%d runs=%+v err=%v", calls, runs, err)
	}
	for _, body := range bodies {
		if body.closed != 1 {
			t.Fatalf("body closes=%d", body.closed)
		}
	}
}
