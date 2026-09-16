//go:build integration

package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
)

// TestJobdefJSONDurationStringsAcceptsDocumentedSyntax drives issue #484
// through the REAL HTTP surface: the console parses YAML with js-yaml and
// POSTs JSON, so documented values like retryDelay: 1s arrive as "1s"
// strings. CLI YAML lint already accepted them; JSON apply/lint did not.
func (s *IntegrationTestSuite) TestJobdefJSONDurationStringsAcceptsDocumentedSyntax() {
	alias := fmt.Sprintf("qa-duration-retry-%d", time.Now().UnixNano())
	defJSON := fmt.Sprintf(`{
		"apiVersion": "v1",
		"kind": "Job",
		"metadata": {
			"alias": %q,
			"taskTimeout": "30s",
			"runTimeout": "1m",
			"sla": {"duration": "5m"}
		},
		"trigger": {"type": "cron", "configuration": {"cron": "0 0 1 1 *"}},
		"steps": [{
			"name": "extract",
			"image": "alpine:3.23",
			"command": ["sh", "-c", "echo ok"],
			"retries": 1,
			"retryDelay": "1s"
		}]
	}`, alias)
	payload := `{"definitions":[` + defJSON + `]}`

	var lintResp struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	lintHTTP, err := s.doJSONRequest(http.MethodPost, s.caesiumURL+"/v1/jobdefs/lint", strings.NewReader(payload))
	s.Require().NoError(err)
	lintBody, err := io.ReadAll(lintHTTP.Body)
	lintHTTP.Body.Close()
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, lintHTTP.StatusCode, string(lintBody))
	s.Require().NoError(json.Unmarshal(lintBody, &lintResp))
	s.Empty(lintResp.Errors, "lint rejected documented duration strings: %s", string(lintBody))

	applyHTTP, err := s.doJSONRequest(http.MethodPost, s.caesiumURL+"/v1/jobdefs/apply", strings.NewReader(payload))
	s.Require().NoError(err)
	applyBody, err := io.ReadAll(applyHTTP.Body)
	applyHTTP.Body.Close()
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, applyHTTP.StatusCode, "apply rejected documented duration strings: %s", string(applyBody))

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	yamlBody, contentType := s.getRaw(fmt.Sprintf("/v1/jobs/%s/manifest", job.ID))
	s.Equal("application/yaml", strings.SplitN(contentType, ";", 2)[0])
	exported, err := schema.Parse(yamlBody)
	s.Require().NoError(err, "exported YAML must parse:\n%s", yamlBody)
	s.Equal(alias, exported.Metadata.Alias)
	s.Equal(time.Second, exported.Steps[0].RetryDelay)
	s.Equal(30*time.Second, exported.Metadata.TaskTimeout)
	s.Equal(time.Minute, exported.Metadata.RunTimeout)
	s.Require().NotNil(exported.Metadata.SLA)
	s.Equal(5*time.Minute, exported.Metadata.SLA.Duration)

	jsonBody, jsonContentType := s.getRaw(fmt.Sprintf("/v1/jobs/%s/manifest?format=json", job.ID))
	s.Contains(jsonContentType, "application/json")
	var asJSON schema.Definition
	s.Require().NoError(json.Unmarshal(jsonBody, &asJSON))
	s.Equal(time.Second, asJSON.Steps[0].RetryDelay)
	s.Equal(30*time.Second, asJSON.Metadata.TaskTimeout)
	s.Equal(time.Minute, asJSON.Metadata.RunTimeout)
	s.Require().NotNil(asJSON.Metadata.SLA)
	s.Equal(5*time.Minute, asJSON.Metadata.SLA.Duration)

	reapplyPayload, err := json.Marshal(map[string]any{"definitions": []schema.Definition{asJSON}})
	s.Require().NoError(err)
	reapplyHTTP, err := s.doJSONRequest(http.MethodPost, s.caesiumURL+"/v1/jobdefs/apply", bytes.NewReader(reapplyPayload))
	s.Require().NoError(err)
	reapplyBody, err := io.ReadAll(reapplyHTTP.Body)
	reapplyHTTP.Body.Close()
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, reapplyHTTP.StatusCode, "reapply of exported JSON lost the duration: %s", string(reapplyBody))

	yamlAgain, _ := s.getRaw(fmt.Sprintf("/v1/jobs/%s/manifest", job.ID))
	again, err := schema.Parse(yamlAgain)
	s.Require().NoError(err)
	s.Equal(time.Second, again.Steps[0].RetryDelay)
	s.Equal(30*time.Second, again.Metadata.TaskTimeout)
	s.Equal(time.Minute, again.Metadata.RunTimeout)
	s.Require().NotNil(again.Metadata.SLA)
	s.Equal(5*time.Minute, again.Metadata.SLA.Duration)
}

func (s *IntegrationTestSuite) TestJobdefJSONDurationStringsRejectInvalidWithFieldName() {
	alias := fmt.Sprintf("qa-duration-retry-invalid-%d", time.Now().UnixNano())
	payload := fmt.Sprintf(`{"definitions":[{
		"apiVersion": "v1",
		"kind": "Job",
		"metadata": {"alias": %q},
		"trigger": {"type": "cron", "configuration": {"cron": "0 0 1 1 *"}},
		"steps": [{"name": "extract", "image": "alpine:3.23", "retryDelay": "not-a-duration"}]
	}]}`, alias)

	var lintResp struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	resp, err := s.doJSONRequest(http.MethodPost, s.caesiumURL+"/v1/jobdefs/lint", strings.NewReader(payload))
	s.Require().NoError(err)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, resp.StatusCode, string(body))
	s.Require().NoError(json.Unmarshal(body, &lintResp))
	s.Require().NotEmpty(lintResp.Errors, "expected a lint error for invalid retryDelay, got %s", string(body))

	joined := ""
	for _, item := range lintResp.Errors {
		joined += item.Message
	}
	s.Contains(joined, "retryDelay")
	s.Contains(joined, "duration string")
	s.NotContains(joined, "cannot unmarshal string into Go struct field")
	s.NotContains(joined, "rawStep")
}
