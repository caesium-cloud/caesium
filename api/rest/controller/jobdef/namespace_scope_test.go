package jobdef

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	internalcontract "github.com/caesium-cloud/caesium/internal/contract"
	"github.com/caesium-cloud/caesium/internal/freshness"
	internaljobdef "github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

const sharedSecret = "finance-shared-webhook-secret"

func scopeDefinition(alias, namespace string) schema.Definition {
	return schema.Definition{
		APIVersion: schema.APIVersionV1,
		Kind:       schema.KindJob,
		Metadata:   schema.Metadata{Alias: alias, Namespace: namespace},
		Trigger:    schema.Trigger{Type: schema.TriggerCron, Configuration: map[string]any{"cron": "0 0 31 2 *"}},
		Steps:      []schema.Step{{Name: "ok", Engine: schema.EngineDocker, Image: "busybox:1.36.1", Command: []string{"true"}}},
	}
}

func producerDefinition(alias, namespace, dataset string) schema.Definition {
	def := scopeDefinition(alias, namespace)
	def.Steps[0].Datasets = &schema.StepDatasets{Produces: []schema.ProducedDataset{{Name: dataset}}}
	return def
}

// scopeCatalog applies a marketing job whose HTTP trigger a finance job
// shares, another finance job, and a finance producer of dataset "ds", and
// points the handlers at it.
func scopeCatalog(t *testing.T) *gorm.DB {
	t.Helper()
	conn := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(conn) })
	previous := catalog
	catalog = func() *gorm.DB { return conn }
	t.Cleanup(func() { catalog = previous })

	importer := internaljobdef.NewImporter(conn)
	mkt := scopeDefinition("mkt", "marketing")
	mkt.Trigger = schema.Trigger{Type: schema.TriggerHTTP, Configuration: map[string]any{"path": "/hooks/mkt", "secret": sharedSecret}}
	for _, def := range []schema.Definition{mkt, scopeDefinition("fin", "finance"), producerDefinition("fin-producer", "finance", "ds")} {
		// Round-trip through JSON, as a request body does, so the schema
		// defaults (step type, engine) are applied.
		encoded, err := json.Marshal(def)
		require.NoError(t, err)
		var decoded schema.Definition
		require.NoError(t, json.Unmarshal(encoded, &decoded))
		_, err = importer.Apply(context.Background(), &decoded)
		require.NoError(t, err)
	}
	var shared models.Job
	require.NoError(t, conn.Where("alias = ?", "mkt").First(&shared).Error)
	require.NoError(t, conn.Model(&models.Job{}).Where("alias = ?", "fin").Update("trigger_id", shared.TriggerID).Error)
	return conn
}

// callJobdef runs handler on body as principal (nil: auth disabled).
func callJobdef(t *testing.T, handler echo.HandlerFunc, principal *auth.Principal, body any) (int, string) {
	t.Helper()
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", strings.NewReader(string(encoded)))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	if principal != nil {
		c.Set(authmw.ContextKeyPrincipal, principal)
	}
	require.NoError(t, handler(c))
	return rec.Code, rec.Body.String()
}

func confined(grants policy.Grants) *auth.Principal {
	return &auth.Principal{Kind: auth.PrincipalAPIKey, Grants: grants, Subject: "test"}
}

func TestDiffBaselineByPrincipal(t *testing.T) {
	scopeCatalog(t)
	empty := map[string]any{"definitions": []schema.Definition{}}

	// Auth disabled and a cluster viewer: today's baseline, unchanged.
	for name, principal := range map[string]*auth.Principal{
		"no auth":        nil,
		"cluster viewer": confined(policy.Grants{models.ClusterNamespace: models.RoleViewer}),
	} {
		status, body := callJobdef(t, Diff, principal, empty)
		require.Equal(t, http.StatusOK, status, name)
		var resp DiffResponse
		require.NoError(t, json.Unmarshal([]byte(body), &resp), name)
		require.Len(t, resp.Removed, 3, name)
		require.Contains(t, body, sharedSecret, name)
		require.Equal(t, []string{}, resp.Namespaces, name)
	}

	// A marketing viewer: marketing's job only, its shared trigger redacted.
	status, body := callJobdef(t, Diff, confined(policy.Grants{"marketing": models.RoleViewer}), empty)
	require.Equal(t, http.StatusOK, status)
	var resp DiffResponse
	require.NoError(t, json.Unmarshal([]byte(body), &resp))
	require.Len(t, resp.Removed, 1)
	require.Equal(t, "mkt", resp.Removed[0].Alias)
	require.Equal(t, map[string]any{"redacted": "the trigger also serves a job in another namespace"}, resp.Removed[0].Trigger.Configuration)
	require.NotContains(t, body, sharedSecret)
	require.NotContains(t, body, `"fin`)

	// Modified: the stored side of the shared trigger stays redacted.
	edited := scopeDefinition("mkt", "marketing")
	edited.Trigger = schema.Trigger{Type: schema.TriggerHTTP, Configuration: map[string]any{"path": "/hooks/mkt", "secret": "caller-secret"}}
	status, body = callJobdef(t, Diff, confined(policy.Grants{"marketing": models.RoleViewer}), map[string]any{"definitions": []schema.Definition{edited}})
	require.Equal(t, http.StatusOK, status)
	require.NoError(t, json.Unmarshal([]byte(body), &resp))
	require.Len(t, resp.Modified, 1)
	require.Contains(t, resp.Modified[0].Diff, "redacted")
	require.NotContains(t, body, sharedSecret)
	require.Equal(t, []string{"marketing"}, resp.Namespaces)

	// An authenticated principal with no grants sees nothing (fail closed).
	status, body = callJobdef(t, Diff, confined(policy.Grants{}), empty)
	require.Equal(t, http.StatusOK, status)
	require.NoError(t, json.Unmarshal([]byte(body), &resp))
	require.Empty(t, resp.Removed)
}

func TestDiffNamespacesTouchedByAMove(t *testing.T) {
	scopeCatalog(t)
	moved := scopeDefinition("fin", "marketing")
	created := scopeDefinition("brand-new", "default")
	status, body := callJobdef(t, Diff, nil, map[string]any{"definitions": []schema.Definition{moved, created}})
	require.Equal(t, http.StatusOK, status)
	var resp DiffResponse
	require.NoError(t, json.Unmarshal([]byte(body), &resp))
	require.Equal(t, []string{"default", "finance", "marketing"}, resp.Namespaces)
}

func TestLintRedactsForeignJobsForAConfinedCaller(t *testing.T) {
	scopeCatalog(t)
	duplicate := producerDefinition("mkt-producer", "marketing", "ds")
	lint := func(principal *auth.Principal) LintResponse {
		status, body := callJobdef(t, Lint, principal, map[string]any{"definitions": []schema.Definition{duplicate}})
		require.Equal(t, http.StatusOK, status)
		var resp LintResponse
		require.NoError(t, json.Unmarshal([]byte(body), &resp))
		require.Len(t, resp.Errors, 1, body)
		return resp
	}

	require.Equal(t, `dataset produced by multiple jobs: "ds" produced by fin-producer, mkt-producer`, lint(nil).Errors[0].Message,
		"auth disabled: the historical message")
	require.Equal(t, `dataset produced by multiple jobs: "ds" produced by fin-producer, mkt-producer`,
		lint(confined(policy.Grants{models.ClusterNamespace: models.RoleViewer})).Errors[0].Message)
	require.Equal(t, `dataset produced by multiple jobs: "ds" produced by mkt-producer, 1 job(s) in other namespaces`,
		lint(confined(policy.Grants{"marketing": models.RoleViewer})).Errors[0].Message)
	require.Equal(t, `dataset produced by multiple jobs: "ds" produced by fin-producer, mkt-producer`,
		lint(confined(policy.Grants{"marketing": models.RoleViewer, "finance": models.RoleViewer})).Errors[0].Message)
}

// TestLintDeclaredNamespaces: server lint reports an undeclared namespace only
// while a policy file is loaded.
func TestLintDeclaredNamespaces(t *testing.T) {
	scopeCatalog(t)
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	body := map[string]any{"definitions": []schema.Definition{scopeDefinition("typo-job", "sales")}}

	policy.SetProcessHolder(nil)
	_, out := callJobdef(t, Lint, nil, body)
	require.NotContains(t, out, "not declared")

	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte("apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  marketing: {}\n"), 0o600))
	holder, err := policy.LoadFile(t.Context(), policy.HolderOptions{Path: path})
	require.NoError(t, err)
	policy.SetProcessHolder(holder)
	_, out = callJobdef(t, Lint, nil, body)
	var resp LintResponse
	require.NoError(t, json.Unmarshal([]byte(out), &resp))
	require.Equal(t, []LintMessage{{Message: `metadata.namespace "sales" of job "typo-job" is not declared in the access policy`}}, resp.Errors)
}

func TestReadScopeErrorMessages(t *testing.T) {
	conn := scopeCatalog(t)
	defs := []schema.Definition{scopeDefinition("mine", "marketing")}
	scope := newReadScopeFor([]string{"marketing"}, conn, defs)
	ctx := context.Background()

	require.Equal(t, "trigger chain cycle detected: mine -> a job in another namespace -> mine",
		scope.errorMessage(ctx, &internaljobdef.TriggerChainCycleError{Path: []string{"mine", "fin", "mine"}}))
	require.Equal(t, "trigger chain cycle detected: mine -> fin -> mine",
		(*readScope)(nil).errorMessage(ctx, &internaljobdef.TriggerChainCycleError{Path: []string{"mine", "fin", "mine"}}))
	require.Equal(t, "a job in another namespace has an invalid trigger configuration",
		scope.errorMessage(ctx, &internaljobdef.TriggerChainJobError{Alias: "fin", Stored: true, Err: errors.New("x")}))
	require.Equal(t, "definition mine: x",
		scope.errorMessage(ctx, &internaljobdef.TriggerChainJobError{Alias: "mine", Err: errors.New("x")}))
	require.Equal(t, "trigger chain cycle detected: a job in another namespace",
		scope.errorMessage(ctx, &internaljobdef.TriggerChainCycleError{Path: []string{"ghost"}}), "an alias with no row is foreign")

	unresolved := freshness.ValidateGraph([]models.DatasetDeclaration{{JobAlias: "fin", Name: "secret-ds", Direction: models.DatasetDirectionConsumes}})
	require.Equal(t, "consumed dataset is not produced or declared: a job in another namespace consumes a dataset which is not produced by any job or declared as a source",
		scope.errorMessage(ctx, unresolved), "neither the foreign consumer nor its dataset is named")
	cycle := freshness.ValidateGraph([]models.DatasetDeclaration{
		{JobAlias: "mine", Name: "x", Direction: models.DatasetDirectionProduces}, {JobAlias: "mine", Name: "y", Direction: models.DatasetDirectionConsumes},
		{JobAlias: "fin", Name: "y", Direction: models.DatasetDirectionProduces}, {JobAlias: "fin", Name: "x", Direction: models.DatasetDirectionConsumes},
	})
	require.NotContains(t, scope.errorMessage(ctx, cycle), "fin")
	require.Contains(t, scope.errorMessage(ctx, cycle), "mine")

	ref := &internaljobdef.ObjectRefNamespaceError{Alias: "mine", JobNamespace: "marketing", Field: "f", Ref: "r", ObjectKind: "an AgentProfile"}
	for namespace, want := range map[string]string{"finance": ref.Redacted(), "marketing": "", "*": ""} {
		ref.ObjectNamespace = namespace
		if want == "" {
			want = ref.Error()
		}
		require.Equal(t, want, scope.errorMessage(ctx, ref), namespace)
	}
	require.Equal(t, "plain", scope.errorMessage(ctx, errors.New("plain")))
	require.Equal(t, "", scope.errorMessage(ctx, nil))
}

func TestReadScopeFiltersContractGraph(t *testing.T) {
	conn := scopeCatalog(t)
	scope := newReadScopeFor([]string{"marketing"}, conn, []schema.Definition{scopeDefinition("incoming", "marketing")})
	ds := &internalcontract.DatasetRef{Name: "d"}
	graph := internalcontract.Graph{
		Nodes: []internalcontract.Node{
			{ID: internalcontract.JobNodeID("mkt"), Kind: internalcontract.NodeKindJob, Alias: "mkt"},
			{ID: internalcontract.JobNodeID("incoming"), Kind: internalcontract.NodeKindJob, Alias: "incoming"},
			{ID: internalcontract.JobNodeID("fin"), Kind: internalcontract.NodeKindJob, Alias: "fin"},
			{ID: "dataset:d", Kind: internalcontract.NodeKindDataset, Dataset: ds},
			{ID: "dataset:other", Kind: internalcontract.NodeKindDataset, Dataset: &internalcontract.DatasetRef{Name: "other"}},
		},
		Edges: []internalcontract.Edge{
			{ID: "own", From: internalcontract.JobNodeID("mkt"), To: internalcontract.JobNodeID("incoming"), Dataset: ds},
			{ID: "foreign-consumer", From: internalcontract.JobNodeID("incoming"), To: internalcontract.JobNodeID("fin"),
				Dataset: &internalcontract.DatasetRef{Name: "other"}},
			{ID: "foreign-producer", From: internalcontract.JobNodeID("fin"), To: internalcontract.JobNodeID("mkt")},
		},
	}
	filtered := scope.filterGraph(context.Background(), graph)
	require.Len(t, filtered.Edges, 1)
	require.Equal(t, "own", filtered.Edges[0].ID)
	ids := make([]string, 0, len(filtered.Nodes))
	for _, node := range filtered.Nodes {
		ids = append(ids, node.ID)
	}
	require.ElementsMatch(t, []string{internalcontract.JobNodeID("mkt"), internalcontract.JobNodeID("incoming"), "dataset:d"}, ids)
	require.Equal(t, graph, (*readScope)(nil).filterGraph(context.Background(), graph), "unrestricted: unchanged")
}

// TestReadScopeRechecksStoredOwnership: a request alias is visible only while
// its stored job is also in the caller's namespaces, so a job moved to
// finance after the middleware authorised the request (here: requested into
// marketing, stored in finance) is hidden from the findings and errors.
func TestReadScopeRechecksStoredOwnership(t *testing.T) {
	conn := scopeCatalog(t)
	scope := newReadScopeFor([]string{"marketing"}, conn, []schema.Definition{scopeDefinition("fin", "marketing"), scopeDefinition("brand-new", "marketing")})
	ctx := context.Background()
	require.Equal(t, hiddenJob, scope.aliasOrHidden(ctx, "fin"))
	require.Equal(t, "brand-new", scope.aliasOrHidden(ctx, "brand-new"), "a request alias with no row yet is the caller's")
	graph := internalcontract.Graph{Edges: []internalcontract.Edge{
		{ID: "moved", From: internalcontract.JobNodeID("brand-new"), To: internalcontract.JobNodeID("fin")},
	}}
	require.Empty(t, scope.filterGraph(ctx, graph).Edges)

	failing := newReadScopeFor([]string{"marketing"}, conn, []schema.Definition{scopeDefinition("brand-new", "marketing")})
	sqlDB, err := conn.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())
	require.Equal(t, hiddenJob, failing.aliasOrHidden(ctx, "brand-new"), "a failed lookup fails closed")
}

// TestReadScopeDropsInferredEdgesOfSharedTriggers: an inferred edge is
// derived from a trigger's configuration; when that trigger also serves a
// finance job, a marketing caller cannot read it, nor the edge.
func TestReadScopeDropsInferredEdgesOfSharedTriggers(t *testing.T) {
	conn := scopeCatalog(t)
	importer := internaljobdef.NewImporter(conn)
	other := scopeDefinition("mkt-other", "marketing")
	encoded, err := json.Marshal(other)
	require.NoError(t, err)
	var decoded schema.Definition
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	_, err = importer.Apply(context.Background(), &decoded)
	require.NoError(t, err)

	scope := newReadScopeFor([]string{"marketing"}, conn, nil)
	graph := internalcontract.Graph{Edges: []internalcontract.Edge{
		{ID: "inferred-shared", Class: internalcontract.EdgeClassInferred, From: internalcontract.JobNodeID("mkt-other"), To: internalcontract.JobNodeID("mkt")},
		{ID: "declared-shared", Class: internalcontract.EdgeClassDeclared, From: internalcontract.JobNodeID("mkt-other"), To: internalcontract.JobNodeID("mkt")},
		{ID: "inferred-own", Class: internalcontract.EdgeClassInferred, From: internalcontract.JobNodeID("mkt"), To: internalcontract.JobNodeID("mkt-other")},
	}}
	// mkt's trigger also serves the finance job "fin" (scopeCatalog).
	kept := scope.filterGraph(context.Background(), graph).Edges
	ids := make([]string, 0, len(kept))
	for _, edge := range kept {
		ids = append(ids, edge.ID)
	}
	require.Equal(t, []string{"declared-shared"}, ids)
	require.Len(t, (*readScope)(nil).filterGraph(context.Background(), graph).Edges, 3)
}

// TestContractGraphReadsOwnershipInTheGraphSnapshot: a confined caller's
// contract graph and the namespace checks that filter it come from one
// snapshot. The producer is in finance while the graph is derived, so the
// finding is built from finance's schema; a concurrent apply then moves the
// producer to marketing. Deciding visibility after the move would show the
// marketing viewer a finding derived from finance's schema, so the edge stays
// hidden until the next request reads the producer in marketing.
func TestContractGraphReadsOwnershipInTheGraphSnapshot(t *testing.T) {
	t.Setenv("CAESIUM_CONTRACT_ENFORCEMENT", "warn")
	reader, writer := snapshotCatalog(t)

	producer := scopeDefinition("fin-schema-producer", "finance")
	producer.Steps[0].OutputSchema = map[string]any{
		"type":       "object",
		"required":   []any{"row_count"},
		"properties": map[string]any{"row_count": map[string]any{"type": "integer"}},
	}
	consumer := scopeDefinition("mkt-schema-consumer", "marketing")
	consumer.Trigger = schema.Trigger{Type: schema.TriggerEvent, Configuration: map[string]any{
		"events":       []any{map[string]any{"type": "run_completed", "source": "caesium", "filter": map[string]any{"job_alias": producer.Metadata.Alias}}},
		"paramMapping": map[string]any{"customer": "$.tasks[0].output.customer_id"},
	}}
	applyDefinitions(t, writer, producer, consumer)

	hasFinding := func(graph internalcontract.Graph) bool {
		for _, edge := range graph.Edges {
			if edge.From == internalcontract.JobNodeID(producer.Metadata.Alias) && edge.To == internalcontract.JobNodeID(consumer.Metadata.Alias) {
				return len(edge.Findings) > 0
			}
		}
		return false
	}
	moved := false
	afterCatalogRead = func() {
		if moved {
			return
		}
		moved = true
		require.NoError(t, writer.Exec(`UPDATE jobs SET namespace = 'marketing' WHERE alias = ?`, producer.Metadata.Alias).Error)
	}
	t.Cleanup(func() { afterCatalogRead = nil })

	scoped, visible, err := newReadScopeFor([]string{"marketing"}, reader, nil).contractGraph(context.Background(), nil, nil)
	require.NoError(t, err)
	require.True(t, moved, "the concurrent move ran between derivation and the visibility checks")
	require.True(t, hasFinding(scoped), "the derived graph carries the finding: %+v", scoped.Edges)
	require.False(t, hasFinding(visible), "an edge derived while its producer was in finance stays hidden")

	_, after, err := newReadScopeFor([]string{"marketing"}, reader, nil).contractGraph(context.Background(), nil, nil)
	require.NoError(t, err)
	require.True(t, hasFinding(after), "once the producer is in marketing, the next snapshot shows the edge")
}

// snapshotCatalog is a file-backed catalog with two handles: reader, which
// the handlers read through (catalog), and writer, which plays a concurrent
// apply while reader holds a read snapshot open (WAL lets the two overlap,
// unlike the single shared-cache connection of testutil.OpenTestDB).
func snapshotCatalog(t *testing.T) (reader, writer *gorm.DB) {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "catalog.db") + "?_journal_mode=WAL&_busy_timeout=5000"
	reader, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	t.Cleanup(func() { testutil.CloseDB(reader) })
	require.NoError(t, reader.AutoMigrate(models.All...))
	writer, err = gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	t.Cleanup(func() { testutil.CloseDB(writer) })
	previous := catalog
	catalog = func() *gorm.DB { return reader }
	t.Cleanup(func() { catalog = previous })
	return reader, writer
}

// applyDefinitions applies defs through the importer, round-tripped through
// JSON as a request body is (so the schema defaults apply).
func applyDefinitions(t *testing.T, conn *gorm.DB, defs ...schema.Definition) {
	t.Helper()
	importer := internaljobdef.NewImporter(conn)
	for idx, def := range defs {
		encoded, err := json.Marshal(def)
		require.NoError(t, err)
		var decoded schema.Definition
		require.NoError(t, json.Unmarshal(encoded, &decoded))
		// A non-canonical alias can no longer be applied (W6-α); a row
		// stored before the rule is produced by renaming a seeded job.
		legacyAlias := ""
		if schema.ValidateAlias(decoded.Metadata.Alias) != nil {
			legacyAlias = decoded.Metadata.Alias
			decoded.Metadata.Alias = fmt.Sprintf("legacy-alias-seed-%d", idx)
		}
		job, err := importer.Apply(context.Background(), &decoded)
		require.NoError(t, err)
		if legacyAlias != "" {
			require.NoError(t, conn.Model(&models.Job{}).Where("id = ?", job.ID).Update("alias", legacyAlias).Error)
		}
	}
}

// TestValidatorErrorRedactedInTheValidationSnapshot: a validator's verdict
// and the redaction of its error share one snapshot. A finance consumer's
// producer is gone, so the dataset graph reports the consumer and its
// dataset; a concurrent apply then moves the consumer into marketing and
// drops the consume. Judged after the move, the consumer would look like the
// marketing caller's own job and the error would name finance's dataset.
func TestValidatorErrorRedactedInTheValidationSnapshot(t *testing.T) {
	reader, writer := snapshotCatalog(t)
	const dataset = "fin-private-dataset"
	consumer := scopeDefinition("fin-orphan-consumer", "finance")
	consumer.Steps[0].Datasets = &schema.StepDatasets{Consumes: []schema.ConsumedDataset{{Name: dataset}}}
	applyDefinitions(t, writer, producerDefinition("fin-gone-producer", "finance", dataset), consumer)
	require.NoError(t, writer.Exec(`UPDATE jobs SET deleted_at = CURRENT_TIMESTAMP WHERE alias = ?`, "fin-gone-producer").Error)

	validate := func(scope *readScope) string {
		return scope.validate(context.Background(), reader, func(conn *gorm.DB) error {
			return internaljobdef.ValidateDatasetGraph(context.Background(), conn, nil)
		})
	}
	unrestricted := validate(nil)
	require.Contains(t, unrestricted, dataset, "a cluster caller sees the error verbatim")
	require.Contains(t, unrestricted, consumer.Metadata.Alias)

	moved := false
	afterCatalogRead = func() {
		if moved {
			return
		}
		moved = true
		require.NoError(t, writer.Exec(`UPDATE jobs SET namespace = 'marketing' WHERE alias = ?`, consumer.Metadata.Alias).Error)
		require.NoError(t, writer.Exec(`DELETE FROM dataset_declarations WHERE job_alias = ?`, consumer.Metadata.Alias).Error)
	}
	t.Cleanup(func() { afterCatalogRead = nil })

	message := validate(newReadScopeFor([]string{"marketing"}, reader, nil))
	require.True(t, moved, "the concurrent move ran between the verdict and its redaction")
	require.NotEmpty(t, message)
	require.NotContains(t, message, dataset, "the dataset of a job that was in finance when validated stays hidden")
	require.NotContains(t, message, consumer.Metadata.Alias)
	require.Contains(t, message, hiddenJob)

	require.Empty(t, validate(newReadScopeFor([]string{"marketing"}, reader, nil)), "after the move the graph is valid")
}
