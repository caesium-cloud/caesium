//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

func (s *IntegrationTestSuite) TestRunReplayCLIJSONStdoutIdempotencyAndDiff() {
	alias := fmt.Sprintf("e2e-replay-cli-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(replayManifest(alias, true))
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	baselineRunID := s.triggerRun(job.ID)
	s.Require().Equal("succeeded", s.awaitRun(job.ID, baselineRunID, runTimeout).Status)

	stdout, stderr, err := s.runCLISeparate("run", "replay", baselineRunID, "--job-id", job.ID, "--json", "--server", s.caesiumURL)
	s.Require().NoError(err, "caesium run replay failed:\nstdout=%s\nstderr=%s", stdout, stderr)
	s.Require().True(json.Valid([]byte(stdout)), "caesium run replay --json stdout was not clean JSON:\n%s", stdout)
	s.Contains(stderr, "replay idempotency key: ")
	s.NotContains(stdout, "replay idempotency key", "idempotency guidance must stay off stdout")
	generatedKey := strings.TrimSpace(strings.TrimPrefix(stderr, "replay idempotency key: "))
	s.Require().NotEmpty(generatedKey)
	s.NotContains(stdout, generatedKey, "generated idempotency key must stay off stdout")
	var auto replayResponse
	s.Require().NoError(json.Unmarshal([]byte(stdout), &auto))
	s.Require().NotEmpty(auto.RunID)
	s.True(auto.Quarantine)
	s.Equal("succeeded", s.awaitRun(job.ID, auto.RunID, runTimeout).Status)

	key := "replay-cli-key-" + time.Now().Format("150405.000000000")
	firstOut, firstErrOut, firstErr := s.runCLISeparate("run", "replay", baselineRunID, "--job-id", job.ID, "--idempotency-key", key, "--json", "--server", s.caesiumURL)
	s.Require().NoError(firstErr, "first keyed replay failed:\nstdout=%s\nstderr=%s", firstOut, firstErrOut)
	s.Require().True(json.Valid([]byte(firstOut)), "first keyed replay stdout was not clean JSON:\n%s", firstOut)
	s.NotContains(firstErrOut, "replay idempotency key: ", "operator-supplied keys must not be echoed")
	var first replayResponse
	s.Require().NoError(json.Unmarshal([]byte(firstOut), &first))
	s.Require().NotEmpty(first.RunID)

	secondOut, secondErrOut, secondErr := s.runCLISeparate("run", "replay", baselineRunID, "--job-id", job.ID, "--idempotency-key", key, "--json", "--server", s.caesiumURL)
	s.Require().NoError(secondErr, "second keyed replay failed:\nstdout=%s\nstderr=%s", secondOut, secondErrOut)
	s.Require().True(json.Valid([]byte(secondOut)), "second keyed replay stdout was not clean JSON:\n%s", secondOut)
	var second replayResponse
	s.Require().NoError(json.Unmarshal([]byte(secondOut), &second))
	s.Equal(first.RunID, second.RunID, "same --idempotency-key must dedupe to the existing replay run")

	diffKey := key + "-diff"
	diffOut, diffErrOut, diffErr := s.runCLISeparate("run", "replay", baselineRunID, "--job-id", job.ID, "--idempotency-key", diffKey, "--diff", "--json", "--server", s.caesiumURL)
	s.Require().NoError(diffErr, "replay --diff failed:\nstdout=%s\nstderr=%s", diffOut, diffErrOut)
	s.Require().True(json.Valid([]byte(diffOut)), "replay --diff --json stdout was not clean JSON:\n%s", diffOut)
	s.Contains(diffErrOut, "awaiting replay run ")
	s.NotContains(diffOut, "awaiting replay run")
	var diff runDiffRESTResponse
	s.Require().NoError(json.Unmarshal([]byte(diffOut), &diff))
	s.Equal(job.ID, diff.JobID)
	s.Equal(baselineRunID, diff.LeftRunID)
	s.NotEmpty(diff.RightRunID)
	s.NotEqual(baselineRunID, diff.RightRunID)
	s.NotEmpty(diff.Tasks)
}

func (s *IntegrationTestSuite) TestRunReplayCLIRequiresJobIDAndRefusesLocalReexec() {
	alias := fmt.Sprintf("e2e-replay-cli-refusal-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(replayManifest(alias, true))
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	baselineRunID := s.triggerRun(job.ID)
	s.Require().Equal("succeeded", s.awaitRun(job.ID, baselineRunID, runTimeout).Status)

	stdout, stderr, err := s.runCLISeparate("run", "replay", baselineRunID, "--json", "--server", s.caesiumURL)
	s.Require().Error(err)
	s.Empty(strings.TrimSpace(stdout), "missing --job-id must not write machine output")
	s.Contains(stderr, "--job-id is required")
	s.NotContains(stderr, "replay idempotency key", "validation must fail before minting a replay key")

	refusalKey := "replay-cli-refusal-" + time.Now().Format("150405.000000000")
	stdout, stderr, err = s.runCLISeparate("run", "replay", baselineRunID, "--job-id", job.ID, "--set", "mode=what-if", "--idempotency-key", refusalKey, "--json", "--server", s.caesiumURL)
	s.Require().Error(err)
	s.Empty(strings.TrimSpace(stdout), "replay refusal must leave stdout clean")
	s.Contains(stderr, "replay refused (409)")
	s.Contains(stderr, "distributed execution mode")
}
