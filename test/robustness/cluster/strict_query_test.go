//go:build integration

package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func queryClient(body string, check func(*http.Request)) *HTTP {
	return &HTTP{Client: &http.Client{Transport: queryRoundTripper(func(req *http.Request) (*http.Response, error) {
		if check != nil {
			check(req)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}}
}

func TestPublicQueryRetainsLegacyCellsAndLimit(t *testing.T) {
	body := `{"limit":200,"row_count":1,"rows":[[42,"text",null,true]],"columns":[{"position":3}]}`
	h := queryClient(body, func(req *http.Request) {
		var payload struct {
			Limit int `json:"limit"`
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil || payload.Limit != 200 {
			t.Fatalf("normalized query payload: %+v, %v", payload, err)
		}
	})
	got, raw, err := h.Query(context.Background(), "http://query.test", "SELECT x", 0)
	if err != nil || string(raw) != body || got.RowCount != 1 || got.Limit != 200 {
		t.Fatalf("Query = %+v, %q, %v", got, raw, err)
	}
	if got.Rows[0][0] != float64(42) || got.Rows[0][1] != "text" || got.Rows[0][2] != nil || got.Rows[0][3] != true || got.Columns[0]["position"] != float64(3) {
		t.Fatalf("public cell types changed: %+v, %+v", got.Rows, got.Columns)
	}
}

func TestQueryLeaseExactGenerationAndMalformedCells(t *testing.T) {
	const id = "e2a55b78-4f0e-4903-a9eb-36a3ff647959"
	for _, tc := range []struct {
		cell string
		want int64
		bad  bool
	}{
		{`9007199254740993`, 9007199254740993, false},
		{`"9223372036854775807"`, 9223372036854775807, false},
		{`1.5`, 0, true}, {`9223372036854775808`, 0, true},
		{`"12 trailing"`, 0, true}, {`null`, 0, true}, {`true`, 0, true},
	} {
		t.Run(tc.cell, func(t *testing.T) {
			h := queryClient(fmt.Sprintf(`{"rows":[[%q,"node",%s,"expiry"]]}`, id, tc.cell), func(req *http.Request) {
				var payload struct {
					Limit int `json:"limit"`
				}
				if err := json.NewDecoder(req.Body).Decode(&payload); err != nil || payload.Limit != 1 {
					t.Fatalf("lease query limit: %+v, %v", payload, err)
				}
			})
			got, err := h.QueryLease(context.Background(), "http://query.test", id)
			if (err != nil) != tc.bad || (!tc.bad && got.Generation != tc.want) {
				t.Fatalf("generation = %+v, %v", got, err)
			}
			if tc.bad && !strings.Contains(err.Error(), "lease generation:") {
				t.Fatal(err)
			}
		})
	}
	h := queryClient(`{}`, func(*http.Request) { t.Fatal("unvalidated UUID reached transport") })
	if _, err := h.QueryLease(context.Background(), "http://query.test", "invalid"); err == nil {
		t.Fatal("invalid UUID accepted")
	}
}

func TestQueryLeaseRejectsWrongIdentityAndShortRows(t *testing.T) {
	const id = "e2a55b78-4f0e-4903-a9eb-36a3ff647959"
	const otherID = "c22a9177-03aa-49e9-9988-984b3e590738"
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "wrong returned run id",
			body: fmt.Sprintf(`{"rows":[[%q,"node",1,"expiry"]]}`, otherID),
			want: "lease run_id " + otherID + " != " + id,
		},
		{
			name: "short lease row",
			body: fmt.Sprintf(`{"rows":[[%q,"node",1]]}`, id),
			want: "lease row has 3 columns, want 4",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := queryClient(tc.body, nil)
			got, err := h.QueryLease(context.Background(), "http://query.test", id)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("QueryLease = %+v, %v; want error %q", got, err, tc.want)
			}
		})
	}
}

func TestQueryTaskRecipesRejectsMalformedCounters(t *testing.T) {
	const id = "e2a55b78-4f0e-4903-a9eb-36a3ff647959"
	for _, tc := range []struct {
		cells string
		field string
	}{
		{`1.5,2,3`, "attempt"}, {`1,"2 trailing",3`, "claim_attempt"},
		{`1,2,9223372036854775808`, "owner_generation"},
	} {
		h := queryClient(fmt.Sprintf(`{"limit":200,"row_count":1,"rows":[[%q,%q,"running","image","command","node",%s,"",null]]}`, id, id, tc.cells), nil)
		if got, err := h.QueryTaskRecipes(context.Background(), "http://query.test", id); err == nil || !strings.Contains(err.Error(), "task_runs."+tc.field) {
			t.Fatalf("%s accepted: %+v, %v", tc.field, got, err)
		}
	}
	h := queryClient(fmt.Sprintf(`{"limit":200,"row_count":1,"rows":[[%q,%q,"running","image","command","node",1,2,9007199254740993,"",null]]}`, id, id), nil)
	got, err := h.QueryTaskRecipes(context.Background(), "http://query.test", id)
	if err != nil || len(got) != 1 || got[0].OwnerGeneration != 9007199254740993 {
		t.Fatalf("exact recipe generation: %+v, %v", got, err)
	}
}

func TestQueryPoliciesRetainStatusAndDecodeLabels(t *testing.T) {
	const id = "e2a55b78-4f0e-4903-a9eb-36a3ff647959"
	h := &HTTP{Client: &http.Client{Transport: queryRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("unavailable"))}, nil
	})}}
	if _, _, err := h.Query(context.Background(), "http://query.test", "SELECT x", 1); err == nil || err.Error() != "query status 503: unavailable" {
		t.Fatal(err)
	}
	if _, err := h.QueryLease(context.Background(), "http://query.test", id); err == nil || err.Error() != "lease query status 503: unavailable" {
		t.Fatal(err)
	}
	h = queryClient(`{`, nil)
	if _, _, err := h.Query(context.Background(), "http://query.test", "SELECT x", 1); err == nil || !strings.HasPrefix(err.Error(), "decode query:") {
		t.Fatal(err)
	}
}
