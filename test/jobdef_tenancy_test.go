//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
)

// Confined job writes and trigger tenancy (identity-and-access B9), driven
// through the real REST surface and `caesium job apply`.

// b9Def is a never-firing one-step job in namespace; a non-empty path gives
// it an HTTP trigger on that webhook path.
func (s *IntegrationTestSuite) b9Def(alias, namespace, path string) schema.Definition {
	if path == "" {
		return s.namespacedJobDefinition(alias, namespace)
	}
	def := s.namespacedJobDefinition(alias, namespace)
	def.Trigger = schema.Trigger{Type: "http", Configuration: map[string]any{"path": path}}
	return def
}

func (s *IntegrationTestSuite) b9Apply(token string, prune bool, defs ...schema.Definition) (int, string) {
	s.T().Helper()
	return s.jobdefRequest("/v1/jobdefs/apply", token, map[string]any{"definitions": defs, "prune": prune})
}

// b9Job is the admin's view of a live job, nil when the alias has no live job.
type b9Job struct {
	ID        string `json:"id"`
	Alias     string `json:"alias"`
	Namespace string `json:"namespace"`
	TriggerID string `json:"trigger_id"`
	UpdatedAt string `json:"updated_at"`
}

func (s *IntegrationTestSuite) b9Job(alias string) *b9Job {
	s.T().Helper()
	var jobs []b9Job
	s.getJSON("/v1/jobs?order_by=created_at%20desc", &jobs)
	for i := range jobs {
		if jobs[i].Alias == alias {
			return &jobs[i]
		}
	}
	return nil
}

func (s *IntegrationTestSuite) b9RequireJob(alias string) *b9Job {
	s.T().Helper()
	job := s.b9Job(alias)
	s.Require().NotNil(job, "job %s must be live", alias)
	return job
}

// b9Cleanup deletes the scenario's jobs (live ones; a pruned job is gone).
func (s *IntegrationTestSuite) b9Cleanup(aliases ...string) {
	s.T().Cleanup(func() {
		for _, alias := range aliases {
			if job := s.b9Job(alias); job != nil {
				status, body := s.requestWithKey(http.MethodDelete, "/v1/jobs/"+job.ID, s.authAPIKey, nil)
				s.Contains([]int{http.StatusAccepted, http.StatusNotFound}, status, "delete %s: %s", alias, body)
			}
		}
	})
}

// b9Share points job b at job a's trigger row: a legacy shared row, which no
// API path creates, seeded through the catalog as the precondition only.
func (s *IntegrationTestSuite) b9Share(a, b string) {
	s.T().Helper()
	catalog := s.openIntegrationCatalogDB()
	defer func() { s.NoError(catalog.Close()) }()
	jobA, jobB := s.b9RequireJob(a), s.b9RequireJob(b)
	_, err := catalog.ExecContext(s.T().Context(), `UPDATE jobs SET trigger_id = ? WHERE id = ?`, jobA.TriggerID, jobB.ID)
	s.Require().NoError(err)
	original := jobB.TriggerID
	s.T().Cleanup(func() {
		catalog := s.openIntegrationCatalogDB()
		defer func() { s.NoError(catalog.Close()) }()
		_, err := catalog.ExecContext(s.T().Context(), `UPDATE jobs SET trigger_id = ? WHERE id = ?`, original, jobB.ID)
		s.NoError(err)
	})
}

// TestAuthJobdefApplyConfinedMoveAndPrune: a marketing operator applies into
// marketing with `caesium job apply`; moving the job to finance needs operator
// in both (refused with one, nothing changed); and its prune retires only
// marketing jobs, keeping the trigger row a retained job still uses.
func (s *IntegrationTestSuite) TestAuthJobdefApplyConfinedMoveAndPrune() {
	s.requireAuthLane()
	stamp := time.Now().UnixNano()
	mktOperator := s.mintNamespacedKey("operator", "marketing")
	both := s.mintNamespacedKey("operator", "marketing", "finance")
	alias := fmt.Sprintf("b9-move-%d", stamp)
	other := fmt.Sprintf("b9-move-other-%d", stamp)
	s.b9Cleanup(alias, other)

	dir := s.writeNamespaceManifests(s.b9Def(alias, "marketing", ""))
	stdout, stderr, err := s.runCLIWithEnv([]string{"CAESIUM_API_KEY=" + mktOperator.Plaintext}, "job", "apply", "--path", dir, "--server", s.caesiumURL)
	s.Require().NoError(err, "apply into marketing: %s %s", stdout, stderr)
	s.Equal("marketing", s.b9RequireJob(alias).Namespace)

	status, body := s.b9Apply(mktOperator.Plaintext, false, s.b9Def(other, "marketing", ""), s.b9Def(alias, "finance", ""))
	s.Require().Equal(http.StatusForbidden, status, body)
	s.Contains(body, `operator required in namespace \"finance\"`)
	s.Equal("marketing", s.b9RequireJob(alias).Namespace, "the refused move changed nothing")
	s.Nil(s.b9Job(other), "nor applied the rest of the batch")

	status, body = s.b9Apply(both.Plaintext, false, s.b9Def(alias, "finance", ""))
	s.Require().Equal(http.StatusOK, status, body)
	s.Equal("finance", s.b9RequireJob(alias).Namespace)

	// Prune: marketing jobs absent from the request are retired; finance and
	// default jobs are never candidates for a marketing operator. The
	// retained job keeps a trigger row the retired one shared (B9 fold 3).
	kept := fmt.Sprintf("b9-prune-kept-%d", stamp)
	gone := fmt.Sprintf("b9-prune-gone-%d", stamp)
	stays := fmt.Sprintf("b9-prune-default-%d", stamp)
	path := fmt.Sprintf("/hooks/b9-prune-%d", stamp)
	s.b9Cleanup(kept, gone, stays)
	s.applyDefinition(s.b9Def(kept, "marketing", path))
	s.applyDefinition(s.b9Def(gone, "marketing", ""))
	s.applyDefinition(s.b9Def(stays, "default", ""))
	s.b9Share(kept, gone)
	keptTrigger := s.b9RequireJob(kept).TriggerID

	// A marketing job another scenario left running would fail the prune
	// (409, naming that marketing job): wait it out.
	s.Require().Eventually(func() bool {
		status, body = s.b9Apply(mktOperator.Plaintext, true, s.b9Def(kept, "marketing", path))
		return status != http.StatusConflict || !strings.Contains(body, "has a running run")
	}, 60*time.Second, 2*time.Second)
	s.Require().Equal(http.StatusOK, status, body)
	var resp struct {
		Pruned int `json:"pruned"`
	}
	s.Require().NoError(json.Unmarshal([]byte(body), &resp), body)
	s.GreaterOrEqual(resp.Pruned, 1)
	s.Nil(s.b9Job(gone), "the absent marketing job is retired")
	s.NotNil(s.b9Job(stays), "a default job is never a marketing operator's candidate")
	s.NotNil(s.b9Job(alias), "nor a finance one")
	s.Require().Equal(keptTrigger, s.b9RequireJob(kept).TriggerID)
	s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/triggers/"+keptTrigger, s.authAPIKey, "")
	hook, err := http.NewRequestWithContext(s.T().Context(), http.MethodPost, s.caesiumURL+"/v1/hooks"+path, strings.NewReader(`{}`))
	s.Require().NoError(err)
	hook.Header.Set("Content-Type", "application/json")
	hookResp, err := http.DefaultClient.Do(hook)
	s.Require().NoError(err)
	s.NoError(hookResp.Body.Close())
	s.Equal(http.StatusAccepted, hookResp.StatusCode, "the retained job's webhook still delivers")
}

// TestAuthJobdefApplyLateRefusalRollsBack: a confined batch whose second
// definition subscribes a webhook path a finance job is on is refused inside
// the apply transaction; the first definition is not applied and the refusal
// names neither the finance job nor its namespace. A job-scoped key's batch is
// all-or-nothing the same way.
func (s *IntegrationTestSuite) TestAuthJobdefApplyLateRefusalRollsBack() {
	s.requireAuthLane()
	stamp := time.Now().UnixNano()
	mktOperator := s.mintNamespacedKey("operator", "marketing")
	fin := fmt.Sprintf("b9-late-fin-%d", stamp)
	first := fmt.Sprintf("b9-late-first-%d", stamp)
	second := fmt.Sprintf("b9-late-second-%d", stamp)
	path := fmt.Sprintf("/hooks/b9-late-%d", stamp)
	s.b9Cleanup(fin, first, second)
	s.applyDefinition(s.b9Def(fin, "finance", path))
	before := s.b9RequireJob(fin)

	dir := s.writeNamespaceManifests(s.b9Def(first, "marketing", ""), s.b9Def(second, "marketing", path))
	stdout, stderr, err := s.runCLIWithEnv([]string{"CAESIUM_API_KEY=" + mktOperator.Plaintext}, "job", "apply", "--path", dir, "--server", s.caesiumURL)
	s.Require().Error(err, "the apply must be refused: %s", stdout)
	combined := stdout + stderr
	s.Contains(combined, "is subscribed by a job outside your access")
	s.NotContains(combined, fin)
	s.NotContains(combined, "finance")
	s.Nil(s.b9Job(first), "the whole batch rolled back")
	s.Nil(s.b9Job(second))
	s.Equal(before.UpdatedAt, s.b9RequireJob(fin).UpdatedAt, "the finance job is untouched")

	// A job-scoped operator key at `*` takes the same one-transaction apply
	// (B9 fold 2): its second definition subscribes the finance job's path,
	// which the key's alias scope does not cover, so neither is applied.
	scopedFirst := fmt.Sprintf("b9-late-sfirst-%d", stamp)
	scopedSecond := fmt.Sprintf("b9-late-ssecond-%d", stamp)
	s.b9Cleanup(scopedFirst, scopedSecond)
	scoped := s.createAPIKeyCLI("--role", "operator", "--scope-jobs", scopedFirst+","+scopedSecond, "--description", fmt.Sprintf("b9 late scoped %d", stamp))
	s.T().Cleanup(func() {
		_, _ = s.runCLIStdout("auth", "key", "revoke", "--id", scoped.Meta.ID, "--server", s.caesiumURL)
	})
	status, body := s.b9Apply(scoped.Plaintext, false, s.b9Def(scopedFirst, "default", ""), s.b9Def(scopedSecond, "default", path))
	s.Require().Equal(http.StatusForbidden, status, body)
	s.Contains(body, "is subscribed by a job outside your access")
	s.NotContains(body, fin)
	s.NotContains(body, "finance")
	s.Nil(s.b9Job(scopedFirst), "the scoped key's whole batch rolled back")
	s.Nil(s.b9Job(scopedSecond))
	s.Equal(before.UpdatedAt, s.b9RequireJob(fin).UpdatedAt, "the finance job is still untouched")
}

// TestAuthTriggerTenancyScopedKeyAndPatch: a job-scoped operator key (cluster
// role, alias scope) cannot rewrite a trigger row shared with a job outside
// its scope, nor subscribe a webhook path such a job is on; a marketing
// operator may PATCH its own trigger but not move it onto a finance job's
// path, nor PATCH a trigger also serving finance.
func (s *IntegrationTestSuite) TestAuthTriggerTenancyScopedKeyAndPatch() {
	s.requireAuthLane()
	stamp := time.Now().UnixNano()
	mine := fmt.Sprintf("b9-scoped-mine-%d", stamp)
	peer := fmt.Sprintf("b9-scoped-peer-%d", stamp)
	busy := fmt.Sprintf("b9-scoped-busy-%d", stamp)
	busyPath := fmt.Sprintf("/hooks/b9-scoped-taken-%d", stamp)
	s.b9Cleanup(mine, peer, busy)
	s.applyDefinition(s.b9Def(mine, "default", ""))
	s.applyDefinition(s.b9Def(peer, "default", ""))
	s.applyDefinition(s.b9Def(busy, "default", busyPath))
	scoped := s.createAPIKeyCLI("--role", "operator", "--scope-jobs", mine, "--description", fmt.Sprintf("b9 scoped %d", stamp))
	s.T().Cleanup(func() {
		_, _ = s.runCLIStdout("auth", "key", "revoke", "--id", scoped.Meta.ID, "--server", s.caesiumURL)
	})

	status, body := s.b9Apply(scoped.Plaintext, false, s.b9Def(mine, "default", busyPath))
	s.Require().Equal(http.StatusForbidden, status, body)
	s.Contains(body, "is subscribed by a job outside your access")
	s.NotContains(body, busy)

	s.b9Share(mine, peer)
	status, body = s.b9Apply(scoped.Plaintext, false, s.b9Def(mine, "default", ""))
	s.Require().Equal(http.StatusForbidden, status, body)
	s.Contains(body, "also serves a job outside your access")
	s.NotContains(body, peer)

	// PATCH by a namespace-confined operator.
	mktOperator := s.mintNamespacedKey("operator", "marketing")
	mkt := fmt.Sprintf("b9-patch-mkt-%d", stamp)
	fin := fmt.Sprintf("b9-patch-fin-%d", stamp)
	finPath := fmt.Sprintf("/hooks/b9-patch-taken-%d", stamp)
	s.b9Cleanup(mkt, fin)
	s.applyDefinition(s.b9Def(mkt, "marketing", fmt.Sprintf("/hooks/b9-patch-mkt-%d", stamp)))
	s.applyDefinition(s.b9Def(fin, "finance", finPath))
	mktTrigger := s.b9RequireJob(mkt).TriggerID
	renamed := fmt.Sprintf("b9-patch-renamed-%d", stamp)
	s.requireNSStatus(http.StatusOK, http.MethodPatch, "/v1/triggers/"+mktTrigger, mktOperator.Plaintext, fmt.Sprintf(`{"alias":%q}`, renamed))
	status, body = s.nsRequest(http.MethodPatch, "/v1/triggers/"+mktTrigger, mktOperator.Plaintext, fmt.Sprintf(`{"configuration":{"path":%q}}`, finPath), nil)
	s.Require().Equal(http.StatusForbidden, status, body)
	s.NotContains(body, fin)
	s.NotContains(s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/triggers/"+mktTrigger, s.authAPIKey, ""), "b9-patch-taken", "the refused PATCH changed nothing")
	if !authLaneWithoutPolicy() {
		// With a policy file the cluster admin is held to one namespace per
		// webhook path too, told which namespace holds it.
		status, body = s.nsRequest(http.MethodPatch, "/v1/triggers/"+mktTrigger, s.authAPIKey, fmt.Sprintf(`{"configuration":{"path":%q}}`, finPath), nil)
		s.Require().Equal(http.StatusConflict, status, body)
		s.Contains(body, `namespace \"finance\"`)
	}
	s.b9Share(mkt, fin)
	s.requireNSStatus(http.StatusForbidden, http.MethodPatch, "/v1/triggers/"+mktTrigger, mktOperator.Plaintext, `{}`)
}

// TestAuthJobdefApplyRepairsLegacySharedTrigger: a legacy trigger row serving
// a marketing and a finance job is repaired by an apply from an operator in
// both namespaces: the written marketing job moves onto a fresh row (its new
// path delivering), and the finance job keeps the old row, its configuration
// and its deliveries, untouched (B9 fold 12).
func (s *IntegrationTestSuite) TestAuthJobdefApplyRepairsLegacySharedTrigger() {
	s.requireAuthLane()
	s.requirePolicyFixture()
	stamp := time.Now().UnixNano()
	both := s.mintNamespacedKey("operator", "marketing", "finance")
	mkt := fmt.Sprintf("b9-repair-mkt-%d", stamp)
	fin := fmt.Sprintf("b9-repair-fin-%d", stamp)
	oldPath := fmt.Sprintf("/hooks/b9-repair-old-%d", stamp)
	newPath := fmt.Sprintf("/hooks/b9-repair-new-%d", stamp)
	s.b9Cleanup(mkt, fin)
	s.applyDefinition(s.b9Def(mkt, "marketing", oldPath))
	s.applyDefinition(s.b9Def(fin, "finance", ""))
	s.b9Share(mkt, fin)
	oldRow := s.b9RequireJob(mkt).TriggerID
	s.Require().Equal(oldRow, s.b9RequireJob(fin).TriggerID)
	oldTrigger := s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/triggers/"+oldRow, s.authAPIKey, "")

	status, body := s.b9Apply(both.Plaintext, false, s.b9Def(mkt, "marketing", newPath))
	s.Require().Equal(http.StatusOK, status, body)
	repaired := s.b9RequireJob(mkt)
	s.NotEqual(oldRow, repaired.TriggerID, "the written job moved onto a fresh trigger row")
	s.Equal(oldRow, s.b9RequireJob(fin).TriggerID, "the finance job keeps the old row")
	s.Equal(oldTrigger, s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/triggers/"+oldRow, s.authAPIKey, ""),
		"the old row, its configuration included, is untouched")
	s.Contains(s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/triggers/"+repaired.TriggerID, s.authAPIKey, ""), "b9-repair-new-")

	for _, path := range []string{newPath, oldPath} {
		hook, err := http.NewRequestWithContext(s.T().Context(), http.MethodPost, s.caesiumURL+"/v1"+path, strings.NewReader(`{}`))
		s.Require().NoError(err)
		hook.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(hook)
		s.Require().NoError(err)
		s.NoError(resp.Body.Close())
		s.Equal(http.StatusAccepted, resp.StatusCode, "%s still delivers after the repair", path)
	}
}

// TestAuthTriggerTenancyWebhookPathRace:a marketing and a finance operator
// race to subscribe one new webhook path; exactly one wins and the other gets
// a clean, non-disclosing refusal.
func (s *IntegrationTestSuite) TestAuthTriggerTenancyWebhookPathRace() {
	s.requireAuthLane()
	stamp := time.Now().UnixNano()
	mktOperator := s.mintNamespacedKey("operator", "marketing")
	finOperator := s.mintNamespacedKey("operator", "finance")
	path := fmt.Sprintf("/hooks/b9-race-%d", stamp)
	mkt, fin := fmt.Sprintf("b9-race-mkt-%d", stamp), fmt.Sprintf("b9-race-fin-%d", stamp)
	s.b9Cleanup(mkt, fin)

	type outcome struct {
		status int
		body   string
	}
	results := make([]outcome, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, call := range []struct {
		token string
		def   schema.Definition
	}{{mktOperator.Plaintext, s.b9Def(mkt, "marketing", path)}, {finOperator.Plaintext, s.b9Def(fin, "finance", path)}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			payload, _ := json.Marshal(map[string]any{"definitions": []schema.Definition{call.def}})
			req, err := http.NewRequestWithContext(s.T().Context(), http.MethodPost, s.caesiumURL+"/v1/jobdefs/apply", strings.NewReader(string(payload)))
			if err != nil {
				results[i] = outcome{body: err.Error()}
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+call.token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				results[i] = outcome{body: err.Error()}
				return
			}
			defer func() { _ = resp.Body.Close() }()
			data, _ := io.ReadAll(resp.Body)
			results[i] = outcome{status: resp.StatusCode, body: string(data)}
		}()
	}
	close(start)
	wg.Wait()

	statuses := []int{results[0].status, results[1].status}
	s.ElementsMatch([]int{http.StatusOK, http.StatusForbidden}, statuses, "%+v", results)
	for i, result := range results {
		if result.status == http.StatusForbidden {
			s.Contains(result.body, "is subscribed", "%+v", result)
			s.NotContains(result.body, []string{fin, mkt}[1-i], "the refusal does not name the winner")
		}
	}
	live := 0
	for _, alias := range []string{mkt, fin} {
		if s.b9Job(alias) != nil {
			live++
		}
	}
	s.Equal(1, live, "exactly one job subscribes the path")
}

// TestAuthJobManifestConfinedViewer: a marketing viewer exports its own job's
// manifest (trigger secret included), but a trigger the job shares with a
// finance job is withheld, and a finance job's manifest is refused; the
// cluster admin's export is unchanged.
func (s *IntegrationTestSuite) TestAuthJobManifestConfinedViewer() {
	s.requireAuthLane()
	stamp := time.Now().UnixNano()
	viewer := s.mintNamespacedKey("viewer", "marketing")
	mkt := fmt.Sprintf("b9-manifest-mkt-%d", stamp)
	fin := fmt.Sprintf("b9-manifest-fin-%d", stamp)
	secret := fmt.Sprintf("b9-manifest-secret-%d", stamp)
	s.b9Cleanup(mkt, fin)
	def := s.b9Def(mkt, "marketing", fmt.Sprintf("/hooks/b9-manifest-%d", stamp))
	def.Trigger.Configuration["secret"] = secret
	s.applyDefinition(def)
	s.applyDefinition(s.b9Def(fin, "finance", ""))
	mktJob := s.b9RequireJob(mkt)

	s.Contains(s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/jobs/"+mktJob.ID+"/manifest", viewer.Plaintext, ""), secret)
	s.requireNSStatus(http.StatusForbidden, http.MethodGet, "/v1/jobs/"+s.b9RequireJob(fin).ID+"/manifest", viewer.Plaintext, "")

	s.b9Share(mkt, fin)
	confinedBody := s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/jobs/"+mktJob.ID[:13]+"/manifest?format=json", viewer.Plaintext, "")
	s.NotContains(confinedBody, secret, "a trigger shared with a finance job is withheld")
	s.Contains(confinedBody, "redacted")
	s.NotContains(confinedBody, fin)
	s.Contains(s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/jobs/"+mktJob.ID+"/manifest", s.authAPIKey, ""), secret, "the cluster admin is unchanged")
}

// TestAuthLegacyJobCreateConfined: the legacy create is namespaced on the
// body's metadata.namespace and the stored namespace of its alias, and cannot
// subscribe a webhook path a job outside the caller's namespaces is on.
func (s *IntegrationTestSuite) TestAuthLegacyJobCreateConfined() {
	s.requireAuthLane()
	stamp := time.Now().UnixNano()
	mktOperator := s.mintNamespacedKey("operator", "marketing")
	fin := fmt.Sprintf("b9-legacy-fin-%d", stamp)
	finPath := fmt.Sprintf("/hooks/b9-legacy-taken-%d", stamp)
	created := fmt.Sprintf("b9-legacy-mkt-%d", stamp)
	s.b9Cleanup(fin, created)
	s.applyDefinition(s.b9Def(fin, "finance", finPath))
	body := func(alias, namespace, path string) string {
		payload, err := json.Marshal(map[string]any{
			"alias":    alias,
			"metadata": map[string]any{"namespace": namespace},
			"trigger":  map[string]any{"alias": alias, "type": "http", "configuration": map[string]any{"path": path}},
			"tasks":    []any{map[string]any{"atom": map[string]any{"engine": s.engineOrDocker(), "image": "busybox:1.36.1", "command": []string{"true"}}}},
		})
		s.Require().NoError(err)
		return string(payload)
	}

	s.requireNSStatus(http.StatusCreated, http.MethodPost, "/v1/jobs", mktOperator.Plaintext, body(created, "marketing", fmt.Sprintf("/hooks/b9-legacy-mkt-%d", stamp)))
	s.Equal("marketing", s.b9RequireJob(created).Namespace)

	out := s.requireNSStatus(http.StatusForbidden, http.MethodPost, "/v1/jobs", mktOperator.Plaintext, body(fmt.Sprintf("b9-legacy-x-%d", stamp), "finance", "/hooks/x"))
	s.Contains(out, `operator required in namespace \"finance\"`)
	out = s.requireNSStatus(http.StatusForbidden, http.MethodPost, "/v1/jobs", mktOperator.Plaintext, body(fin, "marketing", "/hooks/y"))
	s.Contains(out, `operator required in namespace \"finance\"`, "an alias living in finance")
	out = s.requireNSStatus(http.StatusForbidden, http.MethodPost, "/v1/jobs", mktOperator.Plaintext, body(fmt.Sprintf("b9-legacy-z-%d", stamp), "marketing", finPath))
	s.Contains(out, "is subscribed by a job outside your access")
	s.NotContains(out, fin)
}

func (s *IntegrationTestSuite) engineOrDocker() string {
	if s.engineType != "" {
		return s.engineType
	}
	return "docker"
}

// TestAuthJobdefApplyRedactsForeignContractFindings: a marketing producer's
// breaking change to an output a finance consumer reads is blocked (409) for
// a marketing operator without naming the consumer, its team, or the key;
// acknowledging it succeeds with the consumer's warning withheld. The cluster
// admin's 409 names the consumer; a key at `*` scoped to the producer does
// not see it.
func (s *IntegrationTestSuite) TestAuthJobdefApplyRedactsForeignContractFindings() {
	s.requireAuthLane()
	stamp := time.Now().UnixNano()
	mktOperator := s.mintNamespacedKey("operator", "marketing")
	producer := fmt.Sprintf("b9-contract-producer-%d", stamp)
	consumer := fmt.Sprintf("b9-contract-consumer-%d", stamp)
	team := fmt.Sprintf("b9team%d", stamp)
	s.b9Cleanup(producer, consumer)
	inNamespace := func(manifest, namespace string) string {
		return strings.Replace(manifest, "metadata:\n", "metadata:\n  namespace: "+namespace+"\n", 1)
	}
	applied := s.writeContractManifests(map[string]string{
		producer: inNamespace(contractProducerManifest(producer, []string{"customer_id", "row_count"}), "marketing"),
		consumer: inNamespace(contractConsumerManifest(consumer, producer, team, "customer", "customer_id"), "finance"),
	})
	defer os.RemoveAll(applied)
	s.runCLI("job", "apply", "--path", applied, "--server", s.caesiumURL)
	broken := s.writeContractManifests(map[string]string{
		producer: inNamespace(contractProducerManifest(producer, []string{"row_count"}), "marketing"),
	})
	defer os.RemoveAll(broken)

	stdout, stderr, err := s.runCLIWithEnv([]string{"CAESIUM_API_KEY=" + mktOperator.Plaintext}, "job", "apply", "--path", broken, "--server", s.caesiumURL)
	s.Require().Error(err, "the break is blocked: %s", stdout)
	combined := stdout + stderr
	s.Contains(combined, "a contract with a job outside your namespaces is affected")
	for _, foreign := range []string{consumer, team, "customer_id", "finance"} {
		s.NotContains(combined, foreign)
	}

	// The REST body: the whole 409 is free of the consumer's identity.
	def := s.parseContractManifest(broken, producer)
	status, body := s.b9Apply(mktOperator.Plaintext, false, def)
	s.Require().Equal(http.StatusConflict, status, body)
	s.Contains(body, `"withheld_findings":1`)
	for _, foreign := range []string{consumer, team, "customer_id", "finance"} {
		s.NotContains(body, foreign)
	}
	status, body = s.b9Apply(s.authAPIKey, false, def)
	s.Require().Equal(http.StatusConflict, status, body)
	s.Contains(body, consumer, "the cluster admin's 409 names the consumer")

	// A job-scoped operator key at `*`, scoped to the producer alone, is
	// confined by its alias scope: the consumer is withheld from it too.
	scoped := s.createAPIKeyCLI("--role", "operator", "--scope-jobs", producer, "--description", fmt.Sprintf("b9 contract scoped %d", stamp))
	s.T().Cleanup(func() {
		_, _ = s.runCLIStdout("auth", "key", "revoke", "--id", scoped.Meta.ID, "--server", s.caesiumURL)
	})
	status, body = s.b9Apply(scoped.Plaintext, false, def)
	s.Require().Equal(http.StatusConflict, status, body)
	s.Contains(body, `"withheld_findings":1`)
	for _, foreign := range []string{consumer, team, "customer_id", "finance"} {
		s.NotContains(body, foreign)
	}

	// Acknowledging the break (the caller's own subject) applies it; the
	// consumer's warning is withheld and only counted.
	payload := map[string]any{"definitions": []schema.Definition{def}, "allow_breaking": map[string]any{"dataset": producer + ".output.customer_id", "reason": "b9"}}
	status, body = s.jobdefRequest("/v1/jobdefs/apply", mktOperator.Plaintext, payload)
	s.Require().Equal(http.StatusOK, status, body)
	s.Contains(body, `"withheld_contract_warnings":`)
	s.NotContains(body, consumer)
	s.NotContains(body, team)
}

// parseContractManifest reads one manifest written by writeContractManifests.
func (s *IntegrationTestSuite) parseContractManifest(dir, alias string) schema.Definition {
	s.T().Helper()
	data, err := os.ReadFile(dir + "/" + alias + ".job.yaml")
	s.Require().NoError(err)
	def, err := schema.Parse(data)
	s.Require().NoError(err)
	return *def
}

// TestJobdefTriggerTenancyGlobalInvariant (no-auth lane, policy fixture
// served): with a policy file every writer is held to one namespace per
// webhook path: apply, the legacy create and the trigger PATCH answer 409
// naming the namespace that holds it, server lint reports it, and the same
// namespace may still fan out.
func (s *IntegrationTestSuite) TestJobdefTriggerTenancyGlobalInvariant() {
	s.requirePolicyFixture()
	stamp := time.Now().UnixNano()
	path := fmt.Sprintf("/hooks/b9-global-%d", stamp)
	mkt := fmt.Sprintf("b9-global-mkt-%d", stamp)
	mkt2 := fmt.Sprintf("b9-global-mkt2-%d", stamp)
	fin := fmt.Sprintf("b9-global-fin-%d", stamp)
	finOwn := fmt.Sprintf("b9-global-fin-own-%d", stamp)
	s.b9Cleanup(mkt, mkt2, fin, finOwn)
	s.applyDefinition(s.b9Def(mkt, "marketing", path))
	s.applyDefinition(s.b9Def(mkt2, "marketing", path))

	dir := s.writeNamespaceManifests(s.b9Def(fin, "finance", path))
	stdout, stderr, err := s.runCLISeparate("job", "apply", "--path", dir, "--server", s.caesiumURL)
	s.Require().Error(err, "%s", stdout)
	s.Contains(stdout+stderr, `is already subscribed from namespace \"marketing\"`)
	s.Nil(s.b9Job(fin))

	lint := s.serverLint(s.authAPIKey, s.b9Def(fin, "finance", path))
	s.Contains(strings.Join(lint.messages(), "\n"), `is already subscribed from namespace "marketing"`)

	payload, err := json.Marshal(map[string]any{
		"alias": fin, "metadata": map[string]any{"namespace": "finance"},
		"trigger": map[string]any{"alias": fin, "type": "http", "configuration": map[string]any{"path": path}},
		"tasks":   []any{map[string]any{"atom": map[string]any{"engine": s.engineOrDocker(), "image": "busybox:1.36.1", "command": []string{"true"}}}},
	})
	s.Require().NoError(err)
	status, body := s.nsRequest(http.MethodPost, "/v1/jobs", s.authAPIKey, string(payload), nil)
	s.Require().Equal(http.StatusConflict, status, body)
	s.Nil(s.b9Job(fin))

	s.applyDefinition(s.b9Def(finOwn, "finance", fmt.Sprintf("/hooks/b9-global-fin-own-%d", stamp)))
	finTrigger := s.b9RequireJob(finOwn).TriggerID
	status, body = s.nsRequest(http.MethodPatch, "/v1/triggers/"+finTrigger, s.authAPIKey, fmt.Sprintf(`{"configuration":{"path":%q}}`, path), nil)
	s.Require().Equal(http.StatusConflict, status, body)
	s.Contains(body, `namespace \"marketing\"`)
}
