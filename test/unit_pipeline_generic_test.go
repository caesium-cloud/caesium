//go:build integration

package test

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// unit_pipeline_generic_test.go is the GENERICITY GUARANTEE for the unit-pipeline
// pattern (infra-deploy A6, spec 9.12).
//
// The pattern's claim is that Terraform is its first consumer, not its
// definition: a team wanting the same change-gating for dbt, a monorepo, or
// database migrations implements five container contracts (spec 5.2 — stdout
// markers and environment variables) and gets per-unit caching,
// fingerprint-accurate invalidation and audit for free. This test drives the
// EXACT DAG shape of the Terraform binding with a trivial shell binding and
// nothing else:
//
//	source ─┬─> discover-a ─> propose-a ─> apply-a ─┐
//	        ├─> discover-b ─> propose-b ─> apply-b <┘
//	        └─> warm ──────> (every propose + apply) ──> control
//
//	MATERIALIZE   WARM       DISCOVER      PROPOSE       APPLY
//	              (emits     (fingerprint) (artifact +   (consumes the artifact,
//	               nothing)                 branch)       emits an output the
//	                                                      next unit consumes)
//
// It MUST NEVER reference a reagent image. If the 5.2 contracts quietly grow
// Terraform-shaped assumptions — a required env var, an image-side convention,
// a marker only the tf-runner emits — this is the test that fails.

// unitPipelineVolume is a docker/podman NAMED volume, created on demand by the
// engine. It is the shared storage the artifact handoff rides on: only the
// reference (path + sha256) crosses the step boundary, never the payload. The
// name is stable across suite runs on purpose — the artifacts a run leaves
// behind are overwritten by the next run of the same unit, and each test uses a
// fresh job alias (which is part of the cache key), so a cold run always
// executes and rewrites what it reads later.
const unitPipelineVolume = "caesium-integration-unit-pipeline"

// unitPipelineManifest builds the DAG above. The four parameters are the only
// things a scenario perturbs:
//
//   - fingerprintA / fingerprintB: what each unit's DISCOVER step declares its
//     inputs digest to. Discover owns the fingerprint; Caesium never inspects a
//     unit's contents (spec 3.3), so a bump here is exactly "this unit changed".
//   - endpointA: the output unit A's APPLY publishes for unit B to consume —
//     the generic form of a network stack's vpc_id.
//   - warmRevision: perturbs the WARM step's identity ONLY. Warm emits nothing
//     (spec 5.2), which is what makes it the discriminating lever here: the
//     value-verified short-circuit (cache.EquivalentPriorHash) refuses to
//     substitute a prior identity for a step that published no output — silence
//     is not proof of equality — so warm's churn genuinely cascades under the
//     default chain. `control` is the transitive consumer that proves it does.
//     Every propose/apply step depends on warm exactly as spec 5.5's reference
//     manifest wires `plan`/`apply` to `warm-cache`.
//
// NOTE: no step-level `engine:` — writeJobManifest/injectEngine inserts the
// per-tier engine and a hardcoded one would duplicate the key. Single-quoted
// YAML scalars are used for commands so the embedded shell can use double
// quotes and backslash-escaped JSON without a third layer of escaping.
func unitPipelineManifest(alias, fingerprintA, fingerprintB, endpointA, warmRevision string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %[1]s
  cache: true
trigger:
  type: cron
  configuration:
    cron: "0 0 31 2 *"
volumes:
  - name: artifacts
    sources:
      docker:
        volume: %[5]s
      podman:
        volume: %[5]s
      kubernetes:
        pvc: %[5]s-rwx
steps:
  - name: source
    image: alpine:3.23
    command: ['sh','-c','echo "##caesium::output {\"tree\": \"shared-source\"}"']
    next: [discover-a, discover-b, warm]

  - name: warm
    image: alpine:3.23
    dependsOn: [source]
    command: ['sh','-c','echo warming dependency cache rev=%[6]s']
    next: [propose-a, propose-b, apply-a, apply-b, control]

  - name: control
    image: alpine:3.23
    dependsOn: [warm]
    command: ['sh','-c','echo transitive control saw the warm step']

  - name: discover-a
    image: alpine:3.23
    dependsOn: [source]
    command: ['sh','-c','echo "##caesium::output {\"fingerprint\": \"%[2]s\"}"']
    next: [propose-a]

  - name: discover-b
    image: alpine:3.23
    dependsOn: [source]
    command: ['sh','-c','echo "##caesium::output {\"fingerprint\": \"%[3]s\"}"']
    next: [propose-b]

  - name: propose-a
    type: branch
    image: alpine:3.23
    dependsOn: [discover-a, warm]
    cache:
      version: 1
      chain: values
    volumeMounts:
      - {volume: artifacts, path: /artifacts}
    command: ['sh','-c','set -e; echo "plan for $CAESIUM_OUTPUT_DISCOVER_A_FINGERPRINT" > /artifacts/unit-a.plan; d=$(sha256sum /artifacts/unit-a.plan | cut -d" " -f1); echo "##caesium::output-ref {\"key\":\"proposal_artifact\",\"path\":\"/artifacts/unit-a.plan\",\"digest\":\"sha256:$d\"}"; echo "##caesium::branch apply-a"']
    next: [apply-a]

  - name: propose-b
    type: branch
    image: alpine:3.23
    dependsOn: [discover-b, warm]
    cache:
      version: 1
      chain: values
    volumeMounts:
      - {volume: artifacts, path: /artifacts}
    command: ['sh','-c','set -e; echo "plan for $CAESIUM_OUTPUT_DISCOVER_B_FINGERPRINT" > /artifacts/unit-b.plan; d=$(sha256sum /artifacts/unit-b.plan | cut -d" " -f1); echo "##caesium::output-ref {\"key\":\"proposal_artifact\",\"path\":\"/artifacts/unit-b.plan\",\"digest\":\"sha256:$d\"}"; echo "##caesium::branch apply-b"']
    next: [apply-b]

  - name: apply-a
    image: alpine:3.23
    dependsOn: [propose-a, warm]
    cache:
      version: 1
      chain: values
      ttl: never
    volumeMounts:
      - {volume: artifacts, path: /artifacts}
    command: ['sh','-c','set -e; test "$(sha256sum $CAESIUM_OUTPUT_PROPOSE_A_PROPOSAL_ARTIFACT | cut -d" " -f1)" = "${CAESIUM_OUTPUT_PROPOSE_A_PROPOSAL_ARTIFACT_DIGEST#sha256:}"; echo "##caesium::output {\"endpoint\": \"%[4]s\"}"']
    next: [apply-b]

  - name: apply-b
    image: alpine:3.23
    dependsOn: [propose-b, apply-a, warm]
    cache:
      version: 1
      chain: values
      ttl: never
    volumeMounts:
      - {volume: artifacts, path: /artifacts}
    command: ['sh','-c','set -e; test "$(sha256sum $CAESIUM_OUTPUT_PROPOSE_B_PROPOSAL_ARTIFACT | cut -d" " -f1)" = "${CAESIUM_OUTPUT_PROPOSE_B_PROPOSAL_ARTIFACT_DIGEST#sha256:}"; test -n "$CAESIUM_OUTPUT_APPLY_A_ENDPOINT"; echo "unit-b consumed $CAESIUM_OUTPUT_APPLY_A_ENDPOINT"']
`, alias, fingerprintA, fingerprintB, endpointA, unitPipelineVolume, warmRevision)
}

var unitPipelineSteps = []string{
	"source", "warm", "control",
	"discover-a", "discover-b", "propose-a", "propose-b", "apply-a", "apply-b",
}

// unitPipelineUnitSteps are the per-unit propose/apply steps — the four that
// carry `chain: values` and whose containment is the property under test.
var unitPipelineUnitSteps = []string{"propose-a", "propose-b", "apply-a", "apply-b"}

// TestGenericUnitPipelineCachesPerUnit drives the whole pattern with no
// Terraform and no reagent image: a second run caches both units, a fingerprint
// bump re-runs exactly one unit, and a changed apply OUTPUT re-runs its
// consumer.
func (s *IntegrationTestSuite) TestGenericUnitPipelineCachesPerUnit() {
	if s.engineType == "kubernetes" {
		s.T().Skipf("needs an RWX-capable storage class; the kind cluster's default class is RWO — covered on the docker + podman lanes")
	}

	alias := fmt.Sprintf("integration-unit-pipeline-%d", time.Now().UnixNano())

	manifest := unitPipelineManifest(alias, "sha-a-1", "sha-b-1", "endpoint-v1", "warm-r1")
	// The genericity guarantee, asserted rather than merely intended: this
	// binding is five shell contracts over alpine:3.23 images; if a reagent image leaks
	// into this fixture the test stops proving the pattern is tool-agnostic.
	s.Require().NotContains(manifest, "caesiumcloud/",
		"the generic binding must never reference a reagent image")
	s.Require().NotContains(strings.ToLower(manifest), "terraform",
		"the generic binding must never reference Terraform")

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	// --- Run 1: cold. Every role executes. --------------------------------
	_, statuses1 := s.runUnitPipeline(job.ID)
	for _, step := range unitPipelineSteps {
		s.Equal("succeeded", statuses1[step], "run 1: %s should execute", step)
	}

	// --- Run 2: unchanged. Both units cached end to end. -------------------
	// Spec 9.1: "unchanged repo, second run -> every plan and apply cached,
	// zero unit containers".
	_, statuses2 := s.runUnitPipeline(job.ID)
	for _, step := range unitPipelineSteps {
		s.Equal("cached", statuses2[step], "run 2: unchanged %s should be a cache hit", step)
	}

	// --- Run 3: bump unit A's fingerprint only. ----------------------------
	// Spec 9.2, the load-bearing case: "edit one stack -> only that stack
	// re-applies; others stay cached".
	s.writeJobManifestToDir(dir, unitPipelineManifest(alias, "sha-a-2", "sha-b-1", "endpoint-v1", "warm-r1"))
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	run3, statuses3 := s.runUnitPipeline(job.ID)
	s.Equal("cached", statuses3["source"], "run 3: the shared materialize step is unchanged")
	s.Equal("succeeded", statuses3["discover-a"], "run 3: unit A's discover declares a new fingerprint")
	s.Equal("succeeded", statuses3["propose-a"],
		"run 3: unit A's propose consumes the changed fingerprint OUTPUT, so it must re-run "+
			"even under chain: values")
	s.Equal("succeeded", statuses3["apply-a"],
		"run 3: unit A's proposal artifact digest changed, so its apply must re-run")

	s.Equal("cached", statuses3["discover-b"], "run 3: unit B is untouched")
	s.Equal("cached", statuses3["propose-b"], "run 3: unit B is untouched")
	s.Equal("cached", statuses3["apply-b"],
		"run 3: unit B's apply must stay cached — only the edited unit re-applies")
	// NOTE on what this particular assertion does and does not prove. apply-a
	// re-ran here but published a byte-identical `endpoint`, which is exactly the
	// case the value-verified short-circuit (cache.EquivalentPriorHash) covers:
	// apply-a's effective_hash is substituted with its proven-equal prior, so
	// apply-b would stay cached under the DEFAULT chain too. The assertion is the
	// right user-visible expectation, but it does not discriminate chain: values
	// from that pre-existing optimization. Run 5 below is what does.
	applyBWhy := s.parseChainWhy(job.ID, run3, "apply-b")
	s.Equal("CACHE_HIT", applyBWhy.Verdict)
	s.Require().NotNil(applyBWhy.Diff)
	s.Contains(applyBWhy.Diff.Notes, "predecessor hashes excluded (chain: values)",
		"apply-b's skip must at least be EXPLAINED as values-mode, got %+v", applyBWhy.Diff.Notes)

	// --- Run 4: unit A's apply publishes a CHANGED OUTPUT. -----------------
	// Spec 9.4: outputs still chain under chain: values, so the consumer of a
	// changed value re-runs even though its own definition is untouched. This is
	// the guard against the exclusion being an over-broad "ignore upstream".
	s.writeJobManifestToDir(dir, unitPipelineManifest(alias, "sha-a-2", "sha-b-1", "endpoint-v2", "warm-r1"))
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	run4, statuses4 := s.runUnitPipeline(job.ID)
	s.Equal("cached", statuses4["discover-a"], "run 4: no fingerprint moved")
	s.Equal("cached", statuses4["propose-a"], "run 4: unit A's proposal is unchanged")
	s.Equal("cached", statuses4["propose-b"], "run 4: unit B's proposal is unchanged")
	s.Equal("succeeded", statuses4["apply-a"], "run 4: unit A's apply emits a new endpoint")
	s.Equal("succeeded", statuses4["apply-b"],
		"run 4: the consumer of a CHANGED OUTPUT must re-run — outputs still chain")

	applyBMiss := s.parseChainWhy(job.ID, run4, "apply-b")
	s.Equal("CACHE_MISS", applyBMiss.Verdict)
	s.Require().NotNil(applyBMiss.Diff)
	foundOutput := false
	for _, c := range applyBMiss.Diff.Changes {
		if strings.HasPrefix(c.Field, "predecessorOutputs.apply-a.") {
			foundOutput = true
		}
	}
	s.True(foundOutput,
		"the re-run must be attributed to the changed upstream output, got %+v", applyBMiss.Diff.Changes)

	// --- Run 5: the WARM step's identity churns; nothing else moves. -------
	// This is the discriminating scenario, and the one that actually earns the
	// `chain: values` on every propose/apply step.
	//
	// warm emits NOTHING, so the value-verified short-circuit refuses to
	// substitute a prior identity for it (silence is not proof of equality) and
	// its churn genuinely cascades under the default chain — `control` proves
	// that in the same run. Every propose and apply step depends on warm exactly
	// as spec 5.5 wires them, and every one of them must stay cached.
	//
	// Remove `chain: values` from any of those four steps and this block goes
	// red: their key would fold in warm's changed identity hash and the whole
	// fleet would re-plan and re-apply for a dependency-cache refresh.
	s.writeJobManifestToDir(dir, unitPipelineManifest(alias, "sha-a-2", "sha-b-1", "endpoint-v2", "warm-r2"))
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	run5, statuses5 := s.runUnitPipeline(job.ID)

	s.Equal("succeeded", statuses5["warm"], "run 5: the warm step's own identity changed")
	s.Equal("succeeded", statuses5["control"],
		"run 5: a DEFAULT-chain consumer of warm MUST re-run — if this is cached, the "+
			"cascade never happened and the assertions below prove nothing")

	s.Equal("cached", statuses5["source"], "run 5: the materialize step is untouched")
	s.Equal("cached", statuses5["discover-a"], "run 5: no fingerprint moved")
	s.Equal("cached", statuses5["discover-b"], "run 5: no fingerprint moved")
	for _, step := range unitPipelineUnitSteps {
		s.Equal("cached", statuses5[step],
			"run 5: %s carries chain: values, so warm's identity churn must not reach its key", step)
	}

	// And the skip must be explainable, per spec 4.3.
	warmContained := s.parseChainWhy(job.ID, run5, "apply-b")
	s.Equal("CACHE_HIT", warmContained.Verdict)
	s.Require().NotNil(warmContained.Diff)
	s.True(warmContained.Diff.HashEqual)
	s.Contains(warmContained.Diff.Notes, "predecessor hashes excluded (chain: values)",
		"the contained skip must name the exclusion, got %+v", warmContained.Diff.Notes)

	// The control's explanation is the contrast: a real predecessor-hash change,
	// no exclusion note.
	controlWhy := s.parseChainWhy(job.ID, run5, "control")
	s.Equal("CACHE_MISS", controlWhy.Verdict)
	s.Require().NotNil(controlWhy.Diff)
	s.Empty(controlWhy.Diff.Notes, "a transitive explanation must carry no chain note")
	foundPredHashChange := false
	for _, c := range controlWhy.Diff.Changes {
		if c.Field == "predecessorHashes" {
			s.NotEqual("excluded", c.Kind, "a transitive predecessor-hash change is a real change")
			foundPredHashChange = true
		}
	}
	s.True(foundPredHashChange,
		"the control's miss must be attributed to the predecessor hash, got %+v", controlWhy.Diff.Changes)
}

// runUnitPipeline triggers one run, waits for it, and returns the run id plus
// the per-step statuses.
func (s *IntegrationTestSuite) runUnitPipeline(jobID string) (string, map[string]string) {
	s.T().Helper()
	runID := s.triggerRun(jobID)
	run := s.awaitRun(jobID, runID, runTimeout)
	s.Require().Equal("succeeded", run.Status, "unit pipeline run %s should succeed", runID)
	return runID, s.taskStatusesByName(jobID, run)
}
