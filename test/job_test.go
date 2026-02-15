//go:build integration

package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

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
	job := s.createJobWithTrigger("test_http_job", nil, models.TriggerTypeHTTP)
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
	labels := mapFromMap(detail, "labels")
	annotations := mapFromMap(detail, "annotations")
	assert.Equal(s.T(), "data", labels["team"])
	assert.Equal(s.T(), "qa", annotations["owner"])

	tasks := s.jobTasks(stringFromMap(detail, "id"))
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
	cmd := exec.Command(s.cliPath, "job", "apply", "--path", filepath.Join("test", "definitions"), "--server", s.caesiumURL)
	cmd.Dir = s.projectRoot
	output, err := cmd.CombinedOutput()
	assert.Nil(s.T(), err, string(output))

	detail := s.jobDetailByAlias("integration-job-one")
	labels := mapFromMap(detail, "labels")
	assert.Equal(s.T(), "test", labels["env"])

	tasks := s.jobTasks(stringFromMap(detail, "id"))
	assert.Len(s.T(), tasks, 2)

	dagDetail := s.jobDetailByAlias("integration-job-two")
	dagID := stringFromMap(dagDetail, "id")
	dagTasks := s.jobTasks(dagID)
	assert.Len(s.T(), dagTasks, 3)
	expectedLinear := map[string][]string{
		"build":   []string{"test"},
		"test":    []string{"package"},
		"package": []string{},
	}
	assert.Equal(s.T(), expectedLinear, s.describeDAG(dagID))

	branchDetail := s.jobDetailByAlias("integration-job-dag")
	branchID := stringFromMap(branchDetail, "id")
	branchTasks := s.jobTasks(branchID)
	assert.Len(s.T(), branchTasks, 4)
	expectedBranch := map[string][]string{
		"start":    []string{"fanout-a", "fanout-b"},
		"fanout-a": []string{"join"},
		"fanout-b": []string{"join"},
		"join":     []string{},
	}
	assert.Equal(s.T(), expectedBranch, s.describeDAG(branchID))
}

func (s *IntegrationTestSuite) createJob(alias string, metadata *job.MetadataRequest) *models.Job {
	return s.createJobWithTrigger(alias, metadata, models.TriggerTypeCron)
}

func (s *IntegrationTestSuite) createJobWithTrigger(alias string, metadata *job.MetadataRequest, trigType models.TriggerType) *models.Job {
	config := map[string]any{}
	switch trigType {
	case models.TriggerTypeCron:
		config["expression"] = "* * * * *"
	case models.TriggerTypeHTTP:
		config["path"] = fmt.Sprintf("/jobs/%s", alias)
	default:
		config["expression"] = "* * * * *"
	}

	req := job.PostRequest{
		Alias:    alias,
		Metadata: metadata,
		Trigger: &trigger.CreateRequest{
			Type:          string(trigType),
			Configuration: config,
		},
		Tasks: []job.TaskRequest{
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
		aliasVal := stringFromMap(job, "alias")
		if aliasVal == alias {
			id := stringFromMap(job, "id")
			resp, err := http.Get(fmt.Sprintf("%v/v1/jobs/%s", s.caesiumURL, id))
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

func stringFromMap(m map[string]any, key string) string {
	if v := valueFromMap(m, key); v != nil {
		if str, ok := v.(string); ok {
			return str
		}
	}
	return ""
}

func mapFromMap(m map[string]any, key string) map[string]any {
	if v := valueFromMap(m, key); v != nil {
		if mm, ok := convertToStringMap(v); ok {
			return mm
		}
	}
	return map[string]any{}
}

func valueFromMap(m map[string]any, key string) any {
	if v, ok := m[key]; ok {
		return v
	}
	for k, v := range m {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return nil
}

func convertToStringMap(v any) (map[string]any, bool) {
	if mm, ok := v.(map[string]any); ok {
		return mm, true
	}
	mi, ok := v.(map[string]interface{})
	if !ok {
		return nil, false
	}
	out := make(map[string]any, len(mi))
	for k, val := range mi {
		out[k] = val
	}
	return out, true
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

func (s *IntegrationTestSuite) atomDetail(atomID string) map[string]any {
	resp, err := http.Get(fmt.Sprintf("%v/v1/atoms/%s", s.caesiumURL, atomID))
	assert.Nil(s.T(), err)
	defer resp.Body.Close()
	assert.Equal(s.T(), http.StatusOK, resp.StatusCode)
	var detail map[string]any
	assert.Nil(s.T(), json.NewDecoder(resp.Body).Decode(&detail))
	return detail
}

func (s *IntegrationTestSuite) describeDAG(jobID string) map[string][]string {
	resp, err := http.Get(fmt.Sprintf("%v/v1/jobs/%s/dag", s.caesiumURL, jobID))
	assert.Nil(s.T(), err)
	defer resp.Body.Close()
	assert.Equal(s.T(), http.StatusOK, resp.StatusCode)

	var dag struct {
		Nodes []struct {
			ID         string   `json:"id"`
			AtomID     string   `json:"atom_id"`
			Successors []string `json:"successors"`
		} `json:"nodes"`
	}
	assert.Nil(s.T(), json.NewDecoder(resp.Body).Decode(&dag))

	nodes := make(map[string]struct {
		AtomID     string
		Successors []string
	}, len(dag.Nodes))

	for _, node := range dag.Nodes {
		nodes[node.ID] = struct {
			AtomID     string
			Successors []string
		}{
			AtomID:     node.AtomID,
			Successors: append([]string(nil), node.Successors...),
		}
	}

	commandCache := make(map[string]string)
	commandFor := func(atomID string) string {
		if cmd, ok := commandCache[atomID]; ok {
			return cmd
		}
		atom := s.atomDetail(atomID)
		cmd := lastCommandWord(atom)
		commandCache[atomID] = cmd
		return cmd
	}

	transitions := make(map[string][]string, len(dag.Nodes))
	for _, node := range nodes {
		step := commandFor(node.AtomID)
		nextSteps := make([]string, 0, len(node.Successors))
		for _, successorID := range node.Successors {
			successor, ok := nodes[successorID]
			if !ok {
				continue
			}
			nextSteps = append(nextSteps, commandFor(successor.AtomID))
		}
		if len(nextSteps) > 1 {
			sort.Strings(nextSteps)
		}
		transitions[step] = nextSteps
	}

	return transitions
}

func lastCommandWord(atom map[string]any) string {
	raw := valueFromMap(atom, "command")
	if raw == nil {
		return ""
	}
	var parts []string
	switch cmd := raw.(type) {
	case []any:
		for _, v := range cmd {
			if str, ok := v.(string); ok {
				parts = append(parts, str)
			}
		}
	case []string:
		parts = append(parts, cmd...)
	case string:
		cmd = strings.TrimSpace(cmd)
		if cmd == "" {
			break
		}
		if strings.HasPrefix(cmd, "[") {
			var arr []string
			if err := json.Unmarshal([]byte(cmd), &arr); err == nil {
				parts = append(parts, arr...)
				break
			}
		}
		parts = append(parts, cmd)
	}
	if len(parts) == 0 {
		return ""
	}
	last := strings.TrimSpace(parts[len(parts)-1])
	if last == "" {
		return ""
	}
	fields := strings.Fields(last)
	if len(fields) == 0 {
		return last
	}
	return fields[len(fields)-1]
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
