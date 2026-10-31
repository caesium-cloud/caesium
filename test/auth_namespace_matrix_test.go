//go:build integration

package test

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
)

// TestAuthNamespaceAllowDenyMatrix is the namespace allow/deny matrix on the
// api-key lane (identity-and-access A7; the plan's and Acceptance Criterion
// 1's TestNamespaceAllowDenyMatrix, named TestAuth… so the auth lane's -run
// pattern selects it). It drives every route of internal/auth/rbac.go as one
// key per role × namespace (default, marketing, finance, "*"), a runner in
// marketing and finance (the moved job's writes), a viewer minted with an
// explicit --namespace '*', a job-scoped key and an agent-session token per
// incident, against the two-namespace policy fixture.
func (s *IntegrationTestSuite) TestAuthNamespaceAllowDenyMatrix() {
	s.requireAuthLane()
	s.requirePolicyFixture()

	caps := matrixLaneCaps{
		SSO:             true, // the lane configures LDAP
		UserAdmin:       true,
		Connectors:      true,
		Contracts:       true,
		DataAssertions:  true,
		LogConsole:      true,
		DatabaseConsole: true,
		Incidents:       true,
	}
	s.runNamespaceMatrix(caps, func(fx *namespaceMatrixFixture) []matrixPrincipal {
		var principals []matrixPrincipal
		var explicitStar, noNamespace createdAPIKey
		for _, role := range []models.Role{rV, rR, rO, rA} {
			for _, ns := range []string{nsDefault, nsMarketing, nsFinance, models.ClusterNamespace} {
				var key createdAPIKey
				if ns == models.ClusterNamespace {
					key = s.mintNamespacedKey(string(role))
				} else {
					key = s.mintNamespacedKey(string(role), ns)
				}
				if role == rV && ns == models.ClusterNamespace {
					noNamespace = key
				}
				principals = append(principals, bearerPrincipal(string(role)+"@"+ns, matrixKindAPIKey, key.Plaintext, key.Meta.KeyPrefix,
					map[string]models.Role{ns: role}))
			}
		}
		both := s.mintNamespacedKey("runner", nsMarketing, nsFinance)
		principals = append(principals, bearerPrincipal("runner@marketing+finance", matrixKindAPIKey, both.Plaintext, both.Meta.KeyPrefix,
			map[string]models.Role{nsMarketing: rR, nsFinance: rR}))

		// Q6: a viewer minted with an explicit "*" behaves exactly like one
		// minted without a namespace (same oracle; whoami compared below).
		explicitStar = s.createAPIKeyCLI("--role", "viewer", "--namespace", "*", "--description", "mx explicit star")
		fx.f.keys = append(fx.f.keys, explicitStar.Meta.ID)
		principals = append(principals, bearerPrincipal("viewer@* (explicit)", matrixKindAPIKey, explicitStar.Plaintext, explicitStar.Meta.KeyPrefix,
			map[string]models.Role{models.ClusterNamespace: rV}))
		// Q6: the explicit-"*" key and the namespace-less key describe the same
		// principal apart from their identity.
		whoami := func(token string) map[string]any {
			status, body := s.requestWithKey(http.MethodGet, "/auth/whoami", token, nil)
			s.Require().Equal(http.StatusOK, status, body)
			var out map[string]any
			s.Require().NoError(json.Unmarshal([]byte(body), &out), body)
			for key := range out {
				if key == "subject" || strings.HasSuffix(key, "_id") || key == "id" {
					delete(out, key)
				}
			}
			return out
		}
		s.Equal(whoami(noNamespace.Plaintext), whoami(explicitStar.Plaintext), "an explicit '*' grant is the cluster-wide grant")

		// A cluster operator scoped to marketing's main and HTTP jobs.
		m := fx.n(nsMarketing)
		scoped := s.createAPIKeyCLI("--role", "operator", "--scope-jobs", m.alias()+","+m.HTTPJob.Alias, "--description", "mx job-scoped")
		fx.f.keys = append(fx.f.keys, scoped.Meta.ID)
		scopedPrincipal := bearerPrincipal("job-scoped operator", matrixKindAPIKey, scoped.Plaintext, scoped.Meta.KeyPrefix,
			map[string]models.Role{models.ClusterNamespace: rO})
		scopedPrincipal.JobScope = []string{m.alias(), m.HTTPJob.Alias}
		principals = append(principals, scopedPrincipal)

		// Agent-session tokens, minted as the session supervisor mints them
		// (a precondition: the incident manager is the only minting path).
		conn := s.openIntegrationCatalogGorm()
		svc := iauth.NewService(conn, iauth.WithKeyHashSecret(env.Variables().AuthKeyHashSecret))
		for _, n := range fx.pair() {
			if n.Incident == "" {
				continue // no incidents on this lane
			}
			minted, err := svc.MintAgentSessionKey(uuid.MustParse(n.Incident), []string{n.FailJob.Alias}, time.Hour)
			s.Require().NoError(err)
			fx.f.keys = append(fx.f.keys, minted.Key.ID.String())
			agent := bearerPrincipal("agent session ("+n.Name+" incident)", matrixKindAgent, minted.Plaintext, minted.Key.KeyPrefix,
				map[string]models.Role{models.ClusterNamespace: iauth.AgentSessionKeyRole})
			agent.AgentIncident = n.Incident
			principals = append(principals, agent)
		}
		return principals
	})

}
