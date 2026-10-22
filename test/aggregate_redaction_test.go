//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
)

// Aggregate redaction (identity-and-access design §8.3, plan item B5): the
// lineage impact query and the contract graph are computed over every
// namespace, then a namespace-confined caller sees another namespace's nodes
// only as {id, kind: "external"} placeholders and the edges touching them as
// bare endpoints.

type redactedImpactNode struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	DatasetName string `json:"dataset_name"`
	JobAlias    string `json:"job_alias"`
	Depth       int    `json:"depth"`
}

type redactedImpact struct {
	RootNamespace string               `json:"root_namespace"`
	RootName      string               `json:"root_name"`
	Downstream    []redactedImpactNode `json:"downstream"`
}

type redactedGraphNode struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Alias string `json:"alias"`
}

type redactedGraphEdge struct {
	ID    string `json:"id"`
	From  string `json:"from"`
	To    string `json:"to"`
	Class string `json:"class"`
}

type redactedGraph struct {
	Nodes []redactedGraphNode `json:"nodes"`
	Edges []redactedGraphEdge `json:"edges"`
}

// datasetJob is a namespaced job whose one step produces and consumes the
// given declared datasets (each with an optional inline schema).
func (s *IntegrationTestSuite) datasetJob(alias, namespace string, consumes []schema.ConsumedDataset, produces []schema.ProducedDataset) schema.Definition {
	def := s.namespacedJobDefinition(alias, namespace)
	def.Steps[0].Datasets = &schema.StepDatasets{Consumes: consumes, Produces: produces}
	return def
}

func stringProperty(key string) map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{key: map[string]any{"type": "string"}}}
}

// runToSuccess runs the applied job once as the cluster admin and waits for
// it to succeed (its lineage is recorded on completion).
func (s *IntegrationTestSuite) runToSuccess(alias string) {
	s.T().Helper()
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job, alias)
	runID := s.triggerRun(job.ID)
	s.Require().Equal("succeeded", s.awaitRun(job.ID, runID, 120*time.Second).Status, alias)
}

// cleanupInOrder deletes the scenario's live jobs in the order given, so a
// consumer goes before its producer: a dangling consumer would fail every
// later apply's cross-job dataset check (a namespace prune in a later scenario
// may otherwise retire only the producer).
func (s *IntegrationTestSuite) cleanupInOrder(aliases ...string) {
	s.T().Cleanup(func() {
		for _, alias := range aliases {
			if job := s.b9Job(alias); job != nil {
				status, body := s.requestWithKey(http.MethodDelete, "/v1/jobs/"+job.ID, s.authAPIKey, nil)
				s.Contains([]int{http.StatusAccepted, http.StatusNotFound}, status, "delete %s: %s", alias, body)
			}
		}
	})
}

// seedObservedLineage records an observed lineage row on the alias's latest
// task run (a precondition only; the feature is asserted through HTTP).
func (s *IntegrationTestSuite) seedObservedLineage(alias, namespace, name, direction string) {
	s.T().Helper()
	catalog := s.openIntegrationCatalogDB()
	defer func() { s.NoError(catalog.Close()) }()
	var taskRunID string
	s.Require().NoError(catalog.QueryRowContext(s.T().Context(),
		`SELECT tr.id FROM task_runs tr JOIN job_runs jr ON jr.id = tr.job_run_id JOIN jobs j ON j.id = jr.job_id
		 WHERE j.alias = ? ORDER BY jr.started_at DESC LIMIT 1`, alias).Scan(&taskRunID))
	_, err := catalog.ExecContext(s.T().Context(),
		`INSERT INTO lineage_datasets (id, task_run_id, namespace, name, direction, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), taskRunID, namespace, name, direction, time.Now().UTC())
	s.Require().NoError(err)
}

func impactPath(root string) string {
	return "/v1/lineage/impact?" + url.Values{"namespace": {""}, "name": {root}}.Encode()
}

func (s *IntegrationTestSuite) impactAs(token, root string) (redactedImpact, string) {
	s.T().Helper()
	status, _, body := s.nsGet(impactPath(root), token)
	s.Require().Equal(http.StatusOK, status, body)
	var out redactedImpact
	s.Require().NoError(json.Unmarshal([]byte(body), &out), body)
	s.Equal("", out.RootNamespace, "the caller's root is echoed back")
	s.Equal(root, out.RootName, "the caller's root is echoed back")
	return out, body
}

func (s *IntegrationTestSuite) graphAs(token string) (redactedGraph, string) {
	s.T().Helper()
	status, _, body := s.nsGet("/v1/contracts/graph", token)
	s.Require().Equal(http.StatusOK, status, body)
	var out redactedGraph
	s.Require().NoError(json.Unmarshal([]byte(body), &out), body)
	return out, body
}

// TestAuthAggregateRedactionHidesForeignNodes: a marketing producer feeds a
// finance consumer that feeds a marketing consumer, beside a finance-only
// pair. A marketing viewer sees its own jobs and datasets, the finance hops as
// placeholders, and nothing finance named, declared or observed; the cluster
// admin sees everything.
func (s *IntegrationTestSuite) TestAuthAggregateRedactionHidesForeignNodes() {
	s.requireAuthLane()
	s.requirePolicyFixture()
	stamp := time.Now().UnixNano()
	viewer := s.mintNamespacedKey("viewer", "marketing")

	// Every finance identifier carries "b5-fin-" or "b5fin", so one grep
	// covers aliases, dataset names and schema text.
	root := fmt.Sprintf("b5-root-%d", stamp)
	finDataset := fmt.Sprintf("b5-fin-ds-%d", stamp)
	mktOut := fmt.Sprintf("b5-mkt-out-%d", stamp)
	finSecret := fmt.Sprintf("b5-fin-secret-%d", stamp)
	finSecretOut := fmt.Sprintf("b5-fin-secret-out-%d", stamp)
	mktProducer := fmt.Sprintf("b5-mkt-producer-%d", stamp)
	finConsumer := fmt.Sprintf("b5-fin-consumer-%d", stamp)
	mktConsumer := fmt.Sprintf("b5-mkt-consumer-%d", stamp)
	finSecretProducer := fmt.Sprintf("b5-fin-secret-producer-%d", stamp)
	finSecretConsumer := fmt.Sprintf("b5-fin-secret-consumer-%d", stamp)
	mktColumn := fmt.Sprintf("b5mktcol%d", stamp)
	finColumn := fmt.Sprintf("b5fincol%d", stamp)
	finSecretColumn := fmt.Sprintf("b5finsecretcol%d", stamp)

	s.cleanupInOrder(finSecretConsumer, finSecretProducer, mktConsumer, finConsumer, mktProducer)
	s.applyDefinition(s.datasetJob(mktProducer, "marketing", nil,
		[]schema.ProducedDataset{{Name: root, Schema: stringProperty(mktColumn)}}))
	s.applyDefinition(s.datasetJob(finConsumer, "finance",
		[]schema.ConsumedDataset{{Name: root, Schema: stringProperty(mktColumn)}},
		[]schema.ProducedDataset{{Name: finDataset, Schema: stringProperty(finColumn)}}))
	s.applyDefinition(s.datasetJob(mktConsumer, "marketing",
		[]schema.ConsumedDataset{{Name: finDataset}},
		[]schema.ProducedDataset{{Name: mktOut}}))
	s.applyDefinition(s.datasetJob(finSecretProducer, "finance", nil,
		[]schema.ProducedDataset{{Name: finSecret, Schema: stringProperty(finSecretColumn)}}))
	s.applyDefinition(s.datasetJob(finSecretConsumer, "finance",
		[]schema.ConsumedDataset{{Name: finSecret, Schema: stringProperty(finSecretColumn)}},
		[]schema.ProducedDataset{{Name: finSecretOut}}))
	for _, alias := range []string{mktProducer, finConsumer, mktConsumer, finSecretProducer, finSecretConsumer} {
		s.runToSuccess(alias)
	}

	// The cluster admin's impact cone reaches the marketing consumer through
	// the finance hop once lineage is persisted.
	var admin redactedImpact
	s.Require().Eventually(func() bool {
		admin = redactedImpact{}
		if err := s.tryGetJSON(impactPath(root), &admin); err != nil {
			return false
		}
		for _, node := range admin.Downstream {
			if node.DatasetName == mktOut {
				return true
			}
		}
		return false
	}, 60*time.Second, 500*time.Millisecond, "lineage never linked %s to %s", root, mktOut)
	adminBody, err := json.Marshal(admin)
	s.Require().NoError(err)
	s.Contains(string(adminBody), finConsumer, "the cluster admin sees the finance hop")

	// The marketing viewer: the finance hop is a placeholder at depth 0, the
	// marketing consumer past it is shown whole at depth 1.
	confined, body := s.impactAs(viewer.Plaintext, root)
	s.NotContains(body, "b5-fin-")
	s.NotContains(body, "b5fin")
	s.NotContains(body, "finance")
	var placeholder *redactedImpactNode
	var mktNode bool
	for i, node := range confined.Downstream {
		if node.Kind == "external" {
			s.Equal(0, node.Depth)
			s.True(strings.HasPrefix(node.ID, "external:"), node.ID)
			placeholder = &confined.Downstream[i]
		}
		if node.DatasetName == mktOut {
			mktNode = true
			s.Equal(mktConsumer, node.JobAlias)
			s.Equal(1, node.Depth)
		}
	}
	s.Require().NotNil(placeholder, body)
	s.True(mktNode, body)
	again, _ := s.impactAs(viewer.Plaintext, root)
	for _, node := range again.Downstream {
		s.NotEqual(placeholder.ID, node.ID, "placeholder ids are unlinkable across responses")
	}

	// A finance-only root reads exactly as a root nobody recorded: same status
	// and an empty downstream, while the admin sees its consumer.
	foreign, _ := s.impactAs(viewer.Plaintext, finSecret)
	s.Empty(foreign.Downstream)
	missing, _ := s.impactAs(viewer.Plaintext, fmt.Sprintf("b5-nobody-recorded-%d", stamp))
	s.Empty(missing.Downstream)
	s.Require().Eventually(func() bool {
		var whole redactedImpact
		return s.tryGetJSON(impactPath(finSecret), &whole) == nil && len(whole.Downstream) > 0
	}, 60*time.Second, 500*time.Millisecond, "the admin never saw %s's consumer", finSecret)

	// Colliding edge ids: the derived edge id is a plain concatenation, so a
	// marketing edge a -> "<c>:/z" on dataset d and a finance edge a -> "<c>"
	// on dataset "z:/d" share one id, and derivation merges the finance
	// consumer's schema into the marketing edge. The viewer must not read it.
	collideProducer := fmt.Sprintf("b5c-a-%d", stamp)
	collideFinance := fmt.Sprintf("b5c-c-%d", stamp)
	collideMarketing := collideFinance + ":/z"
	collideDataset := fmt.Sprintf("b5c-pub-%d", stamp)
	collideSecret := fmt.Sprintf("b5fin-collide-%d", stamp)
	shared := func(description string) map[string]any {
		property := map[string]any{"type": "string"}
		if description != "" {
			property["description"] = description
		}
		return map[string]any{"type": "object", "properties": map[string]any{"shared": property}}
	}
	s.cleanupInOrder(collideMarketing, collideFinance, collideProducer)
	s.applyDefinition(s.datasetJob(collideProducer, "marketing", nil, []schema.ProducedDataset{
		{Name: collideDataset}, {Name: "z:/" + collideDataset, Schema: shared("")},
	}))
	s.applyDefinition(s.datasetJob(collideMarketing, "marketing", []schema.ConsumedDataset{{Name: collideDataset}}, nil))
	s.applyDefinition(s.datasetJob(collideFinance, "finance",
		[]schema.ConsumedDataset{{Name: "z:/" + collideDataset, Schema: shared(collideSecret)}}, nil))
	adminStatus, _, adminRaw := s.nsGet("/v1/contracts/graph", s.authAPIKey)
	s.Require().Equal(http.StatusOK, adminStatus, adminRaw)
	s.Contains(adminRaw, collideSecret, "the cluster admin's graph carries the merged finance schema")

	// The contract graph, over HTTP and through `caesium contract graph --json`.
	graph, graphBody := s.graphAs(viewer.Plaintext)
	stdout, stderr, err := s.runCLIWithEnv([]string{"CAESIUM_API_KEY=" + viewer.Plaintext},
		"contract", "graph", "--json", "--server", s.caesiumURL)
	s.Require().NoError(err, stderr)
	var cliGraph redactedGraph
	s.Require().NoError(json.Unmarshal([]byte(stdout), &cliGraph), "stdout must be clean JSON: %s", stdout)
	for _, out := range []string{graphBody, stdout} {
		s.NotContains(out, "b5-fin-")
		s.NotContains(out, "b5fin")
		s.NotContains(out, mktColumn, "the producer schema rides only the collapsed edge")
	}
	external := map[string]bool{}
	var producerNode bool
	for _, node := range graph.Nodes {
		if node.Kind == "external" {
			s.Empty(node.Alias)
			external[node.ID] = true
		}
		producerNode = producerNode || node.Alias == mktProducer
	}
	s.True(producerNode, graphBody)
	s.NotContains(graphBody, collideSecret, "a colliding foreign edge's schema never rides a visible edge")
	var collidedEdge bool
	for _, edge := range graph.Edges {
		if edge.From == "job:"+collideProducer && edge.To == "job:"+collideMarketing {
			collidedEdge = true
			s.Empty(edge.Class, "the merged edge is collapsed to its endpoints")
		}
	}
	s.True(collidedEdge, graphBody)
	var intoForeign, outOfForeign bool
	for _, edge := range graph.Edges {
		if edge.From == "job:"+mktProducer && external[edge.To] {
			intoForeign = true
			s.Empty(edge.Class, "a collapsed edge keeps only its endpoints")
		}
		if external[edge.From] && edge.To == "job:"+mktConsumer {
			outOfForeign = true
		}
	}
	s.True(intoForeign, "marketing producer -> placeholder: %s", graphBody)
	s.True(outOfForeign, "placeholder -> marketing consumer: %s", graphBody)
	var cliExternal bool
	for _, node := range cliGraph.Nodes {
		cliExternal = cliExternal || node.Kind == "external"
	}
	s.True(cliExternal, stdout)

	// The dataset filter: an edge observed between the two finance jobs, in
	// an OpenLineage namespace a filter can name. The lineage rows are seeded
	// on the jobs' real runs (the precondition only; declared datasets carry
	// the empty namespace no filter can name).
	observed := fmt.Sprintf("b5-fin-observed-%d", stamp)
	s.seedObservedLineage(finSecretProducer, "lake", observed, "output")
	s.seedObservedLineage(finSecretConsumer, "lake", observed, "input")
	filterPath := "/v1/contracts/graph?" + url.Values{"dataset": {"lake/" + observed}}.Encode()
	var adminFiltered redactedGraph
	s.Require().NoError(s.tryGetJSON(filterPath, &adminFiltered))
	s.Require().NotEmpty(adminFiltered.Edges, "the cluster admin's filter matches the observed edge")
	for _, edge := range adminFiltered.Edges {
		s.Equal("evidence", edge.Class)
	}
	status, _, foreignBody := s.nsGet(filterPath, viewer.Plaintext)
	s.Require().Equal(http.StatusOK, status, foreignBody)
	missingPath := "/v1/contracts/graph?" + url.Values{"dataset": {fmt.Sprintf("lake/b5-nobody-observed-%d", stamp)}}.Encode()
	status, _, missingBody := s.nsGet(missingPath, viewer.Plaintext)
	s.Require().Equal(http.StatusOK, status, missingBody)
	s.JSONEq(`{"nodes":[],"edges":[]}`, foreignBody, "a dataset seen only through collapsed edges matches nothing")
	s.Equal(missingBody, foreignBody, "a foreign dataset filter reads exactly as a nonexistent one")
	cliFiltered, stderr, err := s.runCLIWithEnv([]string{"CAESIUM_API_KEY=" + viewer.Plaintext},
		"contract", "graph", "--dataset", "lake/"+observed, "--json", "--server", s.caesiumURL)
	s.Require().NoError(err, stderr)
	s.NotContains(cliFiltered, "b5-fin-")

	// The cluster admin sees the whole graph.
	var adminGraph redactedGraph
	s.Require().NoError(s.tryGetJSON("/v1/contracts/graph", &adminGraph))
	adminGraphBody, err := json.Marshal(adminGraph)
	s.Require().NoError(err)
	for _, alias := range []string{finConsumer, finSecretProducer, finSecretConsumer} {
		s.Contains(string(adminGraphBody), alias)
	}
	s.NotContains(string(adminGraphBody), `"kind":"external"`)
}

// TestAuthAggregateRedactionConfinedToOwnNamespaces needs no policy file (it
// also runs on the file-less coverage auth journey): to a marketing viewer,
// jobs in the default namespace are foreign, so their aliases, datasets and
// schemas never reach it, and their dataset reads as one nobody recorded.
func (s *IntegrationTestSuite) TestAuthAggregateRedactionConfinedToOwnNamespaces() {
	s.requireAuthLane()
	stamp := time.Now().UnixNano()
	viewer := s.mintNamespacedKey("viewer", "marketing")

	root := fmt.Sprintf("b5-dflt-root-%d", stamp)
	out := fmt.Sprintf("b5-dflt-out-%d", stamp)
	producer := fmt.Sprintf("b5-dflt-producer-%d", stamp)
	consumer := fmt.Sprintf("b5-dflt-consumer-%d", stamp)
	column := fmt.Sprintf("b5dfltcol%d", stamp)
	s.cleanupInOrder(consumer, producer)
	s.applyDefinition(s.datasetJob(producer, "default", nil, []schema.ProducedDataset{{Name: root, Schema: stringProperty(column)}}))
	s.applyDefinition(s.datasetJob(consumer, "default",
		[]schema.ConsumedDataset{{Name: root, Schema: stringProperty(column)}},
		[]schema.ProducedDataset{{Name: out}}))
	s.runToSuccess(producer)
	s.runToSuccess(consumer)
	s.Require().Eventually(func() bool {
		var whole redactedImpact
		return s.tryGetJSON(impactPath(root), &whole) == nil && len(whole.Downstream) > 0
	}, 60*time.Second, 500*time.Millisecond, "lineage never linked %s", root)

	confined, body := s.impactAs(viewer.Plaintext, root)
	s.Empty(confined.Downstream, "a root another namespace produces reads as unrecorded: %s", body)
	missing, _ := s.impactAs(viewer.Plaintext, fmt.Sprintf("b5-dflt-nobody-%d", stamp))
	s.Empty(missing.Downstream)

	graph, graphBody := s.graphAs(viewer.Plaintext)
	s.NotContains(graphBody, "b5-dflt")
	s.NotContains(graphBody, "b5dflt")
	var external bool
	for _, node := range graph.Nodes {
		external = external || node.Kind == "external"
	}
	s.True(external, "the default jobs are placeholders: %s", graphBody)
}
