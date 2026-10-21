//go:build integration

package test

import (
	"net/http"
	"time"
)

// The Keycloak lane's SSO scenarios (plan item G1, design §12.2). Each one
// logs a realm user in through Keycloak's real HTML login form and asserts
// what the session may do, against the lane policy
// test/testdata/keycloak/policy.yaml. G3 extends them with the full allow/deny
// matrix, group refresh and user disable.

// TestKeycloakSSOAdminLogin: admin@example.com, in caesium-admins, holds admin
// in every namespace and sees both lane jobs.
func (s *IntegrationTestSuite) TestKeycloakSSOAdminLogin() {
	lane := s.requireKeycloakLane()
	login := s.keycloakOIDCLogin(lane, "admin@example.com")
	who := s.requireSSOSession(login)

	s.Equal("admin@example.com", who.Subject)
	s.Equal("admin@example.com", who.Email)
	s.Equal("admin", who.Role)
	s.Equal("admin", who.ClusterRole)
	s.Equal(map[string]string{"*": "admin"}, who.Grants)
	s.Equal([]string{"*"}, who.Namespaces)

	aliases := s.keycloakJobAliases(login)
	s.Contains(aliases, keycloakMarketingJob)
	s.Contains(aliases, keycloakFinanceJob)
}

// TestKeycloakSSOConfinedLogin: bob@example.com, in marketing-eng, is an
// operator in marketing only: the finance job is absent from his list and
// refused by id.
func (s *IntegrationTestSuite) TestKeycloakSSOConfinedLogin() {
	lane := s.requireKeycloakLane()
	marketing := s.requireJobByAlias(keycloakMarketingJob)
	finance := s.requireJobByAlias(keycloakFinanceJob)

	login := s.keycloakOIDCLogin(lane, "bob@example.com")
	who := s.requireSSOSession(login)
	s.Equal("bob@example.com", who.Subject)
	s.Empty(who.Role, "a confined principal has no cluster role")
	s.Empty(who.ClusterRole)
	s.Equal(map[string]string{"marketing": "operator"}, who.Grants)
	s.Equal([]string{"marketing"}, who.Namespaces)

	aliases := s.keycloakJobAliases(login)
	s.Contains(aliases, keycloakMarketingJob)
	s.NotContains(aliases, keycloakFinanceJob)

	status, body := login.get(s, "/v1/jobs/"+marketing.ID)
	s.Equal(http.StatusOK, status, "bob reads the marketing job: %s", body)
	status, body = login.get(s, "/v1/jobs/"+finance.ID)
	s.Equal(http.StatusForbidden, status, "bob is refused the finance job: %s", body)
}

// TestKeycloakSSONoBindingDenied: carol@example.com is in no group and matches
// no binding, so her login is denied with reason no_binding and no session.
func (s *IntegrationTestSuite) TestKeycloakSSONoBindingDenied() {
	lane := s.requireKeycloakLane()
	since := time.Now().Add(-time.Minute)
	login := s.keycloakOIDCLogin(lane, "carol@example.com")
	s.requireNoBindingCallback(login)
	s.requireLoginDeniedAudit("carol@example.com", "no_binding", since)
}

// TestKeycloakSSOUnverifiedEmailDenied: dave@example.com is in no group and
// his email is bound to admin in every namespace, but Keycloak reports it
// unverified, so Caesium must not trust it (design §6.3: only OIDC
// email_verified=true email reaches users: bindings). The audit row's actor is
// his email, which proves the claim reached Caesium and was refused, not lost.
func (s *IntegrationTestSuite) TestKeycloakSSOUnverifiedEmailDenied() {
	lane := s.requireKeycloakLane()
	since := time.Now().Add(-time.Minute)
	login := s.keycloakOIDCLogin(lane, "dave@example.com")
	s.requireNoBindingCallback(login)
	s.requireLoginDeniedAudit("dave@example.com", "no_binding", since)
}

// TestKeycloakSSOVerifiedEmailBinding is dave's positive control:
// erin@example.com is in no group either, and her VERIFIED email is bound to
// viewer in finance, so the email binding alone grants exactly that.
func (s *IntegrationTestSuite) TestKeycloakSSOVerifiedEmailBinding() {
	lane := s.requireKeycloakLane()
	marketing := s.requireJobByAlias(keycloakMarketingJob)
	finance := s.requireJobByAlias(keycloakFinanceJob)

	login := s.keycloakOIDCLogin(lane, "erin@example.com")
	who := s.requireSSOSession(login)
	s.Equal("erin@example.com", who.Subject)
	s.Empty(who.ClusterRole)
	s.Equal(map[string]string{"finance": "viewer"}, who.Grants)
	s.Equal([]string{"finance"}, who.Namespaces)

	aliases := s.keycloakJobAliases(login)
	s.Contains(aliases, keycloakFinanceJob)
	s.NotContains(aliases, keycloakMarketingJob)
	status, body := login.get(s, "/v1/jobs/"+finance.ID)
	s.Equal(http.StatusOK, status, "erin reads the finance job: %s", body)
	status, body = login.get(s, "/v1/jobs/"+marketing.ID)
	s.Equal(http.StatusForbidden, status, "erin is refused the marketing job: %s", body)
}
