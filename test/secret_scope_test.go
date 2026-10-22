//go:build integration

package test

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// marketingSecretFixtures are the CAESIUM_IT_POLICY_FIXTURE values whose
// policy gives marketing the env/MARKETING_* secret allow-list (and nothing
// for env/FINANCE_*). Their servers carry MARKETING_IT_TOKEN in their
// environment (justfile integration-up and its parity recipes; the local
// coverage journey's COVERAGE_JOURNEY_SERVER_ENV).
var marketingSecretFixtures = map[string]bool{"two-namespaces": true, "coverage-secret-glob": true}

// requireMarketingSecretRules gates a scenario on a server whose static policy
// fixture gives marketing its env/MARKETING_* allow-list, and FAILS (never
// skips) when the lane says it serves a policy fixture that does not.
func (s *IntegrationTestSuite) requireMarketingSecretRules() {
	s.T().Helper()
	s.requirePolicyFixture()
	if onAuthLane() {
		return
	}
	fixture := strings.TrimSpace(os.Getenv(policyFixtureEnv))
	s.Require().True(marketingSecretFixtures[fixture],
		"%s=%q: this lane's policy fixture must give marketing the env/MARKETING_* allow-list", policyFixtureEnv, fixture)
}

// secretScopeWhyDoc is the part of `caesium why --json` these assertions read.
type secretScopeWhyDoc struct {
	TaskName     string  `json:"taskName"`
	Status       string  `json:"status"`
	FailureClass *string `json:"failureClass"`
}

// TestSecretScopeMarketingAllowList drives namespace secret scoping end to
// end (identity-and-access C5) with the served policy fixture, unmodified:
// marketing allows env/MARKETING_* only. A marketing manifest reading
// env/FINANCE_IT_TOKEN is rejected by server-side `caesium job lint` naming
// only the redacted reference; `caesium job apply` (which never checks secret
// rules) deploys it anyway, and its run fails before any container as a
// config failure that `caesium why --json` reports; the allowed
// env/MARKETING_IT_TOKEN reference lints clean, resolves and succeeds.
func (s *IntegrationTestSuite) TestSecretScopeMarketingAllowList() {
	s.requireMarketingSecretRules()
	const namespace = "marketing"
	const deniedRef = "secret://env/FINANCE_IT_TOKEN"
	const allowedRef = "secret://env/MARKETING_IT_TOKEN"
	suffix := time.Now().UnixNano()

	denied := secretScopeDefinition(fmt.Sprintf("c5-finance-ref-%d", suffix), namespace, deniedRef, s.engineType)
	allowed := secretScopeDefinition(fmt.Sprintf("c5-marketing-ref-%d", suffix), namespace, allowedRef, s.engineType)
	deniedDir := s.writeSecretScopeManifest(denied)
	allowedDir := s.writeSecretScopeManifest(allowed)

	// Server lint: rejected, naming the redacted reference only.
	lint, _ := runCLIJSONFailure[secretScopeLintDoc](s, "job", "lint", "--path", deniedDir, "--server", s.caesiumURL, "--json")
	s.Require().Len(lint.Errors, 1, "%+v", lint.Errors)
	s.Equal(denied.Metadata.Alias+`: secret env/FINANCE_IT_TOKEN/… is not allowed by namespace "marketing" secret rules: no rule matches`, lint.Errors[0].Message)
	s.NotContains(lint.Errors[0].Message, deniedRef)
	clean := runCLIJSON[secretScopeLintDoc](s, "job", "lint", "--path", allowedDir, "--server", s.caesiumURL, "--json")
	s.Empty(clean.Errors, "the allowed reference lints clean")

	// Apply never checks secret rules (lint is the gate operators skip); the
	// run-time check is the executor's, before any cache lookup or container.
	_, applyErr := s.runCLIStdout("job", "apply", "--path", deniedDir, "--server", s.caesiumURL)
	s.Require().NoError(applyErr, "apply deploys the manifest lint rejected")
	deniedJob := s.requireJobByAlias(denied.Metadata.Alias)
	deniedRun := s.awaitRun(deniedJob.ID, s.triggerRun(deniedJob.ID), runTimeout)
	s.Require().Equal("failed", deniedRun.Status)
	s.Require().Equal(namespace, deniedRun.Namespace)
	s.Require().Len(deniedRun.Tasks, 1)
	s.Contains(deniedRun.Tasks[0].Error, `secret env/FINANCE_IT_TOKEN/… is not allowed by namespace "marketing" secret rules`)
	s.NotContains(deniedRun.Tasks[0].Error, deniedRef)
	s.Empty(deniedRun.Tasks[0].RuntimeID, "a denied task never starts a container")

	// caesium why --json: stdout alone is one JSON document naming the class.
	whyStdout, whyErr := s.runCLIStdout("why", deniedRun.ID, "--task", "use", "--job-id", deniedJob.ID, "--server", s.caesiumURL, "--json")
	s.Require().NoError(whyErr)
	var why secretScopeWhyDoc
	s.Require().NoError(decodeCLIJSONStdout([]byte(whyStdout), &why), "why --json stdout: %s", whyStdout)
	s.Equal("failed", why.Status)
	s.Require().NotNil(why.FailureClass, "why --json names the failure class: %s", whyStdout)
	s.Equal("config", *why.FailureClass, "a secret denial is a config failure, never transient_infra")
	s.NotContains(whyStdout, deniedRef)
	// And the human rendering shows it.
	human, humanErr := s.runCLIStdout("why", deniedRun.ID, "--task", "use", "--job-id", deniedJob.ID, "--server", s.caesiumURL)
	s.Require().NoError(humanErr)
	s.Contains(human, "FAILURE-CLASS")
	s.Contains(human, "config")

	// Allowed: resolves in marketing and succeeds; why carries no class.
	_, applyErr = s.runCLIStdout("job", "apply", "--path", allowedDir, "--server", s.caesiumURL)
	s.Require().NoError(applyErr)
	allowedJob := s.requireJobByAlias(allowed.Metadata.Alias)
	allowedRun := s.awaitRun(allowedJob.ID, s.triggerRun(allowedJob.ID), runTimeout)
	s.Require().Equal("succeeded", allowedRun.Status, "%+v", allowedRun.Tasks)
	s.Require().Equal(namespace, allowedRun.Namespace)
	whyStdout, whyErr = s.runCLIStdout("why", allowedRun.ID, "--task", "use", "--job-id", allowedJob.ID, "--server", s.caesiumURL, "--json")
	s.Require().NoError(whyErr)
	why = secretScopeWhyDoc{}
	s.Require().NoError(decodeCLIJSONStdout([]byte(whyStdout), &why), "why --json stdout: %s", whyStdout)
	s.Equal("succeeded", why.Status)
	s.Nil(why.FailureClass, "a task that did not fail carries no failure class")
}
