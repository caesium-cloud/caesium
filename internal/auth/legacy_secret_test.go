package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func legacyRequest(header, bearer string) *http.Request {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/events", nil)
	if header != "" {
		req.Header.Set(LegacySecretHeader, header)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return req
}

// TestLegacyGateMatrix pins the deprecated shared secrets' rule (A6): with a
// principal the secret is optional, read only from its own header and audited
// when valid; without auth it is the only gate, exactly as before.
func TestLegacyGateMatrix(t *testing.T) {
	const secret = "s3cret"
	for _, tc := range []struct {
		name                      string
		header, bearer, expected  string
		hasPrincipal, authEnabled bool
		wantStatus                int
		wantUsed                  bool
	}{
		// Auth enabled, principal present.
		{name: "principal bearer-only env set", bearer: "csk_runner", expected: secret, hasPrincipal: true, authEnabled: true},
		{name: "principal cookie-only env set", expected: secret, hasPrincipal: true, authEnabled: true},
		{name: "principal with valid header", header: secret, bearer: "csk_runner", expected: secret, hasPrincipal: true, authEnabled: true, wantUsed: true},
		{name: "principal with wrong header", header: "nope", bearer: "csk_runner", expected: secret, hasPrincipal: true, authEnabled: true, wantStatus: http.StatusUnauthorized},
		{name: "principal header env unset", header: secret, hasPrincipal: true, authEnabled: true},
		{name: "principal env unset", hasPrincipal: true, authEnabled: true},
		{name: "principal bearer equal to secret is not the secret", bearer: secret, expected: secret, hasPrincipal: true, authEnabled: true},
		// Auth enabled, no principal (unreachable behind the middleware).
		{name: "auth on without principal", header: secret, expected: secret, authEnabled: true, wantStatus: http.StatusUnauthorized},
		// Auth disabled: the secret is the only gate.
		{name: "no auth env unset", header: secret, wantStatus: http.StatusServiceUnavailable},
		{name: "no auth header valid", header: secret, expected: secret, wantUsed: true},
		{name: "no auth bearer valid", bearer: secret, expected: secret, wantUsed: true},
		{name: "no auth missing", expected: secret, wantStatus: http.StatusUnauthorized},
		{name: "no auth wrong", header: "nope", expected: secret, wantStatus: http.StatusUnauthorized},
		{name: "no auth header wins over bearer", header: "nope", bearer: secret, expected: secret, wantStatus: http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := LegacyGate(legacyRequest(tc.header, tc.bearer), LegacyEventIngest, tc.expected, tc.hasPrincipal, tc.authEnabled)
			require.Equal(t, tc.wantStatus, got.Status)
			require.Equal(t, tc.wantUsed, got.Used)
			if tc.wantStatus == http.StatusServiceUnavailable {
				require.Equal(t, LegacyEventIngest.DisabledMessage, got.Message)
			}
			require.NotContains(t, got.Message, secret)
		})
	}
}

func TestLegacySecretWarnings(t *testing.T) {
	require.Empty(t, LegacySecretWarnings("", "  "))
	warnings := LegacySecretWarnings("manual-value", "ingest-value")
	require.Len(t, warnings, 2)
	require.Contains(t, warnings[0], "CAESIUM_MANUAL_TRIGGER_API_KEY")
	require.Contains(t, warnings[0], ActorLegacyManualTrigger)
	require.Contains(t, warnings[1], "CAESIUM_EVENT_INGEST_API_KEY")
	for _, warning := range warnings {
		require.Contains(t, warning, LegacySecretReplacement)
		require.NotContains(t, warning, "manual-value")
		require.NotContains(t, warning, "ingest-value")
	}
}

func TestLegacySecretAuditEntryNamespaces(t *testing.T) {
	principal := &Principal{Subject: "csk_abc"}
	one := LegacySecretAuditEntry(LegacySecretUse{Secret: LegacyEventIngest, Outcome: OutcomeSuccess, Principal: principal, Namespaces: []string{"marketing", "marketing"}, Metadata: map[string]any{"delivered": 2}})
	require.Equal(t, ActorLegacyEventIngest, one.Actor)
	require.Equal(t, ActionLegacySecretUsed, one.Action)
	require.Equal(t, "marketing", one.Namespace)
	require.Equal(t, "csk_abc", one.Metadata["principal"])
	require.Equal(t, 2, one.Metadata["delivered"])
	require.NotContains(t, one.Metadata, "namespaces")

	many := LegacySecretAuditEntry(LegacySecretUse{Secret: LegacyManualTrigger, Outcome: OutcomeError, Namespaces: []string{"finance", "", "marketing"}})
	require.Equal(t, ActorLegacyManualTrigger, many.Actor)
	require.Equal(t, "*", many.Namespace)
	require.Equal(t, []string{"default", "finance", "marketing"}, many.Metadata["namespaces"])
	require.NotContains(t, many.Metadata, "principal")

	none := LegacySecretAuditEntry(LegacySecretUse{Secret: LegacyEventIngest, Outcome: OutcomeDenied})
	require.Equal(t, "", none.Namespace)
	require.Equal(t, OutcomeDenied, none.Outcome)
	raw, err := json.Marshal(none)
	require.NoError(t, err)
	require.False(t, strings.Contains(string(raw), "s3cret"))
}
