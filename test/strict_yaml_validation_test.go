//go:build integration

package test

import (
	"os"
	"path/filepath"
	"strings"
)

func (s *IntegrationTestSuite) TestStrictJobFieldsRejectedAcrossCLIPaths() {
	dir := s.writeJobManifest(`
apiVersion: v1
kind: Job
metadata:
  alias: typo-job
  schemaValidaton: fail
trigger: {type: http, configuration: {path: typo-job}}
steps:
  - name: extract
    image: alpine:3.23
  - name: load
    image: alpine:3.23
    dependsON: extract
`)
	defer os.RemoveAll(dir)
	manifestPath := filepath.Join(dir, "job.yaml")

	commands := [][]string{
		{"job", "lint", "--path", manifestPath},
		{"test", "--path", manifestPath},
		{"dev", "--once", "--path", manifestPath},
		{"job", "apply", "--path", manifestPath, "--server", "http://127.0.0.1:1"},
	}
	for _, args := range commands {
		s.Run(strings.Join(args[:2], " "), func() {
			out, err := s.runCLIRaw(args...)
			s.Error(err, "expected non-zero exit for unknown Job fields")
			s.Contains(out, manifestPath)
			s.Contains(out, `YAML path metadata.schemaValidaton (line `)
			s.Contains(out, `YAML path steps[1].dependsON (line `)
			s.NotContains(out, "Validated 1 job definition")
			s.NotContains(out, "PASS  typo-job")
			s.NotContains(out, "OK    typo-job")
			s.NotContains(out, "Applied 1 job definition")
		})
	}
}

func (s *IntegrationTestSuite) TestTestAndDevRejectTypedJobDecodeErrors() {
	tests := map[string]string{
		"duration": `
apiVersion: v1
kind: Job
metadata: {alias: bad-duration, taskTimeout: banana}
trigger: {type: http, configuration: {path: bad-duration}}
steps: [{name: extract, image: alpine:3.23, command: [echo, hello]}]
`,
		"command": `
apiVersion: v1
kind: Job
metadata: {alias: bad-command}
trigger: {type: http, configuration: {path: bad-command}}
steps: [{name: extract, image: alpine:3.23, command: echo hello}]
`,
	}
	for name, manifest := range tests {
		s.Run(name, func() {
			dir := s.writeJobManifest(manifest)
			defer os.RemoveAll(dir)
			for _, args := range [][]string{
				{"test", "--path", dir},
				{"dev", "--once", "--path", dir},
			} {
				out, err := s.runCLIRaw(args...)
				s.Error(err, "expected non-zero exit for typed decode error")
				s.Contains(out, "cannot unmarshal")
				s.NotContains(out, "No job definitions")
				s.NotContains(out, "PASS")
				s.NotContains(out, "completed successfully")
			}
		})
	}
}

func (s *IntegrationTestSuite) TestTestRejectsInvalidJobInMixedDirectoryAndMultiDocumentFile() {
	valid := `apiVersion: v1
kind: Job
metadata: {alias: valid-job}
trigger: {type: http, configuration: {path: valid-job}}
steps: [{name: run, image: alpine:3.23}]
`
	invalid := `apiVersion: v1
kind: Job
metadata: {alias: invalid-job, taskTimeout: banana}
trigger: {type: http, configuration: {path: invalid-job}}
steps: [{name: run, image: alpine:3.23}]
`

	dir := s.T().TempDir()
	s.Require().NoError(os.WriteFile(filepath.Join(dir, "a-valid.job.yaml"), []byte(valid), 0o644))
	s.Require().NoError(os.WriteFile(filepath.Join(dir, "b-invalid.job.yaml"), []byte(invalid), 0o644))
	out, err := s.runCLIRaw("test", "--path", dir)
	s.Error(err)
	s.Contains(out, "b-invalid.job.yaml")
	s.NotContains(out, "PASS  valid-job")

	multiPath := filepath.Join(s.T().TempDir(), "mixed.job.yaml")
	s.Require().NoError(os.WriteFile(multiPath, []byte(valid+"---\n"+invalid), 0o644))
	out, err = s.runCLIRaw("test", "--path", multiPath)
	s.Error(err)
	s.Contains(out, "mixed.job.yaml")
	s.NotContains(out, "PASS  valid-job")
}

func (s *IntegrationTestSuite) TestHarnessTyposFailBeforeAnyScenarioExecutes() {
	dir := s.writeJobManifest(`
apiVersion: v1
kind: Job
metadata: {alias: harness-typo}
trigger: {type: http, configuration: {path: harness-typo}}
steps: [{name: extract, image: alpine:3.23}]
`)
	defer os.RemoveAll(dir)
	s.writeScenarioManifest(dir, `
apiVersion: v1
kind: Harness
scenarios:
  - name: typo-assertions
    path: ./job.yaml
    expect:
      runStatus: succeeded
      tasks:
        - name: extract
          status: succeeded
          outputs: {rows: "999"}
          logContain: [THIS-MUST-NOT-PASS]
`)

	out, err := s.runCLIRaw("test", "--scenario", dir, "--verbose")
	s.Error(err, "expected non-zero exit for unknown Harness fields")
	s.Contains(out, `YAML path scenarios[0].expect.tasks[0].outputs`)
	s.Contains(out, `YAML path scenarios[0].expect.tasks[0].logContain`)
	s.NotContains(out, "PASS  typo-assertions")
	s.NotContains(out, "FAIL  typo-assertions")
}

func (s *IntegrationTestSuite) TestTestCommandFailsWhenNoHarnessScenariosSelected() {
	dir := s.T().TempDir()
	out, err := s.runCLIRaw("test", "--scenario", dir)
	s.Error(err, "expected non-zero exit when no scenarios are selected")
	s.Contains(out, "no harness scenarios selected")
}

func (s *IntegrationTestSuite) TestDevOnceFailsWhenNoJobsSelected() {
	dir := s.T().TempDir()
	out, err := s.runCLIRaw("dev", "--once", "--path", dir)
	s.Error(err, "expected non-zero exit when no jobs are selected")
	s.Contains(out, "no job definitions selected")
}
