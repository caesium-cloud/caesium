//go:build integration

package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	iincident "github.com/caesium-cloud/caesium/internal/incident"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func (s *IntegrationTestSuite) TestAgentMCPToolsListBundleAndIncidentScope() {
	s.Require().NoError(env.Process())
	vars := env.Variables()
	if vars.AuthMode != "api-key" || !vars.AgentRemediationEnabled {
		s.T().Skip("agent MCP integration requires the auth-enabled remediation lane")
	}

	conn := db.Connection()
	incX, aliasX := seedMCPIncident(s.T(), conn, "x")
	incY, _ := seedMCPIncident(s.T(), conn, "y")

	authSvc := iauth.NewService(conn, iauth.WithKeyHashSecret(vars.AuthKeyHashSecret))
	key, err := authSvc.MintAgentSessionKey(incX.ID, []string{aliasX}, time.Hour)
	s.Require().NoError(err)

	status, body := s.postAgentMCP(key.Plaintext, incX.ID, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/list",
	})
	s.Require().Equal(http.StatusOK, status, string(body))

	var listResp mcpListResponse
	s.Require().NoError(json.Unmarshal(body, &listResp))
	s.Require().Nil(listResp.Error)
	s.Equal("2.0", listResp.JSONRPC)
	var toolNames []string
	for _, tool := range listResp.Result.Tools {
		toolNames = append(toolNames, tool.Name)
	}
	s.ElementsMatch([]string{"get_bundle", "get_context", "propose_action", "add_note"}, toolNames)

	status, body = s.postAgentMCP(key.Plaintext, incX.ID, map[string]any{
		"jsonrpc": "2.0",
		"id":      "bundle",
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "get_bundle",
			"arguments": map[string]any{},
		},
	})
	s.Require().Equal(http.StatusOK, status, string(body))

	var bundleResp mcpBundleResponse
	s.Require().NoError(json.Unmarshal(body, &bundleResp))
	s.Require().Nil(bundleResp.Error)
	s.Equal("2.0", bundleResp.JSONRPC)
	s.Equal(incX.ID.String(), bundleResp.Result.StructuredContent.Incident.ID)
	s.NotEmpty(bundleResp.Result.Content)

	status, body = s.postAgentMCP(key.Plaintext, incY.ID, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/list",
	})
	s.Require().Equal(http.StatusForbidden, status, string(body))
	s.Contains(string(body), authmw.AgentScopeDenyMessage)
}

type mcpErrorResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpListResponse struct {
	JSONRPC string            `json:"jsonrpc"`
	Error   *mcpErrorResponse `json:"error,omitempty"`
	Result  struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	} `json:"result"`
}

type mcpBundleResponse struct {
	JSONRPC string            `json:"jsonrpc"`
	Error   *mcpErrorResponse `json:"error,omitempty"`
	Result  struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent struct {
			Incident struct {
				ID string `json:"id"`
			} `json:"incident"`
		} `json:"structuredContent"`
	} `json:"result"`
}

func (s *IntegrationTestSuite) postAgentMCP(token string, incidentID uuid.UUID, payload any) (int, []byte) {
	s.T().Helper()

	body, err := json.Marshal(payload)
	s.Require().NoError(err)
	req, err := http.NewRequestWithContext(
		s.T().Context(),
		http.MethodPost,
		fmt.Sprintf("%s/v1/agent/incidents/%s/mcp", s.caesiumURL, incidentID),
		bytes.NewReader(body),
	)
	s.Require().NoError(err)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close()

	out, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	return resp.StatusCode, out
}

func seedMCPIncident(t *testing.T, conn *gorm.DB, suffix string) (*models.Incident, string) {
	t.Helper()

	now := time.Now().UTC()
	triggerID := uuid.New()
	if err := conn.Create(&models.Trigger{
		ID:            triggerID,
		Type:          models.TriggerTypeCron,
		Configuration: `{"cron":"0 * * * *"}`,
		CreatedAt:     now,
		UpdatedAt:     now,
	}).Error; err != nil {
		t.Fatalf("seed MCP trigger: %v", err)
	}

	alias := fmt.Sprintf("agent-mcp-%s-%d", suffix, now.UnixNano())
	jobID := uuid.New()
	if err := conn.Create(&models.Job{
		ID:        jobID,
		Alias:     alias,
		TriggerID: triggerID,
		CreatedAt: now,
		UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("seed MCP job: %v", err)
	}

	inc, _, err := iincident.NewStore(conn).OpenOrAppend(t.Context(), iincident.OpenParams{
		JobID:     jobID,
		TaskName:  "extract-" + suffix,
		Class:     iincident.ClassUnknown,
		LastError: "mcp integration seed",
	})
	if err != nil {
		t.Fatalf("seed MCP incident: %v", err)
	}
	return inc, alias
}
