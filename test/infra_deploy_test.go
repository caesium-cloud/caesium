//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The Terraform end-to-end gate (design §9 #1–#6, #9, #11).
//
// Everything else in this stream is a unit test on a role binary. This file is
// the only place the whole pattern is exercised the way an operator would meet
// it: a real job manifest, a real Caesium server, real containers, real
// Terraform, real state — and assertions about which steps RE-RAN.
//
// That last part is the point. "Green CI" for a change-gated deploy system is
// worthless if the gate never gates: a run in which every stack re-applies is
// green, and so is a run in which none of them do. The scenarios below assert
// the exact partition of {re-ran, cached, skipped} after each edit, and their
// failure messages name the step that behaved unexpectedly.

// ---------------------------------------------------------------------------
// The deploy pipeline
// ---------------------------------------------------------------------------

// deployStack is one stack group in the hand-written form (design §5.5's
// fan-out form collapsed into explicit steps, which is what §3.5 says the first
// delivery looks like without step-group templates).
type deployStack struct {
	// Name is the stack, and the suffix of its discover/plan/apply steps.
	Name string
	// Root is the stack directory relative to the staged source tree.
	Root string
	// ImportFrom names upstream APPLY steps whose outputs this stack's plan
	// consumes as TF_VAR_*. It also creates the DAG edge, so it is both the
	// data dependency and the ordering one.
	ImportFrom []string
	// Leaf selects the branch form: the plan step becomes `type: branch` and
	// names the apply step in APPLY_STEP, so an empty plan SKIPS the apply
	// rather than running a container that no-ops. Per the plan's branch-form
	// cascade, only a stack with no consumers may use it.
	Leaf bool
	// Env is extra step environment for this stack's plan/apply/drift steps.
	Env map[string]string
}

func (d deployStack) root() string {
	if d.Root != "" {
		return d.Root
	}
	return "stacks/" + d.Name
}

// infraDeployFixture is the hermetic fixture repo plus the three volumes the
// pipeline needs: the staged source tree, the Terraform state, and the provider
// mirror.
type infraDeployFixture struct {
	*infraFixture

	src, state, cache             string
	hostSrc, hostState, hostCache string
}

// newInfraDeployFixture materializes the fixture repo and lays out the volumes.
//
// The three directories are separate on purpose. `src` is wiped and re-cloned
// on every run (the materialize role requires a clean destination), so state
// cannot live in the stack directories the way the fixture's `backend "local"`
// block says it does — it is redirected to `state` with `-backend-config`
// instead. `cache` is the provider mirror, written by exactly one step.
func (s *IntegrationTestSuite) newInfraDeployFixture(name string) *infraDeployFixture {
	s.T().Helper()

	base := s.newInfraFixture(name)
	f := &infraDeployFixture{
		infraFixture: base,
		src:          filepath.Join(base.workspace, "src"),
		state:        filepath.Join(base.workspace, "state"),
		cache:        filepath.Join(base.workspace, "cache"),
		hostSrc:      filepath.Join(base.hostWorkspace, "src"),
		hostState:    filepath.Join(base.hostWorkspace, "state"),
		hostCache:    filepath.Join(base.hostWorkspace, "cache"),
	}
	for _, dir := range []string{f.src, f.state, f.cache} {
		s.Require().NoError(os.MkdirAll(dir, 0o777))
		// The pack images run as uid 10001 while this process is root, and
		// MkdirAll's mode is masked by umask. Fixture plumbing, not a property
		// of the images.
		s.Require().NoError(os.Chmod(dir, 0o777))
	}
	return f
}

// writePipelinePreamble emits the header, the volumes, and the three steps
// every pipeline over this fixture shares: clear the workspace, materialize the
// source, warm the provider mirror.
func (s *IntegrationTestSuite) writePipelinePreamble(b *strings.Builder, f *infraDeployFixture, alias, sparse string) {
	s.T().Helper()

	fmt.Fprintf(b, `
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
volumes:
  - name: fixture
    source:
      bind: %q
  - name: src
    source:
      bind: %q
  - name: tfstate
    source:
      bind: %q
  - name: tfcache
    source:
      bind: %q
steps:
`, alias, f.hostRepo, f.hostSrc, f.hostState, f.hostCache)

	// The materialize role refuses a non-empty destination, so the previous
	// run's tree is cleared first. Both steps are cache: false — they must run
	// on EVERY run, because a cached checkout would pin the pipeline to
	// whatever tree the first run happened to stage and no edit would ever be
	// seen again.
	fmt.Fprintf(b, `  - name: prepare
    image: alpine:3.23
    cache: false
    command: ["sh", "-c", "find /src -mindepth 1 -delete && echo cleared"]
    volumeMounts:
      - {volume: src, path: /src}
    next: [checkout]

  - name: checkout
    image: %s
    cache: false
    dependsOn: [prepare]
    env:
      GIT_URL: "file:///fixture"
      GIT_REF: "main"
      GIT_SPARSE: %q
      DEST: "/src"
    volumeMounts:
      - {volume: fixture, path: /fixture, readOnly: true}
      - {volume: src, path: /src}

  - name: warm-cache
    image: %s
    cache: false
    dependsOn: [checkout]
    env:
      SRC: "/src"
      CACHE_DIR: "/cache"
    volumeMounts:
      - {volume: src, path: /src, readOnly: true}
      - {volume: tfcache, path: /cache}
`, s.packImage("git-source"), sparse, s.packImage("tf-warm"))
}

// deployManifest renders the hand-written three-step-per-stack pipeline.
func (s *IntegrationTestSuite) deployManifest(f *infraDeployFixture, alias, sparse string, stacks []deployStack) string {
	s.T().Helper()

	var b strings.Builder
	s.writePipelinePreamble(&b, f, alias, sparse)

	for _, stack := range stacks {
		planDeps := append([]string{"discover-" + stack.Name, "warm-cache"}, stack.ImportFrom...)

		fmt.Fprintf(&b, `
  - name: discover-%[1]s
    image: %[2]s
    dependsOn: [checkout]
    env:
      SCAN_ROOT: "/src/%[3]s"
      TF_WORKSPACE: "default"
    volumeMounts:
      - {volume: src, path: /src, readOnly: true}

  - name: plan-%[1]s
    image: %[4]s
    command: ["tf-plan"]
    cache:
      version: 1
      chain: values
    dependsOn: [%[5]s]
`, stack.Name, s.packImage("tf-discover"), stack.root(), s.packImage("tf-runner"), strings.Join(planDeps, ", "))

		if stack.Leaf {
			fmt.Fprintf(&b, "    type: branch\n    next: [apply-%s]\n", stack.Name)
		}
		b.WriteString("    env:\n")
		s.writeRunnerEnv(&b, f, stack)
		if len(stack.ImportFrom) > 0 {
			fmt.Fprintf(&b, "      IMPORT_OUTPUTS_FROM: %q\n", strings.Join(stack.ImportFrom, ","))
		}
		if stack.Leaf {
			fmt.Fprintf(&b, "      APPLY_STEP: %q\n", "apply-"+stack.Name)
		}
		s.writeRunnerMounts(&b)

		// The dataset name carries the job alias as its environment segment.
		// A dataset is global — Caesium rejects two jobs producing the same
		// name outright — and each scenario here applies its own job over its
		// own fixture, so a fixed "test" segment would make the second scenario
		// to run fail at apply time. The spec's own form is
		// `stack:prod/${CAESIUM_PARTITION}`, i.e. environment then stack, so
		// this is that shape with the scenario standing in for the environment.
		fmt.Fprintf(&b, `
  - name: apply-%[1]s
    image: %[2]s
    command: ["tf-apply"]
    cache:
      version: 1
      chain: values
      ttl: never
    dependsOn: [plan-%[1]s]
    datasets:
      produces:
        - name: "stack:%[3]s/%[1]s"
    env:
      PLAN_STEP: "plan-%[1]s"
`, stack.Name, s.packImage("tf-runner"), alias)
		s.writeRunnerEnv(&b, f, stack)
		s.writeRunnerMounts(&b)
	}

	return b.String()
}

// writeRunnerEnv emits the environment every tf-runner phase of one stack
// shares.
func (s *IntegrationTestSuite) writeRunnerEnv(b *strings.Builder, _ *infraDeployFixture, stack deployStack) {
	fmt.Fprintf(b, `      STACK_ROOT: "/src/%s"
      TF_WORKSPACE: "default"
      TF_CLI_CONFIG_FILE: "/cache/terraformrc"
      BACKEND_CONFIG: "path=/state/%s.tfstate"
`, stack.root(), stack.Name)
	for _, key := range sortedKeys(stack.Env) {
		fmt.Fprintf(b, "      %s: %q\n", key, stack.Env[key])
	}
}

// writeRunnerMounts emits the volume mounts every tf-runner phase shares.
//
// The cache volume is readOnly on every step but warm-cache. That is the
// single-writer invariant of design §3.4: a filesystem mirror is safe under
// parallel `init` only because nothing else writes to it, and `readOnly: true`
// is the shipped primitive that enforces it (§5.7).
func (s *IntegrationTestSuite) writeRunnerMounts(b *strings.Builder) {
	b.WriteString(`    volumeMounts:
      - {volume: src, path: /src}
      - {volume: tfstate, path: /state}
      - {volume: tfcache, path: /cache, readOnly: true}
`)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// threeStackDeploy is the §5.5 shape: network exports a vpc_id that app-web
// consumes, account is ordered alongside but consumes nothing, and app-web is
// the only leaf and therefore the only stack allowed the branch form.
func threeStackDeploy() []deployStack {
	return []deployStack{
		{Name: "network"},
		{Name: "account"},
		{Name: "app-web", ImportFrom: []string{"apply-network"}, Leaf: true},
	}
}

// deployRun is one run of the pipeline plus the views the assertions need.
type deployRun struct {
	job      *jobSummary
	run      *runResponse
	statuses map[string]string
	outputs  map[string]map[string]string
}

// applyDeployJob applies the manifest once. Later runs re-use the same job so
// the cache keys line up run over run.
func (s *IntegrationTestSuite) applyDeployJob(f *infraDeployFixture, alias, sparse string, stacks []deployStack) *jobSummary {
	s.T().Helper()

	dir := s.writeJobManifest(s.deployManifest(f, alias, sparse, stacks))
	defer func() { _ = os.RemoveAll(dir) }()

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	return s.requireJobByAlias(alias)
}

// runDeploy triggers one run and waits for it.
func (s *IntegrationTestSuite) runDeploy(job *jobSummary, label string) deployRun {
	s.T().Helper()

	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, 10*time.Minute)
	result := deployRun{
		job:      job,
		run:      run,
		statuses: s.taskStatusesByName(job.ID, run),
		outputs:  s.taskOutputsByName(job.ID, run),
	}
	s.T().Logf("%s: run %s -> %s, statuses %v", label, runID, run.Status, result.statuses)
	return result
}

// requireGreen fails with the whole status map, which is the only useful thing
// to look at when a twelve-step pipeline goes red.
func (s *IntegrationTestSuite) requireGreen(r deployRun, label string) {
	s.T().Helper()
	if r.run.Status == "succeeded" {
		return
	}
	for _, task := range r.run.Tasks {
		if task.Error != "" {
			s.T().Logf("%s: task %s failed: %s", label, task.ID, task.Error)
		}
	}
	s.Require().Equal("succeeded", r.run.Status, "%s failed; statuses: %v", label, r.statuses)
}

// requireStackStatuses asserts the exact {re-ran, cached, skipped} partition
// across every stack's plan and apply, naming the offender.
//
// This is the load-bearing assertion of the whole plan (design §9 #2). A
// deploy system that re-applies everything is green; so is one that applies
// nothing. Only the partition distinguishes a working change gate from a broken
// one, so the failure message says which step behaved unexpectedly rather than
// dumping a map and leaving the reader to diff it.
func (s *IntegrationTestSuite) requireStackStatuses(r deployRun, label string, want map[string]string) {
	s.T().Helper()

	var wrong []string
	for step, expected := range want {
		if got := r.statuses[step]; got != expected {
			wrong = append(wrong, fmt.Sprintf("%s was %q, expected %q", step, got, expected))
		}
	}
	sort.Strings(wrong)
	s.Require().Emptyf(wrong, "%s: %s\nfull statuses: %v", label, strings.Join(wrong, "; "), r.statuses)
}

// ---------------------------------------------------------------------------
// §9 #1–#5, #9: the change gate
// ---------------------------------------------------------------------------

// TestInfraDeployReAppliesOnlyWhatChanged is design §9 scenarios 1–5 and 9,
// driven as one sequence over a single fixture because each assertion is about
// the DIFFERENCE between consecutive runs.
func (s *IntegrationTestSuite) TestInfraDeployReAppliesOnlyWhatChanged() {
	s.requireInfraLane()

	f := s.newInfraDeployFixture("deploy")
	alias := fmt.Sprintf("infra-deploy-%d", time.Now().UnixNano())
	job := s.applyDeployJob(f, alias, "stacks/** modules/**", threeStackDeploy())

	// ---- Run 1: the baseline deploy -------------------------------------
	first := s.runDeploy(job, "run 1 (baseline)")
	s.requireGreen(first, "run 1")
	s.requireStackStatuses(first, "run 1", map[string]string{
		"plan-network": "succeeded", "apply-network": "succeeded",
		"plan-account": "succeeded", "apply-account": "succeeded",
		"plan-app-web": "succeeded", "apply-app-web": "succeeded",
	})

	// The propose contract the Console renders (§5.6, and the E1 renderer's
	// exact expectations): a kind, a summary that is a JSON-encoded STRING, and
	// an artifact key naming a sibling reference.
	planOut := first.outputs["plan-network"]
	s.Require().NotNil(planOut, "plan-network emitted no output row")
	s.Equal("terraform.plan.v1", planOut["proposal_kind"])
	s.Equal("plan", planOut["proposal_artifact"])
	var summary struct {
		Add       int `json:"add"`
		Resources []struct {
			Address string `json:"address"`
			Action  string `json:"action"`
		} `json:"resources"`
	}
	s.Require().NoError(json.Unmarshal([]byte(planOut["proposal_summary"]), &summary),
		"proposal_summary must be a JSON-encoded string, got %q", planOut["proposal_summary"])
	s.Positive(summary.Add, "the first plan should create resources")
	s.NotEmpty(summary.Resources, "the summary should list the resources it counted")
	// The artifact rides as an encoded output reference: path plus digest plus
	// size, with the digest folded into the consuming step's identity hash.
	// The Console decodes exactly this shape.
	var artifact struct {
		Ref    int    `json:"caesiumOutputRef"`
		Path   string `json:"path"`
		Digest string `json:"digest"`
		Size   int64  `json:"size"`
	}
	s.Require().NoErrorf(json.Unmarshal([]byte(planOut[planOut["proposal_artifact"]]), &artifact),
		"proposal_artifact names key %q, whose stored value is not an output reference: %q",
		planOut["proposal_artifact"], planOut[planOut["proposal_artifact"]])
	s.Equal(1, artifact.Ref, "the reference should carry its encoding version")
	s.True(strings.HasPrefix(artifact.Path, "/src/stacks/network/"), "artifact path = %q", artifact.Path)
	s.True(strings.HasPrefix(artifact.Digest, "sha256:"), "artifact digest = %q", artifact.Digest)
	s.Positive(artifact.Size, "the artifact reference reported no size")

	// The cross-stack wiring: app-web's endpoint embeds the vpc_id network
	// exported, which can only have arrived through IMPORT_OUTPUTS_FROM.
	vpcID := first.outputs["apply-network"]["vpc_id"]
	s.Require().NotEmpty(vpcID, "apply-network published no vpc_id")
	s.Contains(first.outputs["apply-app-web"]["endpoint"], vpcID,
		"app-web did not receive network's vpc_id as TF_VAR_vpc_id")

	// §9 #9: the sensitive output must not have escaped anywhere.
	s.assertSensitiveOutputNeverEscaped(f, job, first)

	// ---- Run 2: nothing changed (§9 #1, #5) -----------------------------
	second := s.runDeploy(job, "run 2 (unchanged)")
	s.requireGreen(second, "run 2")
	s.requireStackStatuses(second, "run 2 (unchanged repo)", map[string]string{
		"plan-network": "cached", "apply-network": "cached",
		"plan-account": "cached", "apply-account": "cached",
		"plan-app-web": "cached", "apply-app-web": "cached",
	})
	// The warm step is never Caesium-cached — it always starts a container and
	// self-checks the marker INSIDE the volume, which is what makes a recreated
	// volume self-healing (§6.3).
	s.Equal("succeeded", second.statuses["warm-cache"],
		"the warm step must always run; a cache hit would mean no container looked inside the volume")
	warmLog := s.taskLog(job.ID, second.run.ID, s.jobTaskIDByName(job.ID, "warm-cache"))
	s.Contains(warmLog, "already warm",
		"the second run's warm step should have exited on its marker, not re-mirrored")

	// ---- Run 3: edit one stack (§9 #2 — LOAD-BEARING) -------------------
	//
	// The edit has to change what Terraform would DO, not merely what discover
	// digests: an edit that produces an empty plan would leave app-web's apply
	// skipped by the branch marker, and the scenario would be asserting the
	// wrong thing. app-web's replica_count is pinned by extra.auto.tfvars.json,
	// which overrides the variable's default, so that file is the one to move.
	f.EditFile("stacks/app-web/extra.auto.tfvars.json", "{\n  \"replica_count\": 7\n}\n")
	third := s.runDeploy(job, "run 3 (app-web edited)")
	s.requireGreen(third, "run 3")
	s.requireStackStatuses(third, "run 3: editing stacks/app-web must re-run app-web AND NOTHING ELSE", map[string]string{
		"plan-network": "cached", "apply-network": "cached",
		"plan-account": "cached", "apply-account": "cached",
		"plan-app-web": "succeeded", "apply-app-web": "succeeded",
	})

	// ---- Run 4: edit a shared module (§9 #3) ----------------------------
	f.EditFile("modules/vpc/main.tf", s.editedVPCModule(f))
	fourth := s.runDeploy(job, "run 4 (modules/vpc edited)")
	s.requireGreen(fourth, "run 4")
	s.requireStackStatuses(fourth, "run 4: editing modules/vpc must re-run exactly its users (network, app-web)", map[string]string{
		"plan-network": "succeeded", "apply-network": "succeeded",
		"plan-account": "cached", "apply-account": "cached",
		"plan-app-web": "succeeded", "apply-app-web": "succeeded",
	})

	// ---- Run 5: change network's exported value (§9 #4) -----------------
	// app-web's own code and fingerprint are untouched here; only the VALUE
	// network exports moves. Under `chain: values` that still has to bust
	// app-web's plan, or a stack would keep deploying against a stale upstream.
	f.EditFile("stacks/network/main.tf",
		strings.Replace(f.ReadFile("stacks/network/main.tf"), `name       = "network"`, `name       = "network-renamed"`, 1))
	beforeVPC := fourth.outputs["apply-network"]["vpc_id"]
	fifth := s.runDeploy(job, "run 5 (network's vpc_id changed)")
	s.requireGreen(fifth, "run 5")
	s.requireStackStatuses(fifth, "run 5: a changed upstream OUTPUT must re-plan app-web though its code is untouched", map[string]string{
		"plan-network": "succeeded", "apply-network": "succeeded",
		"plan-account": "cached", "apply-account": "cached",
		"plan-app-web": "succeeded", "apply-app-web": "succeeded",
	})
	s.NotEqual(beforeVPC, fifth.outputs["apply-network"]["vpc_id"],
		"the edit was supposed to move network's vpc_id; without that this scenario proves nothing")
	s.Contains(fifth.outputs["apply-app-web"]["endpoint"], fifth.outputs["apply-network"]["vpc_id"],
		"app-web re-planned but against the old vpc_id")

	// discover is not cache-gated by its own fingerprint — it is the thing that
	// PRODUCES the fingerprint — so it re-runs whenever the checkout moved, and
	// that is exactly why plan needs `chain: values`: under the default
	// transitive chain, discover-account re-running would bust plan-account.
	s.Equal("succeeded", fifth.statuses["discover-account"],
		"discover-account should have re-run (the tree moved), which is what makes plan-account staying cached meaningful")
}

// editedVPCModule changes the shared module in a way Terraform will act on.
//
// A cosmetic edit would move the fingerprint but produce an empty plan, and the
// leaf stack's apply would then be skipped rather than re-run — which would
// make this scenario assert something other than what it claims to.
func (s *IntegrationTestSuite) editedVPCModule(f *infraDeployFixture) string {
	s.T().Helper()
	original := f.ReadFile("modules/vpc/main.tf")
	edited := strings.Replace(original, "byte_length = 4", "byte_length = 6", 1)
	s.Require().NotEqual(original, edited, "modules/vpc no longer contains the line this edit targets")
	return edited
}

// assertSensitiveOutputNeverEscaped is design §9 scenario 9.
//
// The value is read out of Terraform's own state file, so the assertion is
// against the REAL secret rather than a pattern: if the value ever changes
// shape, the test still checks the right string. Three surfaces are covered
// because a step output reaches all three by three different code paths — the
// stored output row, the run API's serialization of it, and `caesium why`'s
// provenance rendering.
func (s *IntegrationTestSuite) assertSensitiveOutputNeverEscaped(f *infraDeployFixture, job *jobSummary, r deployRun) {
	s.T().Helper()

	secret := s.sensitiveStateValue(f, "network", "admin_token")
	s.Require().NotEmpty(secret, "the fixture's sensitive output was not found in state; this assertion would be vacuous")
	s.Require().GreaterOrEqual(len(secret), 16, "the canary is too short to be a meaningful needle")

	// 1. The task output row.
	applyOut := r.outputs["apply-network"]
	s.Require().NotNil(applyOut, "apply-network emitted no output row")
	s.NotContains(applyOut, "admin_token", "the sensitive output was published as a step output")
	for key, value := range applyOut {
		s.NotContainsf(value, secret, "the sensitive value leaked through output key %q", key)
	}
	// The non-sensitive outputs are still there — otherwise this would pass by
	// publishing nothing at all.
	s.NotEmpty(applyOut["vpc_id"], "apply-network published no non-sensitive outputs either")

	// 2. The run API response, serialized end to end.
	body := s.rawGET(fmt.Sprintf("/v1/jobs/%s/runs/%s", job.ID, r.run.ID))
	s.NotContains(body, secret, "the sensitive value reached the run API response")

	// 3. caesium why --json, captured on stdout only.
	out, err := s.runCLIStdout("why", r.run.ID, "--job-id", job.ID, "--task", "apply-network",
		"--json", "--server", s.caesiumURL)
	s.Require().NoError(err, "caesium why failed")
	s.Require().NotEmpty(strings.TrimSpace(out), "caesium why --json wrote nothing to stdout")
	var explanation map[string]any
	s.Require().NoError(json.Unmarshal([]byte(out), &explanation), "caesium why --json is not parseable: %s", out)
	s.NotContains(out, secret, "the sensitive value reached caesium why --json")
}

// sensitiveStateValue reads one root-module output straight out of the stack's
// Terraform state file on the shared volume.
func (s *IntegrationTestSuite) sensitiveStateValue(f *infraDeployFixture, stack, output string) string {
	s.T().Helper()

	data, err := os.ReadFile(filepath.Join(f.state, stack+".tfstate"))
	s.Require().NoError(err, "reading %s state", stack)

	var state struct {
		Outputs map[string]struct {
			Value     any  `json:"value"`
			Sensitive bool `json:"sensitive"`
		} `json:"outputs"`
	}
	s.Require().NoError(json.Unmarshal(data, &state))
	meta, ok := state.Outputs[output]
	s.Require().Truef(ok, "state for %s has no output %q (outputs: %v)", stack, output, state.Outputs)
	s.Require().Truef(meta.Sensitive, "output %q is not marked sensitive in state; the fixture changed", output)
	value, ok := meta.Value.(string)
	s.Require().Truef(ok, "output %q is not a string", output)
	return value
}

// rawGET returns an API response body verbatim, so an assertion can scan the
// whole serialized payload rather than only the fields a typed struct happens
// to model.
func (s *IntegrationTestSuite) rawGET(path string) string {
	s.T().Helper()

	resp, err := s.doJSONRequest(http.MethodGet, s.caesiumURL+path, nil)
	s.Require().NoError(err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, resp.StatusCode, "GET %s: %s", path, body)
	return string(body)
}

// ---------------------------------------------------------------------------
// The drift job (design §3.2, §6.6)
// ---------------------------------------------------------------------------

// driftManifest renders the drift job: the same materialize and warm steps, one
// `tf-drift` step per stack, and nothing else.
//
// Every drift step carries `cache: false`, not merely "no cache block".
//
// The design says drift steps carry no cache block, which is correct when the
// server's cache default is off — but this lane runs with
// CAESIUM_CACHE_ENABLED=true, and an omitted block then means "cacheable". The
// drift step's cache key is its spec plus its predecessors' outputs, and NONE
// of those move when a resource is deleted out of band: the repo is identical,
// so the second run would be a cache hit replaying drift=false and the drift
// job would report clean forever. Caching the thing whose entire purpose is to
// detect out-of-band change defeats it, so the manifest says so explicitly
// rather than depending on a server default.
func (s *IntegrationTestSuite) driftManifest(f *infraDeployFixture, alias, sparse string, stacks []deployStack) string {
	s.T().Helper()

	var b strings.Builder
	s.writePipelinePreamble(&b, f, alias, sparse)

	previous := "warm-cache"
	for _, stack := range stacks {
		fmt.Fprintf(&b, `
  - name: drift-%[1]s
    image: %[2]s
    command: ["tf-drift"]
    cache: false
    dependsOn: [%[3]s]
    env:
`, stack.Name, s.packImage("tf-runner"), previous)
		s.writeRunnerEnv(&b, f, stack)
		s.writeRunnerMounts(&b)
		// Serialized rather than parallel so a failing stack cannot race a
		// healthy one into being skipped, which would make the "the other
		// stacks still report clean" half of the assertion flaky.
		previous = "drift-" + stack.Name
	}
	return b.String()
}

// driftStacks is the drift job's unit set: one stack whose provider read
// consults the real world (so drift can actually be detected) and one that is
// merely along for the ride, to prove drift is attributed per stack rather than
// smeared across the run.
func driftStacks(statePath string) []deployStack {
	return []deployStack{
		{Name: "network"},
		{
			Name: "canary",
			Root: "drift/canary",
			Env:  map[string]string{"TF_VAR_canary_path": statePath},
		},
	}
}

// TestInfraDriftJobGoesRedOnAnOutOfBandChange is design §9's drift scenario and
// the reason §3.2 calls the drift job mandatory rather than optional.
//
// The fingerprint gate answers "did my code change?" and is blind to drift by
// construction. This job is the only thing that closes that hole, so it has to
// be proven to (a) stay green and quiet when nothing has moved, (b) go RED —
// not green-with-a-warning — when something has, and (c) never be served from
// cache.
func (s *IntegrationTestSuite) TestInfraDriftJobGoesRedOnAnOutOfBandChange() {
	s.requireInfraLane()

	f := s.newInfraDeployFixture("drift")
	canaryFile := filepath.Join(f.state, "canary.txt")
	stacks := driftStacks("/state/canary.txt")

	// Deploy first: a drift job over a stack that was never applied has nothing
	// to refresh, and "clean" would be indistinguishable from "empty".
	deployAlias := fmt.Sprintf("infra-drift-deploy-%d", time.Now().UnixNano())
	deployJob := s.applyDeployJob(f, deployAlias, "stacks/** modules/** drift/**", stacks)
	deployed := s.runDeploy(deployJob, "deploy")
	s.requireGreen(deployed, "deploy")
	s.Require().FileExists(canaryFile, "the deploy did not create the resource the drift job watches")

	driftAlias := fmt.Sprintf("infra-drift-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(s.driftManifest(f, driftAlias, "stacks/** modules/** drift/**", stacks))
	defer func() { _ = os.RemoveAll(dir) }()
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	driftJob := s.requireJobByAlias(driftAlias)

	// ---- Clean state: green, drift=false --------------------------------
	clean := s.runDeploy(driftJob, "drift run 1 (clean)")
	s.requireGreen(clean, "drift run 1: a clean stack must not raise an incident")
	for _, stack := range []string{"network", "canary"} {
		step := "drift-" + stack
		s.Equal("succeeded", clean.statuses[step], "%s should have run and passed", step)
		s.Equal("false", clean.outputs[step]["drift"], "%s reported drift on a clean stack", step)
		s.NotContains(clean.outputs[step], "drift_summary", "%s reported a summary with no drift", step)
		// A refresh-only plan is not something anyone applies, so offering it as
		// a reviewable artifact would invite exactly that.
		s.NotContains(clean.outputs[step], "proposal_artifact", "%s emitted an artifact", step)
	}

	// ---- Out-of-band change: red, drift=true ----------------------------
	s.Require().NoError(os.Remove(canaryFile), "removing the managed file out of band")

	drifted := s.runDeploy(driftJob, "drift run 2 (out-of-band deletion)")
	s.Require().Equal("failed", drifted.run.Status,
		"drift must make the run RED so the shipped notification, callback and remediation paths fire; statuses: %v",
		drifted.statuses)
	s.Equal("failed", drifted.statuses["drift-canary"], "the drifted stack's step must fail")

	canary := drifted.outputs["drift-canary"]
	s.Require().NotNil(canary, "the drift step failed without emitting its output; the operator gets a red run and no diagnosis")
	s.Equal("true", canary["drift"])
	s.Require().NotEmpty(canary["drift_summary"], "drift was reported with no summary")
	var summary struct {
		Destroy int `json:"destroy"`
		Outputs int `json:"outputs"`
	}
	s.Require().NoError(json.Unmarshal([]byte(canary["drift_summary"]), &summary),
		"drift_summary must be a JSON-encoded string, got %q", canary["drift_summary"])
	s.Positive(summary.Destroy+summary.Outputs,
		"drift was reported with an all-zero summary, which reads as a false alarm: %s", canary["drift_summary"])

	// The healthy stack is still reported healthy, and — the point of `cache:
	// false` — it RAN rather than replaying the previous run's answer. A cached
	// drift step would report clean forever.
	s.Equal("succeeded", drifted.statuses["drift-network"], "the healthy stack's drift step should still pass")
	s.NotEqual("cached", drifted.statuses["drift-network"],
		"a cached drift step defeats the entire purpose of the drift job")
	s.Equal("false", drifted.outputs["drift-network"]["drift"])

	// And the failure is legible in the log, which is what an operator opens
	// first.
	log := s.taskLog(driftJob.ID, drifted.run.ID, s.jobTaskIDByName(driftJob.ID, "drift-canary"))
	s.Contains(log, "drift detected", "the drift step's log should say what happened:\n%s", log)
}

// ---------------------------------------------------------------------------
// §9 #6: an empty plan is green in both forms
// ---------------------------------------------------------------------------

// TestInfraDeployEmptyPlanIsGreenInBothForms is design §9 scenario 6.
//
// Both forms have to be proven, because they fail differently. The container
// no-op form can regress into "apply ran Terraform anyway" (invisible: the run
// is still green), and the branch form can regress into "apply ran" or into
// "apply skipped and the run went red".
func (s *IntegrationTestSuite) TestInfraDeployEmptyPlanIsGreenInBothForms() {
	s.requireInfraLane()

	f := s.newInfraDeployFixture("empty-plan")
	alias := fmt.Sprintf("infra-empty-plan-%d", time.Now().UnixNano())
	job := s.applyDeployJob(f, alias, "stacks/** modules/**", threeStackDeploy())

	s.requireGreen(s.runDeploy(job, "run 1 (baseline)"), "run 1")

	// A comment-only edit moves each stack's fingerprint — so plan re-runs —
	// while changing nothing Terraform would act on. That is the only way to
	// reach a genuinely empty plan on a re-run: an unchanged tree is cached
	// before Terraform is ever invoked.
	f.EditFile("stacks/account/main.tf", "# a comment, and nothing else, has changed\n"+f.ReadFile("stacks/account/main.tf"))
	f.EditFile("stacks/app-web/main.tf", "# a comment, and nothing else, has changed\n"+f.ReadFile("stacks/app-web/main.tf"))

	second := s.runDeploy(job, "run 2 (comment-only edits)")
	s.requireGreen(second, "run 2: an empty plan must be green, not red")

	// The container no-op form (non-leaf): the apply step RAN and succeeded,
	// without invoking Terraform, and still published its outputs.
	s.requireStackStatuses(second, "run 2", map[string]string{
		"plan-account":  "succeeded",
		"apply-account": "succeeded",
		"plan-app-web":  "succeeded",
		// The branch form: the apply is skipped by the DAG, not run.
		"apply-app-web": "skipped",
		// Untouched stack stays cached, which keeps the comparison honest.
		"plan-network":  "cached",
		"apply-network": "cached",
	})

	accountSummary := second.outputs["plan-account"]["proposal_summary"]
	s.Require().NotEmpty(accountSummary, "plan-account emitted no summary")
	s.Contains(accountSummary, `"add":0`, "the comment-only edit should have produced an empty plan: %s", accountSummary)
	s.NotContains(second.outputs["plan-account"], "proposal_artifact",
		"an empty plan must name no artifact")

	applyLog := s.taskLog(job.ID, second.run.ID, s.jobTaskIDByName(job.ID, "apply-account"))
	s.Contains(applyLog, "not invoking terraform apply",
		"the non-leaf apply invoked Terraform for an empty plan")
	// The always-emit rule: a stack whose plan was empty is still the producer
	// its consumers read from, so its outputs must still be published.
	s.NotEmpty(second.outputs["apply-account"]["account_id"],
		"a no-op apply stopped publishing outputs; every consumer would see the stack as vanished")

	// The branch form's negative half: no branch marker was emitted at all.
	planLog := s.taskLog(job.ID, second.run.ID, s.jobTaskIDByName(job.ID, "plan-app-web"))
	s.NotContains(planLog, "##caesium::branch",
		"plan-app-web emitted a branch marker for an empty plan")
	s.NotContains(second.outputs["plan-app-web"], "proposal_artifact",
		"an empty plan must name no artifact")
}

// ---------------------------------------------------------------------------
// §9 #11: a module nested two levels deep
// ---------------------------------------------------------------------------

// TestInfraDeployNestedModuleEditReAppliesItsUsers is design §9 scenario 11.
//
// modules/tags/inner is reached by a RELATIVE source two levels below each root
// module. `terraform modules -json` renders that call as {"key":"inner",
// "source":"./inner"} — a local name with no parent path and a source relative
// to a parent it never identifies — so a fingerprint built from it could not
// resolve the directory and would silently not cover it. Editing a file in
// there must move every consuming stack's fingerprint.
func (s *IntegrationTestSuite) TestInfraDeployNestedModuleEditReAppliesItsUsers() {
	s.requireInfraLane()

	f := s.newInfraDeployFixture("nested-module")
	alias := fmt.Sprintf("infra-nested-%d", time.Now().UnixNano())
	job := s.applyDeployJob(f, alias, "stacks/** modules/**", threeStackDeploy())

	s.requireGreen(s.runDeploy(job, "run 1 (baseline)"), "run 1")

	f.EditFile("modules/tags/inner/main.tf", `variable "stack" {
  type        = string
  description = "Name of the stack these tags belong to."
}

output "base_tags" {
  value = {
    managed_by = "caesium"
    stack      = var.stack
    tier       = "nested-edit"
  }
}
`)

	second := s.runDeploy(job, "run 2 (modules/tags/inner edited)")
	s.requireGreen(second, "run 2")
	// All three stacks call modules/tags, so all three must re-run. The
	// discriminating half of this pair is scenario #3 above, where editing
	// modules/vpc leaves account cached: together they show the fingerprint
	// tracks the real module graph rather than "anything under modules/".
	s.requireStackStatuses(second, "run 2: editing a two-level-nested module must re-run every stack that uses it", map[string]string{
		"plan-network": "succeeded", "apply-network": "succeeded",
		"plan-account": "succeeded", "apply-account": "succeeded",
		"plan-app-web": "succeeded", "apply-app-web": "succeeded",
	})
	s.Contains(second.outputs["apply-account"]["tags"], "nested-edit",
		"the nested module's new value did not reach the applied stack")
}
