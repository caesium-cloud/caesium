//go:build integration

package test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

// --------------------------------------------------------------------------
// Data-plane memory A1: image digest pinning
// --------------------------------------------------------------------------

// TestPinDigestsMissOnMovedTag is the A1 correctness gate. With
// cache.pinDigests: true, a job whose image tag is re-pushed to new content
// between runs must produce a cache MISS — folding the resolved content digest
// (not the mutable tag) into the cache key makes a moved tag tamper-evident. A
// false hit here would silently serve a stale result, so this asserts the
// invariant directly.
//
// It re-points a unique local tag from one base image to a different one
// between runs. Docker carries the content RepoDigest across a local re-tag, so
// the server (sharing the daemon) resolves a different digest on the second run
// without needing a registry.
//
// The job sets cache.digestTTL: 0 so the resolver re-resolves the tag on every
// check. With the default tag->digest TTL (a perf cache), the moved tag would
// be masked within the window and the second run — milliseconds later — would
// hit the stale digest; digestTTL: 0 opts into immediate moved-tag detection.
func (s *IntegrationTestSuite) TestPinDigestsMissOnMovedTag() {
	if s.engineType != "docker" {
		s.T().Skipf("digest pinning harness uses the docker SDK; engine=%s", s.engineType)
	}

	cli := s.dockerClient()
	defer func() { _ = cli.Close() }()

	// Two distinct, repo-pinned images so the moved tag resolves to a genuinely
	// different content digest. (Both ship a POSIX sh.)
	const (
		imageA = "alpine:3.23"
		imageB = "busybox:1.36.1"
	)
	s.dockerPull(cli, imageA)
	s.dockerPull(cli, imageB)

	movingTag := fmt.Sprintf("caesium-pindigest-%d:moving", time.Now().UnixNano())
	s.dockerTag(cli, imageA, movingTag)
	defer s.dockerRemove(cli, movingTag)

	alias := fmt.Sprintf("integration-pindigest-miss-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  cache:
    pinDigests: true
    digestTTL: 0
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: pinned
    image: %s
    command: ["sh", "-c", "echo '##caesium::output {\"val\": \"42\"}'"]
`, alias, movingTag)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	// First run: executes, populating the cache against imageA's digest.
	run1ID := s.triggerRun(job.ID)
	run1 := s.awaitRun(job.ID, run1ID, runTimeout)
	s.Equal("succeeded", run1.Status, "first run should succeed")
	s.Equal("succeeded", s.taskStatusesByName(job.ID, run1)["pinned"],
		"first run task should execute normally")

	// Move the tag to different content. Same tag string, new digest.
	s.dockerTag(cli, imageB, movingTag)

	// Second run: the resolved digest changed, so the cache key changed —
	// this must MISS and re-execute, not serve the stale cached result.
	run2ID := s.triggerRun(job.ID)
	run2 := s.awaitRun(job.ID, run2ID, runTimeout)
	s.Equal("succeeded", run2.Status, "second run should succeed")
	s.Equal("succeeded", s.taskStatusesByName(job.ID, run2)["pinned"],
		"a moved tag under pinDigests must cause a cache MISS (cacheHit:false), not a cached hit")
}

// TestPinDigestsHitOnStableTag is the steady-state control for the gate above:
// with pinDigests on and the tag unchanged, the resolved digest is identical
// across runs, so the second run is still a cache HIT. This proves the miss in
// TestPinDigestsMissOnMovedTag is caused by the moved digest, not by pinning
// disabling caching wholesale.
func (s *IntegrationTestSuite) TestPinDigestsHitOnStableTag() {
	if s.engineType != "docker" {
		s.T().Skipf("digest pinning harness uses the docker SDK; engine=%s", s.engineType)
	}

	cli := s.dockerClient()
	defer func() { _ = cli.Close() }()

	const imageA = "alpine:3.23"
	s.dockerPull(cli, imageA)

	stableTag := fmt.Sprintf("caesium-pindigest-%d:stable", time.Now().UnixNano())
	s.dockerTag(cli, imageA, stableTag)
	defer s.dockerRemove(cli, stableTag)

	alias := fmt.Sprintf("integration-pindigest-hit-%d", time.Now().UnixNano())
	// digestTTL: 0 forces a fresh resolution on the second run too, so the HIT
	// is proven to come from the tag re-resolving to an identical digest — not
	// from the perf cache short-circuiting resolution.
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  cache:
    pinDigests: true
    digestTTL: 0
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: pinned
    image: %s
    command: ["sh", "-c", "echo '##caesium::output {\"val\": \"42\"}'"]
`, alias, stableTag)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	run1ID := s.triggerRun(job.ID)
	run1 := s.awaitRun(job.ID, run1ID, runTimeout)
	s.Equal("succeeded", run1.Status)
	s.Equal("succeeded", s.taskStatusesByName(job.ID, run1)["pinned"])

	// Tag unchanged -> same digest -> same key -> cache hit.
	run2ID := s.triggerRun(job.ID)
	run2 := s.awaitRun(job.ID, run2ID, runTimeout)
	s.Equal("succeeded", run2.Status)
	s.Equal("cached", s.taskStatusesByName(job.ID, run2)["pinned"],
		"an unchanged pinned digest must still hit the cache")
}

// --------------------------------------------------------------------------
// Data-plane memory D2: value-verified short-circuit
// --------------------------------------------------------------------------

// TestValueVerifiedShortCircuit is the D2 headline assertion. A producer step's
// CODE changes between runs (its command is rewritten) so its identity hash
// changes and it re-executes — a cache MISS. But it emits BYTE-IDENTICAL output.
// A downstream consumer whose only changed input was the producer's identity
// must therefore stay GREEN as a cache hit ("cached"), not cascade a re-run.
// The skip is proven by output equality, not inferred.
//
//   - Run 1: producer + consumer both execute and cache.
//   - Re-apply: producer command rewritten (different code) but the emitted
//     ##caesium::output is the SAME bytes; consumer is byte-for-byte unchanged.
//   - Run 2: producer MISSES (its identity changed) and re-executes, but its
//     output is proven equal to the prior run, so consumer short-circuits and
//     stays "cached".
func (s *IntegrationTestSuite) TestValueVerifiedShortCircuit() {
	alias := fmt.Sprintf("integration-d2-shortcircuit-%d", time.Now().UnixNano())

	// Producer v1: a no-op `true` then the canonical output line.
	manifestV1 := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  cache: true
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: producer
    image: alpine:3.23
    command: ["sh", "-c", "true; echo '##caesium::output {\"val\": \"42\"}'"]
    next: consumer
  - name: consumer
    image: alpine:3.23
    command: ["sh", "-c", "echo got=$CAESIUM_OUTPUT_PRODUCER_VAL && echo '##caesium::output {\"done\": \"yes\"}'"]
    dependsOn: producer
`, alias)

	dir := s.writeJobManifest(manifestV1)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	run1 := s.awaitRun(job.ID, s.triggerRun(job.ID), runTimeout)
	s.Equal("succeeded", run1.Status, "first run should succeed")
	statuses1 := s.taskStatusesByName(job.ID, run1)
	s.Equal("succeeded", statuses1["producer"], "producer executes on first run")
	s.Equal("succeeded", statuses1["consumer"], "consumer executes on first run")

	// Producer v2: DIFFERENT code (`false || true` instead of `true`) so its
	// identity hash changes — but the emitted output is byte-identical. The
	// consumer step is unchanged.
	manifestV2 := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  cache: true
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: producer
    image: alpine:3.23
    command: ["sh", "-c", "false || true; echo '##caesium::output {\"val\": \"42\"}'"]
    next: consumer
  - name: consumer
    image: alpine:3.23
    command: ["sh", "-c", "echo got=$CAESIUM_OUTPUT_PRODUCER_VAL && echo '##caesium::output {\"done\": \"yes\"}'"]
    dependsOn: producer
`, alias)
	s.rewriteJobManifest(dir, manifestV2)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	run2 := s.awaitRun(job.ID, s.triggerRun(job.ID), runTimeout)
	s.Equal("succeeded", run2.Status, "second run should succeed")
	statuses2 := s.taskStatusesByName(job.ID, run2)
	// Producer's code changed -> identity changed -> it must re-execute (MISS).
	s.Equal("succeeded", statuses2["producer"],
		"producer's code changed, so it must re-execute (cache MISS)")
	// Consumer's only changed input was the producer's identity, but the
	// producer's output is byte-identical, so the value-verified short-circuit
	// keeps the consumer a cache hit.
	s.Equal("cached", statuses2["consumer"],
		"a code-only producer change with identical output must NOT cascade a downstream re-run")
}

// TestValueVerifiedShortCircuitRerunsOnRealChange is the correctness control:
// when the producer's output ACTUALLY changes, the consumer's inputs really
// changed, so it MUST re-run. This guards the cache-correctness invariant — a
// short-circuit here would serve a stale downstream result (a P0 bug).
//
//   - Run 1: producer emits {"val":"42"}; both steps execute + cache.
//   - Re-apply: producer emits {"val":"99"} (genuinely different output).
//   - Run 2: producer MISSES and re-executes; the consumer's predecessor output
//     changed, so it MUST re-execute ("succeeded"), never short-circuit.
func (s *IntegrationTestSuite) TestValueVerifiedShortCircuitRerunsOnRealChange() {
	alias := fmt.Sprintf("integration-d2-realchange-%d", time.Now().UnixNano())

	manifestV1 := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  cache: true
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: producer
    image: alpine:3.23
    command: ["sh", "-c", "echo '##caesium::output {\"val\": \"42\"}'"]
    next: consumer
  - name: consumer
    image: alpine:3.23
    command: ["sh", "-c", "echo got=$CAESIUM_OUTPUT_PRODUCER_VAL && echo '##caesium::output {\"done\": \"yes\"}'"]
    dependsOn: producer
`, alias)

	dir := s.writeJobManifest(manifestV1)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	run1 := s.awaitRun(job.ID, s.triggerRun(job.ID), runTimeout)
	s.Equal("succeeded", run1.Status)
	statuses1 := s.taskStatusesByName(job.ID, run1)
	s.Equal("succeeded", statuses1["producer"])
	s.Equal("succeeded", statuses1["consumer"])

	// Producer now emits genuinely different output.
	manifestV2 := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  cache: true
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: producer
    image: alpine:3.23
    command: ["sh", "-c", "echo '##caesium::output {\"val\": \"99\"}'"]
    next: consumer
  - name: consumer
    image: alpine:3.23
    command: ["sh", "-c", "echo got=$CAESIUM_OUTPUT_PRODUCER_VAL && echo '##caesium::output {\"done\": \"yes\"}'"]
    dependsOn: producer
`, alias)
	s.rewriteJobManifest(dir, manifestV2)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	run2 := s.awaitRun(job.ID, s.triggerRun(job.ID), runTimeout)
	s.Equal("succeeded", run2.Status)
	statuses2 := s.taskStatusesByName(job.ID, run2)
	s.Equal("succeeded", statuses2["producer"],
		"producer's output changed, so it must re-execute")
	s.Equal("succeeded", statuses2["consumer"],
		"a real upstream output change MUST re-run the consumer, never short-circuit")
}

// rewriteJobManifest overwrites the single job.yaml in dir (created by
// writeJobManifest) with new contents, applying the same engine injection so
// non-docker engines still run. Used to simulate a code change between applies.
func (s *IntegrationTestSuite) rewriteJobManifest(dir, contents string) {
	s.T().Helper()
	path := filepath.Join(dir, "job.yaml")
	s.Require().NoError(os.WriteFile(path, []byte(strings.TrimSpace(s.injectEngine(contents))), 0o644))
}

// --------------------------------------------------------------------------
// Docker SDK helpers (digest pinning harness)
// --------------------------------------------------------------------------

func (s *IntegrationTestSuite) dockerClient() *client.Client {
	s.T().Helper()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	s.Require().NoError(err, "create docker client")
	return cli
}

func (s *IntegrationTestSuite) dockerPull(cli *client.Client, ref string) {
	s.T().Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	r, err := cli.ImagePull(ctx, ref, image.PullOptions{})
	s.Require().NoError(err, "pull %s", ref)
	defer func() { _ = r.Close() }()
	_, err = io.Copy(io.Discard, r)
	s.Require().NoError(err, "drain pull stream for %s", ref)
}

func (s *IntegrationTestSuite) dockerTag(cli *client.Client, source, target string) {
	s.T().Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s.Require().NoError(cli.ImageTag(ctx, source, target), "tag %s as %s", source, target)
}

func (s *IntegrationTestSuite) dockerRemove(cli *client.Client, ref string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = cli.ImageRemove(ctx, ref, image.RemoveOptions{Force: true})
}
