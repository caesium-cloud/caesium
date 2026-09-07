//go:build integration

package test

import (
	"encoding/json"
	"os"
	"strings"
)

func (s *IntegrationTestSuite) TestAgentProfileCLIListJSONStdout() {
	profileArgs := []string{"agentprofile", "list", "--json", "--server", s.caesiumURL}
	if key := firstEnv("CAESIUM_AGENTPROFILE_API_KEY", "CAESIUM_API_KEY", "CAESIUM_E2E_AUTH_ADMIN_KEY"); key != "" {
		profileArgs = append(profileArgs, "--api-key", key)
	}
	profileOut, err := s.runCLIStdout(profileArgs...)
	s.Require().NoError(err, "caesium agentprofile list --json failed:\n%s", profileOut)
	s.Require().True(json.Valid([]byte(profileOut)), "caesium agentprofile list --json stdout was not clean JSON:\n%s", profileOut)

	var profiles []map[string]any
	s.Require().NoError(json.Unmarshal([]byte(profileOut), &profiles))
}

func (s *IntegrationTestSuite) TestIncidentCLIListJSONStdout() {
	s.requireAuthLane()

	args := []string{"incident", "list", "--json", "--server", s.caesiumURL}
	if key := firstEnv("CAESIUM_INCIDENT_API_KEY", "CAESIUM_API_KEY", "CAESIUM_E2E_AUTH_ADMIN_KEY"); key != "" {
		args = append(args, "--api-key", key)
	}
	incidentOut, err := s.runCLIStdout(args...)
	s.Require().NoError(err, "caesium incident list --json failed:\n%s", incidentOut)
	s.Require().True(json.Valid([]byte(incidentOut)), "caesium incident list --json stdout was not clean JSON:\n%s", incidentOut)

	var incidents struct {
		Incidents []map[string]any `json:"incidents"`
		Total     int              `json:"total"`
		Limit     int              `json:"limit"`
		Offset    int              `json:"offset"`
	}
	s.Require().NoError(json.Unmarshal([]byte(incidentOut), &incidents))
	s.Require().NotNil(incidents.Incidents, "incident list JSON must include incidents array")
}

func firstEnv(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}
