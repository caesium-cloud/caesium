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

// TestAgentProfileCRUD drives the real /v1/agentprofiles REST surface
// (agent-in-the-loop-remediation Stream E2) against the live server: create,
// read, list, update, and delete, asserting on observed HTTP responses —
// not an internal service-package call.
func (s *IntegrationTestSuite) TestAgentProfileCRUD() {
	name := fmt.Sprintf("integration-agent-profile-%d", time.Now().UnixNano())

	createPayload := fmt.Sprintf(`{
		"name": %q,
		"image": "caesiumcloud/triage-agent:latest",
		"engine": "docker",
		"limits": {"cpu": "1", "memory": "512Mi"},
		"secret_refs": {"model_api_key": "secret://env/AGENT_MODEL_API_KEY"},
		"budgets": {"max_actions": 5},
		"playbook": {"autonomy": {"allow": ["retry_from_failure"]}}
	}`, name)

	resp, err := s.doJSONRequest(http.MethodPost, s.caesiumURL+"/v1/agentprofiles", strings.NewReader(createPayload))
	s.Require().NoError(err)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	s.Require().NoError(err)
	s.Require().Equal(http.StatusCreated, resp.StatusCode, string(body))

	var created struct {
		ID         string            `json:"id"`
		Name       string            `json:"name"`
		Image      string            `json:"image"`
		Engine     string            `json:"engine"`
		SecretRefs map[string]string `json:"secret_refs"`
	}
	s.Require().NoError(json.Unmarshal(body, &created))
	s.Require().NotEmpty(created.ID)
	s.Equal(name, created.Name)
	s.Equal("caesiumcloud/triage-agent:latest", created.Image)
	s.Equal("docker", created.Engine)
	s.Equal("secret://env/AGENT_MODEL_API_KEY", created.SecretRefs["model_api_key"])

	// GET returns the same profile.
	var fetched struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Image string `json:"image"`
	}
	s.getJSON("/v1/agentprofiles/"+created.ID, &fetched)
	s.Equal(created.ID, fetched.ID)
	s.Equal(name, fetched.Name)

	// LIST includes the created profile.
	var listed []struct {
		ID string `json:"id"`
	}
	s.getJSON("/v1/agentprofiles", &listed)
	found := false
	for _, p := range listed {
		if p.ID == created.ID {
			found = true
			break
		}
	}
	s.True(found, "expected created profile %s in list response", created.ID)

	// PATCH updates the image.
	updatePayload := `{"image": "caesiumcloud/triage-agent:v2"}`
	resp, err = s.doJSONRequest(http.MethodPatch, s.caesiumURL+"/v1/agentprofiles/"+created.ID, strings.NewReader(updatePayload))
	s.Require().NoError(err)
	body, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, resp.StatusCode, string(body))

	var updated struct {
		Image string `json:"image"`
	}
	s.Require().NoError(json.Unmarshal(body, &updated))
	s.Equal("caesiumcloud/triage-agent:v2", updated.Image)

	// Name conflict: creating a second profile with the same name is rejected.
	resp, err = s.doJSONRequest(http.MethodPost, s.caesiumURL+"/v1/agentprofiles", strings.NewReader(createPayload))
	s.Require().NoError(err)
	body, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	s.Require().NoError(err)
	s.Equal(http.StatusConflict, resp.StatusCode, string(body))

	// DELETE removes it.
	resp, err = s.doJSONRequest(http.MethodDelete, s.caesiumURL+"/v1/agentprofiles/"+created.ID, nil)
	s.Require().NoError(err)
	resp.Body.Close()
	s.Require().Equal(http.StatusNoContent, resp.StatusCode)

	resp, err = s.doJSONRequest(http.MethodGet, s.caesiumURL+"/v1/agentprofiles/"+created.ID, nil)
	s.Require().NoError(err)
	resp.Body.Close()
	s.Require().Equal(http.StatusNotFound, resp.StatusCode)
}

// TestAgentProfileCreateRejectsUnsupportedSecretProvider proves the server
// validates secret_refs syntactically (scheme + known provider) without ever
// resolving them.
func (s *IntegrationTestSuite) TestAgentProfileCreateRejectsUnsupportedSecretProvider() {
	name := fmt.Sprintf("integration-agent-profile-badsecret-%d", time.Now().UnixNano())
	payload := fmt.Sprintf(`{
		"name": %q,
		"image": "caesiumcloud/triage-agent:latest",
		"secret_refs": {"model_api_key": "not-a-secret-uri"}
	}`, name)

	resp, err := s.doJSONRequest(http.MethodPost, s.caesiumURL+"/v1/agentprofiles", strings.NewReader(payload))
	s.Require().NoError(err)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	s.Require().NoError(err)
	s.Require().Equal(http.StatusBadRequest, resp.StatusCode, string(body))
}

// TestJobdefLintVerifiesRemediationProfileReference exercises the
// server-side half of the metadata.remediation lint split
// (docs/design-agent-in-the-loop.md): POST /v1/jobdefs/lint has a database
// connection, so — unlike offline `caesium job lint` — it can and must
// verify that metadata.remediation.profile names a real AgentProfile.
func (s *IntegrationTestSuite) TestJobdefLintVerifiesRemediationProfileReference() {
	profileName := fmt.Sprintf("integration-lint-profile-%d", time.Now().UnixNano())
	createPayload := fmt.Sprintf(`{"name": %q, "image": "caesiumcloud/triage-agent:latest"}`, profileName)
	resp, err := s.doJSONRequest(http.MethodPost, s.caesiumURL+"/v1/agentprofiles", strings.NewReader(createPayload))
	s.Require().NoError(err)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	s.Require().NoError(err)
	s.Require().Equal(http.StatusCreated, resp.StatusCode, string(body))

	alias := fmt.Sprintf("integration-lint-remediation-%d", time.Now().UnixNano())

	validDef := fmt.Sprintf(`{"definitions":[{
		"apiVersion": "v1",
		"kind": "Job",
		"metadata": {"alias": %q, "remediation": {"profile": %q, "classes": ["unknown"]}},
		"trigger": {"type": "cron", "configuration": {"expression": "0 * * * *"}},
		"steps": [{"name": "extract", "image": "busybox:1.36.1"}]
	}]}`, alias, profileName)

	var lintResp struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	resp, err = s.doJSONRequest(http.MethodPost, s.caesiumURL+"/v1/jobdefs/lint", strings.NewReader(validDef))
	s.Require().NoError(err)
	body, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, resp.StatusCode, string(body))
	s.Require().NoError(json.Unmarshal(body, &lintResp))
	s.Empty(lintResp.Errors, "expected no lint errors for a real profile reference, got %+v", lintResp.Errors)

	missingAlias := fmt.Sprintf("integration-lint-remediation-missing-%d", time.Now().UnixNano())
	invalidDef := fmt.Sprintf(`{"definitions":[{
		"apiVersion": "v1",
		"kind": "Job",
		"metadata": {"alias": %q, "remediation": {"profile": "does-not-exist-%d", "classes": ["unknown"]}},
		"trigger": {"type": "cron", "configuration": {"expression": "0 * * * *"}},
		"steps": [{"name": "extract", "image": "busybox:1.36.1"}]
	}]}`, missingAlias, time.Now().UnixNano())

	resp, err = s.doJSONRequest(http.MethodPost, s.caesiumURL+"/v1/jobdefs/lint", strings.NewReader(invalidDef))
	s.Require().NoError(err)
	body, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, resp.StatusCode, string(body))
	s.Require().NoError(json.Unmarshal(body, &lintResp))
	s.Require().NotEmpty(lintResp.Errors, "expected a lint error for an unresolvable profile reference")
	found := false
	for _, e := range lintResp.Errors {
		if strings.Contains(e.Message, "does not reference an existing AgentProfile") {
			found = true
			break
		}
	}
	s.True(found, "expected an AgentProfile-reference error, got %+v", lintResp.Errors)
}
