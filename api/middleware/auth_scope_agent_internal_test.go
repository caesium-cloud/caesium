package middleware

import "testing"

func TestScopeHasAgentClaim(t *testing.T) {
	cases := []struct {
		name  string
		scope string
		want  bool
	}{
		{"nil", "", false},
		{"plain job scope", `{"jobs":["alpha","beta"]}`, false},
		{"empty object", `{}`, false},
		{"agent claim", `{"agent":{"incident_id":"11111111-1111-1111-1111-111111111111"}}`, true},
		{"agent claim alongside jobs", `{"jobs":["alpha"],"agent":{"incident_id":"x"}}`, true},
		{"agent_session claim", `{"agent_session":{"id":"s1"}}`, true},
		{"null agent claim is not present", `{"agent":null}`, false},
		{"malformed json", `{not json`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scopeHasAgentClaim([]byte(tc.scope)); got != tc.want {
				t.Fatalf("scopeHasAgentClaim(%q) = %v, want %v", tc.scope, got, tc.want)
			}
		})
	}
}

func TestIsIncidentApprovalRoute(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/v1/incidents/:id/approvals/:id/approve", true},
		{"/v1/incidents/:id/approvals/:id/reject", true},
		{"/v1/incidents/:id", false},
		{"/v1/incidents", false},
		{"/v1/jobs/:id", false},
	}
	for _, tc := range cases {
		if got := isIncidentApprovalRoute(tc.path); got != tc.want {
			t.Fatalf("isIncidentApprovalRoute(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}
