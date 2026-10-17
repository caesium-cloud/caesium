//go:build integration

package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"gopkg.in/yaml.v3"
)

// Server-side namespace semantics (identity-and-access plan item B2):
// declared namespaces on lint/apply/legacy create while a policy file is
// loaded, and the confined lint/diff baselines. Lanes without a policy file
// (podman, kind/helm, coverage journeys) keep accepting every valid
// namespace; TestCacheNamespaceMove and TestIncidentNamespaceLifecycle apply
// marketing and finance jobs there.

type jobdefLintResult struct {
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
	Contracts *contractCLIContractSummary `json:"contracts,omitempty"`
}

func (r jobdefLintResult) messages() []string {
	out := make([]string, 0, len(r.Errors))
	for _, e := range r.Errors {
		out = append(out, e.Message)
	}
	return out
}

type jobdefDiffResult struct {
	Added      []json.RawMessage `json:"added"`
	Removed    []json.RawMessage `json:"removed"`
	Modified   []json.RawMessage `json:"modified"`
	Namespaces []string          `json:"namespaces"`
	WouldPrune []json.RawMessage `json:"wouldPrune"`
}

type jobdefDiffEntry struct {
	Alias     string `json:"alias"`
	Namespace string `json:"namespace"`
	Diff      string `json:"diff"`
	Trigger   struct {
		Configuration map[string]any `json:"configuration"`
	} `json:"trigger"`
}

func decodeDiffEntries(s *IntegrationTestSuite, raw []json.RawMessage) []jobdefDiffEntry {
	s.T().Helper()
	out := make([]jobdefDiffEntry, 0, len(raw))
	for _, item := range raw {
		var entry jobdefDiffEntry
		s.Require().NoError(json.Unmarshal(item, &entry), string(item))
		out = append(out, entry)
	}
	return out
}

// namespaceProbeDefinition is a never-firing one-step job in namespace.
func namespaceProbeDefinition(alias, namespace string) schema.Definition {
	return schema.Definition{
		APIVersion: "v1",
		Kind:       "Job",
		Metadata:   schema.Metadata{Alias: alias, Namespace: namespace},
		Trigger:    schema.Trigger{Type: "cron", Configuration: map[string]any{"cron": "0 0 31 2 *"}},
		Steps:      []schema.Step{{Name: "ok", Image: "busybox:1.36.1", Command: []string{"sh", "-c", "echo b2"}}},
	}
}

// jobdefRequest POSTs payload as JSON to path with an optional bearer token
// ("" sends none, as on the no-auth lane).
func (s *IntegrationTestSuite) jobdefRequest(path, token string, payload any) (int, string) {
	s.T().Helper()
	encoded, err := json.Marshal(payload)
	s.Require().NoError(err)
	req, err := http.NewRequestWithContext(s.T().Context(), http.MethodPost, s.caesiumURL+path, bytes.NewReader(encoded))
	s.Require().NoError(err)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	return resp.StatusCode, string(body)
}

// serverLint runs POST /v1/jobdefs/lint and decodes the 200 body.
func (s *IntegrationTestSuite) serverLint(token string, defs ...schema.Definition) jobdefLintResult {
	s.T().Helper()
	status, body := s.jobdefRequest("/v1/jobdefs/lint", token, map[string]any{"definitions": defs})
	s.Require().Equal(http.StatusOK, status, body)
	var out jobdefLintResult
	s.Require().NoError(json.Unmarshal([]byte(body), &out), body)
	return out
}

// writeNamespaceManifests writes each definition to its own .job.yaml in a
// fresh directory removed at the end of the scenario.
func (s *IntegrationTestSuite) writeNamespaceManifests(defs ...schema.Definition) string {
	s.T().Helper()
	dir := s.T().TempDir()
	for _, def := range defs {
		encoded, err := yaml.Marshal(def)
		s.Require().NoError(err)
		s.Require().NoError(os.WriteFile(filepath.Join(dir, def.Metadata.Alias+".job.yaml"), encoded, 0o644))
	}
	return dir
}

func undeclaredNamespaceMessage(alias, namespace string) string {
	return fmt.Sprintf("metadata.namespace %q of job %q is not declared in the access policy", namespace, alias)
}

// assertServerLintRejectsUndeclared drives `caesium job lint --server --json`
// (stdout parsed on its own) with an undeclared and a declared namespace.
func (s *IntegrationTestSuite) assertServerLintRejectsUndeclared() {
	s.T().Helper()
	stamp := time.Now().UnixNano()
	alias := fmt.Sprintf("b2-lint-undeclared-%d", stamp)
	undeclared := fmt.Sprintf("b2-undeclared-%d", stamp%1_000_000)

	dir := s.writeNamespaceManifests(s.namespacedJobDefinition(alias, undeclared))
	stdout, err := s.runCLIStdout("job", "lint", "--path", dir, "--server", s.caesiumURL, "--json")
	s.Require().Error(err, "an undeclared namespace must fail server lint")
	var rejected jobdefLintResult
	s.Require().NoError(json.Unmarshal([]byte(stdout), &rejected), "lint --json stdout must be one JSON document: %s", stdout)
	s.Contains(rejected.messages(), undeclaredNamespaceMessage(alias, undeclared))

	dir = s.writeNamespaceManifests(s.namespacedJobDefinition(alias, "marketing"))
	stdout, err = s.runCLIStdout("job", "lint", "--path", dir, "--server", s.caesiumURL, "--json")
	s.Require().NoError(err, "marketing is declared: %s", stdout)
	var accepted jobdefLintResult
	s.Require().NoError(json.Unmarshal([]byte(stdout), &accepted), stdout)
	s.Empty(accepted.Errors)
}

// TestJobdefNamespaceServerLintRejectsUndeclared: the no-auth lane serves the
// policy fixture, so server lint rejects an undeclared namespace by name.
func (s *IntegrationTestSuite) TestJobdefNamespaceServerLintRejectsUndeclared() {
	s.requirePolicyFixture()
	s.assertServerLintRejectsUndeclared()
}

// TestAuthJobdefNamespaceServerLintRejectsUndeclared is the same check on the
// api-key auth lane, as the cluster admin.
func (s *IntegrationTestSuite) TestAuthJobdefNamespaceServerLintRejectsUndeclared() {
	s.requireAuthLane()
	s.requirePolicyFixture()
	s.assertServerLintRejectsUndeclared()
}

// TestJobdefNamespaceApplyRejectsUndeclared: an unrestricted apply into an
// undeclared namespace is refused before anything is written (ValidateBatch),
// and so is the legacy POST /v1/jobs.
func (s *IntegrationTestSuite) TestJobdefNamespaceApplyRejectsUndeclared() {
	s.requirePolicyFixture()
	stamp := time.Now().UnixNano()
	undeclared := fmt.Sprintf("b2-undeclared-%d", stamp%1_000_000)
	alias := fmt.Sprintf("b2-apply-undeclared-%d", stamp)
	declared := fmt.Sprintf("b2-apply-declared-%d", stamp)

	// One batch, one undeclared definition: nothing is applied.
	dir := s.writeNamespaceManifests(s.namespacedJobDefinition(declared, "marketing"), s.namespacedJobDefinition(alias, undeclared))
	stdout, stderr, err := s.runCLISeparate("job", "apply", "--path", dir, "--server", s.caesiumURL)
	s.Require().Error(err, "apply into an undeclared namespace must fail: %s", stdout)
	s.Contains(stderr, jsonEscaped(undeclaredNamespaceMessage(alias, undeclared)))
	s.Nil(s.findJobByAlias(alias), "the undeclared job must not exist")
	s.Nil(s.findJobByAlias(declared), "a refused batch applies nothing")

	// Legacy create.
	legacy := fmt.Sprintf("b2-legacy-undeclared-%d", stamp)
	status, body := s.jobdefRequest("/v1/jobs", s.authAPIKey, map[string]any{
		"alias":    legacy,
		"metadata": map[string]any{"namespace": undeclared},
		"trigger":  map[string]any{"type": "cron", "configuration": map[string]any{"cron": "0 0 1 1 *"}},
		"tasks":    []any{map[string]any{"atom": map[string]any{"engine": s.engineType, "image": "busybox:1.36.1", "command": []string{"sh", "-c", "echo b2"}}}},
	})
	s.Require().Equal(http.StatusBadRequest, status, body)
	s.Contains(body, jsonEscaped(undeclaredNamespaceMessage(legacy, undeclared)))
	s.Nil(s.findJobByAlias(legacy))

	// Declared namespaces still apply.
	s.applyDefinition(s.namespacedJobDefinition(declared, "finance"))
	s.assertJobNamespace(s.requireJobByAlias(declared).ID, "finance", "")
}

// jsonEscaped is msg as it appears inside a JSON string.
func jsonEscaped(msg string) string {
	encoded, _ := json.Marshal(msg)
	return strings.Trim(string(encoded), `"`)
}

// TestJobdefNamespaceDiffListsNamespacesTouched: the apply preview names the
// namespaces an apply would touch — both sides of a move.
func (s *IntegrationTestSuite) TestJobdefNamespaceDiffListsNamespacesTouched() {
	s.requirePolicyFixture()
	alias := fmt.Sprintf("b2-diff-move-%d", time.Now().UnixNano())
	s.applyDefinition(s.namespacedJobDefinition(alias, "marketing"))

	dir := s.writeNamespaceManifests(s.namespacedJobDefinition(alias, "finance"))
	stdout, stderr, err := s.runCLISeparate("job", "diff", "--path", dir, "--server", s.caesiumURL, "--json")
	s.Require().Error(err, "a move is an in-scope change: %s", stderr)
	s.Require().Equal(1, jobDiffCLIExitCode(err))
	var diff jobdefDiffResult
	s.Require().NoError(json.Unmarshal([]byte(stdout), &diff), "diff --json stdout must be one JSON document: %s", stdout)
	s.Equal([]string{"finance", "marketing"}, diff.Namespaces)

	text, _, err := s.runCLISeparate("job", "diff", "--path", dir, "--server", s.caesiumURL)
	s.Require().Error(err)
	s.Contains(text, "Namespaces touched: finance, marketing")
}

// TestAuthJobdefNamespaceDiffBaselineIsConfined: a marketing viewer's diff
// compares against marketing's jobs only, never returns another namespace's
// job, and never returns the configuration of a trigger it may not read (one
// shared with a finance job), in removed or in modified.
func (s *IntegrationTestSuite) TestAuthJobdefNamespaceDiffBaselineIsConfined() {
	s.requireAuthLane()
	s.requirePolicyFixture()
	stamp := time.Now().UnixNano()
	secret := fmt.Sprintf("b2-shared-secret-%d", stamp)
	mktAlias := fmt.Sprintf("b2-diff-mkt-%d", stamp)
	finAlias := fmt.Sprintf("b2-diff-fin-%d", stamp)

	mkt := s.namespacedJobDefinition(mktAlias, "marketing")
	mkt.Trigger = schema.Trigger{Type: "http", Configuration: map[string]any{"path": "/hooks/" + mktAlias, "secret": secret}}
	s.applyDefinition(mkt)
	s.applyDefinition(s.namespacedJobDefinition(finAlias, "finance"))
	mktJob, finJob := s.requireJobByAlias(mktAlias), s.requireJobByAlias(finAlias)

	// The finance job shares the marketing job's HTTP trigger (seeded).
	// Cleanups run last-in first-out: restore the finance job's trigger, then
	// close the catalog. The scenario's context is cancelled by then.
	t := s.T()
	catalog := s.openIntegrationCatalogDB()
	t.Cleanup(func() {
		if err := catalog.Close(); err != nil {
			t.Errorf("close the catalog: %v", err)
		}
	})
	var mktTrigger, finTrigger string
	s.Require().NoError(catalog.QueryRowContext(s.T().Context(), `SELECT trigger_id FROM jobs WHERE id = ?`, mktJob.ID).Scan(&mktTrigger))
	s.Require().NoError(catalog.QueryRowContext(s.T().Context(), `SELECT trigger_id FROM jobs WHERE id = ?`, finJob.ID).Scan(&finTrigger))
	_, err := catalog.ExecContext(s.T().Context(), `UPDATE jobs SET trigger_id = ? WHERE id = ?`, mktTrigger, finJob.ID)
	s.Require().NoError(err)
	t.Cleanup(func() {
		if _, err := catalog.ExecContext(context.Background(), `UPDATE jobs SET trigger_id = ? WHERE id = ?`, finTrigger, finJob.ID); err != nil {
			t.Errorf("restore the finance job's trigger: %v", err)
		}
	})

	// The cluster admin's baseline holds both jobs and the trigger secret.
	status, body := s.jobdefRequest("/v1/jobdefs/diff", s.authAPIKey, map[string]any{"definitions": []schema.Definition{}})
	s.Require().Equal(http.StatusOK, status, body)
	s.Contains(body, finAlias)
	s.Contains(body, secret, "the unrestricted baseline is unchanged")

	viewer := s.mintNamespacedKey("viewer", "marketing")
	status, body = s.jobdefRequest("/v1/jobdefs/diff", viewer.Plaintext, map[string]any{"definitions": []schema.Definition{}})
	s.Require().Equal(http.StatusOK, status, body)
	s.NotContains(body, finAlias, "a marketing viewer never sees a finance job")
	s.NotContains(body, secret, "nor a trigger it shares with one")
	var empty jobdefDiffResult
	s.Require().NoError(json.Unmarshal([]byte(body), &empty), body)
	removed := decodeDiffEntries(s, empty.Removed)
	s.Require().NotEmpty(removed)
	sawMarketing := false
	for _, entry := range removed {
		s.Equal("marketing", entry.Namespace, "removed holds only marketing jobs: %s", entry.Alias)
		if entry.Alias == mktAlias {
			sawMarketing = true
			s.Contains(entry.Trigger.Configuration, "redacted")
		}
	}
	s.True(sawMarketing, "the marketing job is in the confined baseline")

	// Modified: the CLI preview of an edit to the marketing job, carrying a
	// trigger secret of the caller's own.
	mkt.Metadata.Labels = map[string]string{"edited": "yes"}
	mkt.Trigger.Configuration = map[string]any{"path": "/hooks/" + mktAlias, "secret": "caller-supplied-secret"}
	dir := s.writeNamespaceManifests(mkt)
	stdout, stderr, err := s.runCLIWithEnv([]string{"CAESIUM_API_KEY=" + viewer.Plaintext},
		"job", "diff", "--path", dir, "--server", s.caesiumURL, "--json")
	s.Require().Error(err, "the edit is an in-scope change: %s", stderr)
	var preview jobdefDiffResult
	s.Require().NoError(json.Unmarshal([]byte(stdout), &preview), stdout)
	modified := decodeDiffEntries(s, preview.Modified)
	s.Require().Len(modified, 1)
	s.Equal(mktAlias, modified[0].Alias)
	s.NotContains(stdout, finAlias)
	s.NotContains(stdout, secret, "the stored side of the shared trigger is redacted")
	s.Contains(modified[0].Diff, "redacted")
	s.Equal([]string{"marketing"}, preview.Namespaces)
	for _, entry := range decodeDiffEntries(s, preview.WouldPrune) {
		s.Equal("marketing", entry.Namespace, entry.Alias)
	}

	// Naming the finance job's alias is refused, naming the namespace, and
	// audited; lint refuses it the same way.
	for _, path := range []string{"/v1/jobdefs/diff", "/v1/jobdefs/lint"} {
		status, body = s.jobdefRequest(path, viewer.Plaintext, map[string]any{"definitions": []schema.Definition{s.namespacedJobDefinition(finAlias, "marketing")}})
		s.Require().Equal(http.StatusForbidden, status, "%s: %s", path, body)
		s.Contains(body, jsonEscaped(`insufficient permissions: viewer required in namespace "finance"`), path)
	}
	rows, err := s.auditCLI("--actor", viewer.Meta.KeyPrefix, "--action", "auth.denied")
	s.Require().NoError(err)
	denied := map[string]auditRow{}
	for _, row := range rows {
		var meta struct {
			Path       string   `json:"path"`
			Namespaces []string `json:"namespaces"`
		}
		s.Require().NoError(json.Unmarshal(row.Metadata, &meta), string(row.Metadata))
		denied[meta.Path] = row
		s.Contains(meta.Namespaces, "finance", "the denial lists the namespace checked: %s", string(row.Metadata))
	}
	s.Contains(denied, "/v1/jobdefs/diff")
	s.Contains(denied, "/v1/jobdefs/lint")
}

// TestAuthJobdefNamespaceLintRedactsForeignJobs: lint still validates
// against every job, but a marketing viewer's errors and contract findings
// never name a finance job.
func (s *IntegrationTestSuite) TestAuthJobdefNamespaceLintRedactsForeignJobs() {
	s.requireAuthLane()
	s.requirePolicyFixture()
	stamp := time.Now().UnixNano()
	viewer := s.mintNamespacedKey("viewer", "marketing")

	// A dataset produced by a finance job: a marketing producer of the same
	// dataset is a duplicate producer, reported without the finance alias.
	dataset := fmt.Sprintf("b2-dataset-%d", stamp)
	finProducer := fmt.Sprintf("b2-lint-fin-producer-%d", stamp)
	mktProducer := fmt.Sprintf("b2-lint-mkt-producer-%d", stamp)
	producer := func(alias, namespace string) schema.Definition {
		def := s.namespacedJobDefinition(alias, namespace)
		def.Steps[0].Datasets = &schema.StepDatasets{Produces: []schema.ProducedDataset{{Name: dataset}}}
		return def
	}
	s.applyDefinition(producer(finProducer, "finance"))

	admin := s.serverLint(s.authAPIKey, producer(mktProducer, "marketing"))
	s.Require().NotEmpty(admin.Errors)
	s.Contains(strings.Join(admin.messages(), "\n"), finProducer, "the cluster admin's error names the finance producer")

	confined := s.serverLint(viewer.Plaintext, producer(mktProducer, "marketing"))
	s.Require().NotEmpty(confined.Errors, "the duplicate producer is still found")
	joined := strings.Join(confined.messages(), "\n")
	s.NotContains(joined, finProducer)
	s.Contains(joined, mktProducer)
	s.Contains(joined, fmt.Sprintf("%q", dataset), "the caller's own dataset is named")
	s.Contains(joined, "1 job(s) in other namespaces")

	// A finance consumer of a marketing producer's outputs: the breaking
	// change is reported to the cluster admin naming the consumer, and to a
	// marketing viewer not at all.
	contractProducer := fmt.Sprintf("b2-contract-producer-%d", stamp)
	contractConsumer := fmt.Sprintf("b2-contract-consumer-%d", stamp)
	inNamespace := func(manifest, namespace string) string {
		return strings.Replace(manifest, "metadata:\n", "metadata:\n  namespace: "+namespace+"\n", 1)
	}
	applied := s.writeContractManifests(map[string]string{
		contractProducer: inNamespace(contractProducerManifest(contractProducer, []string{"customer_id", "row_count"}), "marketing"),
		contractConsumer: inNamespace(contractConsumerManifest(contractConsumer, contractProducer, "reporting", "customer", "customer_id"), "finance"),
	})
	defer os.RemoveAll(applied)
	s.runCLI("job", "apply", "--path", applied, "--server", s.caesiumURL)
	broken := s.writeContractManifests(map[string]string{
		contractProducer: inNamespace(contractProducerManifest(contractProducer, []string{"row_count"}), "marketing"),
	})
	defer os.RemoveAll(broken)

	stdout, err := s.runCLIStdout("job", "lint", "--path", broken, "--server", s.caesiumURL, "--json")
	s.Require().Error(err)
	var adminContract jobdefLintResult
	s.Require().NoError(json.Unmarshal([]byte(stdout), &adminContract), stdout)
	s.Require().NotNil(adminContract.Contracts)
	s.True(contractCLIFindingsNameConsumer(adminContract.Contracts.Breaking, contractConsumer), "%+v", adminContract.Contracts)

	stdout, _, err = s.runCLIWithEnv([]string{"CAESIUM_API_KEY=" + viewer.Plaintext},
		"job", "lint", "--path", broken, "--server", s.caesiumURL, "--json")
	s.Require().NoError(err, "no finding the marketing viewer can see: %s", stdout)
	var confinedContract jobdefLintResult
	s.Require().NoError(json.Unmarshal([]byte(stdout), &confinedContract), stdout)
	s.Require().NotNil(confinedContract.Contracts)
	s.Empty(confinedContract.Contracts.Breaking)
	s.NotContains(stdout, contractConsumer)

	// An inferred finding is derived from the consumer's trigger (its event
	// pattern and paramMapping). Between two marketing jobs the viewer sees
	// it; once that trigger also serves a finance job (seeded), the viewer may
	// not read the trigger, and the finding is withheld.
	sharedProducer := fmt.Sprintf("b2-shared-producer-%d", stamp)
	sharedConsumer := fmt.Sprintf("b2-shared-consumer-%d", stamp)
	sharedApplied := s.writeContractManifests(map[string]string{
		sharedProducer: inNamespace(contractProducerManifest(sharedProducer, []string{"customer_id", "row_count"}), "marketing"),
		sharedConsumer: inNamespace(contractConsumerManifest(sharedConsumer, sharedProducer, "reporting", "customer", "customer_id"), "marketing"),
	})
	defer os.RemoveAll(sharedApplied)
	s.runCLI("job", "apply", "--path", sharedApplied, "--server", s.caesiumURL)
	sharedBroken := s.writeContractManifests(map[string]string{
		sharedProducer: inNamespace(contractProducerManifest(sharedProducer, []string{"row_count"}), "marketing"),
	})
	defer os.RemoveAll(sharedBroken)
	viewerLint := func() (jobdefLintResult, error) {
		stdout, _, err := s.runCLIWithEnv([]string{"CAESIUM_API_KEY=" + viewer.Plaintext},
			"job", "lint", "--path", sharedBroken, "--server", s.caesiumURL, "--json")
		var out jobdefLintResult
		s.Require().NoError(json.Unmarshal([]byte(stdout), &out), stdout)
		s.Require().NotNil(out.Contracts, stdout)
		return out, err
	}
	before, err := viewerLint()
	s.Require().Error(err, "the breaking change between two marketing jobs is the viewer's to see")
	s.True(contractCLIFindingsNameConsumer(before.Contracts.Breaking, sharedConsumer), "%+v", before.Contracts)

	finSharer := fmt.Sprintf("b2-shared-trigger-fin-%d", stamp)
	s.applyDefinition(s.namespacedJobDefinition(finSharer, "finance"))
	consumerID, finID := s.requireJobByAlias(sharedConsumer).ID, s.requireJobByAlias(finSharer).ID
	t := s.T()
	catalog := s.openIntegrationCatalogDB()
	t.Cleanup(func() {
		if err := catalog.Close(); err != nil {
			t.Errorf("close the catalog: %v", err)
		}
	})
	var consumerTrigger, finTrigger string
	s.Require().NoError(catalog.QueryRowContext(t.Context(), `SELECT trigger_id FROM jobs WHERE id = ?`, consumerID).Scan(&consumerTrigger))
	s.Require().NoError(catalog.QueryRowContext(t.Context(), `SELECT trigger_id FROM jobs WHERE id = ?`, finID).Scan(&finTrigger))
	_, err = catalog.ExecContext(t.Context(), `UPDATE jobs SET trigger_id = ? WHERE id = ?`, consumerTrigger, finID)
	s.Require().NoError(err)
	t.Cleanup(func() {
		if _, err := catalog.ExecContext(context.Background(), `UPDATE jobs SET trigger_id = ? WHERE id = ?`, finTrigger, finID); err != nil {
			t.Errorf("restore the finance job's trigger: %v", err)
		}
	})

	after, err := viewerLint()
	s.Require().NoError(err, "the finding derived from a trigger shared with finance is withheld")
	s.Empty(after.Contracts.Breaking)
	stdout, err = s.runCLIStdout("job", "lint", "--path", sharedBroken, "--server", s.caesiumURL, "--json")
	s.Require().Error(err)
	var adminShared jobdefLintResult
	s.Require().NoError(json.Unmarshal([]byte(stdout), &adminShared), stdout)
	s.True(contractCLIFindingsNameConsumer(adminShared.Contracts.Breaking, sharedConsumer), "the cluster admin still sees it: %+v", adminShared.Contracts)
}

// findJobByAlias returns the live job with alias, or nil.
func (s *IntegrationTestSuite) findJobByAlias(alias string) *jobSummary {
	s.T().Helper()
	var jobs []jobSummary
	s.getJSON("/v1/jobs", &jobs)
	for i := range jobs {
		if jobs[i].Alias == alias {
			return &jobs[i]
		}
	}
	return nil
}

// TestIncidentApplyJobdefPatchRefusesUndeclaredNamespace: an approved agent
// patch on a GIT-SYNCED job (that route renders a dry-run diff and escalates
// it to a human instead of applying) which moves the job into a namespace the
// policy fixture does not declare is refused at execution, as the direct
// route is. Escalating it would hand a human a manifest whose commit then
// fails git sync's validation and stops the server (B2).
func (s *IntegrationTestSuite) TestIncidentApplyJobdefPatchRefusesUndeclaredNamespace() {
	s.requireAuthLane()
	s.requirePolicyFixture()

	alias := fmt.Sprintf("b2-git-patch-%d", time.Now().UnixNano())
	def := failingJobDefinition(alias, s.engineType)
	def.Metadata.Remediation = &schema.MetadataRemediation{
		Profile: "triage-only",
		Classes: []string{"unknown"},
	}
	// Applied with git provenance, as git sync records it: the patch takes
	// the escalate route.
	payload, err := json.Marshal(map[string]any{
		"definitions": []schema.Definition{def},
		"provenance": map[string]any{
			"source_id": "b2-it-git-" + alias,
			"repo":      "https://git.example.invalid/jobs.git",
			"ref":       "main",
			"path":      alias + ".job.yaml",
		},
	})
	s.Require().NoError(err)
	resp, err := s.doJSONRequest(http.MethodPost, s.caesiumURL+"/v1/jobdefs/apply", bytes.NewReader(payload))
	s.Require().NoError(err)
	applied, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, resp.StatusCode, "apply %s: %s", alias, applied)

	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)
	s.awaitRun(job.ID, runID, runTimeout)
	incident := s.awaitIncidentForJobTask(job.ID, "gate", 60*time.Second)
	token := s.mintAgentSessionToken(incident.ID, alias)

	moved := def
	moved.Metadata.Namespace = "sales"
	patch, err := json.Marshal(moved)
	s.Require().NoError(err)
	status, body := s.postWithToken(
		fmt.Sprintf("%s/v1/agent/incidents/%s/actions", s.caesiumURL, incident.ID),
		token,
		map[string]any{
			"type":   "apply_jobdef_patch",
			"params": map[string]any{"definition": json.RawMessage(patch)},
		})
	s.Require().Equal(http.StatusAccepted, status, string(body))
	var proposal struct {
		Action approvalAction `json:"action"`
	}
	s.Require().NoError(json.Unmarshal(body, &proposal))

	detail := s.awaitIncidentStatus(incident.ID, "awaiting_approval", 30*time.Second)
	var pending approvalRequest
	for _, a := range detail.Approvals {
		if a.ActionID == proposal.Action.ID && a.Decision == "pending" {
			pending = a
			break
		}
	}
	s.Require().NotEmpty(pending.ID, "no pending approval for the namespace-moving patch")
	out, err := s.runCLIStdout(s.incidentCLIArgs("approve", incident.ID,
		"--approval", pending.ID, "--reason", "move to the sales team")...)
	s.Require().NoError(err, "caesium incident approve failed:\n%s", out)

	after := s.incidentDetail(incident.ID)
	executed := requireActionByID(s, after, proposal.Action.ID)
	s.Require().Equal("failed", executed.Status,
		"an approved git-synced patch into an undeclared namespace must be refused, not escalated")
	var result map[string]any
	s.Require().NoError(json.Unmarshal(executed.Result, &result))
	s.Require().Contains(fmt.Sprint(result["error"]), undeclaredNamespaceMessage(alias, "sales"),
		"the refusal names the namespace and the job: %v", result)
	s.NotContains(fmt.Sprint(result), "escalate", "nothing was escalated: %v", result)
	s.NotEqual("escalated", after.Incident.Status)

	var jobs []struct {
		Alias     string `json:"alias"`
		Namespace string `json:"namespace"`
	}
	s.getJSON("/v1/jobs", &jobs)
	for _, j := range jobs {
		if j.Alias == alias {
			s.Equal("default", j.Namespace, "the job did not move")
		}
	}
}
