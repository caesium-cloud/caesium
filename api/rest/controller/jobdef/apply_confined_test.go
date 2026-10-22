package jobdef

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/authz"
	internalcontract "github.com/caesium-cloud/caesium/internal/contract"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// callApply runs the apply handler as principal (nil: auth disabled) and
// returns the response, or the error the handler returned (a *authz.Denial
// the auth middleware turns into an audited 403).
func callApply(t *testing.T, principal *auth.Principal, body any) (int, string, error) {
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
	err = Apply(c)
	return rec.Code, rec.Body.String(), err
}

func applyBody(prune bool, defs ...schema.Definition) map[string]any {
	return map[string]any{"definitions": defs, "prune": prune}
}

func httpDefinition(alias, namespace, path string) schema.Definition {
	def := scopeDefinition(alias, namespace)
	def.Trigger = schema.Trigger{Type: schema.TriggerHTTP, Configuration: map[string]any{"path": path}}
	return def
}

func freshCatalog(t *testing.T, defs ...schema.Definition) *gorm.DB {
	t.Helper()
	conn := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(conn) })
	previous := catalog
	catalog = func() *gorm.DB { return conn }
	t.Cleanup(func() { catalog = previous })
	applyDefinitions(t, conn, defs...)
	return conn
}

func loadPolicy(t *testing.T) {
	t.Helper()
	holder, err := policy.LoadFile(t.Context(), policy.HolderOptions{Path: writePolicyFile(t)})
	require.NoError(t, err)
	policy.SetProcessHolder(holder)
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
}

func writePolicyFile(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/policy.yaml"
	require.NoError(t, os.WriteFile(path, []byte("apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  marketing: {}\n  finance: {}\n"), 0o600))
	return path
}

func requireDenial(t *testing.T, err error) *authz.Denial {
	t.Helper()
	denial, ok := errors.AsType[*authz.Denial](err)
	require.True(t, ok, "want an audited denial, got %v", err)
	return denial
}

func jobRow(t *testing.T, conn *gorm.DB, alias string) (models.Job, bool) {
	t.Helper()
	var job models.Job
	err := conn.Unscoped().Where("alias = ?", alias).First(&job).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return job, false
	}
	require.NoError(t, err)
	return job, true
}

func clusterOperator() *auth.Principal {
	return confined(policy.Grants{"*": models.RoleOperator})
}

// An unrestricted caller (auth off, or operator at "*" with an unscoped key)
// keeps the per-definition flow and its response, prune included.
func TestApplyUnrestrictedFlowUnchanged(t *testing.T) {
	for name, principal := range map[string]*auth.Principal{"auth off": nil, "cluster operator": clusterOperator()} {
		t.Run(name, func(t *testing.T) {
			conn := freshCatalog(t, scopeDefinition("stale", "finance"))
			status, body, err := callApply(t, principal, applyBody(true, scopeDefinition("a", "marketing"), scopeDefinition("b", "finance")))
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, status)
			require.JSONEq(t, `{"applied":2,"pruned":1}`, body)
			stale, _ := jobRow(t, conn, "stale")
			require.True(t, stale.DeletedAt.Valid)
		})
	}
}

// A namespace-confined caller's apply re-checks every definition's stored
// namespace in its transaction: a job that lives (or landed) in a namespace
// it lacks is refused with an audited 403 naming the namespace it named.
func TestApplyConfinedRechecksStoredNamespace(t *testing.T) {
	conn := freshCatalog(t, scopeDefinition("fin", "finance"))
	mktOperator := confined(policy.Grants{"marketing": models.RoleOperator})
	_, _, err := callApply(t, mktOperator, applyBody(false, scopeDefinition("new-mkt", "marketing"), scopeDefinition("fin", "marketing")))
	denial := requireDenial(t, err)
	require.Equal(t, `insufficient permissions: operator required in namespace "finance"`, denial.Message)
	require.Equal(t, "insufficient_role", denial.Reason)
	_, exists := jobRow(t, conn, "new-mkt")
	require.False(t, exists, "nothing of the batch is applied")
	fin, _ := jobRow(t, conn, "fin")
	require.Equal(t, "finance", fin.Namespace)

	both := confined(policy.Grants{"marketing": models.RoleOperator, "finance": models.RoleOperator})
	status, body, err := callApply(t, both, applyBody(false, scopeDefinition("fin", "marketing")))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status, body)
	fin, _ = jobRow(t, conn, "fin")
	require.Equal(t, "marketing", fin.Namespace, "operator in both namespaces moves it")
}

// A confined caller may not rewrite a trigger row that also serves a job it
// cannot write; the refusal names neither that job nor its namespace.
func TestApplyConfinedRefusesSharedTriggerWithoutNamingIt(t *testing.T) {
	conn := scopeCatalog(t)
	before, _ := jobRow(t, conn, "mkt")
	mktOperator := confined(policy.Grants{"marketing": models.RoleOperator})
	_, _, err := callApply(t, mktOperator, applyBody(false, httpDefinition("mkt", "marketing", "/hooks/mkt")))
	denial := requireDenial(t, err)
	require.Equal(t, `insufficient permissions: the trigger of job "mkt" also serves a job outside your access`, denial.Message)
	require.Equal(t, "trigger_tenancy", denial.Reason)
	require.NotContains(t, denial.Message, "fin")
	var trigger models.Trigger
	require.NoError(t, conn.First(&trigger, "id = ?", before.TriggerID).Error)
	require.Contains(t, trigger.Configuration, sharedSecret, "the shared row is not rewritten")
}

// A confined caller prunes only within the namespaces it is operator in.
func TestApplyConfinedPruneStaysInItsNamespaces(t *testing.T) {
	conn := freshCatalog(t, scopeDefinition("keep", "marketing"), scopeDefinition("gone", "marketing"), scopeDefinition("fin", "finance"))
	mktOperator := confined(policy.Grants{"marketing": models.RoleOperator, "finance": models.RoleViewer})
	status, body, err := callApply(t, mktOperator, applyBody(true, scopeDefinition("keep", "marketing")))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.JSONEq(t, `{"applied":1,"pruned":1}`, body)
	gone, _ := jobRow(t, conn, "gone")
	fin, _ := jobRow(t, conn, "fin")
	require.True(t, gone.DeletedAt.Valid)
	require.False(t, fin.DeletedAt.Valid, "a namespace where the caller is only viewer is never pruned")
}

// B9 folds 1 and 2: a job-scoped operator key at "*" is a confined writer: its
// whole batch is one transaction, and a later definition subscribing a path
// served by an out-of-scope job rolls the earlier one back. With and without
// a policy file.
func TestApplyJobScopedKeyIsConfinedAndAtomic(t *testing.T) {
	for name, fileBacked := range map[string]bool{"no policy file": false, "policy file": true} {
		t.Run(name, func(t *testing.T) {
			if fileBacked {
				loadPolicy(t)
			}
			conn := freshCatalog(t, httpDefinition("other", "default", "/hooks/other"))
			scoped := clusterOperator()
			scoped.Scope = []byte(`{"jobs":["a","b"]}`)
			_, _, err := callApply(t, scoped, applyBody(false, httpDefinition("a", "default", "/hooks/a"), httpDefinition("b", "default", "/hooks/other")))
			denial := requireDenial(t, err)
			require.Equal(t, `insufficient permissions: webhook path "other" is subscribed by a job outside your access`, denial.Message)
			_, exists := jobRow(t, conn, "a")
			require.False(t, exists, "the earlier definition is rolled back")

			status, _, err := callApply(t, scoped, applyBody(false, httpDefinition("a", "default", "/hooks/a")))
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, status)
		})
	}
}

// With a policy file an unrestricted writer is held to one namespace per
// webhook path: 409 naming the namespace it may read.
func TestApplyUnrestrictedPathConflictIs409(t *testing.T) {
	loadPolicy(t)
	freshCatalog(t, httpDefinition("mkt", "marketing", "/hooks/orders"))
	_, _, err := callApply(t, clusterOperator(), applyBody(false, httpDefinition("fin", "finance", "/hooks/orders")))
	var he *echo.HTTPError
	require.ErrorAs(t, err, &he)
	require.Equal(t, http.StatusConflict, he.Code)
	require.Contains(t, he.Message, `webhook path "orders" is already subscribed from namespace "marketing"`)
}

// Validator errors touching a job the caller cannot read name no dataset
// (the design's foreign-edge boundary, stricter than lint's rendering).
func TestApplyConfinedValidatorErrorNamesNoForeignDataset(t *testing.T) {
	freshCatalog(t, producerDefinition("fin-producer", "finance", "secret-dataset-name"))
	mktOperator := confined(policy.Grants{"marketing": models.RoleOperator})
	_, _, err := callApply(t, mktOperator, applyBody(false, producerDefinition("mkt-producer", "marketing", "secret-dataset-name")))
	var he *echo.HTTPError
	require.ErrorAs(t, err, &he)
	require.Equal(t, http.StatusBadRequest, he.Code)
	require.NotContains(t, he.Message, "secret-dataset-name")
	require.NotContains(t, he.Message, "fin-producer")
	require.Contains(t, he.Message, "a dataset")
}

// findingVisible: a contract finding is readable only with both jobs (and,
// for an inferred edge, neither trigger shared with a foreign job).
func TestReadScopeFindingVisible(t *testing.T) {
	conn := scopeCatalog(t)
	applyDefinitions(t, conn, scopeDefinition("mkt-other", "marketing"))
	view := newReadScopeFor([]string{"marketing"}, conn, nil)
	ctx := context.Background()
	finding := func(producer, consumer string, class internalcontract.EdgeClass) internalcontract.ContractFinding {
		return internalcontract.ContractFinding{Producer: producer, Consumer: consumer, EdgeClass: string(class)}
	}
	require.True(t, view.findingVisible(ctx, finding("mkt-other", "mkt", internalcontract.EdgeClassDeclared)))
	require.False(t, view.findingVisible(ctx, finding("mkt-other", "fin", internalcontract.EdgeClassDeclared)), "a finance consumer")
	require.False(t, view.findingVisible(ctx, finding("mkt-other", "nowhere", internalcontract.EdgeClassDeclared)), "an unknown job is foreign")
	require.False(t, view.findingVisible(ctx, finding("mkt-other", "mkt", internalcontract.EdgeClassInferred)), "mkt's trigger is shared with finance")
	require.False(t, view.findingVisible(ctx, finding("", "mkt", internalcontract.EdgeClassDeclared)))
	var unrestricted *readScope
	require.True(t, unrestricted.findingVisible(ctx, finding("a", "b", internalcontract.EdgeClassInferred)))
}

// Server lint previews trigger tenancy: an unrestricted caller is told the
// conflicting namespace, a confined one only that its path is taken.
func TestLintReportsTriggerTenancy(t *testing.T) {
	loadPolicy(t)
	scopeCatalog(t)
	status, body := callJobdef(t, Lint, nil, map[string]any{"definitions": []schema.Definition{httpDefinition("new-fin", "finance", "/hooks/mkt")}})
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, body, `webhook path \"mkt\" is already subscribed from namespace \"marketing\"`)

	status, body = callJobdef(t, Lint, confined(policy.Grants{"marketing": models.RoleViewer}),
		map[string]any{"definitions": []schema.Definition{httpDefinition("new-mkt", "marketing", "/hooks/mkt")}})
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, body, `webhook path \"mkt\" is subscribed by a job outside your access`)
	require.NotContains(t, body, "finance")
	require.NotContains(t, body, `"fin"`)
}

// A job-scoped key's contract audience (B9): a job outside the key's alias
// scope is foreign even in a namespace the key may read, for a key at "*"
// and for a namespace-confined one alike.
func TestReadScopeHonoursKeyJobScope(t *testing.T) {
	conn := scopeCatalog(t)
	applyDefinitions(t, conn, scopeDefinition("mkt-other", "marketing"), scopeDefinition("mkt-third", "marketing"))
	ctx := context.Background()
	declared := func(producer, consumer string) internalcontract.ContractFinding {
		return internalcontract.ContractFinding{Producer: producer, Consumer: consumer, EdgeClass: string(internalcontract.EdgeClassDeclared)}
	}
	require.Nil(t, newReadScopeForKey(nil, nil, conn, nil), "neither restriction: unrestricted")

	atStar := newReadScopeForKey(nil, []string{"mkt-other", "fin"}, conn, nil)
	require.True(t, atStar.findingVisible(ctx, declared("mkt-other", "fin")), "both in scope, any namespace")
	require.False(t, atStar.findingVisible(ctx, declared("mkt-other", "mkt-third")), "an out-of-scope job in a readable namespace")
	require.Equal(t, hiddenJob, atStar.aliasOrHidden(ctx, "mkt-third"))

	confinedKey := newReadScopeForKey([]string{"marketing"}, []string{"mkt-other"}, conn, nil)
	require.False(t, confinedKey.findingVisible(ctx, declared("mkt-other", "mkt-third")), "same namespace, outside the key's scope")
	require.False(t, confinedKey.findingVisible(ctx, declared("mkt-other", "fin")))

	nothing := newReadScopeForKey([]string{}, []string{}, conn, nil)
	require.False(t, nothing.aliasVisible(ctx, "mkt-other"), "an empty scope reads nothing")
}

// A job-scoped key applies only the exact aliases its key names (PR #668
// review): the middleware's scope check trims, the importer stores the raw
// alias, so "payroll " (ASCII or Unicode space) must never be written. Since
// W6-α Validate refuses a non-canonical alias for every writer with a 400
// naming it (a syntactic check that discloses nothing stored), before the
// in-transaction key-scope refusal is reached; nothing is written.
func TestApplyJobScopedKeyRefusesNonCanonicalAlias(t *testing.T) {
	conn := freshCatalog(t)
	for name, grants := range map[string]policy.Grants{
		"key at *":           {"*": models.RoleOperator},
		"namespace-confined": {"default": models.RoleOperator},
	} {
		t.Run(name, func(t *testing.T) {
			scoped := confined(grants)
			scoped.Scope = []byte(`{"jobs":["payroll"]}`)
			for _, alias := range []string{"payroll ", "payroll ", " payroll"} {
				def := scopeDefinition("payroll", "default")
				def.Metadata.Alias = alias
				_, _, err := callApply(t, scoped, applyBody(false, def))
				require.Error(t, err)
				require.Equal(t, http.StatusBadRequest, echo.StatusCode(err))
				require.Contains(t, err.Error(), "metadata.alias "+strconv.Quote(alias)+" must not have leading or trailing whitespace")
				_, exists := jobRow(t, conn, alias)
				require.False(t, exists, "no job %q was written", alias)
			}
		})
	}
	scoped := clusterOperator()
	scoped.Scope = []byte(`{"jobs":["payroll"]}`)
	status, _, err := callApply(t, scoped, applyBody(false, scopeDefinition("payroll", "default")))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)

	// A key-scoped reader never sees a stored non-canonical alias as its own.
	view := newReadScopeForKey(nil, []string{"payroll"}, conn, nil)
	ctx := context.Background()
	require.True(t, view.aliasVisible(ctx, "payroll"))
	require.False(t, view.aliasVisible(ctx, "payroll "))
	require.False(t, view.findingVisible(ctx, internalcontract.ContractFinding{Producer: "payroll", Consumer: "payroll ", EdgeClass: string(internalcontract.EdgeClassDeclared)}))
}

// Apply's validator errors for a job-scoped key name neither an out-of-scope
// job nor its dataset (B9 fold 6), at "*" and namespace-confined.
func TestApplyJobScopedKeyValidatorErrorNamesNoOutOfScopeJob(t *testing.T) {
	for name, grants := range map[string]policy.Grants{
		"key at *":           {"*": models.RoleOperator},
		"namespace-confined": {"marketing": models.RoleOperator},
	} {
		t.Run(name, func(t *testing.T) {
			freshCatalog(t, producerDefinition("other-producer", "marketing", "secret-dataset-name"))
			scoped := confined(grants)
			scoped.Scope = []byte(`{"jobs":["mine"]}`)
			_, _, err := callApply(t, scoped, applyBody(false, producerDefinition("mine", "marketing", "secret-dataset-name")))
			var he *echo.HTTPError
			require.ErrorAs(t, err, &he)
			require.Equal(t, http.StatusBadRequest, he.Code)
			require.NotContains(t, he.Message, "other-producer")
			require.NotContains(t, he.Message, "secret-dataset-name")

			// The same request by an unscoped cluster operator names both.
			_, _, err = callApply(t, clusterOperator(), applyBody(false, producerDefinition("mine", "marketing", "secret-dataset-name")))
			require.ErrorAs(t, err, &he)
			require.Contains(t, he.Message, "other-producer")
		})
	}
}

// Findings, validator errors and the request keys carry trimmed aliases, so
// a trimmed alias names a job only when exactly one stored row (live or
// retired) carries it (PR #668 review r8). A retired marketing "consumer"
// and a live finance "consumer " (ASCII or Unicode space) are two jobs:
// "consumer" is withheld from a marketing reader, not authorised by the
// retired marketing row. A lone finance twin is foreign to marketing; a lone
// retired marketing "consumer" is marketing's own; a key scoped to
// "consumer" never covers a stored "consumer ".
func TestReadScopeWithholdsAmbiguousTrimmedAlias(t *testing.T) {
	ctx := context.Background()
	finding := internalcontract.ContractFinding{Producer: "producer", Consumer: "consumer", EdgeClass: string(internalcontract.EdgeClassDeclared)}
	retireConsumer := func(conn *gorm.DB) {
		t.Helper()
		require.NoError(t, conn.Where("alias = ?", "consumer").Delete(&models.Job{}).Error)
		row, ok := jobRow(t, conn, "consumer")
		require.True(t, ok)
		require.True(t, row.DeletedAt.Valid, "the marketing consumer is retired")
	}
	for _, space := range []string{" ", " ", " "} {
		twin := scopeDefinition("consumer", "finance")
		twin.Metadata.Alias = "consumer" + space

		conn := freshCatalog(t, scopeDefinition("producer", "marketing"), scopeDefinition("consumer", "marketing"))
		retireConsumer(conn)
		require.True(t, newReadScopeFor([]string{"marketing"}, conn, nil).findingVisible(ctx, finding), "a lone retired marketing consumer is marketing's own")

		applyDefinitions(t, conn, twin)
		for name, allowed := range map[string][]string{"marketing": {"marketing"}, "both": {"marketing", "finance"}} {
			view := newReadScopeFor(allowed, conn, nil)
			require.False(t, view.findingVisible(ctx, finding), "%s: an ambiguous trimmed consumer is withheld", name)
			require.Equal(t, hiddenJob, view.aliasOrHidden(ctx, "consumer"), name)
			require.True(t, view.aliasVisible(ctx, "producer"), name)
		}
		var unrestricted *readScope
		require.True(t, unrestricted.findingVisible(ctx, finding))

		lone := freshCatalog(t, scopeDefinition("producer", "marketing"), twin)
		require.False(t, newReadScopeFor([]string{"marketing"}, lone, nil).findingVisible(ctx, finding), "the finance twin is foreign")
		require.True(t, newReadScopeFor([]string{"finance"}, lone, nil).aliasVisible(ctx, "consumer"), "its own namespace reads it")
		require.False(t, newReadScopeForKey(nil, []string{"consumer"}, lone, nil).aliasVisible(ctx, "consumer"), "a key scope never covers the twin")
	}
}

// The whitespace-alias rows, whose patterns cannot use the alias index, are
// read once per read snapshot, not once per resolved name (PR #668 review
// r9): N findings about N distinct consumers cost one such query, and a new
// snapshot reads them again.
func TestReadScopeReadsWhitespaceAliasesOncePerSnapshot(t *testing.T) {
	ctx := context.Background()
	const n = 12
	defs := []schema.Definition{scopeDefinition("producer", "marketing")}
	for i := range n {
		defs = append(defs, scopeDefinition(fmt.Sprintf("consumer-%d", i), "marketing"))
	}
	twin := scopeDefinition("consumer-0", "finance")
	twin.Metadata.Alias = "consumer-0 "
	conn := freshCatalog(t, append(defs, twin)...)

	var whitespaceQueries, queries int
	name := "test:count_resolve_queries"
	require.NoError(t, conn.Callback().Query().After("gorm:query").Register(name, func(tx *gorm.DB) {
		queries++
		if strings.Contains(tx.Statement.SQL.String(), "LIKE") {
			whitespaceQueries++
		}
	}))
	t.Cleanup(func() { _ = conn.Callback().Query().Remove(name) })

	view := newReadScopeFor([]string{"marketing"}, conn, nil)
	for i := range n {
		finding := internalcontract.ContractFinding{Producer: "producer", Consumer: fmt.Sprintf("consumer-%d", i), EdgeClass: string(internalcontract.EdgeClassDeclared)}
		require.Equal(t, i != 0, view.findingVisible(ctx, finding), "consumer-%d", i)
	}
	require.Equal(t, 1, whitespaceQueries, "one whitespace-alias read for %d names", n)
	require.LessOrEqual(t, queries, 3*n+1, "the exact lookups use the alias index, one name at a time at most")

	whitespaceQueries = 0
	require.NoError(t, view.withSnapshot(ctx, conn, func(*gorm.DB) error {
		require.False(t, view.aliasVisible(ctx, "consumer-0"))
		require.True(t, view.aliasVisible(ctx, "consumer-1"))
		return nil
	}))
	require.Equal(t, 1, whitespaceQueries, "a new snapshot reads them again, once")
}

// The read scope's caches live exactly one transaction (PR #668 review r10).
// The importer binds the contract audience to each apply attempt's
// transaction; between two attempts (a serialization-conflict retry) an
// unrestricted writer moves the lone "consumer " from marketing to finance,
// and the retried attempt must withhold the finding rather than reuse the
// previous attempt's whitespace-alias rows. Leaving a snapshot drops what was
// read inside it as well.
func TestReadScopeCachesLiveOneTransaction(t *testing.T) {
	ctx := context.Background()
	twin := scopeDefinition("consumer", "marketing")
	twin.Metadata.Alias = "consumer "
	conn := freshCatalog(t, scopeDefinition("producer", "marketing"), twin)
	finding := internalcontract.ContractFinding{Producer: "producer", Consumer: "consumer", EdgeClass: string(internalcontract.EdgeClassDeclared)}
	move := func(namespace string) {
		t.Helper()
		require.NoError(t, conn.Model(&models.Job{}).Where("alias = ?", "consumer ").Update("namespace", namespace).Error)
	}

	audience := newReadScopeFor([]string{"marketing"}, conn, nil).contractAudience(ctx)
	attempt := func() bool {
		t.Helper()
		var visible bool
		require.NoError(t, conn.Transaction(func(tx *gorm.DB) error {
			visible = audience(tx)(finding)
			return nil
		}))
		return visible
	}
	require.True(t, attempt(), "first attempt: the lone marketing \"consumer \" is marketing's own")
	move("finance")
	require.False(t, attempt(), "the retried attempt reads the moved row")

	view := newReadScopeFor([]string{"marketing"}, conn, nil)
	require.NoError(t, view.withSnapshot(ctx, conn, func(*gorm.DB) error {
		require.False(t, view.aliasVisible(ctx, "consumer"), "in finance inside the snapshot")
		return nil
	}))
	move("marketing")
	require.True(t, view.aliasVisible(ctx, "consumer"), "nothing read inside the snapshot outlives it")
}
