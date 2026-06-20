//go:build integration

package test

import (
	"context"
	"fmt"
	"io"
	"os"
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
