//go:build integration

package test

import (
	"encoding/json"
	"os"

	"github.com/caesium-cloud/caesium/internal/offlinereport"
)

// TestJobLintJSONReportsFailingLocalManifest drives `job lint --json` with no
// server. A failing manifest must leave the version-1 report on stdout.
func (s *IntegrationTestSuite) TestJobLintJSONReportsFailingLocalManifest() {
	good := s.writeJobManifest(`
apiVersion: v1
kind: Job
metadata:
  alias: offline-json-lint-ok
trigger:
  type: cron
  configuration:
    expression: "*/5 * * * *"
steps:
  - name: hello
    image: alpine:3.23
    command: ["echo", "lint-ok"]
`)
	defer os.RemoveAll(good)

	stdout, stderr, err := s.runCLISeparate("job", "lint", "--json", "--path", good)
	s.Require().NoError(err)
	okReport := s.decodeOfflineReport(stdout, stderr)
	s.Equal("lint", okReport.Command)
	s.True(okReport.OK)
	s.GreaterOrEqual(okReport.Discovered, 1)

	bad := s.writeInvalidJobManifest()
	defer os.RemoveAll(bad)
	stdout, stderr, err = s.runCLISeparate("job", "lint", "--json", "--path", bad)
	s.Require().Error(err)
	s.NotContains(stdout, "--server")
	badReport := s.decodeOfflineReport(stdout, stderr)
	s.Equal("lint", badReport.Command)
	s.False(badReport.OK)
	s.NotEmpty(badReport.Diagnostics)
	s.GreaterOrEqual(badReport.Summary.Failed, 1)
}

// TestTestJSONReportsSchemaFailureAndHarnessScenario drives `test --json`
// through a schema failure and one harness scenario.
func (s *IntegrationTestSuite) TestTestJSONReportsSchemaFailureAndHarnessScenario() {
	bad := s.writeInvalidJobManifest()
	defer os.RemoveAll(bad)

	stdout, stderr, err := s.runCLISeparate("test", "--json", "--path", bad)
	s.Require().Error(err)
	schemaReport := s.decodeOfflineReport(stdout, stderr)
	s.Equal("test", schemaReport.Command)
	s.Equal("schema", schemaReport.Scope.Kind)
	s.False(schemaReport.OK)
	s.NotEmpty(schemaReport.Diagnostics)
	s.GreaterOrEqual(schemaReport.Summary.Failed, 1)

	s.requireDocker()
	dir := s.writeJobManifest(`
apiVersion: v1
kind: Job
metadata:
  alias: offline-json-harness
trigger:
  type: cron
  configuration:
    expression: "*/5 * * * *"
steps:
  - name: hello
    image: alpine:3.23
    command: ["echo", "offline-json"]
`)
	defer os.RemoveAll(dir)
	s.writeScenarioManifest(dir, `
apiVersion: v1
kind: Harness
scenarios:
  - name: offline-json-harness
    path: ./job.yaml
    expect:
      runStatus: succeeded
      tasks:
        - name: hello
          status: succeeded
`)

	stdout, stderr, err = s.runCLISeparate("test", "--json", "--scenario", dir)
	s.Require().NoError(err, "stderr:\n%s", stderr)
	scenarioReport := s.decodeOfflineReport(stdout, stderr)
	s.Equal("test", scenarioReport.Command)
	s.Equal("scenario", scenarioReport.Scope.Kind)
	s.True(scenarioReport.OK)
	s.Equal(1, scenarioReport.Scope.Executed)
	s.Require().Len(scenarioReport.Scenarios, 1)
	s.True(scenarioReport.Scenarios[0].Passed)
	s.Equal("offline-json-harness", scenarioReport.Scenarios[0].Name)
	s.Require().NotNil(scenarioReport.Scenarios[0].Run)
	s.NotEmpty(scenarioReport.Scenarios[0].Run.Tasks)
}

// TestDevOnceJSONRunsLocalJob drives `dev --once --json` through one local step.
func (s *IntegrationTestSuite) TestDevOnceJSONRunsLocalJob() {
	s.requireDocker()
	dir := s.writeJobManifest(`
apiVersion: v1
kind: Job
metadata:
  alias: offline-json-dev
trigger:
  type: cron
  configuration:
    expression: "*/5 * * * *"
steps:
  - name: hello
    image: alpine:3.23
    command: ["echo", "offline-json"]
`)
	defer os.RemoveAll(dir)

	stdout, stderr, err := s.runCLISeparate("dev", "--once", "--json", "--path", dir)
	s.Require().NoError(err, "stderr:\n%s", stderr)
	s.NotContains(stdout, "  OK")
	report := s.decodeOfflineReport(stdout, stderr)
	s.Equal("dev", report.Command)
	s.Equal("dev", report.Scope.Kind)
	s.True(report.OK)
	s.Equal(1, report.Scope.Executed)
	s.Require().Len(report.Runs, 1)
	s.Equal("succeeded", report.Runs[0].Status)
	s.NotEmpty(report.Runs[0].RunID)
	s.Require().NotEmpty(report.Runs[0].Tasks)
	s.Equal("hello", report.Runs[0].Tasks[0].Name)
}

func (s *IntegrationTestSuite) decodeOfflineReport(stdout, stderr string) offlinereport.Report {
	s.T().Helper()
	s.NotContains(stderr, `"version"`)
	s.True(json.Valid([]byte(stdout)), "stdout was not JSON:\n%s\nstderr:\n%s", stdout, stderr)
	var report offlinereport.Report
	s.Require().NoError(json.Unmarshal([]byte(stdout), &report))
	s.Equal(1, report.Version)
	s.NotNil(report.Scope)
	return report
}
