//go:build integration

package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"

	"github.com/caesium-cloud/caesium/api/rest/controller/job"
	"github.com/caesium-cloud/caesium/api/rest/service/atom"
	"github.com/caesium-cloud/caesium/api/rest/service/trigger"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/assert"
)

func (s *IntegrationTestSuite) TestJob() {
	// create a job
	req := job.PostRequest{
		Alias: "test_job",
		Trigger: &trigger.CreateRequest{
			Type: string(models.TriggerTypeCron),
			Configuration: map[string]interface{}{
				"expression": "* * * * *",
			},
		},
		Tasks: []struct {
			Atom   *atom.CreateRequest `json:"atom"`
			NextID *string             `json:"next_id"`
		}{
			{
				Atom: &atom.CreateRequest{
					Engine:  string(models.AtomEngineDocker),
					Image:   "alpine",
					Command: []string{"/bin/sh", "-c", "echo", "$USER"},
				},
			},
		},
	}

	buf, err := json.Marshal(req)
	assert.Nil(s.T(), err)

	resp, err := http.Post(
		fmt.Sprintf("%v/v1/jobs", s.caesiumURL),
		"application/json",
		bytes.NewBuffer(buf),
	)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), http.StatusCreated, resp.StatusCode)

	j := &models.Job{}

	buf, err = ioutil.ReadAll(resp.Body)
	assert.Nil(s.T(), err)

	assert.Nil(s.T(), json.Unmarshal(buf, j))
	assert.NotNil(s.T(), j)
}
