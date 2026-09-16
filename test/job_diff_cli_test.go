//go:build integration

package test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// TestJobDiffCLIAgainstLiveServer drives `caesium job diff` through the shipped
// CLI against the live integration server (issue #509). Default diff must match
// a non-pruning apply: unrelated jobs already on the server are not in-scope
// deletes and must not fail the CI gate.
func (s *IntegrationTestSuite) TestJobDiffCLIAgainstLiveServer() {
	suffix := time.Now().UnixNano()
	aliasA := fmt.Sprintf("job-diff-cli-a-%d", suffix)
	aliasB := fmt.Sprintf("job-diff-cli-b-%d", suffix)

	dirA := s.writeJobManifest(jobDiffCLIManifest(aliasA, "echo a"))
	defer os.RemoveAll(dirA)
	s.runCLI("job", "apply", "--path", dirA, "--server", s.caesiumURL)

	stdout, stderr, err := s.runCLISeparate("job", "diff", "--path", dirA, "--server", s.caesiumURL)
	s.Require().NoError(err, "diff of an already-applied job must exit 0 even when other jobs exist on the server\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	s.Contains(stdout, "No changes detected.")
	s.NotContains(stdout, "Creates:")
	s.NotContains(stdout, "Updates:")
	s.NotContains(stdout, "Deletes:")
	s.NotContains(stdout, `"level":"`)

	jsonOut, jsonErr, jsonCmdErr := s.runCLISeparate("job", "diff", "--path", dirA, "--json", "--server", s.caesiumURL)
	s.Require().NoError(jsonCmdErr, "unchanged --json must exit 0\nstdout:\n%s\nstderr:\n%s", jsonOut, jsonErr)
	s.Require().True(json.Valid([]byte(strings.TrimSpace(jsonOut))), "--json stdout must be valid JSON with no log prefix:\n%s", jsonOut)
	s.NotContains(jsonOut, `"level":"`)
	unchanged := parseJobDiffCLIJSON(s, jsonOut)
	s.Empty(unchanged.Added, "already-applied alias %s must not be a create: %+v", aliasA, unchanged.Added)
	s.Empty(unchanged.Modified, "already-applied alias %s must not be an update: %+v", aliasA, unchanged.Modified)
	s.Empty(unchanged.Removed, "default diff must not treat unrelated server jobs as in-scope deletes")
	s.NotContains(jobDiffCLIAliases(unchanged.Added), aliasA)
	s.NotContains(jobDiffCLIAliases(unchanged.Modified), aliasA)

	dirB := s.writeJobManifest(jobDiffCLIManifest(aliasB, "echo b"))
	defer os.RemoveAll(dirB)

	stdout, stderr, err = s.runCLISeparate("job", "diff", "--path", dirB, "--server", s.caesiumURL)
	s.Require().Error(err, "unapplied job B must be an in-scope create\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	s.Equal(1, jobDiffCLIExitCode(err))
	s.Contains(stdout, "Creates:")
	s.Contains(stdout, aliasB)
	s.NotContains(stdout, "Deletes:")

	jsonOut, jsonErr, jsonCmdErr = s.runCLISeparate("job", "diff", "--path", dirB, "--json", "--server", s.caesiumURL)
	s.Require().Error(jsonCmdErr, "create --json must exit nonzero\nstdout:\n%s\nstderr:\n%s", jsonOut, jsonErr)
	s.Equal(1, jobDiffCLIExitCode(jsonCmdErr))
	s.Require().True(json.Valid([]byte(strings.TrimSpace(jsonOut))), "--json stdout must be valid JSON with no log prefix:\n%s", jsonOut)
	s.NotContains(jsonOut, `"level":"`)
	created := parseJobDiffCLIJSON(s, jsonOut)
	s.Contains(jobDiffCLIAliases(created.Added), aliasB)
	s.Empty(created.Removed, "default --json must not put unrelated jobs in removed")

	s.runCLI("job", "apply", "--path", dirB, "--server", s.caesiumURL)
	s.rewriteJobManifest(dirB, jobDiffCLIManifest(aliasB, "echo b-changed"))

	stdout, stderr, err = s.runCLISeparate("job", "diff", "--path", dirB, "--server", s.caesiumURL)
	s.Require().Error(err, "modified job B must be an in-scope update\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	s.Equal(1, jobDiffCLIExitCode(err))
	s.Contains(stdout, "Updates:")
	s.Contains(stdout, aliasB)
	s.NotContains(stdout, "Deletes:")

	jsonOut, jsonErr, jsonCmdErr = s.runCLISeparate("job", "diff", "--path", dirB, "--json", "--server", s.caesiumURL)
	s.Require().Error(jsonCmdErr, "update --json must exit nonzero\nstdout:\n%s\nstderr:\n%s", jsonOut, jsonErr)
	s.Require().True(json.Valid([]byte(strings.TrimSpace(jsonOut))), jsonOut)
	updated := parseJobDiffCLIJSON(s, jsonOut)
	s.Contains(jobDiffCLIAliases(updated.Modified), aliasB)

	s.runCLI("job", "apply", "--path", dirB, "--server", s.caesiumURL)

	stdout, stderr, err = s.runCLISeparate("job", "diff", "--path", dirB, "--server", s.caesiumURL)
	s.Require().NoError(err, "applied B without --prune must exit 0 even though A and other jobs exist\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	s.NotContains(stdout, "Deletes:")
	s.NotContains(stdout, "Creates:")
	s.NotContains(stdout, "Updates:")

	stdout, stderr, err = s.runCLISeparate("job", "diff", "--path", dirB, "--prune", "--server", s.caesiumURL)
	s.Require().Error(err, "--prune on a path that contains only B must report other jobs as deletes\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	s.Equal(1, jobDiffCLIExitCode(err))
	s.Contains(stdout, "Deletes:")
	s.Contains(stdout, aliasA)
	s.NotContains(stdout, aliasB)

	jsonOut, jsonErr, jsonCmdErr = s.runCLISeparate("job", "diff", "--path", dirB, "--prune", "--json", "--server", s.caesiumURL)
	s.Require().Error(jsonCmdErr, "prune --json must exit nonzero\nstdout:\n%s\nstderr:\n%s", jsonOut, jsonErr)
	s.Require().True(json.Valid([]byte(strings.TrimSpace(jsonOut))), jsonOut)
	pruned := parseJobDiffCLIJSON(s, jsonOut)
	s.Contains(jobDiffCLIAliases(pruned.Removed), aliasA)
	s.NotContains(jobDiffCLIAliases(pruned.Removed), aliasB)
	s.Empty(pruned.WouldPrune)
}

// TestJobDiffCLIRejectsDuplicateAliases is the CLI-surface proof that two
// manifests sharing metadata.alias fail closed locally. The check runs before
// POST /v1/jobdefs/diff, so a closed dummy --server must not be contacted.
func (s *IntegrationTestSuite) TestJobDiffCLIRejectsDuplicateAliases() {
	alias := fmt.Sprintf("job-diff-dup-%d", time.Now().UnixNano())
	dir, err := os.MkdirTemp("", "caesium-job-diff-dup-*")
	s.Require().NoError(err)
	defer os.RemoveAll(dir)

	one := strings.TrimSpace(s.injectEngine(jobDiffCLIManifest(alias, "echo first")))
	two := strings.TrimSpace(s.injectEngine(jobDiffCLIManifest(alias, "echo second")))
	s.Require().NoError(os.WriteFile(filepath.Join(dir, "one.yaml"), []byte(one), 0o644))
	s.Require().NoError(os.WriteFile(filepath.Join(dir, "two.yaml"), []byte(two), 0o644))

	stdout, stderr, err := s.runCLISeparate("job", "diff", "--path", dir, "--server", "http://127.0.0.1:1")
	s.Require().Error(err, "duplicate aliases must exit nonzero\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	s.Equal(1, jobDiffCLIExitCode(err))
	s.Contains(stderr+stdout, fmt.Sprintf("duplicate job alias %q", alias))
	s.NotContains(stdout, "Creates:")
	s.NotContains(stdout, "Updates:")
	s.NotContains(stdout, "No changes detected.")
}

func jobDiffCLIManifest(alias, command string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    cron: "0 0 1 1 *"
steps:
  - name: run
    image: alpine:3.23
    command: ["sh", "-c", %q]
`, alias, command)
}

type jobDiffCLIJSON struct {
	Version    int             `json:"version"`
	Added      []jobDiffCLIJob `json:"added"`
	Modified   []jobDiffCLIJob `json:"modified"`
	Removed    []jobDiffCLIJob `json:"removed"`
	WouldPrune []jobDiffCLIJob `json:"wouldPrune"`
}

type jobDiffCLIJob struct {
	Alias string `json:"alias"`
}

func parseJobDiffCLIJSON(s *IntegrationTestSuite, stdout string) jobDiffCLIJSON {
	s.T().Helper()
	var parsed jobDiffCLIJSON
	s.Require().NoError(json.Unmarshal([]byte(stdout), &parsed), "stdout:\n%s", stdout)
	s.Equal(1, parsed.Version)
	return parsed
}

func jobDiffCLIAliases(jobs []jobDiffCLIJob) []string {
	out := make([]string, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, job.Alias)
	}
	return out
}

func jobDiffCLIExitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
