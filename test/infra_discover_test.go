//go:build integration

package test

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// The materialize + discover half of the unit-pipeline pattern, driven through
// the live server against the hermetic fixture repo (design §9 scenarios 7, 8
// and 10, plus the git-source output contract).
//
// The plan and apply steps here are placeholders: what these scenarios assert
// about them is that they do NOT run when discover fails. tf-runner's own
// behaviour is Stream C's.

// infraPipelineOptions parameterizes the manifest the scenarios below apply.
type infraPipelineOptions struct {
	alias string
	// scanRoot is the path tf-discover scans, inside the staged source volume.
	scanRoot string
	// sshKeyRef, when set, is the GIT_SSH_KEY value given to git-source.
	sshKeyRef string
}

// infraPipelineManifest renders a four-step pipeline: materialize the fixture,
// discover and fingerprint it, then two placeholder steps standing in for
// propose and apply.
func (s *IntegrationTestSuite) infraPipelineManifest(f *infraFixture, opts infraPipelineOptions) string {
	s.T().Helper()

	sshKey := ""
	if opts.sshKeyRef != "" {
		sshKey = fmt.Sprintf("\n      GIT_SSH_KEY: %q", opts.sshKeyRef)
	}

	// The bind sources are HOST paths: the server resolves them through the
	// host Docker daemon, which knows nothing about this test container's
	// mounts. The fixture repo is mounted read-only (git only reads it) and the
	// workspace read-write for checkout, then read-only for discover — which is
	// only possible because tf-discover relocates TF_DATA_DIR out of the source.
	return fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %[1]s
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
volumes:
  - name: fixture
    source:
      bind: %[2]q
  - name: src
    source:
      bind: %[3]q
steps:
  - name: checkout
    image: %[4]s
    env:
      GIT_URL: "file:///fixture"
      GIT_REF: "main"
      GIT_SPARSE: "stacks/** modules/** fail-closed/**"
      DEST: "/src"%[7]s
    volumeMounts:
      - {volume: fixture, path: /fixture, readOnly: true}
      - {volume: src, path: /src}
    next: discover
  - name: discover
    image: %[5]s
    dependsOn: [checkout]
    env:
      SCAN_ROOT: %[6]q
      TF_WORKSPACE: "default"
    volumeMounts:
      - {volume: src, path: /src, readOnly: true}
    next: plan
  - name: plan
    image: alpine:3.23
    dependsOn: [discover]
    command: ["sh", "-c", "echo '##caesium::output {\"proposal_kind\":\"placeholder\"}'"]
    next: apply
  - name: apply
    image: alpine:3.23
    dependsOn: [plan]
    command: ["sh", "-c", "echo '##caesium::output {\"applied\":\"true\"}'"]
`,
		opts.alias,
		f.hostRepo,
		f.hostWorkspace,
		s.reagentImage("git-source"),
		s.reagentImage("tf-discover"),
		opts.scanRoot,
		sshKey,
	)
}

// runInfraPipeline applies the pipeline and runs it once, returning the
// completed run.
func (s *IntegrationTestSuite) runInfraPipeline(f *infraFixture, opts infraPipelineOptions) (*jobSummary, *runResponse) {
	s.T().Helper()

	dir := s.writeJobManifest(s.infraPipelineManifest(f, opts))
	defer func() { _ = os.RemoveAll(dir) }()

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(opts.alias)
	runID := s.triggerRun(job.ID)
	return job, s.awaitRun(job.ID, runID, 5*time.Minute)
}

// taskOutputsByName maps each task's name to its structured output row.
func (s *IntegrationTestSuite) taskOutputsByName(jobID string, run *runResponse) map[string]map[string]string {
	s.T().Helper()

	var tasks []struct {
		ID   string `json:"ID"`
		Name string `json:"Name"`
	}
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/tasks", jobID), &tasks)
	nameByID := make(map[string]string, len(tasks))
	for _, t := range tasks {
		nameByID[t.ID] = t.Name
	}

	out := make(map[string]map[string]string, len(run.Tasks))
	for _, t := range run.Tasks {
		name := nameByID[t.ID]
		if name == "" {
			name = t.ID
		}
		out[name] = t.Output
	}
	return out
}

// taskLog returns the persisted log text for one task in a run.
func (s *IntegrationTestSuite) taskLog(jobID, runID, taskID string) string {
	s.T().Helper()

	resp, err := s.doJSONRequest(http.MethodGet,
		fmt.Sprintf("%s/v1/jobs/%s/runs/%s/logs?task_id=%s", s.caesiumURL, jobID, runID, taskID), nil)
	s.Require().NoError(err)
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, resp.StatusCode, "logs for task %s: %s", taskID, body)
	return string(body)
}

// TestInfraMaterializeAndDiscoverEmitTheirContracts drives the happy path:
// git-source stages the fixture and reports its identity, tf-discover
// fingerprints a stack, and the pipeline runs to completion.
//
// It also guards the git-source secret handling: GIT_SSH_KEY arrives through
// the real secret://env provider and its resolved value must never appear in a
// task log.
func (s *IntegrationTestSuite) TestInfraMaterializeAndDiscoverEmitTheirContracts() {
	s.requireInfraLane()

	keyRef := strings.TrimSpace(os.Getenv("CAESIUM_INFRA_DEPLOY_KEY_REF"))
	canary := strings.TrimSpace(os.Getenv("CAESIUM_INFRA_DEPLOY_KEY_CANARY"))
	s.Require().NotEmpty(keyRef, "the lane must supply CAESIUM_INFRA_DEPLOY_KEY_REF")
	s.Require().NotEmpty(canary, "the lane must supply CAESIUM_INFRA_DEPLOY_KEY_CANARY")

	f := s.newInfraFixture("contract")
	alias := fmt.Sprintf("infra-contract-%d", time.Now().UnixNano())
	job, run := s.runInfraPipeline(f, infraPipelineOptions{
		alias:     alias,
		scanRoot:  "/src/stacks/network",
		sshKeyRef: keyRef,
	})

	statuses := s.taskStatusesByName(job.ID, run)
	s.Require().Equal("succeeded", run.Status, "task statuses: %v", statuses)
	for _, name := range []string{"checkout", "discover", "plan", "apply"} {
		s.Equal("succeeded", statuses[name], "%s should have run", name)
	}

	outputs := s.taskOutputsByName(job.ID, run)

	checkout := outputs["checkout"]
	s.Require().NotNil(checkout, "checkout emitted no output row")
	s.Require().Len(checkout["commit"], 40, "commit should be a full git object id, got %q", checkout["commit"])
	s.Equal(f.HeadCommit(), checkout["commit"], "checkout pinned a different commit than the fixture's HEAD")
	s.Require().True(strings.HasPrefix(checkout["treeDigest"], "sha256:"),
		"treeDigest = %q, want a sha256 digest", checkout["treeDigest"])
	s.Equal("/src", checkout["path"])

	discover := outputs["discover"]
	s.Require().NotNil(discover, "discover emitted no output row")
	s.Require().True(strings.HasPrefix(discover["fingerprint"], "sha256:"),
		"fingerprint = %q, want a sha256 digest", discover["fingerprint"])
	// Per-input digests are what let `caesium why` name the input that moved,
	// including the module reached by a relative source two levels down.
	for _, key := range []string{"input_root", "input_tags", "input_tags_inner", "input_vpc", "input_workspace"} {
		s.True(strings.HasPrefix(discover[key], "sha256:"), "%s = %q", key, discover[key])
	}

	// The resolved deploy key must not be recoverable from any task's log.
	for _, name := range []string{"checkout", "discover"} {
		taskID := s.jobTaskIDByName(job.ID, name)
		s.NotContains(s.taskLog(job.ID, run.ID, taskID), canary,
			"the resolved GIT_SSH_KEY leaked into the %s task log", name)
	}

	// And the stored spec keeps the reference, not the secret.
	specs := s.fetchAtomSpecsByTaskName(job.ID)
	s.Equal(keyRef, specs["checkout"].Env["GIT_SSH_KEY"],
		"the stored spec should carry the secret:// reference, not its value")
	s.NotContains(fmt.Sprintf("%v", specs["checkout"].Env), canary)
}

// TestInfraDiscoverFingerprintIsDeterministic is design §9 scenario 8. Two
// independent checkouts of the same tree, at different host paths, must produce
// a byte-identical fingerprint — a fingerprint that varies between workers
// splits the cache and silently re-applies everything.
func (s *IntegrationTestSuite) TestInfraDiscoverFingerprintIsDeterministic() {
	s.requireInfraLane()

	fingerprints := make([]string, 0, 2)
	paths := make([]string, 0, 2)
	for i := range 2 {
		f := s.newInfraFixture(fmt.Sprintf("determinism-%d", i))
		alias := fmt.Sprintf("infra-determinism-%d-%d", i, time.Now().UnixNano())
		job, run := s.runInfraPipeline(f, infraPipelineOptions{
			alias:    alias,
			scanRoot: "/src/stacks/network",
		})
		s.Require().Equal("succeeded", run.Status,
			"run %d: %v", i, s.taskStatusesByName(job.ID, run))

		discover := s.taskOutputsByName(job.ID, run)["discover"]
		s.Require().NotNil(discover, "run %d: discover emitted no output row", i)
		s.Require().NotEmpty(discover["fingerprint"], "run %d: no fingerprint", i)
		fingerprints = append(fingerprints, discover["fingerprint"])
		paths = append(paths, f.hostWorkspace)
	}

	s.Require().NotEqual(paths[0], paths[1], "the two runs must use different host paths")
	s.Equal(fingerprints[0], fingerprints[1],
		"the same tree checked out at %s and %s produced different fingerprints", paths[0], paths[1])
}

// TestInfraDiscoverFailureMakesTheRunRed is design §9 scenario 7. A discover
// that exits non-zero must fail the run — never leave it green with the
// downstream steps quietly skipped, which would report success for a deploy
// that never happened.
func (s *IntegrationTestSuite) TestInfraDiscoverFailureMakesTheRunRed() {
	s.requireInfraLane()

	f := s.newInfraFixture("discover-failure")
	alias := fmt.Sprintf("infra-discover-failure-%d", time.Now().UnixNano())
	job, run := s.runInfraPipeline(f, infraPipelineOptions{
		alias: alias,
		// A scan root that does not exist: discover cannot enumerate anything
		// and must say so rather than reporting an empty, unchanged world.
		scanRoot: "/src/stacks/does-not-exist",
	})

	statuses := s.taskStatusesByName(job.ID, run)
	s.Require().Equal("failed", run.Status,
		"a failed discover must make the run red, not green-with-skips: %v", statuses)
	s.Equal("succeeded", statuses["checkout"])
	s.Equal("failed", statuses["discover"])
	for _, name := range []string{"plan", "apply"} {
		s.NotEqual("succeeded", statuses[name], "%s must not run after discover failed", name)
		s.NotEqual("cached", statuses[name], "%s must not be served from cache after discover failed", name)
	}

	outputs := s.taskOutputsByName(job.ID, run)
	s.NotContains(outputs["discover"], "fingerprint",
		"a failed discover must emit no fingerprint")
}

// TestInfraDynamicModuleSourceIsRejected is design §9 scenario 10. A module
// source that cannot reduce to a constant makes `terraform get` fail, so
// discover exits non-zero with no fingerprint and the run is red. This
// regression-guards the upstream behaviour §6.2 depends on: if a future
// Terraform started resolving variable module sources, the fingerprint would
// silently stop covering the real module set.
func (s *IntegrationTestSuite) TestInfraDynamicModuleSourceIsRejected() {
	s.requireInfraLane()

	f := s.newInfraFixture("dynamic-source")
	alias := fmt.Sprintf("infra-dynamic-source-%d", time.Now().UnixNano())
	job, run := s.runInfraPipeline(f, infraPipelineOptions{
		alias:    alias,
		scanRoot: "/src/fail-closed/dynamic-source",
	})

	statuses := s.taskStatusesByName(job.ID, run)
	s.Require().Equal("failed", run.Status,
		"a stack whose module source is a variable must make the run red: %v", statuses)
	s.Equal("failed", statuses["discover"])
	for _, name := range []string{"plan", "apply"} {
		s.NotEqual("succeeded", statuses[name], "%s must not run", name)
	}

	outputs := s.taskOutputsByName(job.ID, run)
	s.NotContains(outputs["discover"], "fingerprint",
		"discover's output row must carry no fingerprint when terraform get fails")

	taskID := s.jobTaskIDByName(job.ID, "discover")
	s.Contains(s.taskLog(job.ID, run.ID, taskID), "Unknown module source",
		"the failure should name Terraform's own diagnosis")
}

// TestInfraMultiRootDiscoverDrivesFanOut is the end-to-end proof that the
// object-form partitions discover emits are accepted by the real expansion
// path — not merely by the reagents' own hand-copy of the rules.
//
// The reagents cannot import Caesium, so `##caesium::partitions` is the only thing
// holding the two together. Everything else in this stream drives discover in
// single-root mode, where it emits a plain output row; multi-root mode is the
// form §5.4 calls the real one and it produces a marker nothing else here
// consumes. This scenario runs it through fanOut and asserts the three things a
// bare string array cannot carry survive the round trip: the per-unit
// fingerprint, the ordering edge, and the free-form `root` attribute.
func (s *IntegrationTestSuite) TestInfraMultiRootDiscoverDrivesFanOut() {
	s.requireInfraLane()

	f := s.newInfraFixture("fanout")
	alias := fmt.Sprintf("infra-fanout-%d", time.Now().UnixNano())

	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %[1]s
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
volumes:
  - name: fixture
    source:
      bind: %[2]q
  - name: src
    source:
      bind: %[3]q
steps:
  - name: checkout
    image: %[4]s
    env:
      GIT_URL: "file:///fixture"
      GIT_REF: "main"
      GIT_SPARSE: "stacks/** modules/**"
      DEST: "/src"
    volumeMounts:
      - {volume: fixture, path: /fixture, readOnly: true}
      - {volume: src, path: /src}
    next: discover
  - name: discover
    image: %[5]s
    dependsOn: [checkout]
    env:
      SCAN_ROOT: "/src/stacks"
    volumeMounts:
      - {volume: src, path: /src, readOnly: true}
    next: plan
  - name: plan
    image: alpine:3.23
    dependsOn: [discover]
    command: ["sh", "-c", "echo stack=$CAESIUM_PARTITION && echo json=$CAESIUM_PARTITION_JSON && echo '##caesium::output {\"planned\": \"'$CAESIUM_PARTITION'\"}'"]
    fanOut:
      from: discover
      maxPartitions: 8
      onEmpty: fail
`, alias, f.hostRepo, f.hostWorkspace, s.reagentImage("git-source"), s.reagentImage("tf-discover"))

	dir := s.writeJobManifest(manifest)
	defer func() { _ = os.RemoveAll(dir) }()

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, 5*time.Minute)

	s.Require().Equal("succeeded", run.Status,
		"the fan-out run failed; task statuses: %v", s.taskStatusesByName(job.ID, run))

	planID := s.jobTaskIDByName(job.ID, "plan")
	instances := s.expandedPartitions(s.listPartitions(job.ID, run.ID, planID))
	s.Require().Len(instances, 3, "discover's three stacks should expand to three instances: %#v", instances)

	byValue := map[string]partitionInstance{}
	for _, p := range instances {
		byValue[p.Value] = p
	}
	for _, key := range []string{"network", "account", "app-web"} {
		s.Require().Containsf(byValue, key, "no instance for stack %q (got %v)", key, byValue)
	}

	// The per-unit fingerprint rode along and was persisted per instance —
	// without it every instance shares the producer's identity and no stack can
	// ever be individually cache-gated.
	fingerprints := map[string]string{}
	for value, p := range byValue {
		s.Truef(strings.HasPrefix(p.Fingerprint, "sha256:"),
			"instance %q has no fingerprint (%q)", value, p.Fingerprint)
		s.Equal("succeeded", p.Status, "instance %q", value)
		fingerprints[p.Fingerprint] = value
	}
	s.Len(fingerprints, 3, "each stack must have its own fingerprint, got %v", fingerprints)

	// The ordering edge survived: account and app-web wait on network.
	s.Empty(byValue["network"].DependsOn, "network has no upstream stack")
	for _, key := range []string{"account", "app-web"} {
		s.Equal([]string{"network"}, byValue[key].DependsOn,
			"stack %q lost its ordering edge", key)
	}

	// CAESIUM_PARTITION and the free-form `root` attribute reached the
	// container, which is how tf-runner learns which directory to plan.
	for _, key := range []string{"network", "account", "app-web"} {
		logText := s.taskRunLog(job.ID, run.ID, planID, byValue[key].TaskRunID)
		s.Containsf(logText, "stack="+key, "instance %q did not see its partition key", key)
		s.Containsf(logText, `"root":"`+key+`"`,
			"instance %q did not receive the root attribute in CAESIUM_PARTITION_JSON: %s", key, logText)
		s.Containsf(logText, byValue[key].Fingerprint,
			"instance %q did not receive its fingerprint in CAESIUM_PARTITION_JSON", key)
	}
}

// taskRunLog fetches one fan-out instance's log.
func (s *IntegrationTestSuite) taskRunLog(jobID, runID, taskID, taskRunID string) string {
	s.T().Helper()

	resp, err := s.doJSONRequest(http.MethodGet, fmt.Sprintf(
		"%s/v1/jobs/%s/runs/%s/logs?task_id=%s&task_run_id=%s",
		s.caesiumURL, jobID, runID, taskID, taskRunID), nil)
	s.Require().NoError(err)
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, resp.StatusCode, "instance logs: %s", body)
	return string(body)
}
