//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func (s *IntegrationTestSuite) TestJobdefLintSummaryReportsStepCount() {
	alias := fmt.Sprintf("integration-lint-summary-%d", time.Now().UnixNano())
	payload := fmt.Sprintf(`{"definitions":[{
		"apiVersion": "v1",
		"kind": "Job",
		"metadata": {"alias": %q},
		"trigger": {"type": "cron", "configuration": {"expression": "0 * * * *"}},
		"steps": [
			{"name": "extract", "image": "busybox:1.36.1"},
			{"name": "load", "image": "busybox:1.36.1", "dependsOn": ["extract"]}
		]
	}]}`, alias)

	var lintResp struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
		Summary struct {
			Steps     int    `json:"steps"`
			Contracts string `json:"contracts"`
		} `json:"summary"`
	}

	resp, err := s.doJSONRequest(http.MethodPost, s.caesiumURL+"/v1/jobdefs/lint", strings.NewReader(payload))
	s.Require().NoError(err)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, resp.StatusCode, string(body))
	s.Require().NoError(json.Unmarshal(body, &lintResp))
	s.Empty(lintResp.Errors, "expected no lint errors, got %+v", lintResp.Errors)
	s.Equal(2, lintResp.Summary.Steps)
	s.Empty(lintResp.Summary.Contracts)
}

func (s *IntegrationTestSuite) TestJobdefLintRejectsSteplessDefinition() {
	alias := fmt.Sprintf("integration-lint-stepless-%d", time.Now().UnixNano())
	payload := fmt.Sprintf(`{"definitions":[{
		"apiVersion": "v1",
		"kind": "Job",
		"metadata": {"alias": %q},
		"trigger": {"type": "cron", "configuration": {"expression": "0 * * * *"}},
		"steps": []
	}]}`, alias)

	var lintResp struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
		Summary struct {
			Steps int `json:"steps"`
		} `json:"summary"`
	}

	resp, err := s.doJSONRequest(http.MethodPost, s.caesiumURL+"/v1/jobdefs/lint", strings.NewReader(payload))
	s.Require().NoError(err)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, resp.StatusCode, string(body))
	s.Require().NoError(json.Unmarshal(body, &lintResp))
	s.Equal(0, lintResp.Summary.Steps)
	s.Require().NotEmpty(lintResp.Errors, "expected a lint error for a stepless definition")

	found := false
	for _, err := range lintResp.Errors {
		if strings.Contains(err.Message, "steps must contain at least one entry") {
			found = true
			break
		}
	}
	s.True(found, "expected stepless validation error, got %+v", lintResp.Errors)
}
