//go:build integration

package test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
)

// Identity-and-access A3 on the live auth lane: classified routes and the
// namespace-aware auth middleware, driven over HTTP and through the CLI.
//
// Namespace-confined keys are seeded through the catalog database the way
// TestAuthKeyNamespacesGrantResolution does: minting keys with namespaces is
// A5, so the UPDATE below stands in for `caesium auth key create --namespace`.

// nsRoutesFixture is a marketing job, a finance job and the keys a scenario
// mints, all removed again by close.
type nsRoutesFixture struct {
	stamp     int64
	mktAlias  string
	finAlias  string
	mktJob    *jobSummary
	finJob    *jobSummary
	catalogDB *sql.DB
	jobs      []string
	keys      []string
	undo      []func()
}

func (s *IntegrationTestSuite) namespacedJobDefinition(alias, namespace string) schema.Definition {
	step := schema.Step{
		Name:    "ok",
		Image:   "busybox:1.36.1",
		Command: []string{"sh", "-c", "echo namespaced-route-ok"},
	}
	if s.engineType != "" && s.engineType != "docker" {
		step.Engine = s.engineType
	}
	return schema.Definition{
		APIVersion: "v1",
		Kind:       "Job",
		Metadata:   schema.Metadata{Alias: alias, Namespace: namespace},
		// Never fires on its own: every run in these scenarios is explicit.
		Trigger: schema.Trigger{Type: "cron", Configuration: map[string]any{"cron": "0 0 31 2 *"}},
		Steps:   []schema.Step{step},
	}
}

// namespacedHTTPJobDefinition is namespacedJobDefinition with an HTTP trigger,
// so the scenario can fire it through POST /v1/triggers/:id/fire.
func (s *IntegrationTestSuite) namespacedHTTPJobDefinition(alias, namespace string) schema.Definition {
	def := s.namespacedJobDefinition(alias, namespace)
	def.Trigger = schema.Trigger{Type: "http", Configuration: map[string]any{"path": "/hooks/" + alias}}
	return def
}

func (s *IntegrationTestSuite) newNamespacedRoutesFixture(label string) *nsRoutesFixture {
	s.T().Helper()
	f := &nsRoutesFixture{stamp: time.Now().UnixNano()}
	f.mktAlias = fmt.Sprintf("a3-%s-mkt-%d", label, f.stamp)
	f.finAlias = fmt.Sprintf("a3-%s-fin-%d", label, f.stamp)
	f.catalogDB = s.openIntegrationCatalogDB()
	f.mktJob = s.addNamespacedJob(f, f.mktAlias, "marketing")
	f.finJob = s.addNamespacedJob(f, f.finAlias, "finance")
	return f
}

func (s *IntegrationTestSuite) addNamespacedJob(f *nsRoutesFixture, alias, namespace string) *jobSummary {
	s.T().Helper()
	s.applyDefinition(s.namespacedJobDefinition(alias, namespace))
	job := s.requireJobByAlias(alias)
	f.jobs = append(f.jobs, job.ID)
	return job
}

func (s *IntegrationTestSuite) closeNamespacedRoutesFixture(f *nsRoutesFixture) {
	s.T().Helper()
	for i := len(f.undo) - 1; i >= 0; i-- {
		f.undo[i]()
	}
	for _, id := range f.jobs {
		status, body := s.requestWithKey(http.MethodDelete, "/v1/jobs/"+id, s.authAPIKey, nil)
		s.Contains([]int{http.StatusAccepted, http.StatusNotFound}, status, "deleting job %s: %s", id, body)
	}
	for _, id := range f.keys {
		out, err := s.runCLIStdout("auth", "key", "revoke", "--id", id, "--server", s.caesiumURL)
		s.NoError(err, "caesium auth key revoke %s failed:\n%s", id, out)
	}
	s.NoError(f.catalogDB.Close())
}

// catalogExec runs one seeding statement against the live catalog and
// requires it to touch exactly want rows (want < 0: any number).
func (s *IntegrationTestSuite) catalogExec(f *nsRoutesFixture, want int64, query string, args ...any) {
	s.T().Helper()
	result, err := f.catalogDB.ExecContext(s.T().Context(), query, args...)
	s.Require().NoError(err, query)
	if want >= 0 {
		affected, err := result.RowsAffected()
		s.Require().NoError(err)
		s.Require().Equal(want, affected, query)
	}
}

// namespacedKey mints a key with role and stores namespaces verbatim ("" keeps
// SQL NULL: the role cluster-wide).
func (s *IntegrationTestSuite) namespacedKey(f *nsRoutesFixture, role, namespaces string) createdAPIKey {
	s.T().Helper()
	key := s.createAPIKeyCLI("--role", role, "--description",
		fmt.Sprintf("a3 %s %s %d", role, namespaces, time.Now().UnixNano()))
	f.keys = append(f.keys, key.Meta.ID)
	if namespaces != "" {
		s.catalogExec(f, 1, `UPDATE api_keys SET namespaces = ? WHERE id = ?`, namespaces, key.Meta.ID)
	}
	return key
}

// nsRequest sends one request with token and optional JSON body and extra
// headers, returning the status and body.
func (s *IntegrationTestSuite) nsRequest(method, path, token, body string, headers map[string]string) (int, string) {
	s.T().Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(s.T().Context(), method, s.caesiumURL+path, reader)
	s.Require().NoError(err)
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	return resp.StatusCode, string(out)
}

func (s *IntegrationTestSuite) requireNSStatus(want int, method, path, token, body string) string {
	s.T().Helper()
	status, out := s.nsRequest(method, path, token, body, nil)
	s.Require().Equal(want, status, "%s %s: %s", method, path, out)
	return out
}

func decodeID(s *IntegrationTestSuite, body string) string {
	s.T().Helper()
	var obj struct {
		ID string `json:"id"`
	}
	s.Require().NoError(json.Unmarshal([]byte(body), &obj), body)
	s.Require().NotEmpty(obj.ID, body)
	return obj.ID
}

// TestAuthNamespacedRoutesConfineKey: an operator key confined to marketing
// acts on marketing's job and is refused on finance's; cluster and pending
// routes are cluster-only for it; identity routes answer it; and a cluster
// viewer on a runner route keeps master's 403 even for a missing job.
func (s *IntegrationTestSuite) TestAuthNamespacedRoutesConfineKey() {
	s.requireAuthLane()
	f := s.newNamespacedRoutesFixture("confine")
	defer s.closeNamespacedRoutesFixture(f)

	mktOperator := s.namespacedKey(f, "operator", `["marketing"]`).Plaintext
	clusterViewer := s.namespacedKey(f, "viewer", "").Plaintext
	mkt, fin := f.mktJob.ID, f.finJob.ID

	for _, path := range []string{"/v1/jobs/%s/dag", "/v1/jobs/%s/tasks", "/v1/jobs/%s/queue"} {
		s.requireNSStatus(http.StatusOK, http.MethodGet, fmt.Sprintf(path, mkt), mktOperator, "")
		s.requireNSStatus(http.StatusForbidden, http.MethodGet, fmt.Sprintf(path, fin), mktOperator, "")
	}
	// The manifest (by full id or unique prefix) exports the job's trigger
	// configuration, which a finance job could share: pending B2.
	s.requireNSStatus(http.StatusForbidden, http.MethodGet, "/v1/jobs/"+mkt+"/manifest", mktOperator, "")
	s.requireNSStatus(http.StatusForbidden, http.MethodGet, "/v1/jobs/"+mkt[:13]+"/manifest", mktOperator, "")
	s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/jobs/"+mkt[:13]+"/manifest", clusterViewer, "")

	// Runs: the confined key starts a marketing run and reads it; a finance
	// run is refused; a finance run under the marketing job's path is 404.
	mktRun := decodeID(s, s.requireNSStatus(http.StatusAccepted, http.MethodPost, "/v1/jobs/"+mkt+"/run", mktOperator, ""))
	s.requireNSStatus(http.StatusForbidden, http.MethodPost, "/v1/jobs/"+fin+"/run", mktOperator, "")
	finRun := s.triggerRun(fin)
	s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/jobs/"+mkt+"/runs/"+mktRun, mktOperator, "")
	s.requireNSStatus(http.StatusForbidden, http.MethodGet, "/v1/jobs/"+fin+"/runs/"+finRun, mktOperator, "")
	s.requireNSStatus(http.StatusNotFound, http.MethodGet, "/v1/jobs/"+mkt+"/runs/"+finRun, mktOperator, "")
	s.awaitRun(mkt, mktRun, runTimeout)
	s.awaitRun(fin, finRun, runTimeout)

	// Operator writes on the marketing job only.
	s.requireNSStatus(http.StatusOK, http.MethodPut, "/v1/jobs/"+mkt+"/pause", mktOperator, "")
	s.requireNSStatus(http.StatusOK, http.MethodPut, "/v1/jobs/"+mkt+"/unpause", mktOperator, "")
	s.requireNSStatus(http.StatusForbidden, http.MethodPut, "/v1/jobs/"+fin+"/pause", mktOperator, "")

	// Cluster routes and still-pending routes are cluster-only.
	s.requireNSStatus(http.StatusForbidden, http.MethodGet, "/v1/atoms", mktOperator, "")
	s.requireNSStatus(http.StatusForbidden, http.MethodGet, "/metrics", mktOperator, "")
	s.requireNSStatus(http.StatusForbidden, http.MethodGet, "/v1/jobs", mktOperator, "")
	payload, err := json.Marshal(map[string]any{"definitions": []schema.Definition{s.namespacedJobDefinition(f.mktAlias, "marketing")}})
	s.Require().NoError(err)
	s.requireNSStatus(http.StatusForbidden, http.MethodPost, "/v1/jobdefs/apply", mktOperator, string(payload))
	s.requireNSStatus(http.StatusForbidden, http.MethodPost, "/v1/jobdefs/diff", mktOperator, string(payload))

	// Identity routes answer any authenticated principal.
	s.requireNSStatus(http.StatusOK, http.MethodGet, "/auth/whoami", mktOperator, "")
	s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/system/features", mktOperator, "")

	// D1: a principal whose grants never reach the route's role is refused
	// before the target is looked up — master's 403, not a 404.
	s.requireNSStatus(http.StatusForbidden, http.MethodPost, "/v1/jobs/"+uuid.NewString()+"/run", clusterViewer, "")
	// ...and the cluster viewer still reads every namespace as before.
	s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/jobs/"+fin+"/dag", clusterViewer, "")
}

// TestAuthNamespacedSharedObjectsAndTriggers: shared objects resolve to their
// own namespace ("*" needs the role at * to write, viewer anywhere to read);
// a trigger resolves to every job it serves; and an idempotent start never
// hands a confined caller a run recorded in a namespace it cannot read.
func (s *IntegrationTestSuite) TestAuthNamespacedSharedObjectsAndTriggers() {
	s.requireAuthLane()
	f := s.newNamespacedRoutesFixture("shared")
	defer s.closeNamespacedRoutesFixture(f)
	mktOperator := s.namespacedKey(f, "operator", `["marketing"]`).Plaintext
	admin := s.authAPIKey
	name := func(kind string) string { return fmt.Sprintf("a3-%s-%d", kind, f.stamp) }
	deleteAfter := func(path string) {
		f.undo = append(f.undo, func() {
			status, body := s.nsRequest(http.MethodDelete, path, admin, "", nil)
			s.Contains([]int{http.StatusNoContent, http.StatusNotFound}, status, "cleanup %s: %s", path, body)
		})
	}

	// Notification channels and policies: reads by id are live; creates,
	// edits and deletes are pending D4. Dispatch does not yet match policies
	// by namespace, so a new channel or policy could receive other
	// namespaces' events, and deleting one could silence them.
	channelBody := func(namespace, suffix string) string {
		return fmt.Sprintf(`{"namespace":%q,"name":%q,"type":"webhook","config":{"url":"https://example.invalid/a3"}}`, namespace, name("ch-"+suffix))
	}
	s.requireNSStatus(http.StatusForbidden, http.MethodPost, "/v1/notifications/channels", mktOperator, channelBody("marketing", "mkt-denied"))
	mktChannel := decodeID(s, s.requireNSStatus(http.StatusCreated, http.MethodPost, "/v1/notifications/channels", admin, channelBody("marketing", "mkt")))
	deleteAfter("/v1/notifications/channels/" + mktChannel)
	finChannel := decodeID(s, s.requireNSStatus(http.StatusCreated, http.MethodPost, "/v1/notifications/channels", admin, channelBody("finance", "fin")))
	deleteAfter("/v1/notifications/channels/" + finChannel)
	sharedChannel := decodeID(s, s.requireNSStatus(http.StatusCreated, http.MethodPost, "/v1/notifications/channels", admin, channelBody("*", "shared")))
	deleteAfter("/v1/notifications/channels/" + sharedChannel)
	s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/notifications/channels/"+mktChannel, mktOperator, "")
	s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/notifications/channels/"+sharedChannel, mktOperator, "")
	s.requireNSStatus(http.StatusForbidden, http.MethodGet, "/v1/notifications/channels/"+finChannel, mktOperator, "")
	s.requireNSStatus(http.StatusForbidden, http.MethodPatch, "/v1/notifications/channels/"+mktChannel, mktOperator, `{"enabled":false}`)
	s.requireNSStatus(http.StatusForbidden, http.MethodDelete, "/v1/notifications/channels/"+sharedChannel, mktOperator, "")
	s.requireNSStatus(http.StatusForbidden, http.MethodDelete, "/v1/notifications/channels/"+finChannel, mktOperator, "")

	policyBody := func(namespace, channel, suffix, job string) string {
		return fmt.Sprintf(`{"namespace":%q,"name":%q,"channel_id":%q,"event_types":["run_failed"],"filters":{"job_ids":[%q]}}`,
			namespace, name("pol-"+suffix), channel, job)
	}
	s.requireNSStatus(http.StatusForbidden, http.MethodPost, "/v1/notifications/policies", mktOperator, policyBody("marketing", mktChannel, "mkt-denied", f.mktJob.ID))
	// A marketing policy that today handles finance's failures: dispatch
	// matches its job filter, not its namespace.
	mktPolicy := decodeID(s, s.requireNSStatus(http.StatusCreated, http.MethodPost, "/v1/notifications/policies", admin, policyBody("marketing", mktChannel, "mkt", f.finJob.ID)))
	deleteAfter("/v1/notifications/policies/" + mktPolicy)
	s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/notifications/policies/"+mktPolicy, mktOperator, "")
	s.requireNSStatus(http.StatusForbidden, http.MethodGet, "/v1/notifications/policies/"+mktPolicy, s.namespacedKey(f, "operator", `["finance"]`).Plaintext, "")
	// The marketing operator can delete neither the policy nor its channel
	// (either would silence finance's alerts); the admin deletes both.
	s.requireNSStatus(http.StatusForbidden, http.MethodDelete, "/v1/notifications/policies/"+mktPolicy, mktOperator, "")
	s.requireNSStatus(http.StatusForbidden, http.MethodDelete, "/v1/notifications/channels/"+mktChannel, mktOperator, "")
	s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/notifications/policies/"+mktPolicy, admin, "")
	s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/notifications/channels/"+mktChannel, admin, "")
	s.requireNSStatus(http.StatusNoContent, http.MethodDelete, "/v1/notifications/policies/"+mktPolicy, admin, "")
	s.requireNSStatus(http.StatusNoContent, http.MethodDelete, "/v1/notifications/channels/"+mktChannel, admin, "")
	s.requireNSStatus(http.StatusNotFound, http.MethodGet, "/v1/notifications/policies/"+mktPolicy, admin, "")

	// Agent profiles: reads by the object's namespace; creates, edits and
	// deletes are pending D4 (jobs reference a profile by name, resolved
	// globally at runtime, so a confined delete could disable another
	// namespace's remediation).
	profileBody := func(namespace, suffix string) string {
		return fmt.Sprintf(`{"namespace":%q,"name":%q,"image":"busybox:1.36.1"}`, namespace, name("prof-"+suffix))
	}
	s.requireNSStatus(http.StatusForbidden, http.MethodPost, "/v1/agentprofiles", mktOperator, profileBody("marketing", "mkt-denied"))
	mktProfile := decodeID(s, s.requireNSStatus(http.StatusCreated, http.MethodPost, "/v1/agentprofiles", admin, profileBody("marketing", "mkt")))
	deleteAfter("/v1/agentprofiles/" + mktProfile)
	s.requireNSStatus(http.StatusForbidden, http.MethodPatch, "/v1/agentprofiles/"+mktProfile, mktOperator, `{"image":"busybox:1.36.1"}`)
	finProfile := decodeID(s, s.requireNSStatus(http.StatusCreated, http.MethodPost, "/v1/agentprofiles", admin, profileBody("finance", "fin")))
	deleteAfter("/v1/agentprofiles/" + finProfile)
	sharedProfile := decodeID(s, s.requireNSStatus(http.StatusCreated, http.MethodPost, "/v1/agentprofiles", admin, profileBody("*", "shared")))
	deleteAfter("/v1/agentprofiles/" + sharedProfile)
	s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/agentprofiles/"+mktProfile, mktOperator, "")
	s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/agentprofiles/"+sharedProfile, mktOperator, "")
	s.requireNSStatus(http.StatusForbidden, http.MethodGet, "/v1/agentprofiles/"+finProfile, mktOperator, "")
	s.requireNSStatus(http.StatusForbidden, http.MethodPatch, "/v1/agentprofiles/"+sharedProfile, mktOperator, `{"image":"busybox:1.36.1"}`)
	s.requireNSStatus(http.StatusForbidden, http.MethodDelete, "/v1/agentprofiles/"+finProfile, mktOperator, "")
	s.requireNSStatus(http.StatusForbidden, http.MethodDelete, "/v1/agentprofiles/"+sharedProfile, mktOperator, "")
	s.requireNSStatus(http.StatusForbidden, http.MethodDelete, "/v1/agentprofiles/"+mktProfile, mktOperator, "")
	s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/agentprofiles/"+mktProfile, admin, "")
	s.requireNSStatus(http.StatusNoContent, http.MethodDelete, "/v1/agentprofiles/"+mktProfile, admin, "")
	s.requireNSStatus(http.StatusNotFound, http.MethodGet, "/v1/agentprofiles/"+mktProfile, admin, "")

	// Triggers: the marketing job's own trigger is marketing's; a trigger that
	// also drives a finance job needs the role in finance too. A trigger may
	// serve several jobs; the finance job is pointed at a second marketing
	// job's trigger to make one.
	sharedJob := s.addNamespacedJob(f, fmt.Sprintf("a3-shared-trigger-%d", f.stamp), "marketing")
	httpAlias := fmt.Sprintf("a3-http-trigger-%d", f.stamp)
	s.applyDefinition(s.namespacedHTTPJobDefinition(httpAlias, "marketing"))
	httpJob := s.requireJobByAlias(httpAlias)
	f.jobs = append(f.jobs, httpJob.ID)
	var mktTrigger, sharedTrigger, finTrigger string
	s.Require().NoError(f.catalogDB.QueryRowContext(s.T().Context(), `SELECT trigger_id FROM jobs WHERE id = ?`, httpJob.ID).Scan(&mktTrigger))
	s.Require().NoError(f.catalogDB.QueryRowContext(s.T().Context(), `SELECT trigger_id FROM jobs WHERE id = ?`, sharedJob.ID).Scan(&sharedTrigger))
	s.Require().NoError(f.catalogDB.QueryRowContext(s.T().Context(), `SELECT trigger_id FROM jobs WHERE id = ?`, f.finJob.ID).Scan(&finTrigger))
	s.catalogExec(f, 1, `UPDATE jobs SET trigger_id = ? WHERE id = ?`, sharedTrigger, f.finJob.ID)
	f.undo = append(f.undo, func() { s.catalogExec(f, 1, `UPDATE jobs SET trigger_id = ? WHERE id = ?`, finTrigger, f.finJob.ID) })
	fire := map[string]string{"X-Caesium-API-Key": s.manualTriggerAPIKey}

	s.requireNSStatus(http.StatusForbidden, http.MethodGet, "/v1/triggers/"+sharedTrigger, mktOperator, "")
	s.requireNSStatus(http.StatusForbidden, http.MethodPatch, "/v1/triggers/"+sharedTrigger, mktOperator, `{}`)
	status, body := s.nsRequest(http.MethodPost, "/v1/triggers/"+sharedTrigger+"/fire", mktOperator, "", fire)
	s.Require().Equal(http.StatusForbidden, status, body)

	s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/triggers/"+mktTrigger, mktOperator, "")
	// Editing a trigger's configuration could subscribe it to another
	// namespace's webhook path: pending B2 for confined principals.
	newAlias := fmt.Sprintf("a3-trigger-%d", f.stamp)
	s.requireNSStatus(http.StatusForbidden, http.MethodPatch, "/v1/triggers/"+mktTrigger, mktOperator, fmt.Sprintf(`{"alias":%q}`, newAlias))
	s.NotContains(s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/triggers/"+mktTrigger, s.authAPIKey, ""), newAlias,
		"the refused PATCH changed nothing")
	status, body = s.nsRequest(http.MethodPost, "/v1/triggers/"+mktTrigger+"/fire", mktOperator, "", fire)
	s.Require().Equal(http.StatusAccepted, status, "firing a marketing-only HTTP trigger: %s", body)
	var fired []runResponse
	s.Require().Eventually(func() bool {
		fired = nil
		return s.tryGetJSON("/v1/jobs/"+httpJob.ID+"/runs", &fired) == nil && len(fired) > 0
	}, runTimeout, 500*time.Millisecond, "the fired trigger must start a run of the marketing job")
	s.Equal("marketing", fired[0].Namespace)
	s.awaitRun(httpJob.ID, fired[0].ID, runTimeout)

	// D9: an idempotent start recorded while the job was in marketing; the job
	// then moves to finance. A finance-only runner retrying with the same key
	// gets an audited 403 and no run body, whether or not its request matches
	// the recorded one (the record is authorised before it is compared, so a
	// mismatch cannot be told from a match); a key that can also read
	// marketing gets the run, or 422 for a different request.
	key := map[string]string{"Idempotency-Key": fmt.Sprintf("a3-d9-%d", f.stamp)}
	status, body = s.nsRequest(http.MethodPost, "/v1/jobs/"+sharedJob.ID+"/run", admin, "", key)
	s.Require().Equal(http.StatusAccepted, status, body)
	recorded := decodeID(s, body)
	s.awaitRun(sharedJob.ID, recorded, runTimeout)
	s.catalogExec(f, 1, `UPDATE jobs SET namespace = 'finance' WHERE id = ?`, sharedJob.ID)
	f.undo = append(f.undo, func() { s.catalogExec(f, 1, `UPDATE jobs SET namespace = 'marketing' WHERE id = ?`, sharedJob.ID) })

	const different = `{"priority":"high"}`
	finRunner := s.namespacedKey(f, "runner", `["finance"]`)
	for _, request := range []string{"", different} {
		status, body = s.nsRequest(http.MethodPost, "/v1/jobs/"+sharedJob.ID+"/run", finRunner.Plaintext, request, key)
		s.Require().Equal(http.StatusForbidden, status, "request %q: %s", request, body)
		s.NotContains(body, recorded, "the refused replay must not carry the recorded run")
	}
	denials, err := s.auditCLI("--namespace", "marketing", "--actor", finRunner.Meta.KeyPrefix)
	s.Require().NoError(err)
	s.Require().Len(denials, 2, "both refused retries are audited against the record's namespace")
	for _, row := range denials {
		s.Equal("auth.denied", row.Action)
		var meta struct {
			Reason       string   `json:"reason"`
			RequiredRole string   `json:"required_role"`
			Namespaces   []string `json:"namespaces"`
		}
		s.Require().NoError(json.Unmarshal(row.Metadata, &meta))
		s.Equal("insufficient_role", meta.Reason)
		s.Equal("viewer", meta.RequiredRole)
		s.Equal([]string{"marketing"}, meta.Namespaces)
	}
	bothRunner := s.namespacedKey(f, "runner", `["finance","marketing"]`).Plaintext
	status, body = s.nsRequest(http.MethodPost, "/v1/jobs/"+sharedJob.ID+"/run", bothRunner, "", key)
	s.Require().Equal(http.StatusAccepted, status, body)
	s.Equal(recorded, decodeID(s, body), "a caller that can read the run's namespace gets the original run")
	status, body = s.nsRequest(http.MethodPost, "/v1/jobs/"+sharedJob.ID+"/run", bothRunner, different, key)
	s.Require().Equal(http.StatusUnprocessableEntity, status, "a readable record still refuses a different request: %s", body)
}

// TestAuthNamespacedDatasetsBackfillsIncidents: dataset writes are cluster-only
// for confined principals until B6 (their producer resolvers are unit-tested);
// backfills are cluster-only for them until B4; and the live incident route
// resolves to the incident's namespace.
func (s *IntegrationTestSuite) TestAuthNamespacedDatasetsBackfillsIncidents() {
	s.requireAuthLane()
	f := s.newNamespacedRoutesFixture("data")
	defer s.closeNamespacedRoutesFixture(f)
	mktOperator := s.namespacedKey(f, "operator", `["marketing"]`).Plaintext
	bothOperator := s.namespacedKey(f, "operator", `["marketing","finance"]`).Plaintext
	now := time.Now().UTC()

	// Datasets: "shared" is produced by both jobs, "mkt" by the marketing job.
	shared := fmt.Sprintf("a3-shared-%d", f.stamp)
	mktOnly := fmt.Sprintf("a3-mkt-%d", f.stamp)
	declare := func(job *jobSummary, alias, dataset string) {
		s.catalogExec(f, 1, `INSERT INTO dataset_declarations (id, job_id, job_alias, name, direction, created_at, updated_at) VALUES (?, ?, ?, ?, 'produces', ?, ?)`,
			uuid.NewString(), job.ID, alias, dataset, now, now)
	}
	declare(f.mktJob, f.mktAlias, shared)
	declare(f.finJob, f.finAlias, shared)
	declare(f.mktJob, f.mktAlias, mktOnly)
	hold := func(dataset string) string {
		id := uuid.NewString()
		s.catalogExec(f, 1, `INSERT INTO dataset_holds (id, namespace, name, status, active_key, held_by_job_id, opened_at, created_at, updated_at) VALUES (?, '', ?, 'active', ?, ?, ?, ?, ?)`,
			id, dataset, dataset+"/"+id, f.mktJob.ID, now, now, now)
		return id
	}
	sharedHold, mktHold := hold(shared), hold(mktOnly)
	f.undo = append(f.undo, func() {
		s.catalogExec(f, -1, `DELETE FROM dataset_holds WHERE id IN (?, ?)`, sharedHold, mktHold)
		s.catalogExec(f, -1, `DELETE FROM dataset_declarations WHERE name IN (?, ?)`, shared, mktOnly)
		s.catalogExec(f, -1, `DELETE FROM dataset_states WHERE name IN (?, ?)`, shared, mktOnly)
	})

	advance := func(token, dataset string) (int, string) {
		return s.nsRequest(http.MethodPost, "/v1/datasets/_/"+dataset+"/advance", token, `{"watermark":"2026-10-15"}`, nil)
	}
	// Every dataset route is cluster-only for confined principals until B6:
	// the advance answers with the persisted state (last run, consumed
	// watermarks), which can predate a producer's move from another
	// namespace. Even an operator in every producer namespace is refused; the
	// cluster admin advances exactly as before.
	for _, dataset := range []string{shared, mktOnly} {
		for _, token := range []string{mktOperator, bothOperator} {
			status, body := advance(token, dataset)
			s.Require().Equal(http.StatusForbidden, status, "advance %s: %s", dataset, body)
		}
	}
	requireWatermark := func(status int, body, dataset string) {
		s.T().Helper()
		s.Require().Equal(http.StatusOK, status, body)
		var out struct {
			Outcome string `json:"outcome"`
			State   struct {
				Name      string `json:"name"`
				Watermark string `json:"watermark"`
			} `json:"state"`
		}
		s.Require().NoError(json.Unmarshal([]byte(body), &out), body)
		s.NotEmpty(out.Outcome, body)
		s.Equal(dataset, out.State.Name, body)
	}
	status, body := advance(s.authAPIKey, shared)
	requireWatermark(status, body, shared)

	release := `{"reason":"a3 namespace check"}`
	holdStatus := func(id string) string {
		s.T().Helper()
		var status string
		s.Require().NoError(f.catalogDB.QueryRowContext(s.T().Context(), `SELECT status FROM dataset_holds WHERE id = ?`, id).Scan(&status))
		return status
	}
	// A hold release answers with the whole hold, whose impact snapshot names
	// downstream consumers in any namespace: cluster-only until B6 redacts it,
	// even for an operator in every producer namespace.
	s.requireNSStatus(http.StatusForbidden, http.MethodPost, "/v1/datasets/holds/"+sharedHold+"/release", mktOperator, release)
	s.requireNSStatus(http.StatusForbidden, http.MethodPost, "/v1/datasets/holds/"+sharedHold+"/release", bothOperator, release)
	s.requireNSStatus(http.StatusForbidden, http.MethodPost, "/v1/datasets/holds/"+mktHold+"/release", mktOperator, release)
	s.Equal("active", holdStatus(sharedHold), "a refused release changes nothing")
	s.Equal("active", holdStatus(mktHold))
	s.requireNSStatus(http.StatusOK, http.MethodPost, "/v1/datasets/holds/"+sharedHold+"/release", s.authAPIKey, release)
	s.Equal("released", holdStatus(sharedHold), "the cluster admin releases exactly as before")

	// Backfills (the jobs' cron never fires, so a backfill schedules nothing)
	// are pending B4 for confined principals, even on their own namespace's
	// job: a backfill's total_runs is deduplicated against the job's runs by
	// job id alone, so under a moved job it would count the old namespace's
	// history. The cluster admin creates, reads and cancels exactly as before;
	// the job+backfill resolver is unit-tested.
	window := fmt.Sprintf(`{"start":%q,"end":%q}`, now.Add(-2*time.Hour).Format(time.RFC3339), now.Add(-time.Hour).Format(time.RFC3339))
	for _, key := range []string{mktOperator, bothOperator} {
		s.requireNSStatus(http.StatusForbidden, http.MethodPost, "/v1/jobs/"+f.mktJob.ID+"/backfill", key, window)
	}
	mktBackfill := decodeID(s, s.requireNSStatus(http.StatusAccepted, http.MethodPost, "/v1/jobs/"+f.mktJob.ID+"/backfill", s.authAPIKey, window))
	for _, key := range []string{mktOperator, bothOperator} {
		s.requireNSStatus(http.StatusForbidden, http.MethodGet, "/v1/jobs/"+f.mktJob.ID+"/backfills/"+mktBackfill, key, "")
		s.requireNSStatus(http.StatusForbidden, http.MethodPut, "/v1/jobs/"+f.mktJob.ID+"/backfills/"+mktBackfill+"/cancel", key, "")
	}
	var created struct {
		Namespace string `json:"namespace"`
		JobID     string `json:"job_id"`
	}
	s.Require().NoError(json.Unmarshal([]byte(s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/jobs/"+f.mktJob.ID+"/backfills/"+mktBackfill, s.authAPIKey, "")), &created))
	s.Equal("marketing", created.Namespace, "the backfill carries its job's namespace")
	s.Equal(f.mktJob.ID, created.JobID)
	// The backfill schedules nothing, so it may already be complete: the
	// handler then answers 409 ("not running"); either way the admin's
	// request was authorised and reached it.
	status, body = s.nsRequest(http.MethodPut, "/v1/jobs/"+f.mktJob.ID+"/backfills/"+mktBackfill+"/cancel", s.authAPIKey, "", nil)
	s.Contains([]int{http.StatusOK, http.StatusConflict}, status, body)

	// Incidents (seeded closed, so remediation never picks them up).
	incident := func(job *jobSummary, namespace string) string {
		id := uuid.NewString()
		s.catalogExec(f, 1, `INSERT INTO incidents (id, namespace, job_id, class, status, dedupe_key, opened_at, closed_at, created_at, updated_at) VALUES (?, ?, ?, 'a3_namespace_check', 'closed', ?, ?, ?, ?, ?)`,
			id, namespace, job.ID, "a3/"+id, now, now, now, now)
		return id
	}
	mktIncident, finIncident := incident(f.mktJob, "marketing"), incident(f.finJob, "finance")
	f.undo = append(f.undo, func() {
		s.catalogExec(f, -1, `DELETE FROM incidents WHERE id IN (?, ?)`, mktIncident, finIncident)
	})
	// The live incident route (reject) resolves incidents.namespace: on the
	// marketing incident the request reaches the handler, which answers 404
	// for an approval that is not there; on the finance incident it is
	// refused. Incident detail (the frozen agent allowlist names downstream
	// jobs of any namespace) and approve (executes against the job) stay
	// cluster-only until A8.
	reject := func(incident string) (int, string) {
		return s.nsRequest(http.MethodPost, "/v1/incidents/"+incident+"/approvals/"+uuid.NewString()+"/reject", mktOperator, `{"reason":"a3"}`, nil)
	}
	status, body = reject(mktIncident)
	s.Require().Equal(http.StatusNotFound, status, body)
	status, body = reject(finIncident)
	s.Require().Equal(http.StatusForbidden, status, body)
	s.requireNSStatus(http.StatusForbidden, http.MethodGet, "/v1/incidents/"+mktIncident, mktOperator, "")
	s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/incidents/"+mktIncident, s.authAPIKey, "")
}

type auditRow struct {
	ID        string          `json:"id"`
	Actor     string          `json:"actor"`
	Action    string          `json:"action"`
	Outcome   string          `json:"outcome"`
	Namespace string          `json:"namespace"`
	Metadata  json.RawMessage `json:"metadata"`
}

// auditCLI runs `caesium auth audit` (it always prints JSON) with stdout
// captured on its own and parsed as one JSON array.
func (s *IntegrationTestSuite) auditCLI(args ...string) ([]auditRow, error) {
	s.T().Helper()
	args = append([]string{"auth", "audit", "--server", s.caesiumURL, "--since", "1h", "--limit", "1000"}, args...)
	stdout, stderr, err := s.runCLISeparate(args...)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr))
	}
	var rows []auditRow
	s.Require().NoError(json.Unmarshal([]byte(stdout), &rows), "caesium auth audit stdout must be exactly one JSON array:\n%s", stdout)
	return rows, nil
}

// TestAuthAuditNamespaceFilter: middleware and controller-owned audit rows
// carry the namespace they were about; `caesium auth audit --namespace` filters
// on it; a namespace admin may read only its namespace; and the cluster admin
// without --namespace still sees every row, including identity and legacy ones.
func (s *IntegrationTestSuite) TestAuthAuditNamespaceFilter() {
	s.requireAuthLane()
	f := s.newNamespacedRoutesFixture("audit")
	defer s.closeNamespacedRoutesFixture(f)
	mkt := s.namespacedKey(f, "operator", `["marketing"]`)
	mktAdmin := s.namespacedKey(f, "admin", `["marketing"]`)

	// A middleware success (marketing), a denial (finance), a cluster-route
	// denial (*), and a controller-owned row (marketing channel delete).
	s.requireNSStatus(http.StatusOK, http.MethodPut, "/v1/jobs/"+f.mktJob.ID+"/pause", mkt.Plaintext, "")
	s.requireNSStatus(http.StatusOK, http.MethodPut, "/v1/jobs/"+f.mktJob.ID+"/unpause", mkt.Plaintext, "")
	s.requireNSStatus(http.StatusForbidden, http.MethodPut, "/v1/jobs/"+f.finJob.ID+"/pause", mkt.Plaintext, "")
	s.requireNSStatus(http.StatusForbidden, http.MethodGet, "/v1/atoms", mkt.Plaintext, "")
	// The controller-owned row: a cluster operator deletes a marketing
	// channel the admin created, and the row carries the channel's namespace
	// (every shared-object write by a confined caller is D4's).
	clusterOperator := s.namespacedKey(f, "operator", "")
	channel := decodeID(s, s.requireNSStatus(http.StatusCreated, http.MethodPost, "/v1/notifications/channels", s.authAPIKey,
		fmt.Sprintf(`{"namespace":"marketing","name":"a3-audit-%d","type":"webhook","config":{"url":"https://example.invalid/a3"}}`, f.stamp)))
	f.undo = append(f.undo, func() {
		s.nsRequest(http.MethodDelete, "/v1/notifications/channels/"+channel, s.authAPIKey, "", nil)
	})
	s.requireNSStatus(http.StatusNoContent, http.MethodDelete, "/v1/notifications/channels/"+channel, clusterOperator.Plaintext, "")
	// An unrestricted caller's audited read (the queue) is recorded against
	// the job's namespace too.
	clusterViewer := s.namespacedKey(f, "viewer", "")
	s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/jobs/"+f.mktJob.ID+"/queue", clusterViewer.Plaintext, "")
	// An identity-less row: a failed credential is audited with no namespace.
	badToken := fmt.Sprintf("csk_live_a3audit%d", f.stamp)
	status, _ := s.nsRequest(http.MethodGet, "/v1/atoms", badToken, "", nil)
	s.Require().Equal(http.StatusUnauthorized, status)
	// A pre-migration row: namespace NULL.
	legacyID := uuid.NewString()
	s.catalogExec(f, 1, `INSERT INTO audit_logs (id, timestamp, actor, action, outcome) VALUES (?, ?, ?, 'a3.legacy', 'success')`,
		legacyID, time.Now().UTC(), "a3-legacy-actor")
	f.undo = append(f.undo, func() { s.catalogExec(f, -1, `DELETE FROM audit_logs WHERE id = ?`, legacyID) })

	byAction := func(rows []auditRow) map[string][]auditRow {
		out := map[string][]auditRow{}
		for _, row := range rows {
			out[row.Action] = append(out[row.Action], row)
		}
		return out
	}

	// --namespace marketing (cluster admin from the runner env) returns only
	// marketing rows.
	rows, err := s.auditCLI("--namespace", "marketing", "--actor", mkt.Meta.KeyPrefix)
	s.Require().NoError(err)
	s.Require().NotEmpty(rows)
	for _, row := range rows {
		s.Equal("marketing", row.Namespace, "row %s %s", row.Action, row.ID)
	}
	actions := byAction(rows)
	s.Contains(actions, "job.pause")
	s.Contains(actions, "job.unpause")
	s.NotContains(actions, "auth.denied", "the denials named finance and *, not marketing")

	// The controller-owned channel row is filed under the channel's namespace.
	rows, err = s.auditCLI("--namespace", "marketing", "--actor", clusterOperator.Meta.KeyPrefix)
	s.Require().NoError(err)
	s.Contains(byAction(rows), "notification_channel.delete")

	rows, err = s.auditCLI("--namespace", "marketing", "--actor", clusterViewer.Meta.KeyPrefix)
	s.Require().NoError(err)
	s.Require().Len(rows, 1, "the cluster viewer's queue read")
	s.Equal("run_queue.read", rows[0].Action)

	rows, err = s.auditCLI("--namespace", "finance", "--actor", mkt.Meta.KeyPrefix)
	s.Require().NoError(err)
	s.Require().Len(rows, 1, "the one finance denial")
	s.Equal("auth.denied", rows[0].Action)
	var denial struct {
		Reason     string   `json:"reason"`
		Namespaces []string `json:"namespaces"`
	}
	s.Require().NoError(json.Unmarshal(rows[0].Metadata, &denial))
	s.Equal("insufficient_role", denial.Reason)
	s.Equal([]string{"finance"}, denial.Namespaces)

	rows, err = s.auditCLI("--namespace", "*", "--actor", mkt.Meta.KeyPrefix)
	s.Require().NoError(err)
	s.Require().Len(rows, 1, "the cluster-route denial")
	s.Contains(string(rows[0].Metadata), `"/v1/atoms"`)

	// A marketing admin reads marketing only; without --namespace (that is,
	// "*") or for finance it is refused.
	rows, err = s.auditCLI("--namespace", "marketing", "--actor", mkt.Meta.KeyPrefix, "--api-key", mktAdmin.Plaintext)
	s.Require().NoError(err)
	s.Contains(byAction(rows), "job.pause")
	_, err = s.auditCLI("--api-key", mktAdmin.Plaintext)
	s.Require().Error(err)
	s.Contains(err.Error(), "403")
	_, err = s.auditCLI("--namespace", "finance", "--api-key", mktAdmin.Plaintext)
	s.Require().Error(err)
	s.Contains(err.Error(), "403")

	// The cluster admin without --namespace sees every kind of row.
	rows, err = s.auditCLI()
	s.Require().NoError(err)
	seen := map[string]bool{}
	var legacy, identity bool
	for _, row := range rows {
		if row.Actor == mkt.Meta.KeyPrefix {
			seen[row.Namespace] = true
		}
		if row.ID == legacyID {
			legacy = true
			s.Empty(row.Namespace, "a pre-migration row has no namespace")
		}
		// The middleware audits a failed credential under its 13-character
		// display prefix.
		if row.Actor == badToken[:13] && row.Action == "auth.denied" {
			identity = true
			s.Empty(row.Namespace)
		}
	}
	s.True(seen["marketing"] && seen["finance"] && seen["*"], "rows of every namespace: %v", seen)
	s.True(legacy, "the legacy NULL-namespace row is still visible")
	s.True(identity, "the credential-failure row (no namespace) is still visible")
}

// TestAuthNamespacedReplayGuard drives replay through the live route: a runner
// confined to marketing replays a marketing run, and its idempotent retry
// returns the same replay. Once that recorded replay sits in a namespace the
// caller does not hold (seeded: a namespace move of the replay row; the job
// and baseline stay marketing's, so the middleware still authorises the
// retry), the replay service's guard refuses the retry with an audited 403
// that carries no replay — before anything is resumed.
func (s *IntegrationTestSuite) TestAuthNamespacedReplayGuard() {
	s.requireAuthLane()
	f := s.newNamespacedRoutesFixture("replay")
	defer s.closeNamespacedRoutesFixture(f)

	alias := fmt.Sprintf("a3-replay-%d", f.stamp)
	def := s.namespacedJobDefinition(alias, "marketing")
	def.Metadata.ReplaySafe = true
	def.Metadata.Cache = map[string]any{"ttl": "1h"}
	s.applyDefinition(def)
	job := s.requireJobByAlias(alias)
	f.jobs = append(f.jobs, job.ID)
	baseline := s.triggerRun(job.ID)
	s.Require().Equal("succeeded", s.awaitRun(job.ID, baseline, runTimeout).Status)

	runner := s.namespacedKey(f, "runner", `["marketing"]`)
	key := map[string]string{"Idempotency-Key": fmt.Sprintf("a3-replay-%d", f.stamp)}
	path := "/v1/jobs/" + job.ID + "/runs/" + baseline + "/replay"
	replay := func() (int, string) { return s.nsRequest(http.MethodPost, path, runner.Plaintext, `{"set":{}}`, key) }

	status, body := replay()
	s.Require().Equal(http.StatusAccepted, status, body)
	var first replayResponse
	s.Require().NoError(json.Unmarshal([]byte(body), &first), body)
	s.Require().NotEmpty(first.RunID)
	s.True(first.Quarantine)
	s.Require().Equal("succeeded", s.awaitRun(job.ID, first.RunID, runTimeout).Status)

	status, body = replay()
	s.Require().Equal(http.StatusAccepted, status, body)
	var again replayResponse
	s.Require().NoError(json.Unmarshal([]byte(body), &again), body)
	s.Equal(first.RunID, again.RunID, "the idempotent retry returns the recorded replay")

	s.catalogExec(f, 1, `UPDATE job_runs SET namespace = 'finance' WHERE id = ?`, first.RunID)
	f.undo = append(f.undo, func() { s.catalogExec(f, 1, `UPDATE job_runs SET namespace = 'marketing' WHERE id = ?`, first.RunID) })

	status, body = replay()
	s.Require().Equal(http.StatusForbidden, status, body)
	s.NotContains(body, first.RunID, "the refused retry must not carry the recorded replay")

	rows, err := s.auditCLI("--namespace", "finance", "--actor", runner.Meta.KeyPrefix, "--action", "auth.denied")
	s.Require().NoError(err)
	s.Require().Len(rows, 1, "the refusal is audited against the replay's namespace")
	var denial struct {
		Reason     string   `json:"reason"`
		Path       string   `json:"path"`
		Namespaces []string `json:"namespaces"`
	}
	s.Require().NoError(json.Unmarshal(rows[0].Metadata, &denial))
	s.Equal("insufficient_role", denial.Reason)
	s.Equal("/v1/jobs/:id/runs/:id/replay", denial.Path)
	s.Equal([]string{"finance"}, denial.Namespaces)
}
