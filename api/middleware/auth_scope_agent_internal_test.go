package middleware

import "testing"

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
		{"/v1/agent/incidents/:id/actions", false},
	}
	for _, tc := range cases {
		if got := isIncidentApprovalRoute(tc.path); got != tc.want {
			t.Fatalf("isIncidentApprovalRoute(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}
