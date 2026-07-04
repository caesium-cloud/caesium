//go:build integration

package test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/api/rest/bind"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// TestIncidentOperatorAPIAndApprovalGate drives the D1/D2 REST surface against a
// live server: the operator read API, and the tier-3 approval gate including its
// two hard security preconditions (agent tokens rejected; auth mode required —
// the latter is enforced at env.Process, exercised here by running with auth on).
func TestIncidentOperatorAPIAndApprovalGate(t *testing.T) {
	t.Setenv("CAESIUM_DATABASE_PATH", t.TempDir())
	t.Setenv("CAESIUM_DATABASE_SHARDS", "1")
	t.Setenv("CAESIUM_DATABASE_STANDBYS", "0")
	t.Setenv("CAESIUM_NODE_ADDRESS", freeLoopbackAddress(t))
	t.Setenv("CAESIUM_AUTH_KEY_HASH_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("CAESIUM_AUTH_MODE", "api-key")
	t.Setenv("CAESIUM_AGENT_REMEDIATION_ENABLED", "true")
	require.NoError(t, env.Process())
	require.NoError(t, db.Migrate())

	conn := db.Connection()
	authSvc := iauth.NewService(conn, iauth.WithKeyHashSecret(env.Variables().AuthKeyHashSecret))
	auditor := iauth.NewAuditLogger(conn)
	limiter := iauth.NewRateLimiter(1000, time.Minute)

	// Seed an incident parked in awaiting_approval with a tier-3 action + approval.
	now := time.Now().UTC()
	jobID := uuid.New()
	key := jobID.String() + "|transform|schema_violation"
	inc := models.Incident{
		ID: uuid.New(), JobID: jobID, TaskName: "transform", Class: "schema_violation",
		Status: models.IncidentStatusAwaitingApproval, DedupeKey: key, ActiveDedupeKey: &key,
		OccurrenceCount: 1, OpenedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, conn.Create(&inc).Error)
	action := models.AgentAction{
		ID: uuid.New(), IncidentID: inc.ID, Type: "apply_jobdef_patch", Tier: 3,
		Status: models.AgentActionStatusProposed, Actor: models.AgentActionActorAgent,
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, conn.Create(&action).Error)
	approval := models.ApprovalRequest{
		ID: uuid.New(), IncidentID: inc.ID, ActionID: action.ID,
		Decision: models.ApprovalDecisionPending, CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, conn.Create(&approval).Error)

	// Operator (human) and an agent session token (operator role + agent claim).
	operator, err := authSvc.CreateKey(&iauth.CreateKeyRequest{Role: models.RoleOperator, CreatedBy: "test"})
	require.NoError(t, err)
	agent, err := authSvc.CreateKey(&iauth.CreateKeyRequest{Role: models.RoleOperator, CreatedBy: "agent"})
	require.NoError(t, err)
	agentScope := []byte(`{"agent":{"incident_id":"` + inc.ID.String() + `"}}`)
	require.NoError(t, conn.Model(&models.APIKey{}).Where("id = ?", agent.Key.ID).Update("scope", agentScope).Error)

	e := echo.New()
	bind.All(e.Group("/v1"), event.New(), authSvc, auditor, limiter, nil)
	server := httptest.NewServer(e)
	defer server.Close()

	// D2: operator lists incidents.
	status, body := getWithBearer(t, server.URL+"/v1/incidents", operator.Plaintext)
	require.Equal(t, http.StatusOK, status, body)
	var list struct {
		Incidents []models.Incident `json:"incidents"`
		Total     int64             `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &list))
	require.Equal(t, int64(1), list.Total)
	require.Equal(t, inc.ID, list.Incidents[0].ID)

	// D2: operator reads the timeline.
	status, body = getWithBearer(t, server.URL+"/v1/incidents/"+inc.ID.String(), operator.Plaintext)
	require.Equal(t, http.StatusOK, status, body)
	var detail struct {
		Incident  models.Incident          `json:"incident"`
		Actions   []models.AgentAction     `json:"actions"`
		Approvals []models.ApprovalRequest `json:"approvals"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &detail))
	require.Len(t, detail.Actions, 1)
	require.Len(t, detail.Approvals, 1)

	approveURL := server.URL + "/v1/incidents/" + inc.ID.String() + "/approvals/" + approval.ID.String() + "/approve"

	// D1 security precondition: the agent session token is rejected OUTRIGHT.
	status, body = postWithBearer(t, approveURL, agent.Plaintext, nil)
	require.Equal(t, http.StatusForbidden, status, body)
	require.Contains(t, body, authmw.ApprovalAgentTokenDenyMessage)

	// D1: the human operator approves.
	status, body = postWithBearer(t, approveURL, operator.Plaintext, []byte(`{"reason":"additive rename, safe"}`))
	require.Equal(t, http.StatusOK, status, body)

	// The decision is persisted and the incident resumed.
	status, body = getWithBearer(t, server.URL+"/v1/incidents/"+inc.ID.String(), operator.Plaintext)
	require.Equal(t, http.StatusOK, status, body)
	require.NoError(t, json.Unmarshal([]byte(body), &detail))
	require.Equal(t, models.ApprovalDecisionApproved, detail.Approvals[0].Decision)
	require.Equal(t, models.IncidentStatusTriaging, detail.Incident.Status)

	// Re-deciding a resolved approval is a conflict.
	status, _ = postWithBearer(t, approveURL, operator.Plaintext, nil)
	require.Equal(t, http.StatusConflict, status)

	// D1: a rejection on a fresh proposal ESCALATES the incident (a human owns
	// it) rather than resuming triaging — the agent cannot re-propose the action.
	key2 := jobID.String() + "|load|schema_violation"
	inc2 := models.Incident{
		ID: uuid.New(), JobID: jobID, TaskName: "load", Class: "schema_violation",
		Status: models.IncidentStatusAwaitingApproval, DedupeKey: key2, ActiveDedupeKey: &key2,
		OccurrenceCount: 1, OpenedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, conn.Create(&inc2).Error)
	action2 := models.AgentAction{
		ID: uuid.New(), IncidentID: inc2.ID, Type: "override_schema_gate", Tier: 3,
		Status: models.AgentActionStatusProposed, Actor: models.AgentActionActorAgent,
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, conn.Create(&action2).Error)
	approval2 := models.ApprovalRequest{
		ID: uuid.New(), IncidentID: inc2.ID, ActionID: action2.ID,
		Decision: models.ApprovalDecisionPending, CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, conn.Create(&approval2).Error)

	rejectURL := server.URL + "/v1/incidents/" + inc2.ID.String() + "/approvals/" + approval2.ID.String() + "/reject"
	status, body = postWithBearer(t, rejectURL, operator.Plaintext, []byte(`{"reason":"schema drift is not benign"}`))
	require.Equal(t, http.StatusOK, status, body)

	status, body = getWithBearer(t, server.URL+"/v1/incidents/"+inc2.ID.String(), operator.Plaintext)
	require.Equal(t, http.StatusOK, status, body)
	require.NoError(t, json.Unmarshal([]byte(body), &detail))
	require.Equal(t, models.ApprovalDecisionRejected, detail.Approvals[0].Decision)
	require.Equal(t, models.IncidentStatusEscalated, detail.Incident.Status)
}

func postWithBearer(t *testing.T, target, token string, body []byte) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, target, rdr)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(out)
}
