//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// run_list_pagination_test.go drives GET /v1/jobs/:id/runs through its real
// HTTP surface to pin issue #499: the endpoint used to silently ignore
// `limit`/`offset` and always return the job's whole run history, so
// `?limit=1&offset=0` and `?limit=1&offset=1` came back identical. The fix
// must actually bound and page the query, order runs newest-first and
// stably, and reject invalid paging parameters with 400 rather than
// silently falling back to an unbounded read.

// TestRunListPaginationPagesRealRuns creates three real runs of a job and
// asserts that successive limit=1&offset=N pages are non-overlapping,
// ordered newest-first, and together cover exactly the three runs.
func (s *IntegrationTestSuite) TestRunListPaginationPagesRealRuns() {
	alias := fmt.Sprintf("integration-run-list-paging-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: only
    image: alpine:3.23
    command: ["echo", "ok"]
`, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	var runIDs []string // oldest to newest
	for i := 0; i < 3; i++ {
		runID := s.triggerRun(job.ID)
		run := s.awaitRun(job.ID, runID, runTimeout)
		s.Require().Equal("succeeded", run.Status)
		runIDs = append(runIDs, runID)
		// Give created_at a chance to advance so ordering is unambiguous even
		// on a coarse clock; the tiebreak (id) makes this belt-and-braces.
		time.Sleep(250 * time.Millisecond)
	}

	// Unparameterized request: still an array (backward compatible for
	// clients that send no params — ui/src/lib/api.ts's getJobRuns, and every
	// existing test/*.go helper that decodes this straight into
	// []runResponse), now newest-first.
	all := s.fetchRuns(job.ID)
	s.Require().Len(all, 3)
	s.Equal(runIDs[2], all[0].ID, "unparameterized list must be newest first")
	s.Equal(runIDs[1], all[1].ID)
	s.Equal(runIDs[0], all[2].ID)

	// Successive single-row pages must be non-overlapping, ordered, and
	// together cover exactly the three runs — the actual repro from #499,
	// where limit=1&offset=0 and limit=1&offset=1 came back identical.
	seen := map[string]bool{}
	wantOrder := []string{runIDs[2], runIDs[1], runIDs[0]}
	for offset, wantID := range wantOrder {
		page, headers := s.fetchRunsPage(job.ID, fmt.Sprintf("limit=1&offset=%d", offset))
		s.Require().Len(page, 1, "offset %d should return exactly one run", offset)
		s.False(seen[page[0].ID], "offset %d repeated a run an earlier page already returned", offset)
		seen[page[0].ID] = true
		s.Equal(wantID, page[0].ID, "offset %d returned the wrong run", offset)
		s.Equal("3", headers.Get("X-Caesium-Total-Count"), "total must count the whole job, not the page")
		if offset < len(wantOrder)-1 {
			s.Equal(fmt.Sprint(offset+1), headers.Get("X-Caesium-Next-Offset"),
				"a truncated page must say where to continue")
		} else {
			s.Empty(headers.Get("X-Caesium-Next-Offset"), "the final page must not claim a next offset")
		}
	}
	s.Len(seen, 3, "three single-row pages must cover every run exactly once")

	// An offset past the end returns an empty page, not an error.
	tail, _ := s.fetchRunsPage(job.ID, "limit=1&offset=3")
	s.Empty(tail)
}

// TestRunListPaginationRejectsInvalidParams pins the 400 half of the
// contract: an unparseable, negative, or out-of-range paging parameter must
// be rejected rather than silently substituted with a default.
func (s *IntegrationTestSuite) TestRunListPaginationRejectsInvalidParams() {
	alias := fmt.Sprintf("integration-run-list-paging-bad-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: only
    image: alpine:3.23
    command: ["echo", "ok"]
`, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	for _, query := range []string{
		"limit=-1",
		"limit=0",
		"limit=abc",
		"limit=100000",
		"offset=-1",
		"offset=abc",
	} {
		s.requireGETStatus(http.StatusBadRequest, fmt.Sprintf("/v1/jobs/%s/runs?%s", job.ID, query))
	}
}

// fetchRunsPage is like fetchRuns but also returns the response headers, so
// tests can assert on the total/next_offset continuation contract that lives
// there rather than in the (backward-compatible, array-shaped) body.
func (s *IntegrationTestSuite) fetchRunsPage(jobID, query string) ([]runResponse, http.Header) {
	s.T().Helper()

	path := fmt.Sprintf("/v1/jobs/%s/runs", jobID)
	if query != "" {
		path += "?" + query
	}
	resp, err := s.doRequest(http.MethodGet, s.caesiumURL+path, nil)
	s.Require().NoError(err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, resp.StatusCode, string(body))

	var runs []runResponse
	s.Require().NoError(json.Unmarshal(body, &runs))
	return runs, resp.Header
}
