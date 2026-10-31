package contract

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/authz"
	internalcontract "github.com/caesium-cloud/caesium/internal/contract"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	dbfaults "github.com/caesium-cloud/caesium/internal/testutil"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func objectSchema(keys ...string) map[string]any {
	properties := map[string]any{}
	for _, key := range keys {
		properties[key] = map[string]any{"type": "string"}
	}
	return map[string]any{"type": "object", "properties": properties}
}

func onRunCompleted(producer string, mapping map[string]any) internalcontract.Trigger {
	return internalcontract.Trigger{
		Type: schema.TriggerEvent,
		Configuration: map[string]any{
			"events":       []any{map[string]any{"type": "run_completed", "source": "caesium", "filter": map[string]any{"job_alias": producer}}},
			"paramMapping": mapping,
		},
	}
}

func producing(alias, namespace string, produces []internalcontract.ProducedDataset, consumes []internalcontract.ConsumedDataset) internalcontract.Job {
	return internalcontract.Job{
		ID: uuid.New(), Alias: alias, Namespace: namespace, TriggerID: uuid.New(),
		Steps: []internalcontract.Step{{Name: "step", OutputSchema: objectSchema("x"), Produces: produces, Consumes: consumes}},
	}
}

// redactionWorld is the cross-namespace graph the redaction tests read:
//
//	m1 -mkt-ds-> m3, m4             (marketing, whole)
//	m1 -sales, sales2-> f1          (collapsed to one edge)
//	f1 -fin-ds-> f2 -fin-ds2-> m2   (foreign topology kept as placeholders)
//	m1 ~inferred~> m4               (m4's trigger is shared with finance's f9)
//	ghost ~inferred~> m5            (unknown producer)
//	m1 -lake/obs-> m3 evidence      (observed from a finance run)
func redactionWorld(t *testing.T) internalcontract.Graph {
	t.Helper()
	shared := uuid.New()
	m4 := producing("b5-m4", "marketing", nil, []internalcontract.ConsumedDataset{{Name: "mkt-ds"}})
	m4.TriggerID = shared
	m4.Trigger = onRunCompleted("b5-m1", map[string]any{"x": "$.tasks[0].output.x"})
	f9 := producing("b5-f9-secret", "finance", nil, nil)
	f9.TriggerID = shared
	m1 := producing("b5-m1", "marketing", []internalcontract.ProducedDataset{
		{Name: "mkt-ds"},
		{Name: "sales-secret", Schema: objectSchema("b5mktcol")},
		{Name: "sales2-secret"},
	}, nil)
	m3 := producing("b5-m3", "marketing", nil, []internalcontract.ConsumedDataset{{Name: "mkt-ds"}})
	m5 := producing("b5-m5", "marketing", nil, nil)
	m5.Trigger = onRunCompleted("b5-ghost-secret", map[string]any{"x": "$.tasks[0].output.x"})
	graph, err := internalcontract.DeriveGraph(internalcontract.DeriveInput{
		Jobs: []internalcontract.Job{
			m1, m3, m4, m5, f9,
			producing("b5-f1-secret", "finance", []internalcontract.ProducedDataset{{Name: "fin-ds-secret"}},
				[]internalcontract.ConsumedDataset{{Name: "sales-secret", Schema: objectSchema("b5fincol")}, {Name: "sales2-secret"}}),
			producing("b5-f2-secret", "finance", []internalcontract.ProducedDataset{{Name: "fin-ds2-secret"}}, []internalcontract.ConsumedDataset{{Name: "fin-ds-secret"}}),
			producing("b5-m2", "marketing", nil, []internalcontract.ConsumedDataset{{Name: "fin-ds2-secret"}}),
		},
		Evidence: []internalcontract.EvidenceRecord{{
			ProducerJobID: m1.ID, ProducerJobAlias: "b5-m1", ProducerNamespace: "marketing", ProducerRunNamespace: "marketing",
			ConsumerJobID: m3.ID, ConsumerJobAlias: "b5-m3", ConsumerNamespace: "marketing", ConsumerRunNamespace: "finance",
			Dataset: internalcontract.DatasetRef{Namespace: "lake", Name: "obs-secret"}, LastSeen: time.Now().UTC(),
		}},
	})
	require.NoError(t, err)
	return graph
}

func marketingRedactor(t *testing.T) *authz.Redactor {
	t.Helper()
	r := authz.NewRedactor(authz.WithAllowedNamespaces(context.Background(), []string{"marketing"}))
	require.NotNil(t, r)
	return r
}

func TestRedactGraphCollapsesForeignNodesAndStripsEdges(t *testing.T) {
	whole := redactionWorld(t)
	redacted := RedactGraph(marketingRedactor(t), whole)
	body, err := json.Marshal(redacted)
	require.NoError(t, err)
	for _, secret := range []string{"secret", "b5mktcol", "b5fincol", "finance", "lake/obs"} {
		require.NotContains(t, string(body), secret)
	}

	var visible, external []internalcontract.Node
	for _, node := range redacted.Nodes {
		if node.Kind == internalcontract.NodeKindExternal {
			external = append(external, node)
			require.Equal(t, internalcontract.Node{ID: node.ID, Kind: internalcontract.NodeKindExternal}, node)
			continue
		}
		require.Empty(t, external, "placeholders follow every visible node")
		visible = append(visible, node)
	}
	require.Len(t, external, 4, "f1, f2, f9 and the unknown producer, isolated ones included")
	require.True(t, slices.IsSortedFunc(external, func(a, b internalcontract.Node) int { return strings.Compare(a.ID, b.ID) }))
	var aliases []string
	for _, node := range visible {
		if node.Kind == internalcontract.NodeKindJob {
			aliases = append(aliases, node.Alias)
		} else {
			require.Equal(t, "mkt-ds", node.Dataset.Name, "only a dataset a whole edge carries is shown")
		}
	}
	require.ElementsMatch(t, []string{"b5-m1", "b5-m2", "b5-m3", "b5-m4", "b5-m5"}, aliases)

	isExternal := func(id string) bool { return strings.HasPrefix(id, "external:") }
	var wholeEdges, stripped []internalcontract.Edge
	for _, edge := range redacted.Edges {
		if edge.Class == "" {
			stripped = append(stripped, edge)
			require.Equal(t, internalcontract.Edge{ID: edge.ID, From: edge.From, To: edge.To}, edge, "a stripped edge keeps only its endpoints")
			continue
		}
		require.Empty(t, stripped, "stripped edges follow every whole edge")
		wholeEdges = append(wholeEdges, edge)
	}
	require.Len(t, wholeEdges, 2, "the two declared mkt-ds edges; the trigger-shared inferred edge is withheld")
	for _, edge := range wholeEdges {
		require.Equal(t, internalcontract.EdgeClassDeclared, edge.Class)
		require.Equal(t, "job:b5-m1", edge.From)
	}

	// m1 -> f1 (two datasets, one edge), f1 -> f2, f2 -> m2, ghost -> m5,
	// and the finance-run evidence m1 -> m3 between two visible jobs.
	pairs := map[[2]string]int{}
	for _, edge := range stripped {
		from, to := edge.From, edge.To
		if isExternal(from) {
			from = "ext"
		}
		if isExternal(to) {
			to = "ext"
		}
		pairs[[2]string{from, to}]++
	}
	require.Equal(t, map[[2]string]int{
		{"job:b5-m1", "ext"}:       1,
		{"ext", "ext"}:             1,
		{"ext", "job:b5-m2"}:       1,
		{"ext", "job:b5-m5"}:       1,
		{"job:b5-m1", "job:b5-m3"}: 1,
	}, pairs)
	for _, edge := range stripped {
		for _, id := range []string{edge.From, edge.To} {
			if isExternal(id) {
				require.True(t, slices.ContainsFunc(external, func(n internalcontract.Node) bool { return n.ID == id }), "every collapsed endpoint is a placeholder node")
			}
		}
	}

	// The placeholder f1 -> f2 edge links the same ids the edges into and out
	// of the foreign chain use.
	var intoChain, outOfChain, chain internalcontract.Edge
	for _, edge := range stripped {
		switch {
		case edge.From == "job:b5-m1" && isExternal(edge.To):
			intoChain = edge
		case isExternal(edge.From) && edge.To == "job:b5-m2":
			outOfChain = edge
		case isExternal(edge.From) && isExternal(edge.To):
			chain = edge
		}
	}
	require.Equal(t, intoChain.To, chain.From)
	require.Equal(t, chain.To, outOfChain.From)

	// Another response keys every placeholder afresh.
	again := RedactGraph(marketingRedactor(t), whole)
	againBody, err := json.Marshal(again)
	require.NoError(t, err)
	for _, node := range external {
		require.NotContains(t, string(againBody), node.ID)
	}
}

// A job sharing its trigger with another namespace's job stays visible with
// its declared edges; only the edges derived from the trigger are withheld.
func TestRedactGraphTriggerSharingWithholdsOnlyInferredEdges(t *testing.T) {
	redacted := RedactGraph(marketingRedactor(t), redactionWorld(t))
	var m4 bool
	for _, node := range redacted.Nodes {
		m4 = m4 || node.Alias == "b5-m4"
	}
	require.True(t, m4)
	for _, edge := range redacted.Edges {
		require.NotEqual(t, internalcontract.EdgeClassInferred, edge.Class)
	}
	whole := redactionWorld(t)
	var inferred bool
	for _, edge := range whole.Edges {
		inferred = inferred || (edge.Class == internalcontract.EdgeClassInferred && edge.To == "job:b5-m4")
	}
	require.True(t, inferred, "the whole graph has the inferred edge the redaction withholds")
}

// Two derived edges whose plain-concatenated ids collide ("b:/secret" on
// "public" and "b" on "secret:/public") are merged by derivation, the second's
// schema into the first. The merged edge shows two marketing endpoints but
// carries the finance consumer's schema, so it is collapsed.
func TestRedactGraphCollidingEdgeIDsNeverLeak(t *testing.T) {
	secret := map[string]any{"type": "object", "properties": map[string]any{"shared": map[string]any{"type": "string", "description": "fin-collide-secret"}}}
	graph, err := internalcontract.DeriveGraph(internalcontract.DeriveInput{Jobs: []internalcontract.Job{
		producing("a", "marketing", []internalcontract.ProducedDataset{{Name: "public"}, {Name: "secret:/public", Schema: objectSchema("shared")}}, nil),
		producing("b:/secret", "marketing", nil, []internalcontract.ConsumedDataset{{Name: "public"}}),
		producing("b", "finance", nil, []internalcontract.ConsumedDataset{{Name: "secret:/public", Schema: secret}}),
	}})
	require.NoError(t, err)
	whole, err := json.Marshal(graph)
	require.NoError(t, err)
	require.Contains(t, string(whole), "fin-collide-secret", "the ids collide and derivation merged the finance schema")
	var merged internalcontract.Edge
	for _, edge := range graph.Edges {
		if edge.From == "job:a" && edge.To == "job:b:/secret" {
			merged = edge
		}
	}
	require.Len(t, merged.Contributors(), 2)

	redacted := RedactGraph(marketingRedactor(t), graph)
	body, err := json.Marshal(redacted)
	require.NoError(t, err)
	require.NotContains(t, string(body), "fin-collide-secret")
	for _, edge := range redacted.Edges {
		if edge.From == "job:a" && edge.To == "job:b:/secret" {
			require.Equal(t, internalcontract.Edge{ID: edge.ID, From: edge.From, To: edge.To}, edge)
		}
	}
}

func TestRedactGraphUnrestrictedIsIdentity(t *testing.T) {
	whole := redactionWorld(t)
	before, err := json.Marshal(whole)
	require.NoError(t, err)
	after, err := json.Marshal(RedactGraph(nil, whole))
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))
	for _, edge := range whole.Edges {
		require.NotEmpty(t, edge.Class, "every derived edge has a class, so its omitempty tag never changes the wire")
	}
	require.NotContains(t, string(before), "marketing", "ownership never reaches the wire")
	require.NotContains(t, string(before), `"external"`)
}

// The dataset filter is applied after redaction: a dataset observed only
// between another namespace's jobs reads exactly as one nobody observed.
func TestConfinedGraphFilterOnForeignDatasetLikeNonexistent(t *testing.T) {
	t.Setenv(enforcementEnv, "warn")
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	producer := seedObservedJob(t, db, "b5-filter-fin-producer", "finance")
	consumer := seedObservedJob(t, db, "b5-filter-fin-consumer", "finance")
	seedObservedRun(t, db, producer, "finance", "lake", "orders-secret", "output")
	seedObservedRun(t, db, consumer, "finance", "lake", "orders-secret", "input")

	admin, err := NewWithDatabase(context.Background(), db).Graph("lake/orders-secret", nil)
	require.NoError(t, err)
	require.Len(t, admin.Edges, 1, "the unrestricted filter matches the observed edge")
	require.Equal(t, "orders-secret", admin.Edges[0].Dataset.Name)

	confined := func(filter string) string {
		response, recorded, err := NewWithDatabase(context.Background(), db).ConfinedGraph(marketingRedactor(t), filter)
		require.NoError(t, err)
		require.NotNil(t, recorded)
		body, err := json.Marshal(response)
		require.NoError(t, err)
		return string(body)
	}
	require.Equal(t, `{"nodes":[],"edges":[]}`, confined("lake/orders-secret"))
	require.Equal(t, confined("lake/nobody-observed-this"), confined("lake/orders-secret"))

	// Unfiltered, the foreign topology is two placeholders and one edge.
	var unfiltered internalcontract.Graph
	require.NoError(t, json.Unmarshal([]byte(confined("")), &unfiltered))
	require.Len(t, unfiltered.Nodes, 2)
	require.Len(t, unfiltered.Edges, 1)
	require.Empty(t, unfiltered.Edges[0].Dataset)

	_, _, err = NewWithDatabase(context.Background(), db).ConfinedGraph(marketingRedactor(t), "not-a-filter")
	require.ErrorIs(t, err, ErrInvalidDatasetFilter)
	t.Setenv(enforcementEnv, "")
	_, _, err = NewWithDatabase(context.Background(), db).ConfinedGraph(marketingRedactor(t), "")
	require.ErrorIs(t, err, ErrDisabled)
}

// A failed COMMIT re-derives the whole graph: the response equals an
// uncontended read, with no node or edge doubled.
func TestConfinedGraphRetryEqualsAnUncontendedRead(t *testing.T) {
	t.Setenv(enforcementEnv, "warn")
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	producer := seedObservedJob(t, db, "retry-producer", "marketing")
	consumer := seedObservedJob(t, db, "retry-consumer", "marketing")
	seedObservedRun(t, db, producer, "marketing", "lake", "orders", "output")
	seedObservedRun(t, db, consumer, "marketing", "lake", "orders", "input")
	redactor := marketingRedactor(t)
	uncontended, _, err := NewWithDatabase(context.Background(), db).ConfinedGraph(redactor, "")
	require.NoError(t, err)
	require.NotEmpty(t, uncontended.Edges)

	conn, faults := dbfaults.FailCommits(t, db, errors.New("checkpoint in progress"), 1)
	got, recorded, err := NewWithDatabase(context.Background(), conn).ConfinedGraph(redactor, "")

	require.NoError(t, err)
	require.Equal(t, 2, faults.Begins)
	require.Equal(t, uncontended, got)
	require.Len(t, recorded.Edges, len(uncontended.Edges))
}

func seedObservedJob(t *testing.T, db *gorm.DB, alias, namespace string) models.Job {
	t.Helper()
	trigger := models.Trigger{ID: uuid.New(), Type: models.TriggerTypeCron, Configuration: "{}"}
	require.NoError(t, db.Create(&trigger).Error)
	job := models.Job{ID: uuid.New(), Alias: alias, Namespace: namespace, TriggerID: trigger.ID}
	require.NoError(t, db.Create(&job).Error)
	return job
}

func seedObservedRun(t *testing.T, db *gorm.DB, job models.Job, runNamespace, namespace, name, direction string) {
	t.Helper()
	atom := models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "busybox:1.36.1"}
	require.NoError(t, db.Create(&atom).Error)
	task := models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atom.ID, Name: "step"}
	require.NoError(t, db.Create(&task).Error)
	run := models.JobRun{ID: uuid.New(), JobID: job.ID, TriggerID: job.TriggerID, Namespace: runNamespace, Status: "succeeded", StartedAt: time.Now().UTC()}
	require.NoError(t, db.Create(&run).Error)
	taskRun := models.TaskRun{ID: uuid.New(), JobRunID: run.ID, TaskID: task.ID, AtomID: atom.ID, Engine: models.AtomEngineDocker, Image: "busybox:1.36.1", Command: "[]", Status: "succeeded"}
	require.NoError(t, db.Create(&taskRun).Error)
	require.NoError(t, db.Create(&models.LineageDataset{ID: uuid.New(), TaskRunID: taskRun.ID, Namespace: namespace, Name: name, Direction: direction, CreatedAt: time.Now().UTC()}).Error)
}
