//go:build integration

package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/caesium-cloud/caesium/api/rest/controller/job"
	"github.com/caesium-cloud/caesium/api/rest/service/atom"
	"github.com/caesium-cloud/caesium/api/rest/service/trigger"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/assert"
)

func (s *IntegrationTestSuite) TestCronJob() {
	job := s.createJob("test_cron_job", nil)
	assert.NotNil(s.T(), job)
}

func (s *IntegrationTestSuite) TestHTTPJob() {
	job := s.createJob("test_http_job", nil)
	assert.NotNil(s.T(), job)

	u, err := url.Parse(fmt.Sprintf("%v/v1/triggers/%v", s.caesiumURL, job.TriggerID))
	assert.Nil(s.T(), err)

	resp, err := http.DefaultClient.Do(&http.Request{Method: http.MethodPut, URL: u})
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), http.StatusAccepted, resp.StatusCode)
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
}

func (s *IntegrationTestSuite) TestJobMetadataAndTasks() {
	created := s.createJob("test_job_metadata", &job.MetadataRequest{
		Labels:      map[string]string{"team": "data"},
		Annotations: map[string]string{"owner": "qa"},
	})

	s.validateCreatedMetadata(created, "data", "qa")

	detail := s.jobDetailByAlias("test_job_metadata")
	labels := detail["Labels"].(map[string]any)
	annotations := detail["Annotations"].(map[string]any)
	assert.Equal(s.T(), "data", labels["team"])
	assert.Equal(s.T(), "qa", annotations["owner"])

	tasks := s.jobTasks(detail["ID"].(string))
	assert.Len(s.T(), tasks, 2)
}

func (s *IntegrationTestSuite) TestJobListFilterByTrigger() {
	created := s.createJob("test_job_filter", nil)

	resp, err := http.Get(fmt.Sprintf("%v/v1/jobs?trigger_id=%s", s.caesiumURL, created.TriggerID.String()))
	assert.Nil(s.T(), err)
	defer resp.Body.Close()
	assert.Equal(s.T(), http.StatusOK, resp.StatusCode)

	var jobs []models.Job
	assert.Nil(s.T(), json.NewDecoder(resp.Body).Decode(&jobs))
	assert.Len(s.T(), jobs, 1)
	assert.Equal(s.T(), created.ID, jobs[0].ID)
}

func (s *IntegrationTestSuite) TestJobDeleteRemovesJob() {
	created := s.createJob("test_job_delete", nil)

	req, err := http.NewRequest(http.MethodDelete, fmt.Sprintf("%v/v1/jobs/%s", s.caesiumURL, created.ID.String()), nil)
	assert.Nil(s.T(), err)
	resp, err := http.DefaultClient.Do(req)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), http.StatusAccepted, resp.StatusCode)
	if resp.Body != nil {
		_ = resp.Body.Close()
	}

	resp, err = http.Get(fmt.Sprintf("%v/v1/jobs/%s", s.caesiumURL, created.ID.String()))
	assert.Nil(s.T(), err)
	defer resp.Body.Close()
	assert.Equal(s.T(), http.StatusNotFound, resp.StatusCode)
}

func (s *IntegrationTestSuite) TestJobApplyCommand() {
	cmd := exec.Command("caesium", "job", "apply", "--path", filepath.Join("test", "definitions"), "--server", s.caesiumURL)
	cmd.Env = append(os.Environ(), "GOFLAGS=-buildvcs=false")
	output, err := cmd.CombinedOutput()
	assert.Nil(s.T(), err, string(output))

	detail := s.jobDetailByAlias("integration-job-one")
	labels := detail["Labels"].(map[string]any)
	assert.Equal(s.T(), "test", labels["env"])

	tasks := s.jobTasks(detail["ID"].(string))
	assert.Len(s.T(), tasks, 2)
}

func (s *IntegrationTestSuite) createJob(alias string, metadata *job.MetadataRequest) *models.Job {
	req := job.PostRequest{
		Alias:    alias,
		Metadata: metadata,
		Trigger: &trigger.CreateRequest{
			Type:          string(models.TriggerTypeCron),
			Configuration: map[string]interface{}{"expression": "* * * * *"},
		},
		Tasks: []struct {
			Atom   *atom.CreateRequest `json:"atom"`
			NextID *string             `json:"next_id"`
		}{
			{Atom: &atom.CreateRequest{Engine: string(models.AtomEngineDocker), Image: "alpine", Command: []string{"/bin/sh", "-c", "echo", alias + "-1"}}},
			{Atom: &atom.CreateRequest{Engine: string(models.AtomEngineDocker), Image: "alpine", Command: []string{"/bin/sh", "-c", "echo", alias + "-2"}}},
		},
	}

	buf, err := json.Marshal(req)
	assert.Nil(s.T(), err)

	resp, err := http.Post(fmt.Sprintf("%v/v1/jobs", s.caesiumURL), "application/json", bytes.NewBuffer(buf))
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), http.StatusCreated, resp.StatusCode)
	defer resp.Body.Close()

	var created models.Job
	assert.Nil(s.T(), json.NewDecoder(resp.Body).Decode(&created))
	return &created
}

func (s *IntegrationTestSuite) jobDetailByAlias(alias string) map[string]any {
	resp, err := http.Get(fmt.Sprintf("%v/v1/jobs", s.caesiumURL))
	assert.Nil(s.T(), err)
	defer resp.Body.Close()
	assert.Equal(s.T(), http.StatusOK, resp.StatusCode)

	var jobs []map[string]any
	assert.Nil(s.T(), json.NewDecoder(resp.Body).Decode(&jobs))
	for _, job := range jobs {
		if job["alias"].(string) == alias {
			resp, err := http.Get(fmt.Sprintf("%v/v1/jobs/%s", s.caesiumURL, job["id"].(string)))
			assert.Nil(s.T(), err)
			defer resp.Body.Close()
			assert.Equal(s.T(), http.StatusOK, resp.StatusCode)
			var detail map[string]any
			assert.Nil(s.T(), json.NewDecoder(resp.Body).Decode(&detail))
			return detail
		}
	}
	s.T().Fatalf("job with alias %s not found", alias)
	return nil
}

func (s *IntegrationTestSuite) jobTasks(jobID string) []map[string]any {
	resp, err := http.Get(fmt.Sprintf("%v/v1/jobs/%s/tasks", s.caesiumURL, jobID))
	assert.Nil(s.T(), err)
	defer resp.Body.Close()
	assert.Equal(s.T(), http.StatusOK, resp.StatusCode)
	var tasks []map[string]any
	assert.Nil(s.T(), json.NewDecoder(resp.Body).Decode(&tasks))
	return tasks
}

func (s *IntegrationTestSuite) validateCreatedMetadata(job *models.Job, label, annotation string) {
	if job.Labels != nil {
		if team, ok := job.Labels["team"].(string); label != "" && ok {
			assert.Equal(s.T(), label, team)
		}
	}
	if job.Annotations != nil {
		if owner, ok := job.Annotations["owner"].(string); annotation != "" && ok {
			assert.Equal(s.T(), annotation, owner)
		}
	}
}
