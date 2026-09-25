//go:build integration

package cluster

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestQueryUUIDCanonicalizesSQLCellsAndRejectsInvalidIdentity(t *testing.T) {
	const canonical = "e2a55b78-4f0e-4903-a9eb-36a3ff647959"
	for _, raw := range []string{canonical, strings.ReplaceAll(canonical, "-", ""), strings.ToUpper(canonical)} {
		got, err := queryUUID(raw)
		if err != nil || got != canonical {
			t.Fatalf("queryUUID(%q) = %q, %v; want %q", raw, got, err, canonical)
		}
	}
	for _, bad := range []any{nil, "", "not-a-uuid", 42} {
		if got, err := queryUUID(bad); err == nil {
			t.Fatalf("queryUUID(%v) accepted ambiguous identity %q", bad, got)
		}
	}
}

func TestQueryLeaseAbsentRequiresSuccessfulUnambiguousSQLRead(t *testing.T) {
	const id = "e2a55b78-4f0e-4903-a9eb-36a3ff647959"
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		wantAbsent bool
		wantErr    bool
	}{
		{name: "deleted lease", status: 200, body: `{"row_count":0,"rows":[]}`, wantAbsent: true},
		{name: "live lease", status: 200, body: fmt.Sprintf(`{"row_count":1,"rows":[[%q]]}`, id)},
		{name: "database unavailable", status: 503, body: `{"code":"database_unavailable"}`, wantErr: true},
		{name: "inconsistent row count", status: 200, body: fmt.Sprintf(`{"row_count":0,"rows":[[%q]]}`, id), wantErr: true},
		{name: "invalid row identity", status: 200, body: `{"row_count":1,"rows":[["wrong-run"]]}`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &HTTP{Client: &http.Client{Transport: queryRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/v1/database/query" {
					t.Fatalf("unexpected query path %q", r.URL.Path)
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}}
			absent, err := h.QueryLeaseAbsent(context.Background(), "http://query.test", id)
			if (err != nil) != tc.wantErr || absent != tc.wantAbsent {
				t.Fatalf("QueryLeaseAbsent = (%t, %v), want absent=%t err=%t", absent, err, tc.wantAbsent, tc.wantErr)
			}
		})
	}
}

func TestQueryTaskRecipesKeepsClaimAttemptSeparateFromRetryAttempt(t *testing.T) {
	const runID = "e2a55b78-4f0e-4903-a9eb-36a3ff647959"
	const taskID = "c829d927-22ef-4d57-a250-5137c9257001"
	const instanceID = "af152a84-b713-4583-a944-65bf49a3580f"
	h := &HTTP{Client: &http.Client{Transport: queryRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/database/query" {
			t.Fatalf("unexpected query path %q", r.URL.Path)
		}
		body := fmt.Sprintf(`{"row_count":1,"rows":[[%q,%q,"running","image:v1","[\"sh\"]","node-b",1,2,3,"",null]]}`, instanceID, taskID)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}}
	recipes, err := h.QueryTaskRecipes(context.Background(), "http://query.test", runID)
	if err != nil || len(recipes) != 1 {
		t.Fatalf("QueryTaskRecipes = %+v, %v", recipes, err)
	}
	got := recipes[0]
	if got.ID != instanceID || got.TaskID != taskID || got.Attempt != 1 || got.ClaimAttempt != 2 || got.OwnerGeneration != 3 {
		t.Fatalf("retry and claim identity conflated: %+v", got)
	}
	if got.ResultDigest == "" || got.OutputDigest == "" {
		t.Fatalf("result/output evidence missing: %+v", got)
	}
}

func TestQueryCellDigestDistinguishesNullAndOutputMutation(t *testing.T) {
	nullDigest, err := queryCellDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	emptyDigest, err := queryCellDigest("")
	if err != nil || emptyDigest == nullDigest {
		t.Fatalf("NULL and empty text collapsed: null=%q empty=%q err=%v", nullDigest, emptyDigest, err)
	}
	changedDigest, err := queryCellDigest(`{"stale_probe":"must-not-persist"}`)
	if err != nil || changedDigest == emptyDigest {
		t.Fatalf("output mutation collapsed: empty=%q changed=%q err=%v", emptyDigest, changedDigest, err)
	}
	if _, err := queryCellDigest(42); err == nil {
		t.Fatal("unexpected SQL cell type must be inconclusive")
	}
}

type queryRoundTripper func(*http.Request) (*http.Response, error)

func (f queryRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
