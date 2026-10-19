//go:build integration

package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/connector"
	ctemporal "github.com/caesium-cloud/caesium/internal/connector/temporal"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"gopkg.in/yaml.v3"
)

// Public wire types intentionally exclude the private receipt input, schemas,
// endpoint and idempotency fingerprint. Every action below goes through HTTP.
type connectorActionReceipt struct {
	ID                 string                  `json:"id"`
	ConnectionID       string                  `json:"connection_id"`
	ExecutionID        string                  `json:"execution_id"`
	Execution          connectorReadReference  `json:"execution"`
	Action             string                  `json:"action"`
	BindingName        string                  `json:"binding_name"`
	BindingVersion     string                  `json:"binding_version"`
	ExternalUpdateID   string                  `json:"external_update_id"`
	State              string                  `json:"state"`
	Revision           uint64                  `json:"revision"`
	Attempt            uint64                  `json:"attempt"`
	Actor              connector.ActorEnvelope `json:"actor"`
	ConfigurationEpoch string                  `json:"configuration_epoch"`
	Evidence           json.RawMessage         `json:"evidence"`
	ResendAudit        json.RawMessage         `json:"resend_audit"`
}

type connectorActionResponse struct {
	Operation connectorActionReceipt `json:"operation"`
	Created   bool                   `json:"created"`
	Conflict  bool                   `json:"conflict"`
	Pending   bool                   `json:"pending"`
}

// This helper has no assertions so concurrent requests report errors back to
// the test goroutine instead of calling FailNow from a worker goroutine.
func connectorActionRequest(ctx context.Context, base, key, method, path, idempotency string, input any) (int, []byte, error) {
	var data []byte
	var err error
	if input != nil {
		data, err = json.Marshal(input)
		if err != nil {
			return 0, nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, bytes.NewReader(data))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if idempotency != "" {
		req.Header.Set("Idempotency-Key", idempotency)
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, err
}

// Raw responses are inspected before typed DTO decoding so an unexpected
// private field cannot disappear from the confidentiality proof. Credentials
// stay in memory and never enter a saved proof or a failure message.
type connectorActionRawResponse struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

type connectorActionProof struct {
	suite       *IntegrationTestSuite
	credentials []string
	responses   []connectorActionRawResponse
}

func (p *connectorActionProof) noCredentials(raw []byte) {
	p.suite.T().Helper()
	// Normalize escaped JSON strings as well as checking the literal transport
	// bytes, so a credential encoded with Unicode escapes cannot evade proof.
	var decoded any
	canonical := raw
	if json.Unmarshal(raw, &decoded) == nil {
		if normalized, err := json.Marshal(decoded); err == nil {
			canonical = normalized
		}
	}
	for _, credential := range p.credentials {
		if credential != "" && (bytes.Contains(raw, []byte(credential)) || bytes.Contains(canonical, []byte(credential))) {
			p.suite.T().Fatal("raw connector HTTP response leaked an issued credential; response withheld")
		}
	}
}

func (p *connectorActionProof) addCredential(credential string) {
	p.credentials = append(p.credentials, credential)
	for _, response := range p.responses {
		p.noCredentials(response.Body)
	}
}

func (p *connectorActionProof) capture(method, path string, status int, raw []byte) {
	s := p.suite
	s.T().Helper()
	p.noCredentials(raw)
	var decoded any
	// Avoid echoing invalid/raw server bytes in assertion output. A failing
	// confidentiality assertion must not copy leaked material into artifacts.
	s.Require().NoError(json.Unmarshal(raw, &decoded), "connector response must be valid JSON")
	inspect := func(value any) {
		receipt, ok := value.(map[string]any)
		s.Require().True(ok, "public receipt must be a JSON object")
		for name := range receipt {
			switch strings.ReplaceAll(strings.ToLower(name), "_", "") {
			case "input", "target", "bindingsnapshot", "connectionidentity", "idempotencykey", "requestfingerprint", "endpoint", "secretrefs", "certificatepaths", "openkey", "retryableunsent", "opaqueid", "providerevidence":
				s.T().Fatal("raw connector HTTP response exposed a private receipt field; response withheld")
			}
		}
	}
	if object, ok := decoded.(map[string]any); ok {
		if operation, exists := object["operation"]; exists {
			inspect(operation)
		}
		if operations, exists := object["operations"]; exists {
			page, ok := operations.([]any)
			s.Require().True(ok, "operation listing must be a JSON array")
			for _, operation := range page {
				inspect(operation)
			}
		}
	}
	p.responses = append(p.responses, connectorActionRawResponse{Method: method, Path: path, Status: status, Body: bytes.Clone(raw)})
}

func (s *IntegrationTestSuite) connectorActionHTTP(proof *connectorActionProof, key, method, path, idempotency string, input, output any, statuses ...int) []byte {
	s.T().Helper()
	code, raw, err := connectorActionRequest(s.T().Context(), s.caesiumURL, key, method, path, idempotency, input)
	s.Require().NoError(err)
	proof.capture(method, path, code, raw)
	s.Require().Contains(statuses, code, "%s %s: %s", method, path, raw)
	if output != nil {
		s.Require().NoError(json.Unmarshal(raw, output), "public response does not match its expected shape")
	}
	return raw
}

func connectorActionPath(ref messageFixtureRef) string {
	execution, _ := connector.NewExecutionReference("fixture", map[string]string{"workflow_id": ref.WorkflowID, "run_id": ref.RunID})
	return "/v1/connectors/fixture/executions/" + url.PathEscape(connector.ReferenceID(execution))
}

func connectorActionInput(note string) map[string]any {
	return map[string]any{"binding_name": "publication", "binding_version": "1", "input": map[string]any{"note": note}}
}

func connectorActionOperationPath(id string) string {
	return "/v1/connectors/fixture/operations/" + url.PathEscape(id)
}

func (s *IntegrationTestSuite) connectorActionCompleted(proof *connectorActionProof, key string, receipt connectorActionReceipt) connectorActionReceipt {
	s.T().Helper()
	var response connectorActionResponse
	s.Require().Eventually(func() bool {
		s.connectorActionHTTP(proof, key, http.MethodGet, connectorActionOperationPath(receipt.ID), "", nil, &response, http.StatusOK)
		return response.Operation.State == connector.OperationCompleted
	}, 15*time.Second, 500*time.Millisecond, "same-ID public lookup did not complete")
	s.Require().Equal(receipt.ExternalUpdateID, response.Operation.ExternalUpdateID)
	s.Require().Equal(receipt.Actor, response.Operation.Actor)
	s.Require().Equal(receipt.Execution, response.Operation.Execution)
	return response.Operation
}

func (s *IntegrationTestSuite) connectorActionRefresh(proof *connectorActionProof, key, path, id string) connectorActionReceipt {
	s.T().Helper()
	var response struct {
		Operations []connectorActionReceipt `json:"operations"`
	}
	before := connectorProviderRPCs(s.T())
	s.connectorActionHTTP(proof, key, http.MethodGet, path+"/operations", "", nil, &response, http.StatusOK)
	s.Require().Equal(before, connectorProviderRPCs(s.T()), "refresh must discover durable receipts without querying or sending Updates")
	for _, receipt := range response.Operations {
		if receipt.ID == id {
			return receipt
		}
	}
	s.T().Fatalf("fresh public operation listing omitted %s", id)
	return connectorActionReceipt{}
}

func (s *IntegrationTestSuite) connectorActionWitness(c client.Client, ref messageFixtureRef, receipt connectorActionReceipt, principal createdAPIKey) {
	s.T().Helper()
	state := s.fixtureAwait(c, fixtureRef(ref), "published")
	expected := s.messageFixtureActor(principal, receipt.ID)
	s.Require().Equal(expected, receipt.Actor)
	s.Require().Equal(&expected, state.Actor, "workflow state must expose the actual authenticated API-key principal")
	s.Require().Equal(1, state.ApprovalEffects)
	s.Require().Equal(1, messageFixtureCount(s.messageFixtureEvents(), "approval_effect", "", ref.RunID), "independent activity journal must record exactly one effect")
	found := false
	for _, event := range s.fixtureHistory(c, fixtureRef(ref)) {
		accepted := event.GetWorkflowExecutionUpdateAcceptedEventAttributes()
		if accepted == nil || accepted.GetAcceptedRequest().GetMeta().GetUpdateId() != receipt.ExternalUpdateID {
			continue
		}
		var envelope struct {
			Actor connector.ActorEnvelope `json:"_caesium_actor"`
		}
		s.Require().NoError(converter.GetDefaultDataConverter().FromPayloads(accepted.GetAcceptedRequest().GetInput().GetArgs(), &envelope))
		s.Require().Equal(expected, envelope.Actor, "actual accepted Update history must carry immutable server-derived actor")
		found = true
	}
	s.Require().True(found, "approval proof needs the real accepted Update history")
}

// TestConnectorsActions is the C2 public-surface gate. The selected fixture must
// supply the real candidate server, worker, TLS proxy, restart controller and
// durable journal. It never writes application rows or calls action services.
func (s *IntegrationTestSuite) TestConnectorsActions() {
	if os.Getenv("CAESIUM_CONNECTORS_ACTIONS_FIXTURE") != "true" {
		s.T().Skip("requires selected real connector HTTP action fixture")
	}
	s.Require().Equal("true", os.Getenv("CAESIUM_CONNECTORS_FIXTURE"))
	s.Require().Equal("true", os.Getenv("CAESIUM_CONNECTORS_OWNED_SERVER"))
	s.Require().NotEmpty(s.authAPIKey)
	artifacts := os.Getenv("CAESIUM_CONNECTORS_ARTIFACTS")
	s.Require().NotEmpty(artifacts)
	candidate := os.Getenv("CAESIUM_CONNECTORS_CANDIDATE_SHA")
	s.Require().Len(candidate, 40)
	proof := &connectorActionProof{suite: s}
	proof.addCredential(s.authAPIKey)
	var health map[string]any
	connectorReadHTTP(s.T(), connectorFixtureControlURL(), "", http.MethodGet, "/health", nil, &health)
	s.Require().Equal(true, health["ready"])
	s.Require().Equal("1.49.0", health["sdk"])
	s.Require().Equal("1.9.1", health["cli"])
	s.Require().Equal("1.32.0", health["server"])
	configRaw, err := os.ReadFile(filepath.Join(artifacts, "http-connections.yaml"))
	s.Require().NoError(err)
	config, err := connector.Parse(configRaw, nil)
	s.Require().NoError(err)
	s.Require().Len(config.Connections, 1)
	connection := config.Connections[0]
	c, err := ctemporal.Dial(s.T().Context(), ctemporal.DialConfig{Endpoint: connection.Endpoint, Namespace: connection.Scope, CertificatePaths: connection.CertificatePaths})
	s.Require().NoError(err)
	s.T().Cleanup(c.Close)

	operator := s.createAPIKeyCLI("--role", "operator", "--description", "connector HTTP actions original principal")
	other := s.createAPIKeyCLI("--role", "operator", "--description", "connector HTTP actions second principal")
	viewer := s.createAPIKeyCLI("--role", "viewer", "--description", "connector HTTP explicit Query viewer")
	runner := s.createAPIKeyCLI("--role", "runner", "--description", "connector HTTP denied action runner")
	scoped := s.createAPIKeyCLI("--role", "operator", "--scope-jobs", "connector-fixture-publish", "--description", "connector HTTP denied scoped operator")
	// Direct credential setup must use the same configured hash secret as the
	// live server, including when this is the first selected scenario.
	s.Require().NoError(env.Process())
	agent := s.mintAgentSessionToken(uuid.NewString(), "connector-fixture-publish")
	for _, credential := range []string{operator.Plaintext, other.Plaintext, viewer.Plaintext, runner.Plaintext, scoped.Plaintext, agent} {
		proof.addCredential(credential)
	}
	// The browser gates explicit resend by the authenticated stable identity,
	// never by a key prefix. Drive the real auth route for both operators.
	for _, principal := range []createdAPIKey{operator, other} {
		var identity struct {
			Kind     string `json:"kind"`
			StableID string `json:"stable_id"`
		}
		s.connectorActionHTTP(proof, principal.Plaintext, http.MethodGet, "/auth/whoami", "", nil, &identity, http.StatusOK)
		s.Require().Equal("api_key", identity.Kind)
		s.Require().Equal(principal.Meta.ID, identity.StableID)
	}
	for _, alias := range []string{"connector-fixture-publish", "connector-fixture-notify"} {
		manifest := fmt.Sprintf("apiVersion: v1\nkind: Job\nmetadata:\n  alias: %s\ntrigger:\n  type: cron\n  configuration:\n    cron: '0 0 1 1 *'\nsteps:\n  - name: work\n    image: alpine:3.23\n    command: [sh, -c, 'echo connector-http-approval']\n", alias)
		dir := s.writeJobManifest(manifest)
		defer os.RemoveAll(dir)
		s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	}
	publish, notify := s.requireJobByAlias("connector-fixture-publish"), s.requireJobByAlias("connector-fixture-notify")
	s.messageFixtureControl(http.MethodPost, "/configure", map[string]any{"runner_key": runner.Plaintext}, nil)
	full := s.fixtureStart("http-action-publication", "publication", publish.ID, notify.ID, true, false, false, false)
	ref := messageFixtureRef(full)
	initial := s.fixtureAwait(c, full, "awaiting_approval")
	s.Require().NotNil(initial.First)
	s.Require().NotEmpty(initial.First.RunID)
	path := connectorActionPath(ref)
	var detail connectorReadExecution
	s.connectorActionHTTP(proof, viewer.Plaintext, http.MethodGet, path, "", nil, &detail, http.StatusOK)
	queryBody := map[string]any{"binding_name": "publication", "binding_version": "1"}
	var queried struct {
		Result fixtureState `json:"result"`
	}
	s.connectorActionHTTP(proof, viewer.Plaintext, http.MethodPost, path+"/queries/publication_status", "", queryBody, &queried, http.StatusOK)
	s.Require().Equal("awaiting_approval", queried.Result.Stage)
	s.Require().False(queried.Result.Approved)
	beforeQueries := connectorProviderRPCs(s.T(), "/QueryWorkflow")
	for range 2 {
		s.connectorActionHTTP(proof, viewer.Plaintext, http.MethodGet, path, "", nil, &detail, http.StatusOK)
	}
	s.Require().Equal(beforeQueries, connectorProviderRPCs(s.T(), "/QueryWorkflow"), "detail refresh must never automatically execute declared Queries")

	// Role/scope/agent denial must run before provider RPC or receipt admission.
	beforeDenied := connectorProviderRPCs(s.T())
	for _, key := range []string{viewer.Plaintext, runner.Plaintext, scoped.Plaintext, agent} {
		s.connectorActionHTTP(proof, key, http.MethodPost, path+"/actions/approve_publication", uuid.NewString(), connectorActionInput("denied"), nil, http.StatusForbidden)
		s.connectorActionHTTP(proof, key, http.MethodPost, connectorActionOperationPath(uuid.NewString())+"/resubmit", "", map[string]any{"confirmed": true, "revision": 1}, nil, http.StatusForbidden)
	}
	for _, key := range []string{scoped.Plaintext, agent} {
		s.connectorActionHTTP(proof, key, http.MethodPost, path+"/queries/publication_status", "", queryBody, nil, http.StatusForbidden)
		s.connectorActionHTTP(proof, key, http.MethodGet, path+"/operations", "", nil, nil, http.StatusForbidden)
		s.connectorActionHTTP(proof, key, http.MethodGet, connectorActionOperationPath(uuid.NewString()), "", nil, nil, http.StatusForbidden)
	}
	s.connectorActionHTTP(proof, "", http.MethodPost, path+"/actions/approve_publication", "anonymous", connectorActionInput("denied"), nil, http.StatusUnauthorized)
	s.Require().Equal(beforeDenied, connectorProviderRPCs(s.T()))

	beforeRefusals := connectorProviderRPCs(s.T(), "/UpdateWorkflowExecution")
	for _, invalid := range []struct {
		suffix string
		body   map[string]any
		code   int
	}{
		{"approve_publication", map[string]any{"binding_name": "publication", "binding_version": "1", "input": map[string]any{"note": 42}}, http.StatusBadRequest},
		{"approve_publication", map[string]any{"binding_name": "publication", "binding_version": "stale", "input": map[string]any{}}, http.StatusConflict},
		{"undeclared_handler", connectorActionInput("unknown"), http.StatusConflict},
		{"approve_publication", map[string]any{"binding_name": "publication", "binding_version": "1", "input": map[string]any{"_caesium_actor": map[string]any{"role": "admin"}}}, http.StatusBadRequest},
		{"approve_publication", map[string]any{"binding_name": "publication", "binding_version": "1", "input": map[string]any{}, "_caesium_actor": map[string]any{"role": "admin"}}, http.StatusBadRequest},
		{"approve_publication", map[string]any{"binding_name": "publication", "binding_version": "1", "input": map[string]any{}, "run_id": uuid.NewString()}, http.StatusBadRequest},
	} {
		s.connectorActionHTTP(proof, operator.Plaintext, http.MethodPost, path+"/actions/"+invalid.suffix, uuid.NewString(), invalid.body, nil, invalid.code)
	}
	s.connectorActionHTTP(proof, operator.Plaintext, http.MethodPost, path+"/actions/approve_publication", "", connectorActionInput("no key"), nil, http.StatusBadRequest)
	s.connectorActionHTTP(proof, viewer.Plaintext, http.MethodPost, path+"/queries/undeclared_query", "", queryBody, nil, http.StatusConflict)
	s.connectorActionHTTP(proof, viewer.Plaintext, http.MethodPost, path+"/queries/publication_status", "", map[string]any{"binding_name": "publication", "binding_version": "stale"}, nil, http.StatusConflict)
	wrongRun := connectorActionPath(messageFixtureRef{WorkflowID: ref.WorkflowID, RunID: uuid.NewString()})
	s.connectorActionHTTP(proof, operator.Plaintext, http.MethodPost, wrongRun+"/actions/approve_publication", uuid.NewString(), connectorActionInput("pinned missing run"), nil, http.StatusNotFound, http.StatusConflict)
	unbound := s.messageFixtureStart(c, "http-unbound", "external_only", true)
	s.connectorActionHTTP(proof, operator.Plaintext, http.MethodPost, connectorActionPath(unbound)+"/actions/approve_publication", uuid.NewString(), connectorActionInput("wrong type"), nil, http.StatusConflict, http.StatusBadRequest)
	s.Require().Equal(beforeRefusals, connectorProviderRPCs(s.T(), "/UpdateWorkflowExecution"))

	// Concurrent identical HTTP submissions attach to the same durable receipt.
	key := uuid.NewString()
	type response struct {
		status int
		raw    []byte
		err    error
	}
	results := make([]response, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i].status, results[i].raw, results[i].err = connectorActionRequest(s.T().Context(), s.caesiumURL, operator.Plaintext, http.MethodPost, path+"/actions/approve_publication", key, connectorActionInput("approved through public HTTP"))
		}(i)
	}
	close(start)
	wg.Wait()
	var first connectorActionResponse
	created := 0
	for _, result := range results {
		s.Require().NoError(result.err)
		proof.capture(http.MethodPost, path+"/actions/approve_publication", result.status, result.raw)
		s.Require().Contains([]int{http.StatusOK, http.StatusCreated, http.StatusAccepted}, result.status, string(result.raw))
		var decoded connectorActionResponse
		s.Require().NoError(json.Unmarshal(result.raw, &decoded))
		if first.Operation.ID == "" {
			first = decoded
		}
		s.Require().Equal(first.Operation.ID, decoded.Operation.ID)
		if decoded.Created {
			created++
		}
	}
	s.Require().Equal(1, created)
	approved := s.connectorActionCompleted(proof, operator.Plaintext, first.Operation)
	s.Require().Equal("fixture", approved.ConnectionID)
	s.Require().Equal("fixture", approved.Execution.ConnectionID)
	s.Require().Equal(ref.WorkflowID, approved.Execution.Coordinates["workflow_id"])
	s.Require().Equal(ref.RunID, approved.Execution.Coordinates["run_id"])
	s.Require().Equal(detail.ID, approved.ExecutionID)
	s.Require().Equal("approve_publication", approved.Action)
	s.Require().Equal("publication", approved.BindingName)
	s.Require().Equal("1", approved.BindingVersion)
	s.Require().Equal(1, messageFixtureCount(s.messageFixtureEvents(), "update_rpc", approved.ExternalUpdateID, ""))
	s.connectorActionWitness(c, ref, approved, operator)
	published := s.fixtureAwait(c, full, "published")
	s.Require().NotNil(published.Second, "HTTP approval must advance to the actual second Caesium job")
	s.Require().NotEmpty(published.Second.RunID)
	s.Require().Equal(notify.ID, published.Second.JobID)
	s.Require().NotEqual(published.First.RunID, published.Second.RunID)
	s.connectorActionRefresh(proof, viewer.Plaintext, path, approved.ID)
	s.connectorActionHTTP(proof, operator.Plaintext, http.MethodPost, path+"/actions/approve_publication", key, connectorActionInput("changed payload"), nil, http.StatusConflict)
	s.connectorActionHTTP(proof, other.Plaintext, http.MethodPost, path+"/actions/approve_publication", key, connectorActionInput("approved through public HTTP"), nil, http.StatusConflict)
	var replay connectorActionResponse
	s.connectorActionHTTP(proof, operator.Plaintext, http.MethodPost, path+"/actions/approve_publication", key, connectorActionInput("approved through public HTTP"), &replay, http.StatusOK)
	s.Require().Equal(approved.ID, replay.Operation.ID)
	// A fresh Update ID after terminal receipt completion cannot add an effect.
	var terminal connectorActionResponse
	s.connectorActionHTTP(proof, other.Plaintext, http.MethodPost, path+"/actions/approve_publication", uuid.NewString(), connectorActionInput("new terminal key"), &terminal, http.StatusCreated)
	s.connectorActionCompleted(proof, other.Plaintext, terminal.Operation)
	s.Require().NotEqual(approved.ExternalUpdateID, terminal.Operation.ExternalUpdateID)
	s.Require().Equal(1, messageFixtureCount(s.messageFixtureEvents(), "approval_effect", "", ref.RunID))

	receipts := []connectorActionReceipt{approved}
	var restartEvidence []map[string]any
	for _, fault := range []string{"accepted", "rejected"} {
		principal := s.createAPIKeyCLI("--role", "operator", "--description", "connector HTTP lost "+fault+" reply original principal")
		proof.addCredential(principal.Plaintext)
		lostRef := s.messageFixtureStart(c, "http-lost-"+fault, "publication", fault == "accepted")
		lostPath := connectorActionPath(lostRef)
		s.fixtureAwait(c, fixtureRef(lostRef), "awaiting_approval")
		s.messageFixtureControl(http.MethodPost, "/fault", map[string]any{"workflow_id": lostRef.WorkflowID, "run_id": lostRef.RunID, "mode": fault}, nil)
		lostKey := uuid.NewString()
		var lost connectorActionResponse
		s.connectorActionHTTP(proof, principal.Plaintext, http.MethodPost, lostPath+"/actions/approve_publication", lostKey, connectorActionInput("real "+fault+" reply loss"), &lost, http.StatusAccepted)
		s.Require().Equal(connector.OperationUnknown, lost.Operation.State)
		s.Require().NotEmpty(lost.Operation.ExternalUpdateID)
		s.Require().Equal("fixture", lost.Operation.Execution.ConnectionID)
		s.Require().Equal(lostRef.WorkflowID, lost.Operation.Execution.Coordinates["workflow_id"])
		s.Require().Equal(lostRef.RunID, lost.Operation.Execution.Coordinates["run_id"])
		s.Require().Equal(1, messageFixtureCount(s.messageFixtureEvents(), "update_rpc", lost.Operation.ExternalUpdateID, ""))
		activated := false
		for _, event := range s.messageFixtureEvents() {
			if event.Kind == "update_rpc" && event.UpdateID == lost.Operation.ExternalUpdateID && event.ReplyLost && event.Rejected == (fault == "rejected") && event.RPCCode == "OK" {
				activated = true
			}
		}
		s.Require().True(activated, "must activate a drop of the actual real-provider response")
		s.messageFixtureControl(http.MethodPost, "/fault", map[string]any{}, nil)
		s.connectorActionRefresh(proof, viewer.Plaintext, lostPath, lost.Operation.ID)
		restartEvidence = append(restartEvidence, s.connectorHostRestart())
		var lookup connectorActionResponse
		s.connectorActionHTTP(proof, principal.Plaintext, http.MethodGet, connectorActionOperationPath(lost.Operation.ID), "", nil, &lookup, http.StatusOK)
		s.Require().Equal(lost.Operation.ExternalUpdateID, lookup.Operation.ExternalUpdateID)
		s.Require().Equal(lost.Operation.Actor, lookup.Operation.Actor)
		s.Require().Equal(1, messageFixtureCount(s.messageFixtureEvents(), "update_rpc", lost.Operation.ExternalUpdateID, ""), "restart and lookup must never resend")
		if fault == "accepted" {
			done := s.connectorActionCompleted(proof, principal.Plaintext, lookup.Operation)
			s.connectorActionWitness(c, lostRef, done, principal)
			receipts = append(receipts, done)
			continue
		}
		// NotFound after a lost validator reply preserves unknown origin. Making
		// the workflow eligible never authorizes background or other-actor send.
		s.Require().Equal(connector.OperationUnknown, lookup.Operation.State)
		s.Require().Contains(string(lookup.Operation.Evidence), "NotFound")
		s.messageFixtureControl(http.MethodPost, "/signal", map[string]any{"id": lostRef.WorkflowID, "run_id": lostRef.RunID, "name": "fixture_eligibility", "value": true}, nil)
		s.Require().Eventually(func() bool {
			state, e := s.fixtureQuery(c, fixtureRef(lostRef))
			return e == nil && state.Eligible && !state.Approved
		}, 10*time.Second, 200*time.Millisecond)
		s.connectorActionHTTP(proof, principal.Plaintext, http.MethodPost, lostPath+"/actions/approve_publication", lostKey, connectorActionInput("real rejected reply loss"), &lookup, http.StatusAccepted)
		var conflict connectorActionResponse
		s.connectorActionHTTP(proof, other.Plaintext, http.MethodPost, lostPath+"/actions/approve_publication", uuid.NewString(), connectorActionInput("bypass open guard"), &conflict, http.StatusConflict)
		s.Require().True(conflict.Conflict)
		s.Require().Equal(lost.Operation.ID, conflict.Operation.ID)
		beforeConfirmation := connectorProviderRPCs(s.T(), "/UpdateWorkflowExecution")
		resubmit := connectorActionOperationPath(lost.Operation.ID) + "/resubmit"
		s.connectorActionHTTP(proof, other.Plaintext, http.MethodPost, resubmit, "", map[string]any{"confirmed": true, "revision": lookup.Operation.Revision}, nil, http.StatusForbidden)
		s.connectorActionHTTP(proof, principal.Plaintext, http.MethodPost, resubmit, "", map[string]any{"confirmed": false, "revision": lookup.Operation.Revision}, nil, http.StatusBadRequest, http.StatusConflict)
		s.connectorActionHTTP(proof, principal.Plaintext, http.MethodPost, resubmit, "", map[string]any{"confirmed": true, "revision": lost.Operation.Revision}, nil, http.StatusConflict)
		s.Require().Equal(beforeConfirmation, connectorProviderRPCs(s.T(), "/UpdateWorkflowExecution"))
		s.connectorActionBindingMismatch(proof, viewer, principal, lostPath, lookup.Operation, configRaw)
		// Re-read the current revision after the explicit configuration restarts.
		s.connectorActionHTTP(proof, principal.Plaintext, http.MethodGet, connectorActionOperationPath(lost.Operation.ID), "", nil, &lookup, http.StatusOK)
		s.Require().Equal(connector.OperationUnknown, lookup.Operation.State)
		s.Require().Equal(uint64(1), lookup.Operation.Attempt)
		s.Require().Equal(1, messageFixtureCount(s.messageFixtureEvents(), "update_rpc", lost.Operation.ExternalUpdateID, ""))
		var confirmed connectorActionResponse
		s.connectorActionHTTP(proof, principal.Plaintext, http.MethodPost, resubmit, "", map[string]any{"confirmed": true, "revision": lookup.Operation.Revision}, &confirmed, http.StatusOK, http.StatusAccepted)
		done := s.connectorActionCompleted(proof, principal.Plaintext, confirmed.Operation)
		s.Require().Equal(uint64(2), done.Attempt)
		s.Require().Equal(lost.Operation.ConfigurationEpoch, done.ConfigurationEpoch, "resend retains original admission provenance")
		s.Require().Contains(string(done.ResendAudit), principal.Meta.ID)
		s.Require().Equal(2, messageFixtureCount(s.messageFixtureEvents(), "update_rpc", done.ExternalUpdateID, ""))
		s.connectorActionWitness(c, lostRef, done, principal)
		receipts = append(receipts, done)
	}

	// A visible validator failure has no trustworthy origin bit in this pinned
	// protocol. The API must show failure_origin_unknown, never invent rejection.
	rejected := s.messageFixtureStart(c, "http-observed-failure", "publication", false)
	s.fixtureAwait(c, fixtureRef(rejected), "awaiting_approval")
	failurePrincipal := s.createAPIKeyCLI("--role", "operator", "--description", "connector HTTP observed failure")
	proof.addCredential(failurePrincipal.Plaintext)
	var failure connectorActionResponse
	s.connectorActionHTTP(proof, failurePrincipal.Plaintext, http.MethodPost, connectorActionPath(rejected)+"/actions/approve_publication", uuid.NewString(), connectorActionInput("validator not eligible"), &failure, http.StatusCreated, http.StatusAccepted)
	s.Require().Equal(connector.OperationUnknown, failure.Operation.State)
	s.Require().Contains(string(failure.Operation.Evidence), "failure_origin_unknown")
	s.Require().Zero(messageFixtureCount(s.messageFixtureEvents(), "approval_effect", "", rejected.RunID))

	// Closed-workflow detail reads do not run Queries. Explicit Queries keep the
	// same six-per-minute principal budget on a closed execution.
	s.fixtureSignal(full, "fixture_finish", true)
	closedViewer := s.createAPIKeyCLI("--role", "viewer", "--description", "connector closed explicit query budget")
	proof.addCredential(closedViewer.Plaintext)
	s.Require().Eventually(func() bool {
		s.connectorActionHTTP(proof, closedViewer.Plaintext, http.MethodGet, path, "", nil, &detail, http.StatusOK)
		return detail.Metadata.Terminal
	}, 10*time.Second, 500*time.Millisecond)
	queryStart := connectorProviderRPCs(s.T(), "/QueryWorkflow")
	for range 6 {
		s.connectorActionHTTP(proof, closedViewer.Plaintext, http.MethodPost, path+"/queries/publication_status", "", queryBody, &queried, http.StatusOK)
		s.Require().Equal("published", queried.Result.Stage)
	}
	s.Require().Equal(queryStart+6, connectorProviderRPCs(s.T(), "/QueryWorkflow"))
	s.connectorActionHTTP(proof, closedViewer.Plaintext, http.MethodPost, path+"/queries/publication_status", "", queryBody, nil, http.StatusTooManyRequests)
	s.Require().Equal(queryStart+6, connectorProviderRPCs(s.T(), "/QueryWorkflow"), "query-budget denial must precede RPC")

	// Durable catalog/audit evidence is read through the running application's
	// diagnostic and audit APIs, then preserved with the real provider journal.
	for _, receipt := range receipts {
		s.Require().Equal([][]any{{float64(1)}}, s.connectorRows("SELECT COUNT(*) FROM connector_operations WHERE id = '"+receipt.ID+"'"))
	}
	var audit []struct {
		Actor    string         `json:"actor"`
		Action   string         `json:"action"`
		Metadata map[string]any `json:"metadata"`
	}
	s.connectorActionHTTP(proof, s.authAPIKey, http.MethodGet, "/v1/auth/audit?limit=1000", "", nil, &audit, http.StatusOK)
	for _, receipt := range receipts {
		found := false
		for _, row := range audit {
			if row.Action == "connector.action_dispatch" && row.Metadata["operation_id"] == receipt.ID {
				s.Require().Equal(receipt.Actor.Subject, row.Actor)
				s.Require().Equal(receipt.Actor.StableID, row.Metadata["stable_id"])
				s.Require().Equal(receipt.ExecutionID, row.Metadata["execution_id"])
				found = true
			}
		}
		s.Require().True(found, "each actual dispatch needs durable actor/target audit")
	}
	evidence := map[string]any{"candidate_sha": candidate, "qualification": "real authenticated HTTP, process restart, actual Temporal histories and independent effects", "receipts": receipts, "restart": restartEvidence, "audit": audit, "events": s.messageFixtureEvents(), "sdk": health["sdk"], "cli": health["cli"], "server": health["server"]}
	raw := mustConnectorJSON(s.T(), evidence)
	proof.noCredentials(raw)
	for _, response := range proof.responses {
		proof.noCredentials(response.Body)
	}
	responseEvidence := mustConnectorJSON(s.T(), map[string]any{"candidate_sha": candidate, "responses": proof.responses})
	proof.noCredentials(responseEvidence)
	s.Require().NoError(os.WriteFile(filepath.Join(artifacts, "actions-http-responses.json"), responseEvidence, 0600))
	s.Require().NoError(os.WriteFile(filepath.Join(artifacts, "actions-http-evidence.json"), raw, 0600))
}

func (s *IntegrationTestSuite) connectorActionBindingMismatch(proof *connectorActionProof, viewer, principal createdAPIKey, executionPath string, receipt connectorActionReceipt, original []byte) {
	s.T().Helper()
	var document map[string]any
	s.Require().NoError(yaml.Unmarshal(original, &document))
	connections := document["connections"].([]any)
	bindings := connections[0].(map[string]any)["bindings"].([]any)
	bindings[0].(map[string]any)["version"] = "2"
	changed, err := yaml.Marshal(document)
	s.Require().NoError(err)
	transition := s.connectorReadReconfigure(viewer, changed)
	before := connectorProviderRPCs(s.T())
	s.connectorActionHTTP(proof, principal.Plaintext, http.MethodGet, connectorActionOperationPath(receipt.ID), "", nil, nil, http.StatusConflict)
	s.connectorActionHTTP(proof, principal.Plaintext, http.MethodPost, connectorActionOperationPath(receipt.ID)+"/resubmit", "", map[string]any{"confirmed": true, "revision": receipt.Revision}, nil, http.StatusConflict)
	s.connectorActionRefresh(proof, viewer.Plaintext, executionPath, receipt.ID)
	s.Require().Equal(before, connectorProviderRPCs(s.T()), "changed binding must refuse receipt RPCs before contacting Temporal")
	restore := s.connectorReadReconfigure(viewer, original)
	s.Require().NoError(os.WriteFile(filepath.Join(os.Getenv("CAESIUM_CONNECTORS_ARTIFACTS"), "actions-config-transition.json"), mustConnectorJSON(s.T(), map[string]any{"changed": transition, "restored": restore, "operation_id": receipt.ID, "rpc_delta": 0}), 0600))
}
