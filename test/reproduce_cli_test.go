//go:build integration

package test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/google/uuid"
)

func (s *IntegrationTestSuite) TestReproduceCLIDryRunAndRunMode() {
	jobID, runID, taskRunID := s.createReproduceCLIFixture()
	secretAssertionEnabled := s.engineType != "kubernetes"
	if secretAssertionEnabled {
		catalogDB := s.openIntegrationCatalogDB()
		defer func() { s.Require().NoError(catalogDB.Close()) }()
		s.Require().NoError(addSecretRefToDescriptor(s.T().Context(), catalogDB, taskRunID))
	}

	dryRunOut, err := s.runCLIStdout(
		"reproduce", runID,
		"--job-id", jobID,
		"--task", "transform",
		"--dry-run",
		"--json",
		"--server", s.caesiumURL,
	)
	s.Require().NoError(err)
	var envelope reproduceCLIEnvelope
	s.Require().NoError(json.Unmarshal([]byte(dryRunOut), &envelope))
	s.Equal("transform", envelope.Task)
	s.Equal("fixture-literal", envelope.Env["LITERAL_ENV"])
	s.Equal("vanilla", envelope.Env["CAESIUM_PARAM_FLAVOR"])
	s.Equal("7", envelope.Env["CAESIUM_OUTPUT_PRODUCE_ROWS"])
	s.Equal("raw", envelope.Env["CAESIUM_OUTPUT_PRODUCE_SOURCE"])
	if secretAssertionEnabled {
		s.NotContains(envelope.Env, "SECRET_ENV")
		s.True(reproduceHasOmittedSecret(envelope.OmittedSecrets, "SECRET_ENV"), "omitted secrets = %+v", envelope.OmittedSecrets)
		s.True(reproduceHasWarning(envelope.Warnings, "secret_omitted"), "warnings = %+v", envelope.Warnings)
	}

	// The missing-descriptor exit-2 leg is daemon-free — run it on every lane
	// before the docker-gated run-mode leg below.
	missingOut, missingErrOut, missingErr := s.runCLISeparate(
		"reproduce", uuid.NewString(),
		"--job-id", jobID,
		"--task", "transform",
		"--server", s.caesiumURL,
	)
	s.Require().Error(missingErr)
	s.Equal("", missingOut)
	s.Equal(2, reproduceExitCode(missingErr), "stderr: %s", missingErrOut)
	s.Contains(missingErrOut, "fetch descriptor failed")

	// Run mode executes on the operator's LOCAL Docker daemon by design; the
	// podman and kubernetes lanes' test-runners have no docker.sock, so only
	// the daemon-free legs above run there.
	if s.engineType != "" && s.engineType != "docker" {
		s.T().Logf("skipping reproduce run-mode leg under CAESIUM_TEST_ENGINE=%s; local execution needs the harness Docker daemon (covered on the docker lanes)", s.engineType)
		return
	}

	runOut, runErrOut, runErr := s.runCLISeparate(
		"reproduce", runID,
		"--job-id", jobID,
		"--task", "transform",
		"--json",
		"--server", s.caesiumURL,
	)
	s.Require().NoError(runErr, "stderr: %s", runErrOut)
	var result reproduceCLIRunResult
	s.Require().NoError(json.Unmarshal([]byte(runOut), &result),
		"run-mode --json stdout must be a single JSON document; raw stdout: %q; stderr: %q", runOut, runErrOut)
	s.Equal(0, result.ExitCode)
	s.Equal("succeeded", result.Status)
	s.Equal("yes", result.Output["clean"])
	s.Equal("7", result.Output["rows"])
	s.Equal("raw", result.Output["source"])
}

func (s *IntegrationTestSuite) createReproduceCLIFixture() (jobID, runID, taskRunID string) {
	alias := fmt.Sprintf("e2e-reproduce-cli-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
  replaySafe: true
trigger: { type: cron, configuration: { cron: "0 2 * * *" } }
steps:
  - name: produce
    image: alpine:3.23
    cache: { pinDigests: true }
    command: ["sh","-c","echo '##caesium::output {\"rows\":\"7\",\"source\":\"raw\"}'"]
    next: transform
  - name: transform
    image: alpine:3.23
    cache: { pinDigests: true }
    env:
      LITERAL_ENV: fixture-literal
    command: ["sh","-c","test \"$LITERAL_ENV\" = fixture-literal; test \"$CAESIUM_PARAM_FLAVOR\" = vanilla; test \"$CAESIUM_OUTPUT_PRODUCE_ROWS\" = 7; test \"$CAESIUM_OUTPUT_PRODUCE_SOURCE\" = raw; echo '##caesium::output {\"clean\":\"yes\",\"rows\":\"7\",\"source\":\"raw\"}'"]
    dependsOn: produce
`, alias)
	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	runID = s.triggerRunWithParams(job.ID, map[string]string{"flavor": "vanilla"})
	s.Require().Equal("succeeded", s.awaitRun(job.ID, runID, runTimeout).Status)

	status, body := s.getReproduceDescriptor(job.ID, runID, "transform")
	s.Require().Equal(200, status, string(body))
	var resp reproduceDescriptorResponse
	s.Require().NoError(json.Unmarshal(body, &resp))
	return job.ID, runID, resp.TaskRunID
}

type reproduceCLIEnvelope struct {
	Task           string            `json:"task"`
	Env            map[string]string `json:"env"`
	OmittedSecrets []struct {
		EnvKey string `json:"env_key"`
		Ref    string `json:"ref"`
	} `json:"omitted_secrets"`
	Warnings []struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"warnings"`
}

type reproduceCLIRunResult struct {
	Status   string            `json:"status"`
	ExitCode int               `json:"exit_code"`
	Output   map[string]string `json:"output"`
}

func addSecretRefToDescriptor(ctx context.Context, conn *sql.DB, taskRunID string) error {
	var raw string
	if err := conn.QueryRowContext(ctx, `SELECT execution_descriptor FROM task_runs WHERE id = ?`, taskRunID).Scan(&raw); err != nil {
		return err
	}
	var descriptor map[string]any
	if err := json.Unmarshal([]byte(raw), &descriptor); err != nil {
		return err
	}
	spec, _ := descriptor["containerSpec"].(map[string]any)
	if spec == nil {
		spec = map[string]any{}
		descriptor["containerSpec"] = spec
	}
	env, _ := spec["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
		spec["env"] = env
	}
	env["SECRET_ENV"] = "secret://fixture/db-password"
	descriptor["secretRefs"] = []any{map[string]any{
		"ref":        "secret://fixture/db-password",
		"envKey":     "SECRET_ENV",
		"provider":   "fixture",
		"verifiable": false,
	}}
	updated, err := json.Marshal(descriptor)
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `UPDATE task_runs SET execution_descriptor = ? WHERE id = ?`, string(updated), taskRunID)
	return err
}

func reproduceHasOmittedSecret(secrets []struct {
	EnvKey string `json:"env_key"`
	Ref    string `json:"ref"`
}, envKey string) bool {
	for _, secret := range secrets {
		if secret.EnvKey == envKey {
			return true
		}
	}
	return false
}

func reproduceHasWarning(warnings []struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}, code string) bool {
	for _, warning := range warnings {
		if warning.Code == code {
			return true
		}
	}
	return false
}

func reproduceExitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
