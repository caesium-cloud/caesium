//go:build integration

package robustness

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/test/robustness/cluster"
)

type persistedRoundTripper func(*http.Request) (*http.Response, error)

func (f persistedRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func persistedQueryClient(body string) *cluster.HTTP {
	return &cluster.HTTP{Client: &http.Client{Transport: persistedRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}}
}

func TestPersistedEventsExactSequencesAndBooleanCells(t *testing.T) {
	const runID = "e2a55b78-4f0e-4903-a9eb-36a3ff647959"
	for _, cell := range []string{`true`, `1`, `"t"`} {
		h := persistedQueryClient(fmt.Sprintf(`{"limit":10,"row_count":1,"rows":[[9007199254740993,"done",%q,%s,null]]}`, runID, cell))
		rows, scope, err := readPersistedEvents(context.Background(), h, "http://query.test", runID, 10)
		if err != nil || len(rows) != 1 || rows[0].Sequence != 9007199254740993 || !rows[0].BusPending || !scope.Complete || scope.Min != 9007199254740993 || scope.Max != scope.Min {
			t.Fatalf("event evidence %s: %+v, %+v, %v", cell, rows, scope, err)
		}
	}
	for _, tc := range []struct {
		sequence, pending string
		field             string
	}{
		{`-1`, `false`, "sequence"}, {`1.5`, `false`, "sequence"},
		{`"1 trailing"`, `false`, "sequence"}, {`9223372036854775808`, `false`, "sequence"},
		{`1`, `"unknown"`, "bus_dispatch_pending"}, {`1`, `0.5`, "bus_dispatch_pending"},
	} {
		h := persistedQueryClient(fmt.Sprintf(`{"limit":10,"row_count":1,"rows":[[%s,"done",%q,%s]]}`, tc.sequence, runID, tc.pending))
		if rows, _, err := readPersistedEvents(context.Background(), h, "http://query.test", runID, 10); err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Fatalf("malformed %s accepted: %+v, %v", tc.field, rows, err)
		}
	}
}

func TestLatestSequencePreservesEmptyStoreAndExactCursor(t *testing.T) {
	for _, tc := range []struct {
		body string
		want uint64
		bad  bool
	}{
		{`{"rows":[]}`, 0, false}, {`{"rows":[[]]}`, 0, false}, {`{"rows":[[null]]}`, 0, false},
		{`{"rows":[[9007199254740993]]}`, 9007199254740993, false},
		{`{"rows":[[true]]}`, 1, false}, {`{"rows":[[false]]}`, 0, false},
		{`{"rows":[[-1]]}`, 0, true}, {`{"rows":[[1.5]]}`, 0, true},
		{`{"rows":[["2 trailing"]]}`, 0, true}, {`{"rows":[[9223372036854775808]]}`, 0, true},
		{`{"rows":[[1]]}{}`, 0, true},
	} {
		got, err := latestSequence(context.Background(), persistedQueryClient(tc.body), "http://query.test")
		if (err != nil) != tc.bad || (!tc.bad && got != tc.want) {
			t.Fatalf("%s: cursor=%d, %v", tc.body, got, err)
		}
	}
}

func TestPersistedPageCompleteHonorsServerClampAndTruncation(t *testing.T) {
	rows := make([][]any, 1000)
	clamped := cluster.QueryResponse{Limit: 1000, RowCount: 1000, Rows: rows, Truncated: true}
	if complete, err := persistedPageComplete(clamped, 2000); err != nil || complete {
		t.Fatalf("truncated 1000-row page passed requested 2000-row scope: complete=%t err=%v", complete, err)
	}
	clamped.Truncated = false
	if complete, err := persistedPageComplete(clamped, 2000); err != nil || !complete {
		t.Fatalf("untruncated exact-limit page rejected: complete=%t err=%v", complete, err)
	}
	for _, bad := range []cluster.QueryResponse{
		{Limit: 0, RowCount: 0},
		{Limit: 2001, RowCount: 0},
		{Limit: 1, RowCount: 0, Rows: [][]any{{1}}},
		{Limit: 1, RowCount: 2, Rows: [][]any{{1}, {2}}},
	} {
		if complete, err := persistedPageComplete(bad, 2000); err == nil {
			t.Fatalf("malformed event page passed: complete=%t page=%+v", complete, bad)
		}
	}
}
