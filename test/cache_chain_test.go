//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// cache_chain_test.go drives `cache.chain: values` and `cache.ttl: never`
// through the real surface (infra-deploy A5): the live server, the real CLI
// binary, and an ordinary alpine:3.23 pipeline with no Terraform anywhere.
//
// The behaviour under test is the one the whole feature exists for. A shared
// upstream step's identity hash is computed BEFORE it runs, so it can only
// contain that step's inputs — for a source checkout, the git ref, which moves
// on every commit. Under the default transitive chain that churn propagates
// through PredecessorHashes to every step downstream. `chain: values` stops it
// at one step while still hashing the upstream's OUTPUTS, so a genuinely
// changed value still invalidates consumers.

// chainManifest builds the DAG shape the scenario needs:
//
//	noisy ────┬──> mid (chain: values, ttl: never) ──> leaf
//	producer ─┤
//	          └──> direct                             (transitive control)
//
// Two upstream steps, because the two things `chain: values` separates — a
// predecessor's IDENTITY and its OUTPUTS — must be perturbable independently:
//
//   - `noisy` churns its identity (its command carries `revision`) and emits NO
//     structured output. That is the spec's `warm-cache` shape (5.2: Warm emits
//     nothing) and it is the case the feature exists for. It also defeats the
//     value-verified short-circuit, which by design refuses to substitute a
//     prior identity for a step that emitted nothing — silence is not proof of
//     equality — so under the default chain its churn genuinely cascades.
//   - `producer` publishes `token`, the value its consumers actually depend on.
//
// `direct` is the control: the same two predecessors under the DEFAULT chain.
// Without it a green test could not distinguish "chain: values worked" from
// "the upstream never actually re-ran".
//
// NOTE: no step-level `engine:` — writeJobManifest/injectEngine inserts the
// per-tier engine, and a hardcoded one would duplicate the key.
func chainManifest(alias, revision, token string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %[1]s
  cache: true
trigger:
  type: cron
  configuration:
    cron: "0 0 31 2 *"
steps:
  - name: noisy
    image: alpine:3.23
    command: ["sh","-c","echo warming revision=%[2]s"]
    next: [mid, direct]
  - name: producer
    image: alpine:3.23
    command: ["sh","-c","echo '##caesium::output {\"token\": \"%[3]s\"}'"]
    next: [mid, direct]
  - name: mid
    image: alpine:3.23
    dependsOn: [noisy, producer]
    cache:
      version: 1
      chain: values
      ttl: never
    command: ["sh","-c","echo '##caesium::output {\"plan\": \"plan-for-'$CAESIUM_OUTPUT_PRODUCER_TOKEN'\"}'"]
    next: [leaf]
  - name: direct
    image: alpine:3.23
    dependsOn: [noisy, producer]
    command: ["sh","-c","echo direct token=$CAESIUM_OUTPUT_PRODUCER_TOKEN"]
  - name: leaf
    image: alpine:3.23
    dependsOn: [mid]
    command: ["sh","-c","echo leaf plan=$CAESIUM_OUTPUT_MID_PLAN"]
`, alias, revision, token)
}

// cacheListEntry mirrors the `caesium cache list` JSON. expires_at is omitted
// entirely when the entry has a null expiry, which is what `ttl: never` writes.
type cacheListEntry struct {
	Hash      string  `json:"hash"`
	TaskName  string  `json:"task_name"`
	ExpiresAt *string `json:"expires_at"`
}

type cacheListResponse struct {
	Entries []cacheListEntry `json:"entries"`
}

// chainWhyExplanation mirrors the fields of `caesium why --json` this scenario
// asserts: the verdict, the chain notes, and the per-field diff entries.
type chainWhyExplanation struct {
	Verdict string `json:"verdict"`
	Summary string `json:"summary"`
	Diff    *struct {
		HashEqual bool     `json:"hashEqual"`
		Notes     []string `json:"notes"`
		Changes   []struct {
			Field string `json:"field"`
			Kind  string `json:"kind"`
			Note  string `json:"note"`
		} `json:"changes"`
	} `json:"diff"`
}

// TestCacheChainValuesBreaksUpstreamChurn is the load-bearing scenario: an
// upstream step whose identity moves but whose output does not must leave a
// `chain: values` consumer cached, while an ordinary transitive consumer of the
// same upstream re-runs.
func (s *IntegrationTestSuite) TestCacheChainValuesBreaksUpstreamChurn() {
	alias := fmt.Sprintf("integration-cache-chain-%d", time.Now().UnixNano())

	dir := s.writeJobManifest(chainManifest(alias, "r1", "abc"))
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	// --- Run 1: cold. Everything executes. -------------------------------
	run1 := s.triggerRun(job.ID)
	s.Require().Equal("succeeded", s.awaitRun(job.ID, run1, runTimeout).Status)
	statuses1 := s.taskStatusesByName(job.ID, s.awaitRun(job.ID, run1, runTimeout))
	for _, step := range []string{"noisy", "producer", "mid", "direct", "leaf"} {
		s.Equal("succeeded", statuses1[step], "run 1: %s should execute", step)
	}

	// --- Run 2: nothing changed. Everything is cached. --------------------
	// A sanity gate: if this fails, the later assertions prove nothing about
	// chain at all.
	run2 := s.triggerRun(job.ID)
	s.Require().Equal("succeeded", s.awaitRun(job.ID, run2, runTimeout).Status)
	statuses2 := s.taskStatusesByName(job.ID, s.awaitRun(job.ID, run2, runTimeout))
	for _, step := range []string{"noisy", "producer", "mid", "direct", "leaf"} {
		s.Equal("cached", statuses2[step], "run 2: unchanged %s should be a cache hit", step)
	}

	// --- Run 3: the noisy upstream's IDENTITY changes; no output moves. ---
	// This is the git-ref-moved / warm-cache case. `direct` inherits the churn
	// and re-runs; `mid` breaks the chain and stays cached; `leaf` therefore also
	// stays cached, because the churn was contained at mid.
	s.writeJobManifestToDir(dir, chainManifest(alias, "r2-edited", "abc"))
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	run3 := s.triggerRun(job.ID)
	s.Require().Equal("succeeded", s.awaitRun(job.ID, run3, runTimeout).Status)
	statuses3 := s.taskStatusesByName(job.ID, s.awaitRun(job.ID, run3, runTimeout))

	s.Equal("succeeded", statuses3["noisy"],
		"run 3: the noisy step's own identity changed, so it must re-execute")
	s.Equal("cached", statuses3["producer"], "run 3: the value producer is untouched")
	s.Equal("succeeded", statuses3["direct"],
		"run 3: a DEFAULT-chain consumer must still inherit the upstream churn — "+
			"if this is cached, the scenario proves nothing")
	s.Equal("cached", statuses3["mid"],
		"run 3: chain: values must exclude the changed predecessor hash, so mid stays cached")
	s.Equal("cached", statuses3["leaf"],
		"run 3: with the churn contained at mid, leaf's predecessor identity is unchanged too")

	// --- `caesium why` must NAME the exclusion (spec 4.3). ----------------
	// Without this the skip above is unexplainable: mid is reported cached
	// while its predecessor visibly changed, which reads as a cache bug.
	why := s.parseChainWhy(job.ID, run3, "mid")
	s.Equal("CACHE_HIT", why.Verdict)
	s.Require().NotNil(why.Diff)
	s.True(why.Diff.HashEqual, "a values-mode hit must decompose to equal hashes")
	s.Contains(why.Diff.Notes, "predecessor hashes excluded (chain: values)",
		"why --json must carry the exclusion note, got %+v", why.Diff.Notes)
	s.Contains(why.Summary, "predecessor hashes excluded (chain: values)",
		"the one-line summary must name the exclusion too, got %q", why.Summary)

	foundExcluded := false
	for _, c := range why.Diff.Changes {
		if c.Field == "predecessorHashes" {
			s.Equal("excluded", c.Kind,
				"predecessorHashes must be reported excluded, not as a discriminating change")
			s.Equal("excluded (chain: values)", c.Note)
			foundExcluded = true
		}
	}
	s.True(foundExcluded, "expected a predecessorHashes exclusion entry, got %+v", why.Diff.Changes)

	// The transitive control's explanation is the contrast: a real,
	// discriminating predecessor-hash change and no exclusion note.
	directWhy := s.parseChainWhy(job.ID, run3, "direct")
	s.Equal("CACHE_MISS", directWhy.Verdict)
	s.Require().NotNil(directWhy.Diff)
	s.Empty(directWhy.Diff.Notes, "a transitive explanation must carry no chain note")
	foundRealChange := false
	for _, c := range directWhy.Diff.Changes {
		if c.Field == "predecessorHashes" {
			s.NotEqual("excluded", c.Kind, "a transitive predecessor-hash change is a real change")
			foundRealChange = true
		}
	}
	s.True(foundRealChange,
		"the transitive consumer's miss must be attributed to the predecessor hash, got %+v",
		directWhy.Diff.Changes)

	// --- Run 4: upstream's OUTPUT changes. Outputs still chain. -----------
	// This is the other half of the contract, and the guard against the
	// exclusion being an over-broad "ignore my predecessors" switch.
	s.writeJobManifestToDir(dir, chainManifest(alias, "r2-edited", "xyz"))
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	run4 := s.triggerRun(job.ID)
	s.Require().Equal("succeeded", s.awaitRun(job.ID, run4, runTimeout).Status)
	statuses4 := s.taskStatusesByName(job.ID, s.awaitRun(job.ID, run4, runTimeout))

	s.Equal("cached", statuses4["noisy"], "run 4: the noisy step is untouched this time")
	s.Equal("succeeded", statuses4["producer"], "run 4: the producer emits a new token")
	s.Equal("succeeded", statuses4["mid"],
		"run 4: a changed predecessor OUTPUT must still invalidate a values-mode consumer")
	s.Equal("succeeded", statuses4["leaf"], "run 4: mid re-ran, so leaf's predecessor identity moved")

	midWhy := s.parseChainWhy(job.ID, run4, "mid")
	s.Equal("CACHE_MISS", midWhy.Verdict)
	s.Require().NotNil(midWhy.Diff)
	foundOutputChange := false
	for _, c := range midWhy.Diff.Changes {
		if strings.HasPrefix(c.Field, "predecessorOutputs.producer.") {
			foundOutputChange = true
		}
	}
	s.True(foundOutputChange,
		"the miss must be attributed to the changed upstream output, got %+v", midWhy.Diff.Changes)
}

// TestCacheTTLNeverWritesNullExpiry drives `cache.ttl: never` through the real
// `caesium cache list` surface. The step keyed on it must carry no expiry at
// all, while a sibling on the ordinary inherited TTL still gets one — proving
// the null expiry is the `never` key's doing and not caching being off.
func (s *IntegrationTestSuite) TestCacheTTLNeverWritesNullExpiry() {
	alias := fmt.Sprintf("integration-cache-ttlnever-%d", time.Now().UnixNano())

	dir := s.writeJobManifest(chainManifest(alias, "r1", "abc"))
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	runID := s.triggerRun(job.ID)
	s.Require().Equal("succeeded", s.awaitRun(job.ID, runID, runTimeout).Status)

	out, err := s.runCLIStdout("cache", "list", "--job-id", job.ID, "--server", s.caesiumURL)
	s.Require().NoError(err, "caesium cache list failed:\n%s", out)
	s.Require().True(json.Valid([]byte(out)),
		"caesium cache list stdout was not valid JSON (log contamination?):\n%s", out)

	var listed cacheListResponse
	s.Require().NoError(json.Unmarshal([]byte(out), &listed))

	byTask := make(map[string]cacheListEntry, len(listed.Entries))
	for _, e := range listed.Entries {
		byTask[e.TaskName] = e
	}

	mid, ok := byTask["mid"]
	s.Require().True(ok, "expected a cache entry for mid, got %+v", listed.Entries)
	s.Nil(mid.ExpiresAt,
		"cache.ttl: never must write a null expiry; got %v", mid.ExpiresAt)

	producer, ok := byTask["producer"]
	s.Require().True(ok, "expected a cache entry for producer, got %+v", listed.Entries)
	s.Require().NotNil(producer.ExpiresAt,
		"a step on the inherited CAESIUM_CACHE_TTL must still expire — otherwise the "+
			"null expiry above proves nothing about `never`")
	s.NotEmpty(*producer.ExpiresAt)
}

// parseChainWhy runs `caesium why --json` through the real CLI binary and
// asserts stdout is clean, parseable JSON (stderr captured SEPARATELY — a
// merged capture cannot detect log lines corrupting machine output).
func (s *IntegrationTestSuite) parseChainWhy(jobID, runID, task string) chainWhyExplanation {
	s.T().Helper()
	return s.parseChainWhyPartition(jobID, runID, task, "")
}

// parseChainWhyPartition is parseChainWhy plus `--partition` for a fanned
// instance. stdout is captured separately from stderr so log lines cannot
// masquerade as JSON.
func (s *IntegrationTestSuite) parseChainWhyPartition(jobID, runID, task, partition string) chainWhyExplanation {
	s.T().Helper()
	args := []string{"why", runID, "--job-id", jobID, "--task", task, "--json", "--server", s.caesiumURL}
	if partition != "" {
		args = append(args, "--partition", partition)
	}
	out, err := s.runCLIStdout(args...)
	s.Require().NoError(err, "caesium why failed:\n%s", out)
	s.Require().True(json.Valid([]byte(out)),
		"caesium why --json stdout was not valid JSON (log contamination?):\n%s", out)
	var exp chainWhyExplanation
	s.Require().NoError(json.Unmarshal([]byte(out), &exp))
	return exp
}
