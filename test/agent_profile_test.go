//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// sharedNSProfileView is the slice of the /v1/agentprofiles wire shape the
// namespace scenarios assert on.
type sharedNSProfileView struct {
	ID        string `json:"id"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Image     string `json:"image"`
}

// sharedNSRequest issues a JSON request against the live server and returns
// the status plus the raw body.
func (s *IntegrationTestSuite) sharedNSRequest(method, path, body string) (int, string) {
	s.T().Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	resp, err := s.doJSONRequest(method, s.caesiumURL+path, reader)
	s.Require().NoError(err)
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	return resp.StatusCode, string(out)
}

// sharedNSDelete removes a fixture without asserting, so it is safe in a
// deferred cleanup that runs after a failed assertion.
func (s *IntegrationTestSuite) sharedNSDelete(path string) {
	if resp, err := s.doJSONRequest(http.MethodDelete, s.caesiumURL+path, nil); err == nil {
		resp.Body.Close()
	}
}

func (s *IntegrationTestSuite) createSharedNSProfile(name, namespace string) sharedNSProfileView {
	s.T().Helper()
	payload := fmt.Sprintf(`{"name": %q, "image": "caesiumcloud/triage-agent:latest"}`, name)
	if namespace != "" {
		payload = fmt.Sprintf(`{"namespace": %q, "name": %q, "image": "caesiumcloud/triage-agent:latest"}`, namespace, name)
	}
	status, body := s.sharedNSRequest(http.MethodPost, "/v1/agentprofiles", payload)
	s.Require().Equal(http.StatusCreated, status, body)
	var created sharedNSProfileView
	s.Require().NoError(json.Unmarshal([]byte(body), &created), body)
	s.Require().NotEmpty(created.ID)
	return created
}

// TestAgentProfileNamespaceCreateOnly drives the tenancy namespace on the real
// /v1/agentprofiles surface and the `caesium agentprofile` CLI
// (identity-and-access D3): create accepts it (blank = default, "*" =
// cluster-shared, otherwise a DNS label), every read returns it, and an update
// may echo but never change it.
func (s *IntegrationTestSuite) TestAgentProfileNamespaceCreateOnly() {
	stamp := time.Now().UnixNano()
	var created []string
	defer func() {
		for _, id := range created {
			s.sharedNSDelete("/v1/agentprofiles/" + id)
		}
	}()

	marketing := s.createSharedNSProfile(fmt.Sprintf("integration-ns-profile-mkt-%d", stamp), "marketing")
	created = append(created, marketing.ID)
	s.Equal("marketing", marketing.Namespace)

	var fetched sharedNSProfileView
	s.getJSON("/v1/agentprofiles/"+marketing.ID, &fetched)
	s.Equal("marketing", fetched.Namespace)
	var listed []sharedNSProfileView
	s.getJSON("/v1/agentprofiles?limit=1000", &listed)
	found := false
	for _, p := range listed {
		if p.ID == marketing.ID {
			found = true
			s.Equal("marketing", p.Namespace)
		}
		s.NotEmpty(p.Namespace, "every listed profile carries a namespace: %+v", p)
	}
	s.True(found, "expected %s in the profile list", marketing.ID)

	omitted := s.createSharedNSProfile(fmt.Sprintf("integration-ns-profile-default-%d", stamp), "")
	created = append(created, omitted.ID)
	s.Equal("default", omitted.Namespace, "an omitted namespace means default, never shared")

	shared := s.createSharedNSProfile(fmt.Sprintf("integration-ns-profile-shared-%d", stamp), "*")
	created = append(created, shared.ID)
	s.Equal("*", shared.Namespace)

	status, body := s.sharedNSRequest(http.MethodPost, "/v1/agentprofiles",
		fmt.Sprintf(`{"namespace": "Bad_NS", "name": "integration-ns-profile-bad-%d", "image": "img"}`, stamp))
	s.Require().Equal(http.StatusBadRequest, status, body)

	// Echoing the stored namespace is accepted; the rest of the patch applies.
	status, body = s.sharedNSRequest(http.MethodPatch, "/v1/agentprofiles/"+marketing.ID,
		`{"namespace": "marketing", "image": "caesiumcloud/triage-agent:v2"}`)
	s.Require().Equal(http.StatusOK, status, body)
	var patched sharedNSProfileView
	s.Require().NoError(json.Unmarshal([]byte(body), &patched), body)
	s.Equal("caesiumcloud/triage-agent:v2", patched.Image)
	s.Equal("marketing", patched.Namespace)

	// Changing it is refused with the fixed create-only message, and nothing
	// in the rejected patch is written.
	status, body = s.sharedNSRequest(http.MethodPatch, "/v1/agentprofiles/"+marketing.ID,
		`{"namespace": "finance", "image": "caesiumcloud/triage-agent:v3"}`)
	s.Require().Equal(http.StatusBadRequest, status, body)
	s.Contains(body, "create-only")
	s.getJSON("/v1/agentprofiles/"+marketing.ID, &fetched)
	s.Equal("marketing", fetched.Namespace)
	s.Equal("caesiumcloud/triage-agent:v2", fetched.Image)

	// CLI: a profile file's namespace is forwarded (not silently dropped), and
	// the --json output on stdout carries it.
	cliName := fmt.Sprintf("integration-ns-profile-cli-%d", stamp)
	path := filepath.Join(s.T().TempDir(), "profile.yaml")
	s.Require().NoError(os.WriteFile(path, []byte(fmt.Sprintf("namespace: marketing\nname: %s\nimage: caesiumcloud/triage-agent:latest\n", cliName)), 0o600))
	authArgs := []string{"--server", s.caesiumURL}
	if key := firstEnv("CAESIUM_AGENTPROFILE_API_KEY", "CAESIUM_API_KEY", "CAESIUM_E2E_AUTH_ADMIN_KEY"); key != "" {
		authArgs = append(authArgs, "--api-key", key)
	}
	out, err := s.runCLIStdout(append([]string{"agentprofile", "apply", "--path", path, "--json"}, authArgs...)...)
	s.Require().NoError(err, "caesium agentprofile apply --json failed:\n%s", out)
	s.Require().True(json.Valid([]byte(out)), "caesium agentprofile apply --json stdout was not clean JSON:\n%s", out)
	var applied sharedNSProfileView
	s.Require().NoError(json.Unmarshal([]byte(out), &applied))
	s.Require().NotEmpty(applied.ID)
	created = append(created, applied.ID)
	s.Equal(cliName, applied.Name)
	s.Equal("marketing", applied.Namespace)

	out, err = s.runCLIStdout(append([]string{"agentprofile", "get", applied.ID, "--json"}, authArgs...)...)
	s.Require().NoError(err, "caesium agentprofile get --json failed:\n%s", out)
	s.Require().True(json.Valid([]byte(out)), "caesium agentprofile get --json stdout was not clean JSON:\n%s", out)
	var got sharedNSProfileView
	s.Require().NoError(json.Unmarshal([]byte(out), &got))
	s.Equal("marketing", got.Namespace)
}

// TestAgentProfileLintRejectsCrossNamespaceReferences drives the server-side
// half of the D3 lint rule through POST /v1/jobdefs/lint and /v1/jobdefs/apply:
// a job may reference an AgentProfile or escalation channel only in its own
// namespace or the cluster-shared namespace "*".
func (s *IntegrationTestSuite) TestAgentProfileLintRejectsCrossNamespaceReferences() {
	stamp := time.Now().UnixNano()
	finProfile := s.createSharedNSProfile(fmt.Sprintf("integration-ns-lint-fin-%d", stamp), "finance")
	defer s.sharedNSDelete("/v1/agentprofiles/" + finProfile.ID)
	sharedProfile := s.createSharedNSProfile(fmt.Sprintf("integration-ns-lint-shared-%d", stamp), "*")
	defer s.sharedNSDelete("/v1/agentprofiles/" + sharedProfile.ID)

	channelName := fmt.Sprintf("integration-ns-lint-chan-fin-%d", stamp)
	status, body := s.sharedNSRequest(http.MethodPost, "/v1/notifications/channels",
		fmt.Sprintf(`{"namespace":"finance","name":%q,"type":"webhook","config":{"url":"https://example.invalid/hooks/ns-lint"}}`, channelName))
	s.Require().Equal(http.StatusCreated, status, body)
	var channel struct {
		ID string `json:"id"`
	}
	s.Require().NoError(json.Unmarshal([]byte(body), &channel), body)
	defer s.sharedNSDelete("/v1/notifications/channels/" + channel.ID)
	// POST ignores enabled:false; disable explicitly so nothing can deliver.
	status, body = s.sharedNSRequest(http.MethodPatch, "/v1/notifications/channels/"+channel.ID, `{"enabled":false}`)
	s.Require().Equal(http.StatusOK, status, body)

	definition := func(alias, namespace, profile, escalationChannel string) string {
		remediation := fmt.Sprintf(`{"profile": %q, "classes": ["unknown"]}`, profile)
		if escalationChannel != "" {
			remediation = fmt.Sprintf(`{"profile": %q, "classes": ["unknown"], "escalation": {"channel": %q, "after": "15m"}}`, profile, escalationChannel)
		}
		return fmt.Sprintf(`{"definitions":[{
		"apiVersion": "v1",
		"kind": "Job",
		"metadata": {"alias": %q, "namespace": %q, "remediation": %s},
		"trigger": {"type": "cron", "configuration": {"expression": "0 * * * *"}},
		"steps": [{"name": "extract", "image": "busybox:1.36.1"}]
	}]}`, alias, namespace, remediation)
	}
	lint := func(payload string) []string {
		s.T().Helper()
		status, body := s.sharedNSRequest(http.MethodPost, "/v1/jobdefs/lint", payload)
		s.Require().Equal(http.StatusOK, status, body)
		var resp struct {
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		s.Require().NoError(json.Unmarshal([]byte(body), &resp), body)
		messages := make([]string, 0, len(resp.Errors))
		for _, e := range resp.Errors {
			messages = append(messages, e.Message)
		}
		return messages
	}

	crossAlias := fmt.Sprintf("integration-ns-lint-cross-%d", stamp)
	crossDef := definition(crossAlias, "marketing", finProfile.Name, "")
	errs := lint(crossDef)
	s.Require().Len(errs, 1, "a marketing job referencing a finance profile must fail lint: %v", errs)
	s.Contains(errs[0], finProfile.Name)
	s.Contains(errs[0], `"finance"`)
	s.Contains(errs[0], `"marketing"`)
	crossErr := errs[0]

	s.Empty(lint(definition(fmt.Sprintf("integration-ns-lint-shared-%d", stamp), "marketing", sharedProfile.Name, "")), "a shared profile is visible from every namespace")
	s.Empty(lint(definition(fmt.Sprintf("integration-ns-lint-same-%d", stamp), "finance", finProfile.Name, "")), "a same-namespace profile is visible")

	errs = lint(definition(fmt.Sprintf("integration-ns-lint-chan-%d", stamp), "marketing", sharedProfile.Name, channelName))
	s.Require().Len(errs, 1, "a marketing job escalating to a finance channel must fail lint: %v", errs)
	s.Contains(errs[0], channelName)
	s.Contains(errs[0], `"finance"`)
	s.Contains(errs[0], "metadata.remediation.escalation.channel")

	// Apply runs the same check (Importer.ValidateBatch) and creates nothing.
	status, body = s.sharedNSRequest(http.MethodPost, "/v1/jobdefs/apply", crossDef)
	s.Require().Equal(http.StatusBadRequest, status, body)
	var refusal struct {
		Message string `json:"message"`
	}
	s.Require().NoError(json.Unmarshal([]byte(body), &refusal), body)
	s.Equal(crossErr, refusal.Message, "apply reports the same error lint does")
	s.Contains(refusal.Message, finProfile.Name)
	s.Contains(refusal.Message, `"finance"`, "the apply error names the object's namespace")
	var jobs []jobSummary
	s.getJSON("/v1/jobs", &jobs)
	for _, job := range jobs {
		s.NotEqual(crossAlias, job.Alias, "a rejected apply must not create the job")
	}
}

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
// (docs/design/agent-in-the-loop.md): POST /v1/jobdefs/lint has a database
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
