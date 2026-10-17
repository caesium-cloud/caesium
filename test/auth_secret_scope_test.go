//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/pkg/container"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"gopkg.in/yaml.v3"
)

// secretScopeLintDoc is the `caesium job lint --server --json` document.
type secretScopeLintDoc struct {
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type secretScopeIncident struct {
	ID        string `json:"id"`
	JobID     string `json:"job_id"`
	Class     string `json:"class"`
	Namespace string `json:"namespace"`
	LastError string `json:"last_error"`
}

func secretScopeDefinition(alias, namespace, ref, engine string) schema.Definition {
	step := schema.Step{
		Name:    "use",
		Image:   "alpine:3.23",
		Command: []string{"sh", "-c", `test -n "$TOKEN" && echo token-present`},
		Spec:    container.Spec{Env: map[string]string{"TOKEN": ref}},
	}
	if engine != "" && engine != "docker" {
		step.Engine = engine
	}
	return schema.Definition{
		APIVersion: "v1",
		Kind:       "Job",
		Metadata:   schema.Metadata{Alias: alias, Namespace: namespace},
		Trigger:    schema.Trigger{Type: "cron", Configuration: map[string]any{"cron": "0 0 31 2 *"}},
		Steps:      []schema.Step{step},
	}
}

// writeSecretScopeManifest writes def as a manifest file for the CLI verbs.
func (s *IntegrationTestSuite) writeSecretScopeManifest(def schema.Definition) string {
	s.T().Helper()
	data, err := yaml.Marshal(def)
	s.Require().NoError(err)
	dir := s.T().TempDir()
	s.Require().NoError(os.WriteFile(filepath.Join(dir, def.Metadata.Alias+".job.yaml"), data, 0o600))
	return dir
}

// serveSecretScopePolicy serves the CURRENTLY served policy with the given
// namespaces' secret rules set, reloads it, and returns the new revision.
func (s *IntegrationTestSuite) serveSecretScopePolicy(path string, base []byte, rules map[string][]string) string {
	s.T().Helper()
	p, err := policy.Parse(base)
	s.Require().NoError(err, "the served auth-lane policy must parse")
	for namespace, allow := range rules {
		settings := p.Namespaces[namespace]
		settings.Secrets = &policy.SecretRules{Allow: allow}
		p.Namespaces[namespace] = settings
	}
	data, err := yaml.Marshal(p)
	s.Require().NoError(err)
	_, err = policy.Parse(data)
	s.Require().NoError(err, "the extended policy must stay valid:\n%s", data)
	s.writeAuthPolicy(path, data)
	reload := runCLIJSON[policyReloadDoc](s, "auth", "policy", "reload", "--json", "--server", s.caesiumURL)
	s.Require().Equal(policy.Hash(data), reload.Revision, "the extended policy is in force")
	return reload.Revision
}

// restoreSecretScopePolicy puts the original file back, reloads, and waits
// until the server serves the original revision again.
func (s *IntegrationTestSuite) restoreSecretScopePolicy(path string, original []byte) {
	s.restoreAuthPolicy(path, original)
	want := policy.Hash(original)
	deadline := time.Now().Add(authPolicyPollWait)
	for {
		_, _, _ = s.runCLISeparate("auth", "policy", "reload", "--server", s.caesiumURL)
		stdout, _, err := s.runCLISeparate("auth", "policy", "show", "--json", "--server", s.caesiumURL)
		var show policyShowDoc
		if err == nil && decodeCLIJSONStdout([]byte(stdout), &show) == nil && show.Revision == want {
			return
		}
		if time.Now().After(deadline) {
			s.T().Errorf("the auth-lane policy did not return to revision %s (serving %q)", want, show.Revision)
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// postSecretScopeLint posts raw definitions to POST /v1/jobdefs/lint, so a
// definition can omit metadata.namespace exactly as an API client may.
func (s *IntegrationTestSuite) postSecretScopeLint(body string) []string {
	s.T().Helper()
	resp, err := s.doJSONRequest(http.MethodPost, s.caesiumURL+"/v1/jobdefs/lint", strings.NewReader(body))
	s.Require().NoError(err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, resp.StatusCode, "%s", raw)
	var doc secretScopeLintDoc
	s.Require().NoError(json.Unmarshal(raw, &doc), "%s", raw)
	messages := make([]string, 0, len(doc.Errors))
	for _, e := range doc.Errors {
		messages = append(messages, e.Message)
	}
	return messages
}

// omittedNamespaceLintBody is one definition with no metadata.namespace.
func omittedNamespaceLintBody(alias, ref string) string {
	return fmt.Sprintf(`{"definitions":[{"apiVersion":"v1","kind":"Job","metadata":{"alias":%q},`+
		`"trigger":{"type":"cron","configuration":{"cron":"0 0 31 2 *"}},`+
		`"steps":[{"name":"use","image":"alpine:3.23","env":{"TOKEN":%q}}]}]}`, alias, ref)
}

func (s *IntegrationTestSuite) awaitConfigIncident(jobID string) secretScopeIncident {
	s.T().Helper()
	var found secretScopeIncident
	s.Require().Eventually(func() bool {
		var list struct {
			Incidents []secretScopeIncident `json:"incidents"`
		}
		if err := s.tryGetJSON("/v1/incidents?job_id="+jobID, &list); err != nil {
			return false
		}
		for _, incident := range list.Incidents {
			if incident.JobID == jobID {
				found = incident
				return true
			}
		}
		return false
	}, 60*time.Second, 200*time.Millisecond, "no incident opened for the denied job %s", jobID)
	return found
}

// TestAuthSecretScope drives namespace secret rules (identity-and-access C4)
// through the live server: server lint and the apply preview report a denied
// reference redacted; apply does not check, and the run fails before the cache
// with the config incident class; an allowed reference resolves, is cached,
// and once the rule is revoked the next run is refused rather than served from
// cache.
func (s *IntegrationTestSuite) TestAuthSecretScope() {
	s.requireAuthLane()
	const namespace = "iaa-c4"
	const allowedRef = "secret://env/CAESIUM_IT_REGISTRY_CREDS"
	const deniedRef = "secret://env/PATH"
	path, original := s.authPolicyFixture()
	s.T().Cleanup(func() { s.restoreSecretScopePolicy(path, original) })
	suffix := time.Now().UnixNano()

	// A definition that omits metadata.namespace is checked as the default
	// namespace: allow-all without rules, then an allow-list, then deny-all.
	omittedAlias := fmt.Sprintf("iaa-c4-omitted-%d", suffix)
	s.serveSecretScopePolicy(path, original, map[string][]string{namespace: {"env/CAESIUM_IT_REGISTRY_*"}})
	s.Empty(s.postSecretScopeLint(omittedNamespaceLintBody(omittedAlias, deniedRef)), "default has no rules: allow all")
	s.serveSecretScopePolicy(path, original, map[string][]string{namespace: {"env/CAESIUM_IT_REGISTRY_*"}, "default": {"env/CAESIUM_IT_REGISTRY_*"}})
	s.Empty(s.postSecretScopeLint(omittedNamespaceLintBody(omittedAlias, allowedRef)))
	s.Equal([]string{omittedAlias + `: secret env/PATH/… is not allowed by namespace "default" secret rules: no rule matches`},
		s.postSecretScopeLint(omittedNamespaceLintBody(omittedAlias, deniedRef)))
	s.serveSecretScopePolicy(path, original, map[string][]string{namespace: {"env/CAESIUM_IT_REGISTRY_*"}, "default": {}})
	s.Equal([]string{omittedAlias + `: secret env/CAESIUM_IT_REGISTRY_CREDS/… is not allowed by namespace "default" secret rules: no rule matches`},
		s.postSecretScopeLint(omittedNamespaceLintBody(omittedAlias, allowedRef)))
	s.serveSecretScopePolicy(path, original, map[string][]string{namespace: {"env/CAESIUM_IT_REGISTRY_*"}})

	allowed := secretScopeDefinition(fmt.Sprintf("iaa-c4-allowed-%d", suffix), namespace, allowedRef, s.engineType)
	denied := secretScopeDefinition(fmt.Sprintf("iaa-c4-denied-%d", suffix), namespace, deniedRef, s.engineType)
	allowedDir := s.writeSecretScopeManifest(allowed)
	deniedDir := s.writeSecretScopeManifest(denied)

	// Server lint names the redacted reference and never the raw one.
	lint, _ := runCLIJSONFailure[secretScopeLintDoc](s, "job", "lint", "--path", deniedDir, "--server", s.caesiumURL, "--json")
	s.Require().Len(lint.Errors, 1, "%+v", lint.Errors)
	s.Equal(denied.Metadata.Alias+`: secret env/PATH/… is not allowed by namespace "iaa-c4" secret rules: no rule matches`, lint.Errors[0].Message)
	clean := runCLIJSON[secretScopeLintDoc](s, "job", "lint", "--path", allowedDir, "--server", s.caesiumURL, "--json")
	s.Empty(clean.Errors, "the allowed reference lints clean")

	// The apply preview rejects it the same way.
	stdout, stderr, err := s.runCLISeparate("job", "diff", "--path", deniedDir, "--server", s.caesiumURL, "--json")
	s.Require().Error(err, "the apply preview must refuse a denied reference; stdout: %s", stdout)
	s.Contains(stderr, "job diff failed (400)")
	s.Contains(stderr, `secret env/PATH/… is not allowed by namespace \"iaa-c4\" secret rules`)
	s.NotContains(stderr+stdout, deniedRef)

	// Apply does not check secret rules: the run fails, before any cache
	// lookup and without a container, as a config incident.
	s.applyDefinition(denied)
	deniedJob := s.requireJobByAlias(denied.Metadata.Alias)
	deniedRun := s.awaitRun(deniedJob.ID, s.triggerRun(deniedJob.ID), runTimeout)
	s.Require().Equal("failed", deniedRun.Status)
	s.Require().Equal(namespace, deniedRun.Namespace)
	s.Require().Len(deniedRun.Tasks, 1)
	s.Contains(deniedRun.Tasks[0].Error, `env TOKEN: secret env/PATH/… is not allowed by namespace "iaa-c4" secret rules`)
	s.NotContains(deniedRun.Tasks[0].Error, deniedRef)
	s.Empty(deniedRun.Tasks[0].RuntimeID, "a denied task never starts a container")
	incident := s.awaitConfigIncident(deniedJob.ID)
	s.Equal("config", incident.Class, "a secret denial is a config failure, never transient_infra")
	s.Equal(namespace, incident.Namespace)
	s.NotContains(incident.LastError, deniedRef)
	listed := runCLIJSON[struct {
		Incidents []secretScopeIncident `json:"incidents"`
	}](s, "incident", "list", "--json", "--server", s.caesiumURL, "--job-id", deniedJob.ID, "--class", "config")
	s.Require().NotEmpty(listed.Incidents, "caesium incident list --class config finds the denial")

	// Allowed: resolves and is cached; a second run is a cache hit.
	s.applyDefinition(allowed)
	allowedJob := s.requireJobByAlias(allowed.Metadata.Alias)
	first := s.awaitRun(allowedJob.ID, s.triggerRun(allowedJob.ID), runTimeout)
	s.Require().Equal("succeeded", first.Status, "%+v", first.Tasks)
	second := s.awaitRun(allowedJob.ID, s.triggerRun(allowedJob.ID), runTimeout)
	s.Require().Equal("succeeded", second.Status)
	s.Require().Equal(1, second.CacheHits, "while allowed, the cache serves the task")
	// A replay of the baseline reuses its cached step without reaching an
	// executor (no dispatch, so it works in local mode).
	replayOut, replayErrOut, replayErr := s.runCLISeparate("run", "replay", first.ID, "--job-id", allowedJob.ID, "--json", "--server", s.caesiumURL)
	s.Require().NoError(replayErr, "while allowed, the cached replay is accepted:\nstdout=%s\nstderr=%s", replayOut, replayErrOut)
	var replayed replayResponse
	s.Require().NoError(json.Unmarshal([]byte(replayOut), &replayed), "replay --json stdout: %s", replayOut)
	s.Require().Equal("succeeded", s.awaitRun(allowedJob.ID, replayed.RunID, runTimeout).Status)

	// Revoke: the next run is refused before the cache lookup.
	s.serveSecretScopePolicy(path, original, map[string][]string{namespace: {}})
	third := s.awaitRun(allowedJob.ID, s.triggerRun(allowedJob.ID), runTimeout)
	s.Require().Equal("failed", third.Status)
	s.Zero(third.CacheHits, "a now-denied reference is never served from cache")
	s.Require().Len(third.Tasks, 1)
	s.Contains(third.Tasks[0].Error, `secret env/CAESIUM_IT_REGISTRY_CREDS/… is not allowed by namespace "iaa-c4" secret rules`)
	s.False(strings.Contains(third.Tasks[0].Error, allowedRef))

	// And a replay of the same baseline no longer reuses the cached step.
	replayOut, replayErrOut, replayErr = s.runCLISeparate("run", "replay", first.ID, "--job-id", allowedJob.ID, "--json", "--server", s.caesiumURL)
	s.Require().Error(replayErr, "a replay must not reuse a now-denied step:\nstdout=%s", replayOut)
	s.Empty(strings.TrimSpace(replayOut), "a refused replay leaves stdout clean")
	s.Contains(replayErrOut, "replay-safe refusal (422)")
	s.Contains(replayErrOut, `env/CAESIUM_IT_REGISTRY_CREDS/… is not allowed by namespace`)
	s.NotContains(replayErrOut, allowedRef)
}

// TestAuthSecretScopeWebhook drives the HTTP trigger's secret:// webhook
// secret through the live public webhook route: the secret belongs to the
// trigger's job namespace, so while that namespace's rules allow it the hook
// is accepted and starts a run, and once they deny it the hook is refused
// (500, no run) without the secret being read.
func (s *IntegrationTestSuite) TestAuthSecretScopeWebhook() {
	s.requireAuthLane()
	const namespace = "iaa-c4-hook"
	path, original := s.authPolicyFixture()
	s.T().Cleanup(func() { s.restoreSecretScopePolicy(path, original) })
	s.serveSecretScopePolicy(path, original, map[string][]string{namespace: {"env/CAESIUM_IT_REGISTRY_*"}})

	suffix := time.Now().UnixNano()
	hookPath := fmt.Sprintf("iaa-c4/hook-%d", suffix)
	def := secretScopeDefinition(fmt.Sprintf("iaa-c4-hook-%d", suffix), namespace, "plain", s.engineType)
	def.Trigger = schema.Trigger{Type: "http", Configuration: map[string]any{
		"path":            "/" + hookPath,
		"secret":          "secret://env/CAESIUM_IT_REGISTRY_CREDS",
		"signatureScheme": "bearer",
		"signatureHeader": "X-C4-Hook-Token",
	}}
	s.applyDefinition(def)
	job := s.requireJobByAlias(def.Metadata.Alias)

	fire := func() int {
		req, err := http.NewRequestWithContext(s.T().Context(), http.MethodPost,
			fmt.Sprintf("%s/v1/hooks/%s", s.caesiumURL, hookPath), strings.NewReader(`{}`))
		s.Require().NoError(err)
		req.Header.Set("Content-Type", "application/json")
		// The resolved value of secret://env/CAESIUM_IT_REGISTRY_CREDS.
		req.Header.Set("X-C4-Hook-Token", "Bearer ci-user:ci-pass")
		resp, err := http.DefaultClient.Do(req)
		s.Require().NoError(err)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	s.Require().Equal(http.StatusAccepted, fire(), "the namespace allows the webhook secret")
	s.Require().Eventually(func() bool { return len(s.fetchRuns(job.ID)) == 1 }, 30*time.Second, 250*time.Millisecond,
		"an accepted hook starts one run")
	first := s.fetchRuns(job.ID)[0]
	s.awaitRun(job.ID, first.ID, runTimeout)

	s.serveSecretScopePolicy(path, original, map[string][]string{namespace: {}})
	s.Equal(http.StatusInternalServerError, fire(), "a denied webhook secret refuses the hook")
	time.Sleep(2 * time.Second)
	s.Len(s.fetchRuns(job.ID), 1, "a refused hook starts no run")
}

// TestDevOnceSecretWithAPolicyFileConfigured drives the client-side exemption
// through the real CLI binary: `caesium dev --once` inherits
// CAESIUM_AUTH_POLICY_FILE from the operator's shell, but only the server
// loads that file, so a secret-bearing step must resolve at once with the
// operator's own credentials rather than wait for a policy that never loads
// and then fail denied. It needs no server, so it runs on every lane.
func (s *IntegrationTestSuite) TestDevOnceSecretWithAPolicyFileConfigured() {
	const (
		namespace = "iaa-c4-dev"
		envName   = "CAESIUM_IT_C4_DEV_TOKEN"
		value     = "c4-dev-value"
	)
	policyPath := filepath.Join(s.T().TempDir(), "policy.yaml")
	s.Require().NoError(os.WriteFile(policyPath, []byte(fmt.Sprintf(
		"apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  %s:\n    secrets: {allow: [\"env/CAESIUM_IT_C4_DEV_*\"]}\n",
		namespace)), 0o600))

	def := secretScopeDefinition(fmt.Sprintf("iaa-c4-dev-%d", time.Now().UnixNano()), namespace, "secret://env/"+envName, s.engineType)
	def.Steps[0].Command = []string{"sh", "-c", fmt.Sprintf(`test "$TOKEN" = %q && echo token-ok`, value)}
	dir := s.writeSecretScopeManifest(def)

	started := time.Now()
	stdout, stderr, err := s.runCLIWithEnv([]string{
		"CAESIUM_AUTH_POLICY_FILE=" + policyPath,
		"CAESIUM_AUTH_ROLE_MAPPING=",
		envName + "=" + value,
	}, "dev", "--once", "--path", dir)
	elapsed := time.Since(started)
	s.Require().NoError(err, "dev --once resolves the secret client-side\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	s.Contains(stdout, def.Metadata.Alias)
	s.Contains(stdout, "OK")
	s.NotContains(stdout+stderr, "is not allowed by namespace", "a client never applies the server's secret rules")
	s.Less(elapsed, 25*time.Second, "a client never waits for the server's policy to load")
}
