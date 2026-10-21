package jobdef

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// tenancyFixture is an importer on a fresh catalog, with or without a loaded
// policy file (the process holder rule 2 reads).
type tenancyFixture struct {
	t        *testing.T
	db       *gorm.DB
	importer *Importer
}

func newTenancyFixture(t *testing.T, fileBacked bool) *tenancyFixture {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	if fileBacked {
		holder, _ := fileHolder(t, declaredPolicy)
		policy.SetProcessHolder(holder)
		t.Cleanup(func() { policy.SetProcessHolder(nil) })
	}
	return &tenancyFixture{t: t, db: db, importer: NewImporter(db)}
}

// def is a one-step job; path "" gives it a cron trigger, otherwise an HTTP
// trigger on path.
func (f *tenancyFixture) def(alias, namespace, path string) *schema.Definition {
	f.t.Helper()
	def, err := schema.Parse([]byte(strings.Replace(testutil.SampleJob, "csv-to-parquet", alias, 1)))
	require.NoError(f.t, err)
	def.Metadata.Namespace = namespace
	if path != "" {
		def.Trigger = schema.Trigger{Type: "http", Configuration: map[string]any{"path": path}}
	}
	return def
}

func (f *tenancyFixture) apply(alias, namespace, path string, opts *ApplyOptions) (*models.Job, error) {
	f.t.Helper()
	return f.importer.ApplyWithOptions(context.Background(), f.def(alias, namespace, path), opts)
}

func (f *tenancyFixture) mustApply(alias, namespace, path string) *models.Job {
	f.t.Helper()
	job, err := f.apply(alias, namespace, path, nil)
	require.NoError(f.t, err)
	return job
}

func (f *tenancyFixture) job(alias string) models.Job {
	f.t.Helper()
	var job models.Job
	require.NoError(f.t, f.db.Unscoped().Where("alias = ?", alias).First(&job).Error)
	return job
}

func (f *tenancyFixture) jobExists(alias string) bool {
	f.t.Helper()
	var count int64
	require.NoError(f.t, f.db.Unscoped().Model(&models.Job{}).Where("alias = ?", alias).Count(&count).Error)
	return count > 0
}

func (f *tenancyFixture) trigger(id uuid.UUID) models.Trigger {
	f.t.Helper()
	var trigger models.Trigger
	require.NoError(f.t, f.db.Unscoped().First(&trigger, "id = ?", id).Error)
	return trigger
}

// share points job b at job a's trigger row: a legacy shared row (no API path
// creates one).
func (f *tenancyFixture) share(a, b string) uuid.UUID {
	f.t.Helper()
	row := f.job(a).TriggerID
	require.NoError(f.t, f.db.Model(&models.Job{}).Where("alias = ?", b).Update("trigger_id", row).Error)
	return row
}

func requireTenancyRefusal(t *testing.T, err error, reason string) *NamespaceAllowlistError {
	t.Helper()
	nsErr, ok := errors.AsType[*NamespaceAllowlistError](err)
	require.True(t, ok, "want a tenancy refusal, got %v", err)
	require.Equal(t, reason, nsErr.Reason)
	return nsErr
}

func TestTenancyScopeAllows(t *testing.T) {
	var unrestricted *TenancyScope
	require.True(t, unrestricted.Allows("x", "finance"))
	require.Nil(t, NewTenancyScope(nil, nil), "no restriction is the nil scope")

	byNamespace := NewTenancyScope([]string{"marketing"}, nil)
	require.True(t, byNamespace.Allows("x", "marketing"))
	require.False(t, byNamespace.Allows("x", "finance"))
	require.False(t, byNamespace.Allows("x", ""), "blank is the default namespace")

	// A job-scoped key at "*" is confined by its aliases alone (B9 fold 1).
	byAlias := NewTenancyScope(nil, []string{"a"})
	require.NotNil(t, byAlias)
	require.True(t, byAlias.Allows("a", "finance"))
	require.False(t, byAlias.Allows(" a ", "finance"), "a non-canonical alias is a different job (PR #668 review)")
	require.False(t, byAlias.Allows("b", "default"))

	none := NewTenancyScope([]string{}, nil)
	require.NotNil(t, none, "confined to no namespace is not unrestricted")
	require.False(t, none.Allows("x", "default"))
}

// Without a policy file and without a scope nothing changes: two namespaces
// may share a webhook path (fan-out), and the importer issues no tenancy
// query at all.
func TestTenancyNoPolicyNoScopeIsUnchanged(t *testing.T) {
	f := newTenancyFixture(t, false)
	queries := 0
	require.NoError(t, f.db.Callback().Query().After("gorm:query").Register("tenancy:count", func(tx *gorm.DB) {
		if strings.Contains(tx.Statement.SQL.String(), "normalized_path") {
			queries++
		}
	}))
	f.mustApply("mkt", "marketing", "/hooks/shared")
	f.mustApply("fin", "finance", "/hooks/shared")
	require.Zero(t, queries, "no tenancy lookups for an unrestricted writer without a policy file")
	require.Equal(t, "shared", f.trigger(f.job("fin").TriggerID).NormalizedPath)
}

// Rule 2: with a policy file, a webhook path is subscribed from one
// namespace, for every writer; the same namespace may still fan out.
func TestTenancyFileBackedRefusesCrossNamespacePath(t *testing.T) {
	f := newTenancyFixture(t, true)
	f.mustApply("mkt", "marketing", "/hooks/orders")
	f.mustApply("mkt-2", "marketing", "hooks/orders/")

	_, err := f.apply("fin", "finance", "/v1/hooks/orders", nil)
	require.ErrorIs(t, err, ErrTriggerTenancy)
	tenancyErr, ok := errors.AsType[*TriggerTenancyError](err)
	require.True(t, ok)
	require.Equal(t, "orders", tenancyErr.WebhookPath)
	require.Equal(t, []string{"marketing"}, tenancyErr.Others)
	require.Contains(t, err.Error(), `webhook path "orders" is already subscribed from namespace "marketing"`)
	require.False(t, f.jobExists("fin"), "the refused definition wrote nothing")
	testutil.AssertCount(t, f.db, &models.Trigger{}, 2)

	// Moving the only subscriber off the path frees it.
	f.mustApply("mkt", "marketing", "/hooks/other")
	f.mustApply("mkt-2", "marketing", "/hooks/other")
	f.mustApply("fin", "finance", "/hooks/orders")
}

// Rule 1 for a namespace-confined writer, with no policy file: it cannot
// subscribe a path that already serves a job outside its namespaces, and
// the refusal is raised before any write.
func TestTenancyConfinedWriterCannotSubscribeForeignPath(t *testing.T) {
	f := newTenancyFixture(t, false)
	fin := f.mustApply("fin", "finance", "/hooks/payments")
	finTrigger := f.trigger(fin.TriggerID)

	_, err := f.apply("mkt", "marketing", "/hooks/payments", &ApplyOptions{NamespaceAllowlist: []string{"marketing"}})
	nsErr := requireTenancyRefusal(t, err, NamespaceReasonWebhookPath)
	require.Equal(t, "payments", nsErr.WebhookPath)
	require.False(t, f.jobExists("mkt"))
	testutil.AssertCount(t, f.db, &models.Trigger{}, 1)
	require.Equal(t, finTrigger.UpdatedAt, f.trigger(fin.TriggerID).UpdatedAt, "the finance trigger is untouched")

	// A writer confined to both namespaces may (no policy file, no rule 2).
	_, err = f.apply("mkt", "marketing", "/hooks/payments", &ApplyOptions{NamespaceAllowlist: []string{"marketing", "finance"}})
	require.NoError(t, err)
}

// Rule 1 for a job-scoped key at "*" (B9 fold 1): its alias scope alone
// confines it, with or without a policy file.
func TestTenancyJobScopedKeyIsConfined(t *testing.T) {
	for _, fileBacked := range []bool{false, true} {
		t.Run(map[bool]string{false: "no policy file", true: "policy file"}[fileBacked], func(t *testing.T) {
			f := newTenancyFixture(t, fileBacked)
			f.mustApply("other", "default", "/hooks/scoped")
			_, err := f.apply("mine", "default", "/hooks/scoped", &ApplyOptions{JobAliasScope: []string{"mine"}})
			requireTenancyRefusal(t, err, NamespaceReasonWebhookPath)
			require.False(t, f.jobExists("mine"))

			// A shared trigger row serving an out-of-scope alias.
			f.mustApply("mine", "default", "")
			f.mustApply("peer", "default", "")
			row := f.share("mine", "peer")
			before := f.trigger(row)
			_, err = f.apply("mine", "default", "", &ApplyOptions{JobAliasScope: []string{"mine"}})
			nsErr := requireTenancyRefusal(t, err, NamespaceReasonTriggerShared)
			require.Equal(t, "mine", nsErr.Alias)
			require.Equal(t, before.UpdatedAt, f.trigger(row).UpdatedAt, "the shared row is not modified")
			require.Equal(t, row, f.job("mine").TriggerID)

			_, err = f.apply("mine", "default", "", &ApplyOptions{JobAliasScope: []string{"mine", "peer"}})
			require.NoError(t, err, "a key scoped to every job the row serves may write it")
		})
	}
}

// Rule 2 repair (B9 fold 12): a writer authorised over every job a shared row
// serves detaches the written job onto a fresh row when the row would serve
// two namespaces; the old row keeps serving the other job, unchanged.
func TestTenancyDetachesLegacySharedRow(t *testing.T) {
	f := newTenancyFixture(t, true)
	f.mustApply("mkt", "marketing", "")
	f.mustApply("fin", "finance", "")
	row := f.share("mkt", "fin")
	before := f.trigger(row)

	job, err := f.apply("mkt", "marketing", "", nil)
	require.NoError(t, err)
	require.NotEqual(t, row, job.TriggerID, "the written job gets its own row")
	require.Equal(t, row, f.job("fin").TriggerID, "the other job keeps the old row")
	require.Equal(t, before.Configuration, f.trigger(row).Configuration)
	require.Equal(t, before.UpdatedAt, f.trigger(row).UpdatedAt, "the old row is not rewritten")
	require.Equal(t, before.Configuration, f.trigger(job.TriggerID).Configuration, "the new row carries the job's configuration")

	// A confined writer that cannot authorise every served job is refused.
	f2 := newTenancyFixture(t, true)
	f2.mustApply("mkt", "marketing", "")
	f2.mustApply("fin", "finance", "")
	shared := f2.share("mkt", "fin")
	_, err = f2.apply("mkt", "marketing", "", &ApplyOptions{NamespaceAllowlist: []string{"marketing"}})
	requireTenancyRefusal(t, err, NamespaceReasonTriggerShared)
	require.Equal(t, shared, f2.job("mkt").TriggerID)
}

// Without a policy file a shared row is reconciled in place, as before.
func TestTenancyNoPolicyKeepsSharedRow(t *testing.T) {
	f := newTenancyFixture(t, false)
	f.mustApply("mkt", "marketing", "")
	f.mustApply("fin", "finance", "")
	row := f.share("mkt", "fin")
	job, err := f.apply("mkt", "marketing", "", nil)
	require.NoError(t, err)
	require.Equal(t, row, job.TriggerID)
}

// A detach that leaves the old row on the same webhook path as the written
// job is still a cross-namespace path: refused (409), nothing written; with a
// new path the repair succeeds.
func TestTenancyDetachThenPathConflict(t *testing.T) {
	f := newTenancyFixture(t, true)
	f.mustApply("mkt", "marketing", "/hooks/legacy")
	f.mustApply("fin", "finance", "/hooks/fin-own")
	row := f.share("mkt", "fin")

	_, err := f.apply("mkt", "marketing", "/hooks/legacy", nil)
	require.ErrorIs(t, err, ErrTriggerTenancy)
	require.Equal(t, row, f.job("mkt").TriggerID, "nothing was written")

	job, err := f.apply("mkt", "marketing", "/hooks/legacy-moved", nil)
	require.NoError(t, err)
	require.NotEqual(t, row, job.TriggerID)
}

// B9 fold 8: an allowlisted batch skips a tenancy refusal before writing
// anything for it, so the skipped manifest leaves every row unchanged.
func TestTenancyAllowlistedBatchSkipsWithoutWriting(t *testing.T) {
	f := newTenancyFixture(t, false)
	fin := f.mustApply("fin", "finance", "/hooks/sync")
	finRow := f.trigger(fin.TriggerID)
	f.mustApply("mkt-existing", "marketing", "/hooks/mkt-old")
	existing := f.job("mkt-existing")
	existingRow := f.trigger(existing.TriggerID)

	opts := &ApplyOptions{NamespaceAllowlist: []string{"marketing"}}
	result, err := f.importer.ApplyBatchWithOptions(context.Background(), []BatchPlan{
		{Def: f.def("mkt-new", "marketing", "/hooks/fresh"), Opts: opts},
		{Def: f.def("mkt-existing", "marketing", "/hooks/sync"), Opts: opts},
	})
	require.NoError(t, err)
	require.Equal(t, 1, result.Applied)
	require.Len(t, result.Refused, 1)
	require.Equal(t, 1, result.Refused[0].Index)
	require.Equal(t, NamespaceReasonWebhookPath, result.Refused[0].Err.Reason)

	after := f.job("mkt-existing")
	require.Equal(t, existing.UpdatedAt, after.UpdatedAt, "the skipped job row is unchanged")
	require.Equal(t, existingRow.Configuration, f.trigger(existing.TriggerID).Configuration)
	require.Equal(t, existingRow.UpdatedAt, f.trigger(existing.TriggerID).UpdatedAt)
	require.Equal(t, finRow.UpdatedAt, f.trigger(fin.TriggerID).UpdatedAt)
	require.True(t, f.jobExists("mkt-new"))
}

func TestApplyConfinedBatchIsAllOrNothing(t *testing.T) {
	f := newTenancyFixture(t, false)
	f.mustApply("fin", "finance", "/hooks/fin")
	opts := &ApplyOptions{NamespaceAllowlist: []string{"marketing"}}

	_, err := f.importer.ApplyConfinedBatch(context.Background(), []BatchPlan{
		{Def: f.def("mkt-first", "marketing", "/hooks/mkt"), Opts: opts},
		{Def: f.def("mkt-second", "marketing", "/hooks/fin"), Opts: opts},
	}, ConfinedBatchOptions{})
	requireTenancyRefusal(t, err, NamespaceReasonWebhookPath)
	require.False(t, f.jobExists("mkt-first"), "a later refusal rolls the earlier definition back")

	// A job-scoped key's batch is atomic too (B9 fold 2).
	scoped := &ApplyOptions{JobAliasScope: []string{"a", "b"}}
	_, err = f.importer.ApplyConfinedBatch(context.Background(), []BatchPlan{
		{Def: f.def("a", "default", "/hooks/a"), Opts: scoped},
		{Def: f.def("b", "default", "/hooks/fin"), Opts: scoped},
	}, ConfinedBatchOptions{})
	requireTenancyRefusal(t, err, NamespaceReasonWebhookPath)
	require.False(t, f.jobExists("a"))

	result, err := f.importer.ApplyConfinedBatch(context.Background(), []BatchPlan{
		{Def: f.def("mkt-first", "marketing", "/hooks/mkt"), Opts: opts},
		{Def: f.def("mkt-second", "marketing", ""), Opts: opts},
	}, ConfinedBatchOptions{})
	require.NoError(t, err)
	require.Equal(t, ConfinedBatchResult{Applied: 2}, result)
}

// A job created in another namespace after the middleware decided (here,
// between the request and the transaction) is refused inside it: nothing of
// the batch is applied.
func TestApplyConfinedBatchRefusesLateForeignJob(t *testing.T) {
	f := newTenancyFixture(t, false)
	opts := &ApplyOptions{NamespaceAllowlist: []string{"marketing"}}
	created := false
	beforeConfinedBatchTx = func() {
		if !created {
			created = true
			f.mustApply("contested", "finance", "")
		}
	}
	t.Cleanup(func() { beforeConfinedBatchTx = nil })

	_, err := f.importer.ApplyConfinedBatch(context.Background(), []BatchPlan{
		{Def: f.def("mkt-ok", "marketing", ""), Opts: opts},
		{Def: f.def("contested", "marketing", ""), Opts: opts},
	}, ConfinedBatchOptions{})
	nsErr := requireTenancyRefusal(t, err, NamespaceReasonStored)
	require.Equal(t, "finance", nsErr.StoredNamespace)
	require.False(t, f.jobExists("mkt-ok"))
	require.Equal(t, "finance", f.job("contested").Namespace, "the foreign job is untouched")
}

// B9 fold 11: a confined prune's candidates are the absent jobs in the
// namespaces it may prune; jobs elsewhere are never candidates.
func TestApplyConfinedBatchPrunesOnlyAllowedNamespaces(t *testing.T) {
	f := newTenancyFixture(t, false)
	f.mustApply("keep", "marketing", "")
	f.mustApply("gone", "marketing", "")
	f.mustApply("fin", "finance", "")
	opts := &ApplyOptions{NamespaceAllowlist: []string{"marketing"}}

	result, err := f.importer.ApplyConfinedBatch(context.Background(), []BatchPlan{
		{Def: f.def("keep", "marketing", ""), Opts: opts},
	}, ConfinedBatchOptions{Prune: true, PruneNamespaces: []string{"marketing"}})
	require.NoError(t, err)
	require.Equal(t, ConfinedBatchResult{Applied: 1, Pruned: 1}, result)
	require.True(t, f.job("gone").DeletedAt.Valid)
	require.False(t, f.job("fin").DeletedAt.Valid, "the finance job is never a candidate")

	_, err = f.importer.ApplyConfinedBatch(context.Background(), []BatchPlan{
		{Def: f.def("keep", "marketing", ""), Opts: opts},
	}, ConfinedBatchOptions{Prune: true})
	require.Error(t, err, "a confined prune with no namespace set fails closed")
}

// A running prune candidate fails the whole confined batch.
func TestApplyConfinedBatchPruneRunningJobRollsBack(t *testing.T) {
	f := newTenancyFixture(t, false)
	f.mustApply("keep", "marketing", "")
	busy := f.mustApply("busy", "marketing", "")
	require.NoError(t, f.db.Create(&models.JobRun{ID: uuid.New(), JobID: busy.ID, Namespace: "marketing", Status: "running"}).Error)
	opts := &ApplyOptions{NamespaceAllowlist: []string{"marketing"}}
	_, err := f.importer.ApplyConfinedBatch(context.Background(), []BatchPlan{
		{Def: f.def("new-one", "marketing", ""), Opts: opts},
	}, ConfinedBatchOptions{Prune: true, PruneNamespaces: []string{"marketing"}})
	require.ErrorIs(t, err, ErrJobRunning)
	require.False(t, f.jobExists("new-one"))
}

// B9 fold 3: pruning a job keeps a trigger row another live job still uses.
func TestPruneKeepsTriggerOfRetainedJob(t *testing.T) {
	f := newTenancyFixture(t, false)
	f.mustApply("pruned", "default", "")
	f.mustApply("kept", "default", "")
	row := f.share("pruned", "kept")
	alone := f.mustApply("alone", "default", "")

	n, err := f.importer.PruneMissing(context.Background(), []string{"kept"}, nil)
	require.NoError(t, err)
	require.Equal(t, 2, n)
	require.False(t, f.trigger(row).DeletedAt.Valid, "the retained job's trigger stays live")
	require.Equal(t, row, f.job("kept").TriggerID)
	require.True(t, f.trigger(alone.TriggerID).DeletedAt.Valid, "an unshared trigger is retired with its job")
}

func TestValidateTriggerTenancyPreviewsTheBatch(t *testing.T) {
	f := newTenancyFixture(t, true)
	f.mustApply("fin", "finance", "/hooks/stored")
	defs := func(list ...*schema.Definition) []schema.Definition {
		out := make([]schema.Definition, 0, len(list))
		for _, def := range list {
			out = append(out, *def)
		}
		return out
	}

	err := ValidateTriggerTenancy(context.Background(), f.db, defs(f.def("mkt", "marketing", "/hooks/stored")), nil)
	require.ErrorIs(t, err, ErrTriggerTenancy, "a stored subscription from another namespace")

	err = ValidateTriggerTenancy(context.Background(), f.db, defs(f.def("a", "marketing", "/hooks/new"), f.def("b", "finance", "/hooks/new")), nil)
	require.ErrorIs(t, err, ErrTriggerTenancy, "two incoming definitions from two namespaces")

	// The stored subscriber moving off the path earlier in the batch frees it.
	err = ValidateTriggerTenancy(context.Background(), f.db, defs(f.def("fin", "finance", "/hooks/elsewhere"), f.def("mkt", "marketing", "/hooks/stored")), nil)
	require.NoError(t, err)

	err = ValidateTriggerTenancy(context.Background(), f.db, defs(f.def("mkt", "marketing", "/hooks/stored")), NewTenancyScope([]string{"marketing"}, nil))
	requireTenancyRefusal(t, err, NamespaceReasonWebhookPath)
	require.Equal(t, "webhook path \"stored\" is subscribed by a job outside your access", RedactedTenancyMessage(err))
}

// The preview follows the importer's trigger-row writes: a job written onto a
// shared row rewrites the row in place, so the row's other job leaves the old
// path with it, and the apply agrees; a retired job revived on its row
// subscribes the path again.
func TestValidateTriggerTenancyFollowsTriggerRowWrites(t *testing.T) {
	ctx := context.Background()
	f := newTenancyFixture(t, true)
	f.mustApply("a", "marketing", "/hooks/old")
	f.mustApply("b", "marketing", "")
	row := f.share("a", "b")
	batch := []schema.Definition{*f.def("a", "marketing", "/hooks/new"), *f.def("c", "finance", "/hooks/old")}
	require.NoError(t, ValidateTriggerTenancy(ctx, f.db, batch, nil), "a's in-place write takes b off /old too")
	for i := range batch {
		_, err := f.importer.ApplyWithOptions(ctx, &batch[i], nil)
		require.NoError(t, err, "the apply agrees with the preview")
	}
	require.Equal(t, row, f.job("b").TriggerID)
	require.Equal(t, "new", f.trigger(row).NormalizedPath)

	g := newTenancyFixture(t, true)
	g.mustApply("r", "marketing", "/hooks/p")
	g.mustApply("keep", "default", "")
	_, err := g.importer.PruneMissing(ctx, []string{"keep"}, nil)
	require.NoError(t, err)
	require.True(t, g.job("r").DeletedAt.Valid)
	revive := []schema.Definition{*g.def("r", "marketing", "/hooks/p"), *g.def("s", "finance", "/hooks/p")}
	require.ErrorIs(t, ValidateTriggerTenancy(ctx, g.db, revive, nil), ErrTriggerTenancy, "the revived job subscribes /p again")
}

func TestCheckJobCreateTx(t *testing.T) {
	f := newTenancyFixture(t, false)
	f.mustApply("taken", "finance", "/hooks/legacy")
	scope := NewTenancyScope([]string{"marketing"}, nil)
	cfg := map[string]any{"path": "/hooks/legacy"}

	err := CheckJobCreateTx(f.db, "taken", "marketing", "cron", nil, scope)
	nsErr := requireTenancyRefusal(t, err, NamespaceReasonStored)
	require.Equal(t, "finance", nsErr.StoredNamespace)

	err = CheckJobCreateTx(f.db, "fresh", "marketing", "http", cfg, scope)
	requireTenancyRefusal(t, err, NamespaceReasonWebhookPath)

	require.NoError(t, CheckJobCreateTx(f.db, "fresh", "marketing", "http", map[string]any{"path": "/hooks/mine"}, scope))
	require.NoError(t, CheckJobCreateTx(f.db, "fresh", "marketing", "http", cfg, nil), "no scope, no policy file")

	g := newTenancyFixture(t, true)
	g.mustApply("taken", "finance", "/hooks/legacy")
	require.ErrorIs(t, CheckJobCreateTx(g.db, "fresh", "marketing", "http", cfg, nil), ErrTriggerTenancy)
}

// nonCanonicalAliases are "payroll" with an ASCII or Unicode space around it:
// the trimmed form a key scope names, but a distinct stored alias.
var nonCanonicalAliases = []string{"payroll ", "payroll ", " payroll", " payroll"}

// A job-scoped writer writes only the exact aliases its key names (PR #668
// review): a non-canonical alias is refused inside the transaction on every
// write path, nothing is written, and the exact alias still applies. Scoped
// to "*" and namespace-confined alike; a writer without an alias scope is
// unchanged.
func TestJobScopedWriterNeedsExactAlias(t *testing.T) {
	ctx := context.Background()
	for name, namespaces := range map[string][]string{"key at *": nil, "namespace-confined": {"default"}} {
		t.Run(name, func(t *testing.T) {
			f := newTenancyFixture(t, false)
			scope := NewTenancyScope(namespaces, []string{"payroll"})
			opts := &ApplyOptions{NamespaceAllowlist: namespaces, JobAliasScope: []string{"payroll"}}
			for _, alias := range nonCanonicalAliases {
				def := f.def("payroll", "default", "")
				def.Metadata.Alias = alias

				_, err := f.importer.ApplyWithOptions(ctx, def, opts)
				requireTenancyRefusal(t, err, NamespaceReasonAliasScope)
				_, err = f.importer.ApplyConfinedBatch(ctx, []BatchPlan{{Def: def, Opts: opts}}, ConfinedBatchOptions{})
				requireTenancyRefusal(t, err, NamespaceReasonAliasScope)
				requireTenancyRefusal(t, CheckJobCreateTx(f.db, alias, "default", "cron", nil, scope), NamespaceReasonAliasScope)
				require.False(t, f.jobExists(alias), "no job %q was written", alias)

				denial, _ := WriteRefusal(err, scope)
				require.NotNil(t, denial)
				require.Equal(t, "insufficient_scope", denial.Reason)
				require.Equal(t, "insufficient permissions", denial.Message)
				require.False(t, scope.Allows(alias, "default"), "a stored %q is outside the scope", alias)
			}
			require.NoError(t, CheckJobCreateTx(f.db, "payroll", "default", "cron", nil, scope))
			_, err := f.importer.ApplyConfinedBatch(ctx, []BatchPlan{{Def: f.def("payroll", "default", ""), Opts: opts}}, ConfinedBatchOptions{})
			require.NoError(t, err)
			require.True(t, f.jobExists("payroll"))

			// The legacy create's stored-row lookup is by the raw alias.
			if namespaces != nil {
				taken := f.def("taken", "finance", "")
				taken.Metadata.Alias = "taken "
				_, err := f.importer.ApplyWithOptions(ctx, taken, nil)
				require.NoError(t, err)
				require.NoError(t, CheckJobCreateTx(f.db, "taken", "default", "cron", nil, NewTenancyScope(namespaces, nil)), "a different stored alias")
				requireTenancyRefusal(t, CheckJobCreateTx(f.db, "taken ", "default", "cron", nil, NewTenancyScope(namespaces, nil)), NamespaceReasonStored)
			}
		})
	}

	// Without an alias scope nothing changes (no policy file: no check at all).
	f := newTenancyFixture(t, false)
	def := f.def("payroll", "default", "")
	def.Metadata.Alias = "payroll "
	_, err := f.importer.ApplyWithOptions(ctx, def, nil)
	require.NoError(t, err)
	require.NoError(t, CheckJobCreateTx(f.db, "other ", "default", "cron", nil, nil))
}

func TestCheckTriggerPatchTx(t *testing.T) {
	f := newTenancyFixture(t, true)
	mkt := f.mustApply("mkt", "marketing", "/hooks/mkt")
	f.mustApply("fin", "finance", "/hooks/fin")
	scope := NewTenancyScope([]string{"marketing"}, nil)

	require.NoError(t, CheckTriggerPatchTx(f.db, TriggerPatch{TriggerID: mkt.TriggerID}, scope), "alias-only, own trigger")
	require.NoError(t, CheckTriggerPatchTx(f.db, TriggerPatch{TriggerID: mkt.TriggerID, Path: "mkt-2", PathChanges: true}, scope))
	err := CheckTriggerPatchTx(f.db, TriggerPatch{TriggerID: mkt.TriggerID, Path: "fin", PathChanges: true}, scope)
	requireTenancyRefusal(t, err, NamespaceReasonWebhookPath)
	err = CheckTriggerPatchTx(f.db, TriggerPatch{TriggerID: mkt.TriggerID, Path: "fin", PathChanges: true}, nil)
	require.ErrorIs(t, err, ErrTriggerTenancy, "a cluster writer is held to rule 2")

	// B9 fold 7: a row serving two namespaces is refused for any PATCH,
	// even by a writer authorised in both, alias-only included.
	row := f.share("mkt", "fin")
	both := NewTenancyScope([]string{"marketing", "finance"}, nil)
	for _, patch := range []TriggerPatch{{TriggerID: row}, {TriggerID: row, Path: "unused", PathChanges: true}} {
		require.ErrorIs(t, CheckTriggerPatchTx(f.db, patch, both), ErrTriggerTenancy)
		require.ErrorIs(t, CheckTriggerPatchTx(f.db, patch, nil), ErrTriggerTenancy)
	}
	// ...and a confined writer outside one of them is refused first.
	requireTenancyRefusal(t, CheckTriggerPatchTx(f.db, TriggerPatch{TriggerID: row}, scope), NamespaceReasonTriggerShared)
}

func TestExportConfinedTx(t *testing.T) {
	f := newTenancyFixture(t, false)
	mkt := f.mustApply("mkt", "marketing", "/hooks/secret-path")
	f.mustApply("fin", "finance", "")
	scope := NewTenancyScope([]string{"marketing"}, nil)
	redacted := map[string]any{"redacted": "x"}
	outside := errors.New("outside")
	deny := func(*models.Job) error { return outside }

	def, err := ExportConfinedTx(f.db, mkt.ID.String(), scope, redacted, deny)
	require.NoError(t, err)
	require.Equal(t, "/hooks/secret-path", def.Trigger.Configuration["path"])

	_, err = ExportConfinedTx(f.db, f.job("fin").ID.String(), scope, redacted, deny)
	require.ErrorIs(t, err, outside)

	f.share("mkt", "fin")
	def, err = ExportConfinedTx(f.db, mkt.ID.String()[:13], scope, redacted, deny)
	require.NoError(t, err)
	require.Equal(t, redacted, def.Trigger.Configuration, "a trigger shared with a job outside the scope is withheld")
	require.Nil(t, def.Trigger.DefaultParams)
}

func TestWriteRefusalMessages(t *testing.T) {
	scope := NewTenancyScope([]string{"marketing"}, nil)
	denial, conflict := WriteRefusal(&NamespaceAllowlistError{Alias: "a", RequestedNamespace: "finance", Reason: NamespaceReasonRequested}, scope)
	require.Empty(t, conflict)
	require.Equal(t, `insufficient permissions: operator required in namespace "finance"`, denial.Message)
	require.Equal(t, "insufficient_role", denial.Reason)

	denial, _ = WriteRefusal(&NamespaceAllowlistError{Alias: "a", RequestedNamespace: "marketing", StoredNamespace: "finance", Reason: NamespaceReasonStored}, scope)
	require.Equal(t, `insufficient permissions: operator required in namespace "finance"`, denial.Message)

	denial, _ = WriteRefusal(&NamespaceAllowlistError{Alias: "a", RequestedNamespace: "marketing", Reason: NamespaceReasonTriggerShared}, scope)
	require.Equal(t, `insufficient permissions: the trigger of job "a" also serves a job outside your access`, denial.Message)
	require.Equal(t, "trigger_tenancy", denial.Reason)
	require.Equal(t, []string{"marketing"}, denial.Namespaces)

	conflictErr := &TriggerTenancyError{Alias: "a", WebhookPath: "p", Namespace: "marketing", Others: []string{"finance"}}
	denial, _ = WriteRefusal(conflictErr, scope)
	require.Equal(t, `insufficient permissions: webhook path "p" is subscribed from another namespace`, denial.Message)
	require.NotContains(t, denial.Message, "finance")
	denial, conflict = WriteRefusal(conflictErr, nil)
	require.Nil(t, denial)
	require.Contains(t, conflict, `namespace "finance"`)

	denial, conflict = WriteRefusal(errors.New("other"), scope)
	require.Nil(t, denial)
	require.Empty(t, conflict)
}
