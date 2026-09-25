//go:build integration

package robustness

import (
	"testing"

	"github.com/caesium-cloud/caesium/test/robustness/cluster"
)

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
