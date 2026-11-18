//go:build integration

package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	stdout, stderr, err := s.runCLISeparate(args...)
	if err != nil {
		return stdout, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr))
	}
	return stdout, nil
}

// runCLISeparate runs the CLI while keeping stdout and stderr separate. Use it
// when a test must prove machine-readable stdout is not contaminated by cobra
// diagnostics, logs, or operator guidance printed to stderr.
func (s *IntegrationTestSuite) runCLISeparate(args ...string) (string, string, error) {
	s.T().Helper()
	cmd := exec.CommandContext(s.T().Context(), s.cliPath, args...)
	cmd.Dir = s.projectRoot
	cmd.Env = os.Environ()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
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
    command: ["sh","-c","echo scenario=$CAESIUM_PARAM_SCENARIO; echo '##caesium::output {\"rows\": \"100\"}'"]
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

// TestRunDiffAttributesChangedField verifies the causal run-diff CLI
// end-to-end: the command drives the live REST endpoint, emits clean parseable
// JSON on stdout, and names the changed run param as a task-level
// discriminating field.
func (s *IntegrationTestSuite) TestRunDiffAttributesChangedField() {
	alias := fmt.Sprintf("e2e-rundiff-cli-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(runDiffManifest(alias))
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	run1 := s.triggerRunWithParams(job.ID, map[string]string{"flavor": "vanilla"})
	s.Require().Equal("succeeded", s.awaitRun(job.ID, run1, runTimeout).Status)
	run2 := s.triggerRunWithParams(job.ID, map[string]string{"flavor": "chocolate"})
	s.Require().Equal("succeeded", s.awaitRun(job.ID, run2, runTimeout).Status)

	out, err := s.runCLIStdout("run", "diff", run1, run2, "--job-id", job.ID, "--json", "--server", s.caesiumURL)
	s.Require().NoError(err, "caesium run diff failed:\n%s", out)
	s.Require().True(json.Valid([]byte(out)), "caesium run diff --json stdout was not valid JSON (log contamination?):\n%s", out)

	var diff runDiffRESTResponse
	s.Require().NoError(json.Unmarshal([]byte(out), &diff))
	s.Equal(job.ID, diff.JobID)
	s.Equal(run1, diff.LeftRunID)
	s.Equal(run2, diff.RightRunID)
	s.True(hasRunDiffChange(diff.ParamChanges, "params.flavor", "vanilla", "chocolate"),
		"run-level param diff should include params.flavor, got %+v", diff.ParamChanges)
	task := requireRunDiffTask(s, diff.Tasks, "render")
	s.Equal("RERAN", task.Verdict)
	s.True(hasRunDiffChange(task.Changes, "runParams.flavor", "vanilla", "chocolate"),
		"task diff should include runParams.flavor, got %+v", task.Changes)
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

	// Field-level image/command drift is read from the completed run's task
	// rows. A definition re-apply does not mutate those rows, so this updates
	// the row directly and posts the original receipt to the verify endpoint.
	s.assertReceiptTaskFieldDrift(job.ID, runID, receipt)
}

// assertReceiptTaskFieldDrift updates the run's task image and command in the
// catalog and asserts POST /receipt/verify reports image_changed and
// command_changed against the receipt captured before the update. Kubernetes
// lanes cannot open the catalog; the unit tests cover the comparison there.
func (s *IntegrationTestSuite) assertReceiptTaskFieldDrift(jobID, runID, receipt string) {
	s.T().Helper()
	if s.engineType == "kubernetes" {
		return
	}

	var committed struct {
		Tasks []struct {
			TaskName string `json:"task_name"`
			Image    string `json:"image"`
			Command  string `json:"command"`
		} `json:"tasks"`
	}
	s.Require().NoError(json.Unmarshal([]byte(receipt), &committed))
	s.Require().NotEmpty(committed.Tasks)
	original := committed.Tasks[0]
	s.Equal("produce", original.TaskName)
	s.Equal("alpine:3.23", original.Image)
	s.NotEmpty(original.Command, "a new receipt must record the task row's command")

	const driftedImage = "busybox:1.36.1"
	const driftedCommand = `["sh","-c","echo drifted"]`
	s.NotEqual(driftedCommand, original.Command)

	db := s.openIntegrationCatalogDB()
	defer db.Close()
	res, err := db.ExecContext(s.T().Context(),
		`UPDATE task_runs SET image = ?, command = ? WHERE job_run_id = ?`,
		driftedImage, driftedCommand, runID)
	s.Require().NoError(err)
	n, err := res.RowsAffected()
	s.Require().NoError(err)
	s.Equal(int64(1), n, "the completed run has one task row to mutate")

	resp, err := s.doJSONRequest(http.MethodPost,
		fmt.Sprintf("%s/v1/jobs/%s/runs/%s/receipt/verify", s.caesiumURL, jobID, runID),
		bytes.NewReader([]byte(receipt)))
	s.Require().NoError(err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, resp.StatusCode, string(body))

	var result struct {
		Match  bool `json:"match"`
		Drifts []struct {
			Kind     string `json:"kind"`
			Task     string `json:"task"`
			Expected string `json:"expected"`
			Actual   string `json:"actual"`
		} `json:"drifts"`
	}
	s.Require().NoError(json.Unmarshal(body, &result), string(body))
	s.False(result.Match, "task-row image and command drift must fail verification: %s", body)

	var image, command bool
	for _, d := range result.Drifts {
		switch d.Kind {
		case "image_changed":
			image = true
			s.Equal("produce", d.Task)
			s.Equal(original.Image, d.Expected)
			s.Equal(driftedImage, d.Actual)
		case "command_changed":
			command = true
			s.Equal("produce", d.Task)
			s.Equal(original.Command, d.Expected)
			s.Equal(driftedCommand, d.Actual)
		case "image_digest_mismatch":
			s.Fail("image reference drift must not be reported as a resolved-digest mismatch")
		}
	}
	s.True(image, "verify must report image_changed from the task row, got %+v", result.Drifts)
	s.True(command, "verify must report command_changed from the task row, got %+v", result.Drifts)
}

type impactResult struct {
	Downstream []struct {
		DatasetName string `json:"dataset_name"`
		// ProducingStep is the step that emits the downstream dataset. It was
		// permanently empty in production because the mapper persisted the facet
		// summary flat while the impact query read it nested — asserted here so a
		// re-divergence is caught on the real HTTP surface, not just in a unit test.
		ProducingStep string `json:"producing_step"`
		// RunID is the job run whose task row wrote the dataset. JobID is the
		// producing job. Both are required for run-level click-through.
		RunID string `json:"run_id"`
		JobID string `json:"job_id"`
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
	// Use tryGetJSON, not getJSON: a transient non-200 during async lineage
	// startup (e.g. a brief 500) must NOT FailNow the test — swallow it and keep
	// polling until the downstream edge appears or the deadline passes.
	for s.T().Context().Err() == nil {
		res = impactResult{}
		_ = s.tryGetJSON("/v1/lineage/impact?namespace=caesium&name="+rootName, &res)
		if len(res.Downstream) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	s.Require().NotEmpty(res.Downstream, "impact query returned no downstream consumer for %s", rootName)
	found := false
	producingStep := ""
	for _, n := range res.Downstream {
		if n.DatasetName == wantName {
			found = true
			producingStep = n.ProducingStep
		}
	}
	s.True(found, "expected %s downstream of %s, got %+v", wantName, rootName, res.Downstream)
	// producing_step must name the step that emits the downstream dataset. The
	// UI renders it (LineageGraph's producingStep); before the facet shapes were
	// reconciled it was always "" on every real deployment.
	s.Equal("transform", producingStep,
		"producing_step must resolve from the persisted facet summary, got %+v", res.Downstream)
	for _, n := range res.Downstream {
		if n.DatasetName == wantName {
			s.Equal(runID, n.RunID, "impact node must name the run that produced the dataset")
			s.Equal(job.ID, n.JobID)
		}
	}

	// The run lists the dataset identities it persisted, so the console can
	// open lineage without the operator typing namespace and name. An unknown
	// run is 404. A run that persisted nothing returns an empty list, not a
	// fabricated dataset.
	var datasets struct {
		RunID    string `json:"run_id"`
		JobID    string `json:"job_id"`
		Datasets []struct {
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
			Direction string `json:"direction"`
			TaskName  string `json:"task_name"`
		} `json:"datasets"`
	}
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/runs/%s/datasets", job.ID, runID), &datasets)
	s.Equal(runID, datasets.RunID)
	s.Equal(job.ID, datasets.JobID)
	s.Require().NotNil(datasets.Datasets)
	seen := map[string]string{}
	for _, d := range datasets.Datasets {
		s.Equal("caesium", d.Namespace)
		s.NotEmpty(d.TaskName)
		seen[d.Direction+":"+d.Name] = d.TaskName
	}
	s.Equal("extract", seen["output:"+rootName])
	s.Equal("transform", seen["input:"+rootName])
	s.Equal("transform", seen["output:"+wantName])
	s.assertGetStatus(fmt.Sprintf("/v1/jobs/%s/runs/%s/datasets", job.ID, "00000000-0000-0000-0000-000000000001"), http.StatusNotFound)
}
