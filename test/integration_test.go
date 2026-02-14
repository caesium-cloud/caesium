//go:build integration

package test

import (
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
	build := exec.Command("go", "build", "-o", binPath, ".")
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

		resp, err := client.Get(fmt.Sprintf("%v/health", s.caesiumURL))
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
	resp, err := http.Get(fmt.Sprintf("%v/health", s.caesiumURL))
	assert.Nil(s.T(), err)
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
	cmd := exec.Command(s.cliPath, args...)
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
	resp, err := http.Get(s.caesiumURL + path)
	require.NoError(s.T(), err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(s.T(), err)

	require.Equal(s.T(), http.StatusOK, resp.StatusCode, string(body))
	require.NoError(s.T(), json.Unmarshal(body, target))
}
