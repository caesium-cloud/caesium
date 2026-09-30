//go:build integration

package test

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// multiPartReadStallBound is one Linux delayed-ACK minimum (40 ms). While the
// dqlite node left Nagle's algorithm on (#588), a query whose result spanned
// more than one 4 KiB response part could not send its second part until the
// client acknowledged the first, and the client's kernel delays that ACK by at
// least this long whenever the connection is in its request/response
// ("pingpong") mode. Unstalled, the reads below take about a millisecond.
const multiPartReadStallBound = 40 * time.Millisecond

// TestMultiPartReadsAreNotStalled drives #588's product fix through the real
// surface. A task prints ~72 KiB, which the executor persists as the task's
// log snapshot, so reading the run (its task rows) and reading the finished
// task's log (the snapshot row itself) are multi-part dqlite results. Each is
// read back-to-back over HTTP, and at most one read in ten may take as long as
// one delayed-ACK stall. Against the pre-fix build this integration server
// stalled 19 of 60 finished-log reads (p90 42.7 ms) and so fails here; the
// fixed build stalled none of either read (max 1.8 ms).
func (s *IntegrationTestSuite) TestMultiPartReadsAreNotStalled() {
	alias := fmt.Sprintf("multipart-read-%d", time.Now().UnixNano())
	const lines, line = 1024, "caesium-588 multi-part read payload 0123456789abcdef0123456789abcdef"
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: http
  configuration:
    path: %s
steps:
  - name: chatty
    image: alpine:3.23
    cache: false
    command: ["sh", "-c", "yes '%s' | head -n %d"]
`, alias, alias, line, lines)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	stdout, err := s.runCLIStdout("run", "start", "--job-id", job.ID, "--server", s.caesiumURL)
	s.Require().NoError(err, "run start stdout=%q", stdout)
	runID, err := uuid.Parse(strings.TrimSpace(stdout))
	s.Require().NoError(err, "run start stdout must be a run id on its own stream, got %q", stdout)
	completed := s.awaitRun(job.ID, runID.String(), runTimeout)
	s.Require().Equal("succeeded", completed.Status, "run must succeed: %s", completed.Error)
	s.Require().Len(completed.Tasks, 1)

	runURL := fmt.Sprintf("%s/v1/jobs/%s/runs/%s", s.caesiumURL, job.ID, runID)
	logURL := fmt.Sprintf("%s/v1/jobs/%s/runs/%s/logs?task_id=%s", s.caesiumURL, job.ID, runID, completed.Tasks[0].ID)

	// Precondition: the finished task's log is served from its persisted
	// snapshot, and that snapshot spans many dqlite response parts. Without it
	// this test would prove nothing about multi-part reads.
	logBody := s.readMultiPartTarget(logURL)
	s.Require().GreaterOrEqual(len(logBody), 64*1024, "the persisted log snapshot must exceed 16 response parts")
	s.Require().Contains(logBody, line)

	// A fresh connection starts in the kernel's quick-ACK mode and leaves the
	// delayed-ACK mode again after each delayed-ACK timeout, so a stalled
	// server delays some reads, not all of them. The first reads are discarded
	// and the verdict counts stalled reads over a long run of back-to-back
	// reads rather than trusting a handful.
	const warmup, measured = 20, 100
	for _, target := range []struct{ name, url string }{
		{"run read", runURL},
		{"finished task log read", logURL},
	} {
		for range warmup {
			s.readMultiPartTarget(target.url)
		}
		samples := make([]time.Duration, 0, measured)
		stalled := 0
		for range measured {
			began := time.Now()
			s.readMultiPartTarget(target.url)
			elapsed := time.Since(began)
			samples = append(samples, elapsed)
			if elapsed >= multiPartReadStallBound {
				stalled++
			}
		}
		slices.Sort(samples)
		median, p90 := samples[len(samples)/2], samples[len(samples)*9/10]
		s.T().Logf("%s: %d of %d reads >= %s, median %s, p90 %s, max %s",
			target.name, stalled, measured, multiPartReadStallBound, median, p90, samples[len(samples)-1])
		if s.engineType == "kubernetes" {
			// Measured and logged, never asserted, on this lane (see below).
			continue
		}
		s.Require().LessOrEqualf(stalled*10, measured,
			"%s: %d of %d reads waited >= %s, as dqlite response parts held back for a delayed ACK do (median %s, p90 %s)",
			target.name, stalled, measured, multiPartReadStallBound, median, p90)
	}
	if s.engineType == "kubernetes" {
		// Under CAESIUM_TEST_ENGINE=kubernetes the runner reaches the server
		// through `kubectl port-forward`, so an HTTP read's wall clock includes
		// the forwarder's streaming hops as well as the server. On the fixed
		// build that lane still saw 18 of 100 run reads at 40-88 ms (median
		// 3.7 ms against ~1 ms in the shared-namespace lanes), which this
		// wall-clock bound cannot tell apart from a dqlite delayed-ACK stall.
		// The bound is enforced on the docker and podman lanes, where the
		// runner shares the server's network namespace, and pkg/db's
		// TestSetNodeNoDelayCoversListenerAndAcceptedSockets checks the socket
		// options directly on Linux.
		s.T().Skipf("multi-part read timings measured, not asserted, under CAESIUM_TEST_ENGINE=%s: reads traverse kubectl port-forward (see the logged figures)", s.engineType)
	}
}

func (s *IntegrationTestSuite) readMultiPartTarget(target string) string {
	s.T().Helper()
	resp, err := s.doRequest(http.MethodGet, target, nil)
	s.Require().NoError(err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, resp.StatusCode, "GET %s: %s", target, string(body))
	return string(body)
}
