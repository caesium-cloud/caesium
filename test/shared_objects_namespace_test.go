//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// shared_objects_namespace_test.go drives identity-and-access D4 through the
// live surface: notification channels, notification policies and agent
// profiles confined to namespaces (writes, lists, references, declared
// namespaces), and notification routing by the namespace that owns an event.

type sharedObjectView struct {
	ID        string `json:"id"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func decodeSharedObjects(s *IntegrationTestSuite, body string) []sharedObjectView {
	s.T().Helper()
	var out []sharedObjectView
	s.Require().NoError(json.Unmarshal([]byte(body), &out), body)
	return out
}

func sharedObjectIDs(objects []sharedObjectView) []string {
	ids := make([]string, 0, len(objects))
	for _, o := range objects {
		ids = append(ids, o.ID)
	}
	return ids
}

// sharedObjectCleanup deletes what a scenario created with the admin key.
type sharedObjectCleanup struct {
	s     *IntegrationTestSuite
	paths []string
}

func (c *sharedObjectCleanup) add(path string) { c.paths = append(c.paths, path) }

func (c *sharedObjectCleanup) run() {
	for i := len(c.paths) - 1; i >= 0; i-- {
		status, body := c.s.nsRequest(http.MethodDelete, c.paths[i], c.s.authAPIKey, "", nil)
		c.s.Contains([]int{http.StatusNoContent, http.StatusNotFound}, status, "cleanup %s: %s", c.paths[i], body)
	}
}

// TestAuthSharedObjectsConfinedToNamespaces: a marketing operator writes its
// own namespace's shared objects, reads shared ones, and can neither edit nor
// delete finance's or "*" ones nor create "*" ones; lists show only
// marketing and "*"; a policy reference to a channel it cannot read fails
// exactly as a missing one; a name held elsewhere is a bare conflict; and the
// confined create is audited against marketing.
func (s *IntegrationTestSuite) TestAuthSharedObjectsConfinedToNamespaces() {
	s.requireAuthLane()
	stamp := time.Now().UnixNano()
	name := func(kind string) string { return fmt.Sprintf("d4-%s-%d", kind, stamp) }
	admin := s.authAPIKey
	mktOperator := s.mintNamespacedKey("operator", "marketing").Plaintext
	mktViewer := s.mintNamespacedKey("viewer", "marketing").Plaintext
	cleanup := &sharedObjectCleanup{s: s}
	defer cleanup.run()

	channelBody := func(namespace, n string) string {
		return fmt.Sprintf(`{"namespace":%q,"name":%q,"type":"webhook","config":{"url":"https://example.invalid/d4"},"enabled":false}`, namespace, n)
	}
	create := func(token, path, body string) string {
		id := decodeID(s, s.requireNSStatus(http.StatusCreated, http.MethodPost, path, token, body))
		cleanup.add(path + "/" + id)
		return id
	}

	// --- channels ---------------------------------------------------------
	finChannel := create(admin, "/v1/notifications/channels", channelBody("finance", name("ch-fin")))
	sharedChannel := create(admin, "/v1/notifications/channels", channelBody("*", name("ch-shared")))
	mktChannel := create(mktOperator, "/v1/notifications/channels", channelBody("marketing", name("ch-mkt")))
	s.requireNSStatus(http.StatusOK, http.MethodPatch, "/v1/notifications/channels/"+mktChannel, mktOperator, `{"enabled":false}`)
	s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/notifications/channels/"+sharedChannel, mktViewer, "")
	for _, call := range []struct{ method, path, body string }{
		{http.MethodPatch, "/v1/notifications/channels/" + finChannel, `{"enabled":false}`},
		{http.MethodDelete, "/v1/notifications/channels/" + finChannel, ""},
		{http.MethodPatch, "/v1/notifications/channels/" + sharedChannel, `{"enabled":false}`},
		{http.MethodDelete, "/v1/notifications/channels/" + sharedChannel, ""},
		{http.MethodPost, "/v1/notifications/channels", channelBody("*", name("ch-denied-shared"))},
		{http.MethodPost, "/v1/notifications/channels", channelBody("finance", name("ch-denied-fin"))},
	} {
		s.requireNSStatus(http.StatusForbidden, call.method, call.path, mktOperator, call.body)
	}
	listed := sharedObjectIDs(decodeSharedObjects(s, s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/notifications/channels?limit=1000", mktViewer, "")))
	s.Contains(listed, mktChannel)
	s.Contains(listed, sharedChannel)
	s.NotContains(listed, finChannel)
	for _, ch := range decodeSharedObjects(s, s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/notifications/channels?limit=1000", mktViewer, "")) {
		s.Contains([]string{"marketing", "*"}, ch.Namespace, "a marketing viewer lists only marketing and shared channels: %+v", ch)
	}

	// A name held by a channel marketing cannot read: a bare conflict.
	status, body := s.nsRequest(http.MethodPost, "/v1/notifications/channels", mktOperator, channelBody("marketing", name("ch-fin")), nil)
	s.Require().Equal(http.StatusConflict, status, body)
	s.Equal(`{"message":"conflict"}`, strings.TrimSpace(body))

	// --- policies ---------------------------------------------------------
	policyBody := func(namespace, n, channel string) string {
		return fmt.Sprintf(`{"namespace":%q,"name":%q,"channel_id":%q,"event_types":["run_failed"],"filters":{"job_ids":[%q]}}`,
			namespace, n, channel, uuid.NewString())
	}
	finPolicy := create(admin, "/v1/notifications/policies", policyBody("finance", name("pol-fin"), finChannel))
	mktPolicy := create(mktOperator, "/v1/notifications/policies", policyBody("marketing", name("pol-mkt"), mktChannel))
	create(mktOperator, "/v1/notifications/policies", policyBody("marketing", name("pol-mkt-shared-channel"), sharedChannel))
	// A finance channel and one that does not exist are indistinguishable.
	foreignStatus, foreignBody := s.nsRequest(http.MethodPost, "/v1/notifications/policies", mktOperator, policyBody("marketing", name("pol-foreign"), finChannel), nil)
	missingStatus, missingBody := s.nsRequest(http.MethodPost, "/v1/notifications/policies", mktOperator, policyBody("marketing", name("pol-missing"), uuid.NewString()), nil)
	s.Equal(http.StatusBadRequest, foreignStatus, foreignBody)
	s.Equal(missingStatus, foreignStatus)
	s.Equal(missingBody, foreignBody, "a reference to a channel the caller cannot read must fail exactly as a missing one")
	s.NotContains(foreignBody, "finance")
	foreignStatus, foreignBody = s.nsRequest(http.MethodPatch, "/v1/notifications/policies/"+mktPolicy, mktOperator, fmt.Sprintf(`{"channel_id":%q}`, finChannel), nil)
	missingStatus, missingBody = s.nsRequest(http.MethodPatch, "/v1/notifications/policies/"+mktPolicy, mktOperator, fmt.Sprintf(`{"channel_id":%q}`, uuid.NewString()), nil)
	s.Equal(http.StatusBadRequest, foreignStatus, foreignBody)
	s.Equal(missingStatus, foreignStatus)
	s.Equal(missingBody, foreignBody)
	s.requireNSStatus(http.StatusForbidden, http.MethodDelete, "/v1/notifications/policies/"+finPolicy, mktOperator, "")
	s.requireNSStatus(http.StatusForbidden, http.MethodPost, "/v1/notifications/policies", mktOperator, policyBody("*", name("pol-denied-shared"), sharedChannel))
	policies := sharedObjectIDs(decodeSharedObjects(s, s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/notifications/policies?limit=1000", mktViewer, "")))
	s.Contains(policies, mktPolicy)
	s.NotContains(policies, finPolicy)

	// --- agent profiles ---------------------------------------------------
	profileBody := func(namespace, n string) string {
		return fmt.Sprintf(`{"namespace":%q,"name":%q,"image":"busybox:1.36.1"}`, namespace, n)
	}
	finProfile := create(admin, "/v1/agentprofiles", profileBody("finance", name("prof-fin")))
	sharedProfile := create(admin, "/v1/agentprofiles", profileBody("*", name("prof-shared")))
	mktProfile := create(mktOperator, "/v1/agentprofiles", profileBody("marketing", name("prof-mkt")))
	s.requireNSStatus(http.StatusOK, http.MethodPatch, "/v1/agentprofiles/"+mktProfile, mktOperator, `{"image":"busybox:1.36.1"}`)
	s.requireNSStatus(http.StatusForbidden, http.MethodPatch, "/v1/agentprofiles/"+sharedProfile, mktOperator, `{"image":"busybox:1.36.1"}`)
	s.requireNSStatus(http.StatusForbidden, http.MethodDelete, "/v1/agentprofiles/"+finProfile, mktOperator, "")
	s.requireNSStatus(http.StatusForbidden, http.MethodPost, "/v1/agentprofiles", mktOperator, profileBody("*", name("prof-denied")))

	// The profile list through the real CLI, as the marketing viewer.
	out, err := s.runCLIStdout("agentprofile", "list", "--json", "--limit", "1000", "--server", s.caesiumURL, "--api-key", mktViewer)
	s.Require().NoError(err, out)
	var profiles []sharedObjectView
	s.Require().NoError(json.Unmarshal([]byte(out), &profiles), "caesium agentprofile list --json stdout must be one JSON array:\n%s", out)
	names := make([]string, 0, len(profiles))
	for _, p := range profiles {
		names = append(names, p.Name)
		s.Contains([]string{"marketing", "*"}, p.Namespace, "%+v", p)
	}
	s.Contains(names, name("prof-mkt"))
	s.Contains(names, name("prof-shared"))
	s.Contains(names, "triage-only")
	s.NotContains(names, name("prof-fin"))

	// Marketing deletes its own objects; the confined create is audited
	// against marketing, by the confined key.
	s.requireNSStatus(http.StatusNoContent, http.MethodDelete, "/v1/agentprofiles/"+mktProfile, mktOperator, "")
	s.requireNSStatus(http.StatusNoContent, http.MethodDelete, "/v1/notifications/policies/"+mktPolicy, mktOperator, "")
	rows, err := s.auditCLI("--namespace", "marketing", "--action", "notification_channel.create")
	s.Require().NoError(err)
	found := false
	for _, row := range rows {
		if strings.Contains(string(row.Metadata), name("ch-mkt")) {
			found = true
			s.Equal("marketing", row.Namespace)
			s.True(strings.HasPrefix(row.Actor, "csk_"), "an API key's create is attributed to its prefix: %+v", row)
		}
	}
	s.True(found, "the marketing operator's channel create must be audited against marketing: %+v", rows)
}

// TestSharedObjectsRejectUndeclaredNamespace: with a policy file loaded,
// creating a shared object in a namespace it does not declare is refused,
// naming the namespace; declared ones, "*" and the default are accepted.
func (s *IntegrationTestSuite) TestSharedObjectsRejectUndeclaredNamespace() {
	s.requirePolicyFixture()
	stamp := time.Now().UnixNano()
	undeclared := fmt.Sprintf("d4-undeclared-%d", stamp%1_000_000)
	cleanup := &sharedObjectCleanup{s: s}
	defer cleanup.run()

	post := func(path, body string) (int, string) {
		resp, err := s.doJSONRequest(http.MethodPost, s.caesiumURL+path, strings.NewReader(body))
		s.Require().NoError(err)
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		s.Require().NoError(err)
		return resp.StatusCode, string(raw)
	}
	var shared string
	for i, namespace := range []string{"*", "finance", ""} {
		body := fmt.Sprintf(`{"namespace":%q,"name":"d4-declared-%d-%d","type":"webhook","config":{"url":"https://example.invalid/d4"},"enabled":false}`, namespace, i, stamp)
		status, out := post("/v1/notifications/channels", body)
		s.Require().Equal(http.StatusCreated, status, out)
		id := decodeID(s, out)
		cleanup.add("/v1/notifications/channels/" + id)
		if namespace == "*" {
			shared = id
		}
	}
	for _, call := range []struct{ path, body string }{
		{"/v1/notifications/channels", fmt.Sprintf(`{"namespace":%q,"name":"d4-undeclared-ch-%d","type":"webhook","config":{}}`, undeclared, stamp)},
		{"/v1/notifications/policies", fmt.Sprintf(`{"namespace":%q,"name":"d4-undeclared-pol-%d","channel_id":%q,"event_types":["run_failed"]}`, undeclared, stamp, shared)},
		{"/v1/agentprofiles", fmt.Sprintf(`{"namespace":%q,"name":"d4-undeclared-prof-%d","image":"busybox:1.36.1"}`, undeclared, stamp)},
	} {
		status, out := post(call.path, call.body)
		s.Require().Equal(http.StatusBadRequest, status, "%s: %s", call.path, out)
		var refusal struct {
			Message string `json:"message"`
		}
		s.Require().NoError(json.Unmarshal([]byte(out), &refusal), out)
		s.Equal(fmt.Sprintf("namespace %q is not declared in the access policy", undeclared), refusal.Message, call.path)
	}
}

// d4Capture records the bodies a notification webhook channel delivers. The
// test runner shares the server's network namespace on the docker lanes, so
// the server reaches it on loopback.
type d4Capture struct {
	server *httptest.Server
	mu     sync.Mutex
	bodies []string
}

func newD4Capture() *d4Capture {
	c := &d4Capture{}
	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		c.mu.Lock()
		c.bodies = append(c.bodies, string(body))
		c.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	return c
}

func (c *d4Capture) URL() string { return c.server.URL }

func (c *d4Capture) Close() { c.server.Close() }

func (c *d4Capture) Snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.bodies...)
}

func d4Label(namespace string) string {
	if namespace == "*" {
		return "shared"
	}
	return namespace
}

// d4WebhookChannel creates a webhook channel in namespace pointing at target.
func (s *IntegrationTestSuite) d4WebhookChannel(token, namespace, name, target string) string {
	s.T().Helper()
	body := fmt.Sprintf(`{"namespace":%q,"name":%q,"type":"webhook","config":{"url":%q},"enabled":true}`, namespace, name, target)
	return decodeID(s, s.requireNSStatus(http.StatusCreated, http.MethodPost, "/v1/notifications/channels", token, body))
}

// d4Policy creates a policy in namespace routing eventType for jobIDs.
func (s *IntegrationTestSuite) d4Policy(token, namespace, name, channel, eventType string, jobIDs ...string) string {
	s.T().Helper()
	ids, err := json.Marshal(jobIDs)
	s.Require().NoError(err)
	body := fmt.Sprintf(`{"namespace":%q,"name":%q,"channel_id":%q,"event_types":[%q],"filters":{"job_ids":%s},"enabled":true}`,
		namespace, name, channel, eventType, ids)
	return decodeID(s, s.requireNSStatus(http.StatusCreated, http.MethodPost, "/v1/notifications/policies", token, body))
}

// deliveredRunIDs parses the run ids out of captured webhook bodies.
func deliveredRunIDs(bodies []string) []string {
	var ids []string
	for _, body := range bodies {
		var payload struct {
			RunID string `json:"run_id"`
		}
		if json.Unmarshal([]byte(body), &payload) == nil {
			ids = append(ids, payload.RunID)
		}
	}
	return ids
}

func (s *IntegrationTestSuite) skipWithoutHostWebhooks() {
	s.T().Helper()
	if s.engineType == "podman" || s.engineType == "kubernetes" {
		s.T().Skipf("notification-webhook capture is not host-reachable under CAESIUM_TEST_ENGINE=%s; routing is covered by internal/notification/route_test.go", s.engineType)
	}
}

// TestAuthSharedPolicyMatchesOwnNamespaceOnly: three policies whose job
// filters name both a marketing and a finance job. A marketing run reaches
// the shared and the marketing policy and never the finance one; a finance
// run (the positive control for finance's channel) reaches shared and
// finance, never marketing. The marketing and finance objects are created by
// confined operators.
func (s *IntegrationTestSuite) TestAuthSharedPolicyMatchesOwnNamespaceOnly() {
	s.requireAuthLane()
	s.skipWithoutHostWebhooks()
	stamp := time.Now().UnixNano()
	admin := s.authAPIKey
	mktOperator := s.mintNamespacedKey("operator", "marketing").Plaintext
	finOperator := s.mintNamespacedKey("operator", "finance").Plaintext
	cleanup := &sharedObjectCleanup{s: s}
	defer cleanup.run()

	mktAlias := fmt.Sprintf("d4-route-mkt-%d", stamp)
	finAlias := fmt.Sprintf("d4-route-fin-%d", stamp)
	s.applyDefinition(s.namespacedJobDefinition(mktAlias, "marketing"))
	s.applyDefinition(s.namespacedJobDefinition(finAlias, "finance"))
	mktJob, finJob := s.requireJobByAlias(mktAlias), s.requireJobByAlias(finAlias)

	captures := map[string]*d4Capture{}
	for namespace, token := range map[string]string{"*": admin, "marketing": mktOperator, "finance": finOperator} {
		capture := newD4Capture()
		defer capture.Close()
		captures[namespace] = capture
		channel := s.d4WebhookChannel(token, namespace, fmt.Sprintf("d4-route-%s-%d", d4Label(namespace), stamp), capture.URL())
		cleanup.add("/v1/notifications/channels/" + channel)
		policy := s.d4Policy(token, namespace, fmt.Sprintf("d4-route-%s-%d", d4Label(namespace), stamp), channel, "run_completed", mktJob.ID, finJob.ID)
		cleanup.add("/v1/notifications/policies/" + policy)
	}

	received := func(namespace, runID string) bool {
		return slices.Contains(deliveredRunIDs(captures[namespace].Snapshot()), runID)
	}
	for _, tc := range []struct {
		job       *jobSummary
		owner     string
		forbidden string
	}{
		{mktJob, "marketing", "finance"},
		{finJob, "finance", "marketing"},
	} {
		runID := s.triggerRun(tc.job.ID)
		s.Require().Equal("succeeded", s.awaitRun(tc.job.ID, runID, runTimeout).Status)
		s.Require().Eventually(func() bool { return received("*", runID) && received(tc.owner, runID) }, 60*time.Second, 250*time.Millisecond,
			"a %s run must reach the shared and the %s policy", tc.owner, tc.owner)
		s.Require().Never(func() bool { return received(tc.forbidden, runID) }, 3*time.Second, 250*time.Millisecond,
			"a %s run must never reach a %s policy, whatever its job filter says", tc.owner, tc.forbidden)
	}
}

func d4NamespacedManifest(manifest, namespace string) string {
	lines := strings.SplitN(manifest, "\n", 5)
	// apiVersion, kind, metadata:, "  alias: x", rest
	return strings.Join(lines[:4], "\n") + "\n  namespace: " + namespace + "\n" + lines[4]
}

// TestContractBreakNotificationScopedToNamespace: a declared contract break
// by a marketing producer with a marketing and a finance consumer. The
// marketing policy receives the marketing consumer's notice naming only
// marketing's consumer; the finance policy receives nothing (the producer and
// the break are marketing's); the shared policy receives both in full.
func (s *IntegrationTestSuite) TestContractBreakNotificationScopedToNamespace() {
	s.requirePolicyFixture()
	s.skipWithoutHostWebhooks()
	if onAuthLane() {
		s.T().Skip("runs on the no-auth lane; job apply through the CLI sends no credential")
	}
	suffix := time.Now().UnixNano()
	dataset := fmt.Sprintf("lake.d4_contract_%d", suffix)
	producer := fmt.Sprintf("d4-contract-producer-%d", suffix)
	mktConsumer := fmt.Sprintf("d4-contract-mkt-consumer-%d", suffix)
	finConsumer := fmt.Sprintf("d4-contract-fin-consumer-%d", suffix)
	cleanup := &sharedObjectCleanup{s: s}
	defer cleanup.run()

	// Each consumer needs its own key, and the break removes both: the
	// marketing notice must not carry the key only finance needs.
	finOnlyKey := fmt.Sprintf("ledger_code_%d", suffix%1_000_000)
	finConsumerManifest := strings.ReplaceAll(contractDatasetConsumerManifest(finConsumer, dataset, "fin-team"), "customer_id", finOnlyKey)
	initial := s.writeContractManifests(map[string]string{
		producer:    d4NamespacedManifest(contractDatasetProducerManifest(producer, dataset, []string{"customer_id", finOnlyKey, "row_count"}, 1), "marketing"),
		mktConsumer: d4NamespacedManifest(contractDatasetConsumerManifest(mktConsumer, dataset, "mkt-team"), "marketing"),
		finConsumer: d4NamespacedManifest(finConsumerManifest, "finance"),
	})
	defer os.RemoveAll(initial)
	s.runCLI("job", "apply", "--path", initial, "--server", s.caesiumURL)
	mktJob, finJob := s.requireJobByAlias(mktConsumer), s.requireJobByAlias(finConsumer)

	captures := map[string]*d4Capture{}
	for _, namespace := range []string{"*", "marketing", "finance"} {
		capture := newD4Capture()
		defer capture.Close()
		captures[namespace] = capture
		label := d4Label(namespace)
		channel := s.d4WebhookChannel(s.authAPIKey, namespace, fmt.Sprintf("d4-contract-%s-%d", label, suffix), capture.URL())
		cleanup.add("/v1/notifications/channels/" + channel)
		policy := s.d4Policy(s.authAPIKey, namespace, fmt.Sprintf("d4-contract-%s-%d", label, suffix), channel, "contract_break_declared", mktJob.ID, finJob.ID)
		cleanup.add("/v1/notifications/policies/" + policy)
	}

	breaking := s.writeContractManifests(map[string]string{
		producer: d4NamespacedManifest(contractDatasetProducerManifest(producer, dataset, []string{"row_count"}, 2), "marketing"),
	})
	defer os.RemoveAll(breaking)
	stdout, stderr, err := s.runCLISeparate("job", "apply", "--path", breaking, "--server", s.caesiumURL,
		"--allow-breaking", "dataset="+dataset, "--reason", "d4 scoped break")
	s.Require().NoError(err, stderr)
	s.Contains(stdout, "Applied 1 job definition")

	s.Require().Eventually(func() bool { return len(captures["*"].Snapshot()) >= 2 }, 60*time.Second, 250*time.Millisecond,
		"the shared policy receives both consumers' notices")
	shared := strings.Join(captures["*"].Snapshot(), "\n")
	s.Contains(shared, mktConsumer)
	s.Contains(shared, finConsumer)
	s.Contains(shared, finOnlyKey, "the shared notices name the finance consumer's broken key")

	s.Require().Eventually(func() bool { return len(captures["marketing"].Snapshot()) >= 1 }, 30*time.Second, 250*time.Millisecond)
	s.Require().Never(func() bool { return len(captures["finance"].Snapshot()) > 0 }, 3*time.Second, 250*time.Millisecond,
		"a break by a marketing producer reaches no finance destination")
	marketing := captures["marketing"].Snapshot()
	s.Require().Len(marketing, 1, "only the marketing consumer's notice reaches marketing")
	s.Contains(marketing[0], mktConsumer)
	s.Contains(marketing[0], producer)
	s.NotContains(marketing[0], finConsumer, "the marketing notice must not name finance's consumer")
	s.NotContains(marketing[0], "fin-team")
	s.Contains(marketing[0], "customer_id", "the marketing notice names its own consumer's broken key")
	s.NotContains(marketing[0], finOnlyKey, "the marketing notice must not carry a key only finance's consumer needs")
}
