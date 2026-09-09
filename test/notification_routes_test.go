//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Stream C6 — the by-id half of the notification surface. The list and create
// routes were already driven by the replay-matrix scenario, but
// GET/PATCH/DELETE on /v1/notifications/channels/:id and
// /v1/notifications/policies/:id had no integration coverage: a handler that
// returned the wrong record, ignored the patch, or soft-deleted nothing would
// have shipped green.

type notificationChannelView struct {
	ID      string         `json:"id"`
	Name    string         `json:"name"`
	Type    string         `json:"type"`
	Config  map[string]any `json:"config"`
	Enabled bool           `json:"enabled"`
}

type notificationPolicyView struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	ChannelID  string          `json:"channel_id"`
	EventTypes json.RawMessage `json:"event_types"`
	Filters    json.RawMessage `json:"filters"`
	Enabled    bool            `json:"enabled"`
}

// notificationRequest issues a request against the notification surface and
// returns the status plus the raw body.
func (s *IntegrationTestSuite) notificationRequest(method, path, body string) (int, string) {
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

func (s *IntegrationTestSuite) TestNotificationChannelAndPolicyByID() {
	stamp := time.Now().UnixNano()
	channelName := fmt.Sprintf("integration-notif-channel-%d", stamp)
	policyName := fmt.Sprintf("integration-notif-policy-%d", stamp)
	const secretURL = "https://example.invalid/hooks/integration-secret-path"

	// --- channel: create -------------------------------------------------
	status, body := s.notificationRequest(http.MethodPost, "/v1/notifications/channels",
		fmt.Sprintf(`{"name":%q,"type":"webhook","config":{"url":%q},"enabled":true}`, channelName, secretURL))
	s.Require().Equal(http.StatusCreated, status, body)

	var channel notificationChannelView
	s.Require().NoError(json.Unmarshal([]byte(body), &channel), body)
	s.Require().NotEmpty(channel.ID)

	// --- channel: get by id ----------------------------------------------
	status, body = s.notificationRequest(http.MethodGet, "/v1/notifications/channels/"+channel.ID, "")
	s.Require().Equal(http.StatusOK, status, body)

	var fetched notificationChannelView
	s.Require().NoError(json.Unmarshal([]byte(body), &fetched), body)
	s.Equal(channel.ID, fetched.ID)
	s.Equal(channelName, fetched.Name)
	s.Equal("webhook", fetched.Type)
	s.True(fetched.Enabled)
	// The webhook URL is a redacted config key: the by-id read must mask it the
	// same way the list read does, or a secret leaks through the narrower route.
	s.NotContains(body, secretURL, "the channel URL must be redacted on the by-id read")
	s.Contains(fmt.Sprint(fetched.Config["url"]), "****")

	// --- channel: patch --------------------------------------------------
	renamed := channelName + "-renamed"
	status, body = s.notificationRequest(http.MethodPatch, "/v1/notifications/channels/"+channel.ID,
		fmt.Sprintf(`{"name":%q,"enabled":false}`, renamed))
	s.Require().Equal(http.StatusOK, status, body)

	var patched notificationChannelView
	s.Require().NoError(json.Unmarshal([]byte(body), &patched), body)
	s.Equal(renamed, patched.Name)
	s.False(patched.Enabled, "PATCH must persist enabled=false")

	status, body = s.notificationRequest(http.MethodGet, "/v1/notifications/channels/"+channel.ID, "")
	s.Require().Equal(http.StatusOK, status, body)
	s.Require().NoError(json.Unmarshal([]byte(body), &fetched), body)
	s.Equal(renamed, fetched.Name, "the patch must survive a re-read, not just echo back")
	s.False(fetched.Enabled)

	// --- policy: create --------------------------------------------------
	// The policy is filtered to a job id that does not exist (and its channel
	// is already disabled by the patch above), so nothing this scenario creates
	// can fire a delivery at another test's run on the shared server.
	status, body = s.notificationRequest(http.MethodPost, "/v1/notifications/policies",
		fmt.Sprintf(`{"name":%q,"channel_id":%q,"event_types":["run_failed"],"filters":{"job_ids":[%q]},"enabled":true}`,
			policyName, channel.ID, uuid.NewString()))
	s.Require().Equal(http.StatusCreated, status, body)

	var policy notificationPolicyView
	s.Require().NoError(json.Unmarshal([]byte(body), &policy), body)
	s.Require().NotEmpty(policy.ID)

	// --- policy: get by id -----------------------------------------------
	status, body = s.notificationRequest(http.MethodGet, "/v1/notifications/policies/"+policy.ID, "")
	s.Require().Equal(http.StatusOK, status, body)

	var fetchedPolicy notificationPolicyView
	s.Require().NoError(json.Unmarshal([]byte(body), &fetchedPolicy), body)
	s.Equal(policy.ID, fetchedPolicy.ID)
	s.Equal(policyName, fetchedPolicy.Name)
	s.Equal(channel.ID, fetchedPolicy.ChannelID)
	s.JSONEq(`["run_failed"]`, string(fetchedPolicy.EventTypes))

	// --- policy: patch ---------------------------------------------------
	status, body = s.notificationRequest(http.MethodPatch, "/v1/notifications/policies/"+policy.ID,
		`{"event_types":["run_failed","run_completed"],"enabled":false}`)
	s.Require().Equal(http.StatusOK, status, body)

	status, body = s.notificationRequest(http.MethodGet, "/v1/notifications/policies/"+policy.ID, "")
	s.Require().Equal(http.StatusOK, status, body)
	s.Require().NoError(json.Unmarshal([]byte(body), &fetchedPolicy), body)
	s.JSONEq(`["run_failed","run_completed"]`, string(fetchedPolicy.EventTypes))
	s.False(fetchedPolicy.Enabled)

	// --- deletes ---------------------------------------------------------
	// The policy first: it references the channel.
	status, body = s.notificationRequest(http.MethodDelete, "/v1/notifications/policies/"+policy.ID, "")
	s.Require().Equal(http.StatusNoContent, status, body)

	status, body = s.notificationRequest(http.MethodGet, "/v1/notifications/policies/"+policy.ID, "")
	s.Equal(http.StatusNotFound, status, "a deleted policy must not be readable: %s", body)

	status, body = s.notificationRequest(http.MethodDelete, "/v1/notifications/channels/"+channel.ID, "")
	s.Require().Equal(http.StatusNoContent, status, body)

	status, body = s.notificationRequest(http.MethodGet, "/v1/notifications/channels/"+channel.ID, "")
	s.Equal(http.StatusNotFound, status, "a deleted channel must not be readable: %s", body)

	// --- unknown ids ------------------------------------------------------
	const unknownID = "00000000-0000-0000-0000-000000000000"
	status, body = s.notificationRequest(http.MethodGet, "/v1/notifications/channels/"+unknownID, "")
	s.Equal(http.StatusNotFound, status, body)
	status, body = s.notificationRequest(http.MethodGet, "/v1/notifications/policies/"+unknownID, "")
	s.Equal(http.StatusNotFound, status, body)

	// A malformed id is a 400, not a 500.
	status, body = s.notificationRequest(http.MethodGet, "/v1/notifications/channels/not-a-uuid", "")
	s.Equal(http.StatusBadRequest, status, body)
}

// TestNotificationChannelAndPolicyMutationsAreAudited drives the same
// create/update/delete surface as TestNotificationChannelAndPolicyByID and
// asserts each mutation lands an entry in the audit spine (issue #413):
// before this, an operator disabling or deleting a paging channel/policy left
// no forensic trail at all. It extends test/auth_keys_test.go's
// TestAuthKeysREST/TestAuthAuditCLI pattern (same auditEntry type,
// auditContainsResource helper, GET /v1/auth/audit surface) to the
// notification routes.
//
// Requires the auth lane: GET /v1/auth/audit is only mounted when
// CAESIUM_AUTH_MODE=api-key (see api/rest/bind.All), same as the existing
// audit assertions in test/auth_keys_test.go.
func (s *IntegrationTestSuite) TestNotificationChannelAndPolicyMutationsAreAudited() {
	s.requireAuthLane()
	s.Require().NotEmpty(s.authAPIKey,
		"the auth lane must export an admin key (CAESIUM_API_KEY / CAESIUM_AUTH_ADMIN_KEY) to the runner")

	stamp := time.Now().UnixNano()
	channelName := fmt.Sprintf("integration-notif-audit-channel-%d", stamp)
	policyName := fmt.Sprintf("integration-notif-audit-policy-%d", stamp)

	// --- channel: create, patch, delete -----------------------------------
	status, body := s.notificationRequest(http.MethodPost, "/v1/notifications/channels",
		fmt.Sprintf(`{"name":%q,"type":"webhook","config":{"url":"https://example.invalid/hooks/audit"},"enabled":true}`, channelName))
	s.Require().Equal(http.StatusCreated, status, body)
	var channel notificationChannelView
	s.Require().NoError(json.Unmarshal([]byte(body), &channel), body)

	status, body = s.notificationRequest(http.MethodPatch, "/v1/notifications/channels/"+channel.ID,
		`{"enabled":false}`)
	s.Require().Equal(http.StatusOK, status, body)

	// Filtered to a job id that does not exist, same as
	// TestNotificationChannelAndPolicyByID above: nothing this scenario
	// creates can fire a delivery at another test's run on the shared server.
	status, body = s.notificationRequest(http.MethodPost, "/v1/notifications/policies",
		fmt.Sprintf(`{"name":%q,"channel_id":%q,"event_types":["run_failed"],"filters":{"job_ids":[%q]},"enabled":true}`,
			policyName, channel.ID, uuid.NewString()))
	s.Require().Equal(http.StatusCreated, status, body)
	var policy notificationPolicyView
	s.Require().NoError(json.Unmarshal([]byte(body), &policy), body)

	status, body = s.notificationRequest(http.MethodPatch, "/v1/notifications/policies/"+policy.ID,
		`{"event_types":["run_failed","run_completed"]}`)
	s.Require().Equal(http.StatusOK, status, body)

	status, body = s.notificationRequest(http.MethodDelete, "/v1/notifications/policies/"+policy.ID, "")
	s.Require().Equal(http.StatusNoContent, status, body)

	status, body = s.notificationRequest(http.MethodDelete, "/v1/notifications/channels/"+channel.ID, "")
	s.Require().Equal(http.StatusNoContent, status, body)

	// --- audit assertions --------------------------------------------------
	assertAudited := func(action, resourceID, resourceType string) {
		s.T().Helper()
		status, body := s.notificationRequest(http.MethodGet,
			fmt.Sprintf("/v1/auth/audit?action=%s&limit=200", action), "")
		s.Require().Equal(http.StatusOK, status, body)

		var entries []auditEntry
		s.Require().NoError(json.Unmarshal([]byte(body), &entries), body)
		s.Require().NotEmpty(entries, "GET /v1/auth/audit?action=%s returned no entries:\n%s", action, body)

		var found *auditEntry
		for i := range entries {
			s.Equal(action, entries[i].Action, "--action must filter the query server-side")
			if entries[i].ResourceID == resourceID {
				found = &entries[i]
			}
		}
		s.Require().NotNil(found, "%s of %s must appear in the audit log:\n%s", action, resourceID, body)
		s.Equal(resourceType, found.ResourceType)
		s.Equal("success", found.Outcome)
		s.NotEmpty(found.Actor)
	}

	assertAudited("notification_channel.create", channel.ID, "notification_channel")
	assertAudited("notification_channel.update", channel.ID, "notification_channel")
	assertAudited("notification_channel.delete", channel.ID, "notification_channel")
	assertAudited("notification_policy.create", policy.ID, "notification_policy")
	assertAudited("notification_policy.update", policy.ID, "notification_policy")
	assertAudited("notification_policy.delete", policy.ID, "notification_policy")

	// The channel's config (a webhook URL) must never leak into the audit
	// trail — the REST API itself redacts it (see redactChannel).
	status, body = s.notificationRequest(http.MethodGet,
		"/v1/auth/audit?action=notification_channel.create&limit=200", "")
	s.Require().Equal(http.StatusOK, status, body)
	s.NotContains(body, "example.invalid/hooks/audit",
		"the channel's webhook URL must not appear in the audit log")
}
