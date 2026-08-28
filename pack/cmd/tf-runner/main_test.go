package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/pack/internal/protocol"
	"github.com/caesium-cloud/caesium/pack/internal/tf"
)

// sensitiveCanary is the value of the offline module's `sensitive = true`
// output. Nothing this role emits may contain it.
const sensitiveCanary = "s3cr3t-canary"

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// stack copies pack/testdata/offline/stack to a scratch directory and returns a
// config pointing at it.
//
// The module is provider-free by construction (see its README), so every test
// below runs REAL terraform init/plan/apply with no network and no mirror. That
// matters: the phases under test are almost entirely about what Terraform
// actually returns — the -detailed-exitcode boolean, the plan JSON's typed
// fields, the Sensitive flag on an output — and a fake would be a test of the
// fake.
func newStack(t *testing.T) config {
	t.Helper()
	if _, err := os.Stat(terraformPath(t)); err != nil {
		t.Skipf("terraform is not on PATH: %v", err)
	}

	root := filepath.Join(t.TempDir(), "stack")
	src := filepath.Join("..", "..", "testdata", "offline", "stack")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, entry.Name())) //nolint:gosec // fixture path.
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, entry.Name()), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return config{
		Root:        root,
		Workspace:   "default",
		ArtifactDir: filepath.Join(root, ".caesium"),
		DataDir:     filepath.Join(root, ".caesium", "tfdata"),
		ExecPath:    terraformPath(t),
	}
}

func terraformPath(t *testing.T) string {
	t.Helper()
	if path := os.Getenv("TF_CLI_PATH"); path != "" {
		return path
	}
	return "/usr/local/bin/terraform"
}

// emit runs one phase and returns the marker lines it actually flushed, so a
// test asserts on what a Caesium task would have read from stdout.
func emit(t *testing.T, phase func(context.Context, config, *protocol.Emitter, io.Writer) error, cfg config) ([]string, error) {
	t.Helper()
	var out, logs bytes.Buffer
	e := protocol.New("test", &out, &logs)
	err := phase(context.Background(), cfg, e, &logs)
	if err != nil {
		// Mirror protocol.Run: an error discards the buffer, so a failing phase
		// emits nothing at all.
		e.FailClosed(err)
	} else if flushErr := e.Flush(); flushErr != nil {
		t.Fatalf("flush: %v", flushErr)
	}
	t.Logf("logs:\n%s", logs.String())
	return markerLines(out.String()), err
}

// emitWithLog is emit, additionally returning what the role wrote to its log
// stream (which is what lands in the task log).
func emitWithLog(t *testing.T, phase func(context.Context, config, *protocol.Emitter, io.Writer) error, cfg config) ([]string, string, error) {
	t.Helper()
	var out, logs bytes.Buffer
	e := protocol.New("test", &out, &logs)
	err := phase(context.Background(), cfg, e, &logs)
	if err != nil {
		e.FailClosed(err)
	} else if flushErr := e.Flush(); flushErr != nil {
		t.Fatalf("flush: %v", flushErr)
	}
	return markerLines(out.String()), logs.String(), err
}

func markerLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "##caesium::") {
			out = append(out, line)
		}
	}
	return out
}

// outputs parses the single ##caesium::output marker from a phase's emission.
func outputs(t *testing.T, lines []string) map[string]string {
	t.Helper()
	for _, line := range lines {
		payload, ok := strings.CutPrefix(line, protocol.OutputMarker+" ")
		if !ok {
			continue
		}
		var values map[string]string
		if err := json.Unmarshal([]byte(payload), &values); err != nil {
			t.Fatalf("output marker is not a string map: %v (%s)", err, payload)
		}
		return values
	}
	return nil
}

func outputRef(t *testing.T, lines []string) map[string]any {
	t.Helper()
	for _, line := range lines {
		payload, ok := strings.CutPrefix(line, protocol.OutputRefMarker+" ")
		if !ok {
			continue
		}
		var ref map[string]any
		if err := json.Unmarshal([]byte(payload), &ref); err != nil {
			t.Fatalf("output-ref marker is not JSON: %v", err)
		}
		return ref
	}
	return nil
}

func branches(lines []string) []string {
	var out []string
	for _, line := range lines {
		if name, ok := strings.CutPrefix(line, protocol.BranchMarker+" "); ok {
			out = append(out, name)
		}
	}
	return out
}

// plannedEnv makes the proposal a plan phase produced visible to a subsequent
// apply phase exactly the way Caesium does: as CAESIUM_OUTPUT_<STEP>_<KEY>,
// with an output reference exposed as its path plus a companion _DIGEST.
func plannedEnv(t *testing.T, step string, lines []string) {
	t.Helper()
	prefix := "CAESIUM_OUTPUT_" + normalizeStepName(step) + "_"
	for k, v := range outputs(t, lines) {
		t.Setenv(prefix+normalizeStepName(k), v)
	}
	if ref := outputRef(t, lines); ref != nil {
		key, _ := ref["key"].(string)
		path, _ := ref["path"].(string)
		digest, _ := ref["digest"].(string)
		t.Setenv(prefix+normalizeStepName(key), path)
		t.Setenv(prefix+normalizeStepName(key)+"_DIGEST", digest)
	}
}

// ---------------------------------------------------------------------------
// tf-plan
// ---------------------------------------------------------------------------

func TestPlanProposesChangesWithADigestedArtifact(t *testing.T) {
	cfg := newStack(t)
	lines, err := emit(t, runPlan, cfg)
	if err != nil {
		t.Fatalf("tf-plan: %v", err)
	}

	values := outputs(t, lines)
	if values["proposal_kind"] != tf.ProposalKind {
		t.Fatalf("proposal_kind = %q", values["proposal_kind"])
	}
	if values["proposal_artifact"] != "plan" {
		t.Fatalf("proposal_artifact = %q", values["proposal_artifact"])
	}

	// The summary is a JSON-ENCODED STRING, never a nested object:
	// pkg/task.ParseOutput keeps only scalars and would drop the key outright.
	summary, err := tf.DecodeSummary(values["proposal_summary"])
	if err != nil {
		t.Fatalf("proposal_summary does not decode: %v (%q)", err, values["proposal_summary"])
	}
	if summary.Add != 1 || summary.Change != 0 || summary.Destroy != 0 {
		t.Fatalf("summary = %+v, want one create", summary)
	}
	if len(summary.Resources) != 1 || summary.Resources[0].Address != "terraform_data.canary" ||
		summary.Resources[0].Action != "add" {
		t.Fatalf("resource list = %+v", summary.Resources)
	}

	ref := outputRef(t, lines)
	if ref == nil {
		t.Fatal("tf-plan emitted no output-ref for the plan artifact")
	}
	if ref["key"] != "plan" {
		t.Fatalf("output-ref key = %v", ref["key"])
	}
	wantPath := filepath.Join(cfg.ArtifactDir, planFileName)
	if ref["path"] != wantPath {
		t.Fatalf("output-ref path = %v, want %s", ref["path"], wantPath)
	}
	digest, _ := ref["digest"].(string)
	if !protocol.ValidDigest(digest) {
		t.Fatalf("output-ref digest = %q", digest)
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("the referenced plan artifact does not exist: %v", err)
	}
}

// The sensitive output's value is in the plan (Terraform put it there), so the
// only thing between it and dqlite is StripSensitive plus a summary that reads
// nothing but addresses and actions.
func TestPlanNeverEmitsASensitiveValue(t *testing.T) {
	cfg := newStack(t)
	lines, err := emit(t, runPlan, cfg)
	if err != nil {
		t.Fatalf("tf-plan: %v", err)
	}
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, sensitiveCanary) {
		t.Fatalf("a sensitive value reached the emitted markers:\n%s", joined)
	}
	// And prove the value really is in the plan file, so the test is not passing
	// because there was nothing to leak.
	data, err := os.ReadFile(filepath.Join(cfg.ArtifactDir, planFileName)) //nolint:gosec // test-local path.
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(sensitiveCanary)) {
		t.Skip("terraform no longer stores the sensitive value in the plan file; the leak test would be vacuous")
	}
}

// terraform-exec tees a JSON command's raw response into the handle's stdout
// writer as well as into its decoder. For `show -json` that would dump the
// entire unsanitised plan — sensitive_values and all — into the task log on
// every propose step, undoing the strip one line downstream of it.
func TestPlanDoesNotEchoThePlanJSONIntoTheLog(t *testing.T) {
	cfg := newStack(t)
	_, log, err := emitWithLog(t, runPlan, cfg)
	if err != nil {
		t.Fatalf("tf-plan: %v", err)
	}
	for _, forbidden := range []string{sensitiveCanary, `"sensitive_values"`, `"after_sensitive"`, `"prior_state"`} {
		if strings.Contains(log, forbidden) {
			t.Fatalf("the raw plan JSON reached the task log (found %q):\n%s", forbidden, log)
		}
	}
	// The human-readable plan is still there — the point is to suppress the
	// JSON dump, not Terraform's diagnostics.
	if !strings.Contains(log, "terraform_data.canary") {
		t.Fatalf("terraform's own output stopped reaching the log:\n%s", log)
	}
}

// `terraform output -json` prints sensitive values IN FULL — the CLI masks them
// only in its human rendering. With the same tee left connected, every secret
// tf-apply carefully withholds from the output row would land in the task log
// instead.
func TestApplyDoesNotEchoTerraformOutputJSONIntoTheLog(t *testing.T) {
	cfg := newStack(t)
	planLines, err := emit(t, runPlan, cfg)
	if err != nil {
		t.Fatalf("tf-plan: %v", err)
	}
	plannedEnv(t, "plan-offline", planLines)
	cfg.PlanStep = "plan-offline"

	_, log, err := emitWithLog(t, runApply, cfg)
	if err != nil {
		t.Fatalf("tf-apply: %v", err)
	}
	if strings.Contains(log, sensitiveCanary) {
		t.Fatalf("the sensitive output value reached the task log:\n%s", log)
	}
}

func TestPlanOnAnAlreadyAppliedStackProposesNothing(t *testing.T) {
	cfg := newStack(t)
	planLines, err := emit(t, runPlan, cfg)
	if err != nil {
		t.Fatalf("first tf-plan: %v", err)
	}
	plannedEnv(t, "plan-offline", planLines)
	cfg.PlanStep = "plan-offline"
	if _, err := emit(t, runApply, cfg); err != nil {
		t.Fatalf("tf-apply: %v", err)
	}

	lines, err := emit(t, runPlan, cfg)
	if err != nil {
		t.Fatalf("second tf-plan: %v", err)
	}
	values := outputs(t, lines)
	summary, err := tf.DecodeSummary(values["proposal_summary"])
	if err != nil {
		t.Fatal(err)
	}
	if summary.HasChanges() {
		t.Fatalf("an applied stack still proposes changes: %+v", summary)
	}
	// No artifact and no branch: there is nothing to review and nothing to run.
	if _, present := values["proposal_artifact"]; present {
		t.Fatalf("an empty plan named an artifact: %v", values)
	}
	if ref := outputRef(t, lines); ref != nil {
		t.Fatalf("an empty plan emitted an output-ref: %v", ref)
	}
	if got := branches(lines); len(got) != 0 {
		t.Fatalf("an empty plan emitted a branch marker: %v", got)
	}
}

// The leaf-stack branch form: APPLY_STEP names the successor, and the marker is
// emitted only when there is something to apply.
func TestPlanBranchesToTheApplyStepOnlyWhenThereAreChanges(t *testing.T) {
	cfg := newStack(t)
	cfg.ApplyStep = "apply-offline"

	lines, err := emit(t, runPlan, cfg)
	if err != nil {
		t.Fatalf("tf-plan: %v", err)
	}
	if got := branches(lines); len(got) != 1 || got[0] != "apply-offline" {
		t.Fatalf("branch markers = %v, want [apply-offline]", got)
	}

	plannedEnv(t, "plan-offline", lines)
	applyCfg := cfg
	applyCfg.PlanStep = "plan-offline"
	if _, err := emit(t, runApply, applyCfg); err != nil {
		t.Fatalf("tf-apply: %v", err)
	}

	lines, err = emit(t, runPlan, cfg)
	if err != nil {
		t.Fatalf("second tf-plan: %v", err)
	}
	if got := branches(lines); len(got) != 0 {
		t.Fatalf("an empty plan still branched to the apply step: %v", got)
	}
}

// IMPORT_OUTPUTS_FROM is the cross-stack wiring. It has to survive the shape
// Caesium actually delivers upstream outputs in, and it has to tolerate the
// upstream stack exporting values this one never declared — which is the norm,
// since a stack's outputs serve every consumer, not just this one.
func TestPlanImportsUpstreamOutputsAsTerraformVariables(t *testing.T) {
	cfg := newStack(t)
	cfg.ImportOutputsFrom = []string{"apply-network"}
	t.Setenv("CAESIUM_OUTPUT_APPLY_NETWORK_GREETING", "imported")
	t.Setenv("CAESIUM_OUTPUT_APPLY_NETWORK_NOT_A_VARIABLE_HERE", "ignored")
	t.Setenv("CAESIUM_OUTPUT_APPLY_NETWORK_PLAN", "/some/path")
	t.Setenv("CAESIUM_OUTPUT_APPLY_NETWORK_PLAN_DIGEST", "sha256:"+strings.Repeat("a", 64))
	t.Setenv("TF_VAR_greeting", "")

	lines, err := emit(t, runPlan, cfg)
	if err != nil {
		t.Fatalf("tf-plan: %v", err)
	}
	if os.Getenv("TF_VAR_greeting") != "imported" {
		t.Fatalf("TF_VAR_greeting = %q, want the upstream output", os.Getenv("TF_VAR_greeting"))
	}
	if os.Getenv("TF_VAR_not_a_variable_here") != "ignored" {
		t.Fatal("an undeclared upstream output was not exported; the import must not filter on this stack's variables")
	}
	// A digest belongs to an output reference, not to a Terraform variable.
	if _, set := os.LookupEnv("TF_VAR_plan_digest"); set {
		t.Fatal("the output reference's companion digest was exported as a variable")
	}
	if len(lines) == 0 {
		t.Fatal("tf-plan emitted nothing")
	}
}

func TestPlanFailuresEmitNoMarkers(t *testing.T) {
	cfg := newStack(t)
	if err := os.WriteFile(filepath.Join(cfg.Root, "broken.tf"), []byte("resource \"nope\" {\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, err := emit(t, runPlan, cfg)
	if err == nil {
		t.Fatal("tf-plan succeeded on an unparseable configuration")
	}
	if len(lines) != 0 {
		t.Fatalf("a failed tf-plan emitted markers: %v", lines)
	}
}

// ---------------------------------------------------------------------------
// tf-apply
// ---------------------------------------------------------------------------

func TestApplyAppliesTheProposedArtifactAndPublishesOutputs(t *testing.T) {
	cfg := newStack(t)
	planLines, err := emit(t, runPlan, cfg)
	if err != nil {
		t.Fatalf("tf-plan: %v", err)
	}
	plannedEnv(t, "plan-offline", planLines)
	cfg.PlanStep = "plan-offline"

	lines, log, err := emitWithLog(t, runApply, cfg)
	if err != nil {
		t.Fatalf("tf-apply: %v", err)
	}
	values := outputs(t, lines)
	if values["greeting"] != "hello" {
		t.Fatalf("greeting = %q", values["greeting"])
	}
	// A non-scalar output is rendered as compact JSON; emitting it as a nested
	// object would have it silently dropped by pkg/task.ParseOutput.
	if values["structured"] != `{"a":1,"b":"two"}` {
		t.Fatalf("structured = %q", values["structured"])
	}
	if _, present := values["token"]; present {
		t.Fatal("the sensitive output was published")
	}
	if strings.Contains(strings.Join(lines, "\n"), sensitiveCanary) {
		t.Fatal("the sensitive value reached the emitted markers")
	}
	if !strings.Contains(log, "withheld 1 sensitive output") {
		t.Fatalf("apply did not report withholding the sensitive output:\n%s", log)
	}
	if _, err := os.Stat(filepath.Join(cfg.Root, "terraform.tfstate")); err != nil {
		t.Fatalf("apply wrote no state: %v", err)
	}
}

// The always-emit rule. A stack whose plan was empty is still the producer its
// consumers read from; an apply that emitted nothing because it had nothing to
// do would look identical to a stack that vanished, and the cascade would
// re-plan or fail every consumer.
func TestApplyWithAnEmptyPlanSkipsTerraformButStillPublishesOutputs(t *testing.T) {
	cfg := newStack(t)
	planLines, err := emit(t, runPlan, cfg)
	if err != nil {
		t.Fatalf("tf-plan: %v", err)
	}
	plannedEnv(t, "plan-offline", planLines)
	cfg.PlanStep = "plan-offline"
	if _, err := emit(t, runApply, cfg); err != nil {
		t.Fatalf("first tf-apply: %v", err)
	}

	emptyLines, err := emit(t, runPlan, cfg)
	if err != nil {
		t.Fatalf("second tf-plan: %v", err)
	}
	plannedEnv(t, "plan-offline", emptyLines)

	lines, log, err := emitWithLog(t, runApply, cfg)
	if err != nil {
		t.Fatalf("second tf-apply: %v", err)
	}
	if !strings.Contains(log, "not invoking terraform apply") {
		t.Fatalf("apply invoked terraform for an empty plan:\n%s", log)
	}
	values := outputs(t, lines)
	if values["greeting"] != "hello" {
		t.Fatalf("a no-op apply stopped publishing outputs: %v", values)
	}
}

// The plan artifact sits on a volume other steps can write to and decides what
// happens to real infrastructure. Applying it without re-verifying the digest
// would make the propose/apply split decorative.
func TestApplyRefusesAnArtifactThatDoesNotMatchTheProposal(t *testing.T) {
	cfg := newStack(t)
	planLines, err := emit(t, runPlan, cfg)
	if err != nil {
		t.Fatalf("tf-plan: %v", err)
	}
	plannedEnv(t, "plan-offline", planLines)
	cfg.PlanStep = "plan-offline"

	planPath := filepath.Join(cfg.ArtifactDir, planFileName)
	if err := os.WriteFile(planPath, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}

	lines, err := emit(t, runApply, cfg)
	if err == nil {
		t.Fatal("tf-apply applied an artifact that does not match its proposal")
	}
	if !strings.Contains(err.Error(), "does not match the proposal") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(lines) != 0 {
		t.Fatalf("a refused apply emitted markers: %v", lines)
	}
}

func TestApplyRejectsAnIncompleteOrForeignProposal(t *testing.T) {
	base := newStack(t)
	base.PlanStep = "plan-offline"
	prefix := "CAESIUM_OUTPUT_PLAN_OFFLINE_"

	t.Run("no proposal at all", func(t *testing.T) {
		if _, err := emit(t, runApply, base); err == nil {
			t.Fatal("tf-apply ran without a proposal")
		}
	})

	t.Run("another tool's proposal", func(t *testing.T) {
		t.Setenv(prefix+"PROPOSAL_SUMMARY", `{"add":1,"change":0,"destroy":0,"replace":0,"import":0,"outputs":0,"resources":[]}`)
		t.Setenv(prefix+"PROPOSAL_KIND", "dbt.compile.v1")
		_, err := emit(t, runApply, base)
		if err == nil || !strings.Contains(err.Error(), "cannot apply") {
			t.Fatalf("tf-apply accepted a foreign proposal kind: %v", err)
		}
	})

	t.Run("changes proposed but no artifact", func(t *testing.T) {
		t.Setenv(prefix+"PROPOSAL_SUMMARY", `{"add":1,"change":0,"destroy":0,"replace":0,"import":0,"outputs":0,"resources":[]}`)
		t.Setenv(prefix+"PROPOSAL_KIND", tf.ProposalKind)
		_, err := emit(t, runApply, base)
		if err == nil || !strings.Contains(err.Error(), "named no artifact") {
			t.Fatalf("tf-apply accepted a change proposal with no artifact: %v", err)
		}
	})

	t.Run("artifact named but not located", func(t *testing.T) {
		t.Setenv(prefix+"PROPOSAL_SUMMARY", `{"add":1,"change":0,"destroy":0,"replace":0,"import":0,"outputs":0,"resources":[]}`)
		t.Setenv(prefix+"PROPOSAL_KIND", tf.ProposalKind)
		t.Setenv(prefix+"PROPOSAL_ARTIFACT", "plan")
		_, err := emit(t, runApply, base)
		if err == nil || !strings.Contains(err.Error(), "cannot be verified") {
			t.Fatalf("tf-apply accepted an unlocatable artifact: %v", err)
		}
	})

	t.Run("no PLAN_STEP", func(t *testing.T) {
		cfg := base
		cfg.PlanStep = ""
		if _, err := emit(t, runApply, cfg); err == nil {
			t.Fatal("tf-apply ran with no PLAN_STEP")
		}
	})
}

// ---------------------------------------------------------------------------
// tf-drift
// ---------------------------------------------------------------------------

func TestDriftOnACleanStackIsGreen(t *testing.T) {
	cfg := newStack(t)
	planLines, err := emit(t, runPlan, cfg)
	if err != nil {
		t.Fatalf("tf-plan: %v", err)
	}
	plannedEnv(t, "plan-offline", planLines)
	applyCfg := cfg
	applyCfg.PlanStep = "plan-offline"
	if _, err := emit(t, runApply, applyCfg); err != nil {
		t.Fatalf("tf-apply: %v", err)
	}

	lines, err := emit(t, runDrift, cfg)
	if err != nil {
		t.Fatalf("tf-drift: %v", err)
	}
	values := outputs(t, lines)
	if values["drift"] != "false" {
		t.Fatalf("drift = %q, want false", values["drift"])
	}
	if _, present := values["drift_summary"]; present {
		t.Fatalf("a clean stack reported a drift summary: %v", values)
	}
	// Drift writes no artifact: nothing applies a refresh-only plan, so
	// offering one as a reviewable object would invite exactly that.
	if ref := outputRef(t, lines); ref != nil {
		t.Fatalf("tf-drift emitted an artifact reference: %v", ref)
	}
	assertNoStrayArtifacts(t, cfg.ArtifactDir)
}

// The drifted path is proven end to end in test/infra_drift_test.go, which
// needs a provider whose read consults the real world. What is proven HERE is
// the part this role owns and that a scenario cannot easily observe: that the
// output is emitted BEFORE the non-zero exit. Emitting after would leave the
// operator with a red run and no drift summary to act on.
func TestDriftEmitsItsOutputBeforeFailing(t *testing.T) {
	var out, logs bytes.Buffer
	e := protocol.New("tf-drift", &out, &logs)

	if err := e.Output(map[string]string{"drift": "true", "drift_summary": `{"add":0,"change":1,"destroy":0,"replace":0,"import":0,"outputs":0,"resources":[]}`}); err != nil {
		t.Fatal(err)
	}
	if err := e.Flush(); err != nil {
		t.Fatal(err)
	}
	// protocol.Run's failure path discards the buffer; already-flushed lines
	// survive, which is the ordering the drift phase relies on.
	if code := e.FailClosed(errDrift); code == 0 {
		t.Fatal("FailClosed returned a success code")
	}
	if !strings.Contains(out.String(), `"drift":"true"`) {
		t.Fatalf("the drift output did not survive the failure:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "drift_summary") {
		t.Fatalf("the drift summary did not survive the failure:\n%s", out.String())
	}
}

func assertNoStrayArtifacts(t *testing.T, artifactDir string) {
	t.Helper()
	err := filepath.WalkDir(artifactDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.Contains(d.Name(), "refresh") {
			t.Fatalf("tf-drift left a refresh plan in the artifact directory: %s", path)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

func TestResolveRootPrefersStackRootAndFallsBackToThePartition(t *testing.T) {
	env := map[string]string{"STACK_ROOT": "/src/stacks/network"}
	got, err := resolveRoot(func(k string) string { return env[k] })
	if err != nil || got != "/src/stacks/network" {
		t.Fatalf("resolveRoot = %q, %v", got, err)
	}

	env = map[string]string{
		"SCAN_ROOT":              "/src/stacks",
		"CAESIUM_PARTITION_JSON": `{"key":"app-web","fingerprint":"sha256:aa","dependsOn":["network"],"root":"app-web"}`,
	}
	got, err = resolveRoot(func(k string) string { return env[k] })
	if err != nil || got != "/src/stacks/app-web" {
		t.Fatalf("resolveRoot = %q, %v", got, err)
	}
}

// Every one of these would otherwise plan or apply the WRONG directory, which
// is a deployment nobody asked for.
func TestResolveRootFailsClosed(t *testing.T) {
	cases := map[string]map[string]string{
		"nothing at all":       {},
		"partition, no root":   {"SCAN_ROOT": "/src/stacks", "CAESIUM_PARTITION_JSON": `{"key":"app-web"}`},
		"partition, no key":    {"SCAN_ROOT": "/src/stacks", "CAESIUM_PARTITION_JSON": `{"root":"app-web"}`},
		"root, no scan root":   {"CAESIUM_PARTITION_JSON": `{"key":"a","root":"app-web"}`},
		"absolute root":        {"SCAN_ROOT": "/src/stacks", "CAESIUM_PARTITION_JSON": `{"key":"a","root":"/etc"}`},
		"escaping root":        {"SCAN_ROOT": "/src/stacks", "CAESIUM_PARTITION_JSON": `{"key":"a","root":"../../etc"}`},
		"partition not object": {"SCAN_ROOT": "/src/stacks", "CAESIUM_PARTITION_JSON": `not json`},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := resolveRoot(func(k string) string { return env[k] }); err == nil {
				t.Fatalf("resolveRoot accepted %s", name)
			}
		})
	}
}

func TestLoadConfigDefaultsTheArtifactAndDataDirectories(t *testing.T) {
	env := map[string]string{"STACK_ROOT": "/src/stacks/network", "TF_CLI_PATH": "/usr/local/bin/terraform"}
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.ArtifactDir != "/src/stacks/network/.caesium" {
		t.Fatalf("ArtifactDir = %q", cfg.ArtifactDir)
	}
	// Terraform's data directory must not land beside the configuration: the
	// discover role fingerprints that directory and init writes into this one.
	if cfg.DataDir != "/src/stacks/network/.caesium/tfdata" {
		t.Fatalf("DataDir = %q", cfg.DataDir)
	}
	if cfg.Workspace != "default" {
		t.Fatalf("Workspace = %q", cfg.Workspace)
	}

	env["BACKEND_CONFIG"] = "path=/state/network.tfstate"
	cfg, err = loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(cfg.BackendConfig) != 1 || cfg.BackendConfig[0] != "path=/state/network.tfstate" {
		t.Fatalf("BackendConfig = %v", cfg.BackendConfig)
	}

	env["BACKEND_CONFIG"] = "not-a-setting"
	if _, err := loadConfig(func(k string) string { return env[k] }); err == nil {
		t.Fatal("loadConfig accepted a malformed BACKEND_CONFIG")
	}
}

func TestSubcommandsCoverEveryDocumentedPhase(t *testing.T) {
	for _, name := range []string{"tf-plan", "tf-apply", "tf-drift"} {
		if _, ok := subcommands[name]; !ok {
			t.Fatalf("subcommand %q is missing", name)
		}
	}
	if len(subcommands) != 3 {
		t.Fatalf("subcommands = %v", subcommandNames())
	}
}

// ---------------------------------------------------------------------------
// Fix round 1
// ---------------------------------------------------------------------------

// I2. "Are there changes?" must be decided ONCE, by Terraform. The counts are a
// summary for people; if they are also the thing that decides whether apply
// invokes Terraform, then the day Terraform introduces an action set
// actionLabel does not recognise, a plan Terraform called non-empty reaches an
// apply that reads all zeros and does nothing — and the run is green.
func TestApplyTrustsTerraformsChangeAnswerOverTheCounts(t *testing.T) {
	cfg := newStack(t)
	planLines, err := emit(t, runPlan, cfg)
	if err != nil {
		t.Fatalf("tf-plan: %v", err)
	}
	values := outputs(t, planLines)
	summary, err := tf.DecodeSummary(values["proposal_summary"])
	if err != nil {
		t.Fatal(err)
	}
	if summary.Changes == nil || !*summary.Changes {
		t.Fatalf("tf-plan did not record Terraform's own change answer: %+v", summary)
	}

	// Simulate the future action set: Terraform says there are changes, the
	// counts say nothing was recognised.
	blind := tf.Summary{Resources: []tf.ResourceChange{}}.WithChanges(true)
	encoded, err := blind.Encode()
	if err != nil {
		t.Fatal(err)
	}
	plannedEnv(t, "plan-offline", planLines)
	t.Setenv("CAESIUM_OUTPUT_PLAN_OFFLINE_PROPOSAL_SUMMARY", encoded)
	cfg.PlanStep = "plan-offline"

	_, log, err := emitWithLog(t, runApply, cfg)
	if err != nil {
		t.Fatalf("tf-apply: %v", err)
	}
	if strings.Contains(log, "not invoking terraform apply") {
		t.Fatalf("apply skipped a plan Terraform called non-empty — a green run that deployed nothing:\n%s", log)
	}
	if _, err := os.Stat(filepath.Join(cfg.Root, "terraform.tfstate")); err != nil {
		t.Fatalf("apply did not run: %v", err)
	}
}

// A proposal written before the field existed decodes as nil and must fall back
// to the counts, not decode as false and skip an apply that should run.
func TestSummaryWithoutTheChangesFieldFallsBackToTheCounts(t *testing.T) {
	summary, err := tf.DecodeSummary(`{"add":1,"change":0,"destroy":0,"replace":0,"import":0,"outputs":0,"resources":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Changes != nil {
		t.Fatalf("Changes should be absent, got %v", *summary.Changes)
	}
	if !summary.HasChanges() {
		t.Fatal("a legacy proposal with a non-zero count reported no changes")
	}
}

// I3. A stack that STOPS having publishable outputs must still publish a row.
// Emitting nothing makes a vanished VALUE indistinguishable from a vanished
// STACK: the consumer's TF_VAR_ disappears, an undeclared TF_VAR_ is silently
// ignored by Terraform, and the consumer plans against a default — green.
func TestApplyPublishesARowEvenWhenEveryOutputIsSensitive(t *testing.T) {
	cfg := newStack(t)
	allSensitive := `terraform {
  required_version = ">= 1.10.0"
  backend "local" {
    path = "terraform.tfstate"
  }
}

resource "terraform_data" "canary" {
  input = "hello"
}

output "token" {
  value     = "` + sensitiveCanary + `"
  sensitive = true
}
`
	if err := os.WriteFile(filepath.Join(cfg.Root, "main.tf"), []byte(allSensitive), 0o600); err != nil {
		t.Fatal(err)
	}

	planLines, err := emit(t, runPlan, cfg)
	if err != nil {
		t.Fatalf("tf-plan: %v", err)
	}
	plannedEnv(t, "plan-offline", planLines)
	cfg.PlanStep = "plan-offline"

	lines, err := emit(t, runApply, cfg)
	if err != nil {
		t.Fatalf("tf-apply: %v", err)
	}
	values := outputs(t, lines)
	if values == nil {
		t.Fatal("tf-apply published no output row at all; a vanished value is now indistinguishable from a vanished stack")
	}
	if values["caesium_outputs_published"] != "0" {
		t.Fatalf("the published-count sentinel is wrong: %v", values)
	}
	if strings.Contains(strings.Join(lines, "\n"), sensitiveCanary) {
		t.Fatal("the sensitive value was published")
	}
}

func TestApplyReportsHowManyOutputsItPublished(t *testing.T) {
	cfg := newStack(t)
	planLines, err := emit(t, runPlan, cfg)
	if err != nil {
		t.Fatalf("tf-plan: %v", err)
	}
	plannedEnv(t, "plan-offline", planLines)
	cfg.PlanStep = "plan-offline"

	lines, err := emit(t, runApply, cfg)
	if err != nil {
		t.Fatalf("tf-apply: %v", err)
	}
	// greeting + structured, with token withheld.
	if got := outputs(t, lines)["caesium_outputs_published"]; got != "2" {
		t.Fatalf("caesium_outputs_published = %q, want 2", got)
	}
}

// I4. The environment prefix of one step can contain another's, and a silent
// last-write-wins on a collision resolves a real ambiguity by os.Environ()
// ordering — an implementation detail of whichever engine formatted it.
func TestImportedOutputsAreStepExactAndFailClosedOnAmbiguity(t *testing.T) {
	t.Run("nested step prefixes are refused", func(t *testing.T) {
		_, err := exportImportedOutputs([]string{"apply-network", "apply-network-2"},
			[]string{"CAESIUM_OUTPUT_APPLY_NETWORK_2_VPC_ID=vpc-1"}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "cannot be told apart") {
			t.Fatalf("want a nested-prefix refusal, got %v", err)
		}
	})

	t.Run("colliding variable names are refused", func(t *testing.T) {
		_, err := exportImportedOutputs([]string{"apply-network", "apply-account"}, []string{
			"CAESIUM_OUTPUT_APPLY_NETWORK_REGION=us-east-1",
			"CAESIUM_OUTPUT_APPLY_ACCOUNT_REGION=eu-west-1",
		}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "TF_VAR_region") {
			t.Fatalf("want a collision refusal naming the variable, got %v", err)
		}
	})

	t.Run("a legitimate output named foo_digest is not swallowed", func(t *testing.T) {
		t.Setenv("TF_VAR_content_digest", "")
		t.Setenv("TF_VAR_plan", "")
		names, err := exportImportedOutputs([]string{"apply-network"}, []string{
			"CAESIUM_OUTPUT_APPLY_NETWORK_CONTENT_DIGEST=sha256:abc",
			"CAESIUM_OUTPUT_APPLY_NETWORK_PLAN=/src/tf.plan",
			"CAESIUM_OUTPUT_APPLY_NETWORK_PLAN_DIGEST=sha256:def",
		}, io.Discard)
		if err != nil {
			t.Fatalf("exportImportedOutputs: %v", err)
		}
		if strings.Join(names, ",") != "content_digest,plan" {
			t.Fatalf("exported %v; a real output named *_digest must survive while an output reference's companion is skipped", names)
		}
	})

	t.Run("every export is logged with its source", func(t *testing.T) {
		t.Setenv("TF_VAR_vpc_id", "")
		var log bytes.Buffer
		if _, err := exportImportedOutputs([]string{"apply-network"},
			[]string{"CAESIUM_OUTPUT_APPLY_NETWORK_VPC_ID=vpc-9"}, &log); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(log.String(), "TF_VAR_vpc_id <- CAESIUM_OUTPUT_APPLY_NETWORK_VPC_ID") {
			t.Fatalf("the export was not logged; a mis-named variable would be invisible: %q", log.String())
		}
	})

	// The step-exact half. `apply-network-extra`'s environment keys all begin
	// with `apply-network`'s prefix, and `extra_foo` is a perfectly good
	// Terraform identifier, so prefix matching imports a sibling's outputs into
	// this stack silently — the manifest never named that step. Every tf-apply
	// publishes the count sentinel, which is what makes the sibling's own prefix
	// recoverable and the match exact.
	t.Run("an unnamed sibling step's outputs are not imported", func(t *testing.T) {
		t.Setenv("TF_VAR_vpc_id", "")
		t.Setenv("TF_VAR_extra_foo", "")
		names, err := exportImportedOutputs([]string{"apply-network"}, []string{
			"CAESIUM_OUTPUT_APPLY_NETWORK_VPC_ID=vpc-1",
			"CAESIUM_OUTPUT_APPLY_NETWORK_CAESIUM_OUTPUTS_PUBLISHED=1",
			"CAESIUM_OUTPUT_APPLY_NETWORK_EXTRA_FOO=leaked",
			"CAESIUM_OUTPUT_APPLY_NETWORK_EXTRA_CAESIUM_OUTPUTS_PUBLISHED=1",
		}, io.Discard)
		if err != nil {
			t.Fatalf("exportImportedOutputs: %v", err)
		}
		if strings.Join(names, ",") != "vpc_id" {
			t.Fatalf("exported %v; apply-network-extra's outputs belong to a step the manifest never named", names)
		}
		if os.Getenv("TF_VAR_extra_foo") == "leaked" {
			t.Fatal("a sibling step's output reached this stack as a Terraform variable")
		}
	})

	// NC1. Every tf-apply publishes the sentinel, so importing from two upstream
	// applies — the diamond form the contract explicitly allows — used to
	// collide with itself on a key the operator never wrote.
	t.Run("the reserved sentinel never becomes a variable", func(t *testing.T) {
		t.Setenv("TF_VAR_vpc_id", "")
		t.Setenv("TF_VAR_account_id", "")
		t.Setenv("TF_VAR_caesium_outputs_published", "")
		names, err := exportImportedOutputs([]string{"apply-network", "apply-account"}, []string{
			"CAESIUM_OUTPUT_APPLY_NETWORK_VPC_ID=vpc-1",
			"CAESIUM_OUTPUT_APPLY_NETWORK_CAESIUM_OUTPUTS_PUBLISHED=2",
			"CAESIUM_OUTPUT_APPLY_ACCOUNT_ACCOUNT_ID=acct-1",
			"CAESIUM_OUTPUT_APPLY_ACCOUNT_CAESIUM_OUTPUTS_PUBLISHED=2",
		}, io.Discard)
		if err != nil {
			t.Fatalf("importing from two upstream applies must work; got %v", err)
		}
		if strings.Join(names, ",") != "account_id,vpc_id" {
			t.Fatalf("exported %v", names)
		}
		if got := os.Getenv("TF_VAR_caesium_outputs_published"); got != "" {
			t.Fatalf("the protocol sentinel was exported as a Terraform variable (%q)", got)
		}
	})
}

// discoverStepPrefixes is what makes the match step-exact; it has to recover a
// step's prefix from the sentinel and leave a step that publishes no sentinel
// alone rather than inventing a boundary.
func TestDiscoverStepPrefixesRecoversStepsFromTheSentinel(t *testing.T) {
	present := map[string]struct{}{
		"CAESIUM_OUTPUT_APPLY_NETWORK_CAESIUM_OUTPUTS_PUBLISHED":       {},
		"CAESIUM_OUTPUT_APPLY_NETWORK_VPC_ID":                          {},
		"CAESIUM_OUTPUT_APPLY_NETWORK_EXTRA_CAESIUM_OUTPUTS_PUBLISHED": {},
		"CAESIUM_OUTPUT_CHECKOUT_COMMIT":                               {},
		"PATH":                                                         {},
	}
	got := discoverStepPrefixes(present, map[string]string{"CAESIUM_OUTPUT_DISCOVER_NETWORK_": "discover-network"})

	for _, want := range []string{
		"CAESIUM_OUTPUT_APPLY_NETWORK_",
		"CAESIUM_OUTPUT_APPLY_NETWORK_EXTRA_",
		"CAESIUM_OUTPUT_DISCOVER_NETWORK_",
	} {
		if _, ok := got[want]; !ok {
			t.Fatalf("did not discover %s (got %v)", want, got)
		}
	}
	// checkout publishes no sentinel, so its boundary is not recoverable and
	// must not be guessed at.
	if _, ok := got["CAESIUM_OUTPUT_CHECKOUT_"]; ok {
		t.Fatalf("invented a step boundary for a producer with no sentinel: %v", got)
	}

	owner, ok := longestOwner(got, "CAESIUM_OUTPUT_APPLY_NETWORK_EXTRA_FOO")
	if !ok || owner != "CAESIUM_OUTPUT_APPLY_NETWORK_EXTRA_" {
		t.Fatalf("longestOwner = %q, %v; the longer step must win", owner, ok)
	}
}
