//go:build integration

package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"

	"github.com/caesium-cloud/caesium/api/rest/controller/job"
	"github.com/caesium-cloud/caesium/api/rest/service/atom"
	"github.com/caesium-cloud/caesium/api/rest/service/trigger"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/assert"
)

func (s *IntegrationTestSuite) TestCronJob() {
	req := job.PostRequest{
		Alias: "test_cron_job",
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

func (s *IntegrationTestSuite) TestHTTPJob() {
	// create job
	req := job.PostRequest{
		Alias: "test_http_job",
		Trigger: &trigger.CreateRequest{
			Type:          string(models.TriggerTypeHTTP),
			Configuration: map[string]interface{}{},
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

	// trigger job
	u, err := url.Parse(fmt.Sprintf(
		"%v/v1/triggers/%v",
		s.caesiumURL,
		j.TriggerID,
	))

	assert.Nil(s.T(), err)

	resp, err = http.DefaultClient.Do(&http.Request{
		Method: http.MethodPut,
		URL:    u,
	})
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), http.StatusAccepted, resp.StatusCode)
}
