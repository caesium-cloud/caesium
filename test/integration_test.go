//go:build integration

package test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

type IntegrationTestSuite struct {
	suite.Suite
	caesiumURL  string
	cliPath     string
	projectRoot string
}

func (s *IntegrationTestSuite) SetupSuite() {
	cwd, err := os.Getwd()
	if err != nil {
		s.T().Fatalf("getwd: %v", err)
	}
	s.projectRoot = filepath.Clean(filepath.Join(cwd, ".."))

	binPath := filepath.Join(s.projectRoot, "caesium-cli")
	build := exec.CommandContext(s.T().Context(), "go", "build", "-o", binPath, ".")
	build.Dir = s.projectRoot
	build.Env = append(os.Environ(), "GOFLAGS=-buildvcs=false")
	if output, err := build.CombinedOutput(); err != nil {
		s.T().Fatalf("build caesium cli: %v\n%s", err, string(output))
	}
	s.cliPath = binPath
	s.T().Cleanup(func() {
		_ = os.Remove(binPath)
	})

	host := os.Getenv("CAESIUM_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	s.caesiumURL = fmt.Sprintf("http://%v:8080", host)

	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(2 * time.Minute)
	var (
		lastErr    error
		lastStatus int
	)
	for {
		if time.Now().After(deadline) {
			s.T().Fatalf(
				"timeout waiting for caesium /health to be ready (url=%s, last_status=%d, last_error=%v)",
				s.caesiumURL,
				lastStatus,
				lastErr,
			)
		}

		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, fmt.Sprintf("%v/health", s.caesiumURL), nil)
		if err != nil {
			lastErr = err
			time.Sleep(time.Second)
			continue
		}

		resp, err := client.Do(req)
		if err == nil && resp != nil {
			lastStatus = resp.StatusCode
			var body []byte
			if resp.Body != nil {
				body, _ = io.ReadAll(resp.Body)
				_ = resp.Body.Close()
			}
			if resp.StatusCode == http.StatusOK {
				break
			}
			lastErr = fmt.Errorf("body: %s", string(body))
		} else {
			lastErr = err
		}
		time.Sleep(time.Second)
	}
}

func (s *IntegrationTestSuite) TestHealth() {
	resp, err := s.doRequest(http.MethodGet, fmt.Sprintf("%v/health", s.caesiumURL), nil)
	assert.Nil(s.T(), err)
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	assert.Equal(s.T(), http.StatusOK, resp.StatusCode)
}

func (s *IntegrationTestSuite) TestAtomSpecPersistence() {
	alias := fmt.Sprintf("integration-atom-spec-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    cron: "*/10 * * * *"
steps:
  - name: env-check
    image: alpine
    command: ["sh", "-c", "echo $INTEGRATION_ENV > /tmp/out"]
    workdir: /tmp
    env:
      INTEGRATION_ENV: spec-working
    mounts:
      - source: /tmp
        target: /tmp
`, alias)

	tmpDir := s.writeJobManifest(manifest)
	defer os.RemoveAll(tmpDir)

	s.runCLI("job", "apply", "--path", tmpDir)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	tasks := s.fetchTasks(job.ID)
	s.Require().NotEmpty(tasks, "no tasks for job %s", job.ID)

	atomID := tasks[0]
	spec := s.fetchAtomSpec(atomID)

	s.Require().NotNil(spec.Env)
	s.Equal("spec-working", spec.Env["INTEGRATION_ENV"])
	s.Equal("/tmp", spec.WorkDir)
	s.Require().Len(spec.Mounts, 1)
	s.Equal("/tmp", spec.Mounts[0].Source)
	s.Equal("/tmp", spec.Mounts[0].Target)
}

func (s *IntegrationTestSuite) TestJobApplyReconcilesExistingDefinition() {
	alias := fmt.Sprintf("integration-reconcile-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  annotations:
    owner: first
trigger:
  type: cron
  configuration:
    cron: "*/10 * * * *"
steps:
  - name: extract
    image: alpine
    command: ["sh", "-c", "echo extract"]
  - name: load
    image: alpine
    command: ["sh", "-c", "echo load"]
`, alias)

	tmpDir := s.writeJobManifest(manifest)
	defer os.RemoveAll(tmpDir)

	s.runCLI("job", "apply", "--path", tmpDir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)
	firstID := job.ID

	updated := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  annotations:
    owner: second
trigger:
  type: cron
  configuration:
    cron: "*/10 * * * *"
steps:
  - name: extract
    image: alpine
    command: ["sh", "-c", "echo extract updated"]
`, alias)
	require.NoError(s.T(), os.WriteFile(filepath.Join(tmpDir, "job.yaml"), []byte(strings.TrimSpace(updated)), 0o644))

	s.runCLI("job", "apply", "--path", tmpDir, "--server", s.caesiumURL)
	job = s.requireJobByAlias(alias)
	s.Equal(firstID, job.ID)
	s.Len(s.fetchTasks(job.ID), 1)
}

func TestIntegrationTestSuite(t *testing.T) {
	suite.Run(t, new(IntegrationTestSuite))
}

func (s *IntegrationTestSuite) writeJobManifest(contents string) string {
	dir, err := os.MkdirTemp("", "caesium-job-*")
	require.NoError(s.T(), err)

	path := filepath.Join(dir, "job.yaml")
	require.NoError(s.T(), os.WriteFile(path, []byte(strings.TrimSpace(contents)), 0o644))
	return dir
}

func (s *IntegrationTestSuite) runCLI(args ...string) {
	cmd := exec.CommandContext(s.T().Context(), s.cliPath, args...)
	cmd.Dir = s.projectRoot
	cmd.Env = os.Environ()
	output, err := cmd.CombinedOutput()
	require.NoError(s.T(), err, "cli %v failed: %s", args, string(output))
}

type jobSummary struct {
	ID    string `json:"id"`
	Alias string `json:"alias"`
}

func (s *IntegrationTestSuite) requireJobByAlias(alias string) *jobSummary {
	query := url.Values{}
	query.Set("order_by", "created_at desc")
	var jobs []jobSummary
	s.getJSON("/v1/jobs?"+query.Encode(), &jobs)
	for _, job := range jobs {
		if job.Alias == alias {
			return &job
		}
	}
	s.T().Fatalf("job %s not found", alias)
	return nil
}

func (s *IntegrationTestSuite) fetchTasks(jobID string) []string {
	var tasks []struct {
		AtomID string `json:"AtomID"`
	}
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/tasks", jobID), &tasks)
	ids := make([]string, 0, len(tasks))
	for _, t := range tasks {
		if t.AtomID != "" {
			ids = append(ids, t.AtomID)
		}
	}
	return ids
}

func (s *IntegrationTestSuite) fetchAtomSpec(atomID string) container.Spec {
	var atomResp struct {
		Spec container.Spec `json:"spec"`
	}
	s.getJSON(fmt.Sprintf("/v1/atoms/%s", atomID), &atomResp)
	return atomResp.Spec
}

func (s *IntegrationTestSuite) getJSON(path string, target any) {
	resp, err := s.doRequest(http.MethodGet, s.caesiumURL+path, nil)
	require.NoError(s.T(), err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(s.T(), err)

	require.Equal(s.T(), http.StatusOK, resp.StatusCode, string(body))
	require.NoError(s.T(), json.Unmarshal(body, target))
}

// tryGetJSON is like getJSON but returns an error instead of calling
// require.  This is safe to call from testify's Eventually/Never callbacks
// which run the condition function in a separate goroutine.
func (s *IntegrationTestSuite) tryGetJSON(path string, target any) error {
	resp, err := s.doRequest(http.MethodGet, s.caesiumURL+path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, body)
	}
	return json.Unmarshal(body, target)
}

func (s *IntegrationTestSuite) doRequest(method, target string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(s.T().Context(), method, target, body)
	if err != nil {
		return nil, err
	}

	//nolint:bodyclose // Response body ownership is transferred to the caller.
	return http.DefaultClient.Do(req)
}
