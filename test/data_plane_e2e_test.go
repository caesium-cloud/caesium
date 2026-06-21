//go:build integration

package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// These scenarios drive the data-plane-memory features (caesium why,
// reproducibility receipt + verify, and the cross-job lineage impact query)
// through their REAL surfaces against the live integration server. They exist
// because the original ship of those features passed CI on unit tests over
// synthetic state while the end-to-end paths were hollow (the impact query
// never persisted rows; the receipt CLI contaminated stdout). Each assertion
// here would have turned those bugs red.

// runCLIStdout runs the CLI capturing stdout and stderr SEPARATELY, returning
// stdout only. The shared runCLIRaw helper merges the streams (CombinedOutput),
// which is precisely why no existing test could detect log lines leaking onto
// stdout and corrupting machine-readable command output. On failure the
// returned error carries stderr so the cause is visible in CI (the command's
// own diagnostics go to stderr, which the stdout-only return would otherwise
// drop).
func (s *IntegrationTestSuite) runCLIStdout(args ...string) (string, error) {
	s.T().Helper()
	cmd := exec.CommandContext(s.T().Context(), s.cliPath, args...)
	cmd.Dir = s.projectRoot
	cmd.Env = os.Environ()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// whyExplanation mirrors the fields of the `caesium why --json` output that the
// assertions below read.
type whyExplanation struct {
	Verdict string `json:"verdict"`
	Diff    *struct {
		Changes []struct {
			Field string `json:"field"`
		} `json:"changes"`
	} `json:"diff"`
}

// TestCaesiumWhyExplainsHitAndMiss verifies the EXPLAIN feature end-to-end:
// `caesium why` reports CACHE_HIT when inputs are unchanged and a field-level
// CACHE_MISS when a run param changes.
func (s *IntegrationTestSuite) TestCaesiumWhyExplainsHitAndMiss() {
	alias := fmt.Sprintf("e2e-why-%d", time.Now().UnixNano())
	// NOTE: do NOT set a step-level `engine:` — writeJobManifest/injectEngine
	// inserts the runtime engine (docker/podman/kubernetes) per CI tier, and a
	// hardcoded engine would duplicate that key. Caching is enabled job-wide.
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
  cache:
    enabled: true
trigger: { type: cron, configuration: { cron: "0 2 * * *" } }
steps:
  - name: produce
    image: alpine:3.23
    command: ["sh","-c","echo '##caesium::output {\"rows\": \"100\"}'"]
`, alias)
	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	// Run #1 (cold) then run #2 (same inputs → cache hit on the cacheable step).
	run1 := s.triggerRun(job.ID)
	s.Require().Equal("succeeded", s.awaitRun(job.ID, run1, 90*time.Second).Status)
	run2 := s.triggerRun(job.ID)
	s.Require().Equal("succeeded", s.awaitRun(job.ID, run2, 90*time.Second).Status)

	hit := s.parseWhy(job.ID, run2, "produce")
	s.Equal("CACHE_HIT", hit.Verdict, "second run of an unchanged step must be a cache hit")

	// Run #3 with a changed run param → cache miss attributable to that field.
	run3 := s.triggerRunWithParams(job.ID, map[string]string{"scenario": "changed"})
	s.Require().Equal("succeeded", s.awaitRun(job.ID, run3, 90*time.Second).Status)
	miss := s.parseWhy(job.ID, run3, "produce")
	s.Equal("CACHE_MISS", miss.Verdict, "a changed run param must produce a cache miss")
	s.Require().NotNil(miss.Diff, "a miss vs a prior run must carry a field-level diff")
	foundParam := false
	for _, c := range miss.Diff.Changes {
		if strings.HasPrefix(c.Field, "runParams.") {
			foundParam = true
		}
	}
	s.True(foundParam, "the changed run param must appear as a discriminating field, got %+v", miss.Diff.Changes)
}

func (s *IntegrationTestSuite) parseWhy(jobID, runID, task string) whyExplanation {
	s.T().Helper()
	out, err := s.runCLIStdout("why", runID, "--job-id", jobID, "--task", task, "--json", "--server", s.caesiumURL)
	s.Require().NoError(err, "caesium why failed:\n%s", out)
	s.Require().True(json.Valid([]byte(out)), "caesium why --json stdout was not valid JSON (log contamination?):\n%s", out)
	var exp whyExplanation
	s.Require().NoError(json.Unmarshal([]byte(out), &exp))
	return exp
}

// TestReproducibilityReceiptRoundTrip verifies the REPRODUCE feature end-to-end
// AND guards the documented `receipt get > file` → `verify file` workflow: the
// receipt printed to stdout must be clean JSON (no leaked log lines), and the
// committed file must verify against the run's persisted state.
func (s *IntegrationTestSuite) TestReproducibilityReceiptRoundTrip() {
	alias := fmt.Sprintf("e2e-receipt-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
  cache:
    enabled: true
    pinDigests: true
trigger: { type: cron, configuration: { cron: "0 2 * * *" } }
steps:
  - name: produce
    image: alpine:3.23
    command: ["sh","-c","echo '##caesium::output {\"rows\": \"1\"}'"]
`, alias)
	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	runID := s.triggerRun(job.ID)
	s.Require().Equal("succeeded", s.awaitRun(job.ID, runID, 90*time.Second).Status)

	// `receipt get` stdout MUST be clean JSON — this is the regression for log
	// lines leaking onto stdout and breaking the round-trip.
	receipt, err := s.runCLIStdout("receipt", "get", "--job-id", job.ID, "--run-id", runID, "--server", s.caesiumURL)
	s.Require().NoError(err, "receipt get failed:\n%s", receipt)
	s.Require().True(json.Valid([]byte(receipt)), "receipt get stdout was not clean JSON (log contamination):\n%s", receipt)

	// Commit the receipt and verify it — the documented audit workflow. verify
	// writes its verdict (OK / UNVERIFIABLE) to stdout; a parse failure would
	// surface as a cobra error on stderr (carried in err here). Capturing the
	// streams separately avoids a stray stderr log line matching the verdict.
	receiptFile := filepath.Join(dir, "receipt.json")
	s.Require().NoError(os.WriteFile(receiptFile, []byte(receipt), 0o644))
	vout, verr := s.runCLIStdout("verify", receiptFile, "--server", s.caesiumURL)
	diag := vout
	if verr != nil {
		diag += "\n" + verr.Error()
	}
	s.NotContains(diag, "invalid character", "verify must parse the committed receipt file, not choke on contamination:\n%s", diag)
	if verr != nil {
		// A degraded (unpinned) run legitimately exits non-zero as UNVERIFIABLE
		// (the verdict is still on stdout); the contamination bug would instead
		// produce the parse error asserted against above.
		s.Contains(vout, "UNVERIFIABLE", "verify failed for a reason other than a degraded run:\n%s", diag)
	} else {
		s.Contains(vout, "OK", "verify of a pinned run should report a match:\n%s", vout)
	}
}

type impactResult struct {
	Downstream []struct {
		DatasetName string `json:"dataset_name"`
	} `json:"downstream"`
}

// TestLineageImpactReturnsDownstream verifies the impact query end-to-end: a
// producer→consumer pipeline (linked via declared schemas) populates the
// persisted lineage graph so /lineage/impact returns the downstream consumer.
// The original bug was that the graph was never persisted at all, so this query
// returned empty regardless of topology; here it must traverse a real edge.
//
// This exercises a within-job producer→consumer edge. The impact BFS is
// job-agnostic (it joins datasets by namespace+name), so it spans jobs whenever
// two jobs share a dataset identity — but the schema-derived dataset name is
// currently job-scoped (`<alias>.<step>.output`), so genuine cross-job linkage
// needs a shared-dataset reference and is tracked as a follow-up.
//
// Requires OPEN_LINEAGE enabled on the integration server (set in
// `just integration-up`).
func (s *IntegrationTestSuite) TestLineageImpactReturnsDownstream() {
	alias := fmt.Sprintf("e2e-lineage-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger: { type: cron, configuration: { cron: "0 2 * * *" } }
steps:
  - name: extract
    image: alpine:3.23
    outputSchema: { type: object, properties: { rows: { type: string } } }
    command: ["sh","-c","echo '##caesium::output {\"rows\": \"1\"}'"]
    next: transform
  - name: transform
    image: alpine:3.23
    inputSchema: { extract: { properties: { rows: { type: string } } } }
    outputSchema: { type: object, properties: { clean: { type: string } } }
    command: ["sh","-c","echo got $CAESIUM_OUTPUT_EXTRACT_ROWS; echo '##caesium::output {\"clean\": \"y\"}'"]
    dependsOn: extract
`, alias)
	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	runID := s.triggerRun(job.ID)
	s.Require().Equal("succeeded", s.awaitRun(job.ID, runID, 90*time.Second).Status)

	// Lineage persistence is driven asynchronously by the event subscriber, so
	// poll the impact endpoint until the downstream edge appears.
	rootName := alias + ".extract.output"
	wantName := alias + ".transform.output"
	deadline := time.Now().Add(30 * time.Second)
	var res impactResult
	// Loop while the test context is live (cancelled/timed-out → stop polling).
	for s.T().Context().Err() == nil {
		res = impactResult{}
		s.getJSON("/v1/lineage/impact?namespace=caesium&name="+rootName, &res)
		if len(res.Downstream) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	s.Require().NotEmpty(res.Downstream, "impact query returned no downstream consumer for %s", rootName)
	found := false
	for _, n := range res.Downstream {
		if n.DatasetName == wantName {
			found = true
		}
	}
	s.True(found, "expected %s downstream of %s, got %+v", wantName, rootName, res.Downstream)
}
