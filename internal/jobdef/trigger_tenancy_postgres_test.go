package jobdef

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// The PostgreSQL races of trigger tenancy (identity-and-access B9), executed
// against a real server: opt-in with CAESIUM_TEST_POSTGRES_DSN (the repo's
// convention), each test in a fresh schema. Two writers start together and
// the one that takes the advisory locks second holds them for a while before
// its transaction, so without the locks (or with locks taken after the
// REPEATABLE READ snapshot) both would read the absent alias or webhook path
// and both would commit.

func openTenancyPostgres(t *testing.T) *gorm.DB {
	t.Helper()
	return openTenancyPostgresPool(t, 8)
}

// openTenancyPostgresPool is openTenancyPostgres with a writer pool of
// maxOpen connections.
func openTenancyPostgresPool(t *testing.T, maxOpen int) *gorm.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("CAESIUM_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("CAESIUM_TEST_POSTGRES_DSN is not set; PostgreSQL tenancy race tests are opt-in")
	}
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	adminSQL, err := admin.DB()
	require.NoError(t, err)
	schemaName := "caesium_tenancy_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	require.NoError(t, admin.Exec("CREATE SCHEMA "+schemaName).Error)

	scoped := dsn + " search_path=" + schemaName
	if strings.Contains(dsn, "://") {
		parsed, err := url.Parse(dsn)
		require.NoError(t, err)
		query := parsed.Query()
		query.Set("search_path", schemaName)
		parsed.RawQuery = query.Encode()
		scoped = parsed.String()
	}
	db, err := gorm.Open(postgres.Open(scoped), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(maxOpen)
	t.Cleanup(func() {
		require.NoError(t, sqlDB.Close())
		require.NoError(t, admin.Exec("DROP SCHEMA "+schemaName+" CASCADE").Error)
		require.NoError(t, adminSQL.Close())
	})
	var current string
	require.NoError(t, db.Raw("SELECT current_schema()").Scan(&current).Error)
	require.Equal(t, schemaName, current)
	require.NoError(t, db.AutoMigrate(tenancyPostgresModels()...))
	return db
}

// tenancyPostgresModels is models.All without the tables whose columns are
// declared with SQLite's blob type (the internal CA, node enrollments and run
// checkpoints), which PostgreSQL does not know and the importer never touches.
func tenancyPostgresModels() []any {
	out := make([]any, 0, len(models.All))
	for _, model := range models.All {
		switch model.(type) {
		case *models.InternalCAGeneration, *models.InternalNodeEnrollment, *models.RunCheckpoint:
			continue
		}
		out = append(out, model)
	}
	return out
}

// holdAdvisoryLocks makes every writer keep its advisory locks for a while
// before its transaction begins, so the second writer of a race is still
// waiting when the first commits.
func holdAdvisoryLocks(t *testing.T) {
	t.Helper()
	lockSessionHook = func([]string) { time.Sleep(250 * time.Millisecond) }
	t.Cleanup(func() { lockSessionHook = nil })
}

// race runs both writers at once and returns their errors.
func race(first, second func() error) (error, error) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, 2)
	for i, fn := range []func() error{first, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = fn()
		}()
	}
	close(start)
	wg.Wait()
	return errs[0], errs[1]
}

// oneRefused asserts exactly one writer won and the other got the refusal.
func oneRefused(t *testing.T, a, b error, check func(error)) {
	t.Helper()
	switch {
	case a == nil && b != nil:
		check(b)
	case b == nil && a != nil:
		check(a)
	default:
		t.Fatalf("want exactly one writer refused, got %v and %v", a, b)
	}
}

func uniqueName(prefix string) string {
	return fmt.Sprintf("%s-%s", prefix, strings.ReplaceAll(uuid.NewString(), "-", "")[:12])
}

// Two confined callers creating one alias in their own namespaces: the
// second waits for the first's locks, then reads its row and is refused, not
// a unique-constraint error.
func TestPostgresConfinedBatchesRaceForAbsentAlias(t *testing.T) {
	db := openTenancyPostgres(t)
	holdAdvisoryLocks(t)
	f := &tenancyFixture{t: t, db: db, importer: NewImporter(db)}
	alias := uniqueName("contested")
	apply := func(namespace string) func() error {
		return func() error {
			opts := &ApplyOptions{NamespaceAllowlist: []string{namespace}}
			_, err := f.importer.ApplyConfinedBatch(context.Background(), []BatchPlan{{Def: f.def(alias, namespace, ""), Opts: opts}}, ConfinedBatchOptions{})
			return err
		}
	}
	a, b := race(apply("marketing"), apply("finance"))
	oneRefused(t, a, b, func(err error) { requireTenancyRefusal(t, err, NamespaceReasonStored) })
	var count int64
	require.NoError(t, db.Unscoped().Model(&models.Job{}).Where("alias = ?", alias).Count(&count).Error)
	require.EqualValues(t, 1, count)
}

// Two confined callers subscribing one new webhook path from two namespaces:
// exactly one commits.
func TestPostgresConfinedBatchesRaceForWebhookPath(t *testing.T) {
	db := openTenancyPostgres(t)
	holdAdvisoryLocks(t)
	f := &tenancyFixture{t: t, db: db, importer: NewImporter(db)}
	path := "/hooks/" + uniqueName("race")
	apply := func(alias, namespace string) func() error {
		return func() error {
			opts := &ApplyOptions{NamespaceAllowlist: []string{namespace}}
			_, err := f.importer.ApplyConfinedBatch(context.Background(), []BatchPlan{{Def: f.def(alias, namespace, path), Opts: opts}}, ConfinedBatchOptions{})
			return err
		}
	}
	a, b := race(apply(uniqueName("mkt"), "marketing"), apply(uniqueName("fin"), "finance"))
	oneRefused(t, a, b, func(err error) { requireTenancyRefusal(t, err, NamespaceReasonWebhookPath) })
	requireOneSubscribingNamespace(t, db, models.NormalizedTriggerPath(path))
}

// With a policy file every writer is held to rule 2, under the per-definition
// (READ COMMITTED) apply too.
func TestPostgresUnrestrictedAppliesRaceForWebhookPath(t *testing.T) {
	db := openTenancyPostgres(t)
	holder, _ := fileHolder(t, declaredPolicy)
	policy.SetProcessHolder(holder)
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	holdAdvisoryLocks(t)
	f := &tenancyFixture{t: t, db: db, importer: NewImporter(db)}
	path := "/hooks/" + uniqueName("open")
	apply := func(alias, namespace string) func() error {
		return func() error {
			_, err := f.importer.ApplyWithOptions(context.Background(), f.def(alias, namespace, path), nil)
			return err
		}
	}
	a, b := race(apply(uniqueName("mkt"), "marketing"), apply(uniqueName("fin"), "finance"))
	oneRefused(t, a, b, func(err error) { require.ErrorIs(t, err, ErrTriggerTenancy) })
	requireOneSubscribingNamespace(t, db, models.NormalizedTriggerPath(path))
}

// A trigger PATCH moving a marketing trigger onto a path, racing a finance
// apply subscribing the same path: exactly one wins.
func TestPostgresTriggerPatchRacesApplyForWebhookPath(t *testing.T) {
	db := openTenancyPostgres(t)
	f := &tenancyFixture{t: t, db: db, importer: NewImporter(db)}
	mkt := f.mustApply(uniqueName("mkt"), "marketing", "/hooks/"+uniqueName("old"))
	holdAdvisoryLocks(t)
	target := uniqueName("target")

	patch := func() error {
		scope := NewTenancyScope([]string{"marketing"}, nil)
		return WriteTx(context.Background(), db, []string{PathLockKey(target)}, func(tx *gorm.DB) error {
			if err := CheckTriggerPatchTx(tx, TriggerPatch{TriggerID: mkt.TriggerID, Path: target, PathChanges: true}, scope); err != nil {
				return err
			}
			return tx.Model(&models.Trigger{}).Where("id = ?", mkt.TriggerID).
				Updates(map[string]any{"configuration": `{"path":"/hooks/` + target + `"}`, "normalized_path": target}).Error
		})
	}
	apply := func() error {
		opts := &ApplyOptions{NamespaceAllowlist: []string{"finance"}}
		_, err := f.importer.ApplyConfinedBatch(context.Background(), []BatchPlan{{Def: f.def(uniqueName("fin"), "finance", "/hooks/"+target), Opts: opts}}, ConfinedBatchOptions{})
		return err
	}
	a, b := race(patch, apply)
	oneRefused(t, a, b, func(err error) { requireTenancyRefusal(t, err, NamespaceReasonWebhookPath) })
	requireOneSubscribingNamespace(t, db, target)
}

// Pruning one job of a shared row while another apply revives the row's
// other (retired) job: the row ends up live exactly when a live job uses it.
func TestPostgresPruneKeepsRowOfConcurrentlyRevivedJob(t *testing.T) {
	db := openTenancyPostgres(t)
	f := &tenancyFixture{t: t, db: db, importer: NewImporter(db)}
	pruned, revived := uniqueName("pruned"), uniqueName("revived")
	f.mustApply(pruned, "default", "")
	f.mustApply(revived, "default", "")
	row := f.share(pruned, revived)
	_, err := f.importer.PruneMissing(context.Background(), []string{pruned}, nil)
	require.NoError(t, err)
	require.True(t, f.job(revived).DeletedAt.Valid)

	a, b := race(func() error {
		_, err := f.importer.PruneMissing(context.Background(), []string{revived}, nil)
		return err
	}, func() error {
		_, err := f.apply(revived, "default", "", nil)
		return err
	})
	require.NoError(t, a)
	require.NoError(t, b)
	job := f.job(revived)
	if !job.DeletedAt.Valid && job.TriggerID == row {
		require.False(t, f.trigger(row).DeletedAt.Valid, "a live job's trigger row is never retired under it")
	}
}

// sharedRowWithRetiredFinanceJob is a legacy trigger row serving a live
// marketing job and a retired finance job (no policy file).
func sharedRowWithRetiredFinanceJob(t *testing.T, f *tenancyFixture) (mkt, fin string, row uuid.UUID) {
	t.Helper()
	mkt, fin = uniqueName("mkt"), uniqueName("fin")
	f.mustApply(mkt, "marketing", "")
	f.mustApply(fin, "finance", "")
	row = f.share(mkt, fin)
	_, err := f.importer.PruneMissing(context.Background(), []string{mkt}, nil)
	require.NoError(t, err)
	require.True(t, f.job(fin).DeletedAt.Valid)
	require.False(t, f.trigger(row).DeletedAt.Valid, "the marketing job keeps the row")
	return mkt, fin, row
}

// reviveDuringPeerRead makes the next tenancy check, between its first read
// of the row's jobs and its lock of the row, wait for an unrestricted apply
// that revives the finance job onto the row and commits (PR #668 review r1).
func reviveDuringPeerRead(t *testing.T, f *tenancyFixture, fin string) *bool {
	t.Helper()
	revived := false
	afterTenancyPeerRead = func() {
		if revived {
			return
		}
		revived = true
		_, err := f.importer.ApplyWithOptions(context.Background(), f.def(fin, "finance", ""), nil)
		require.NoError(t, err)
	}
	t.Cleanup(func() { afterTenancyPeerRead = nil })
	return &revived
}

// An allowlisted git-sync batch (READ COMMITTED) reads a shared row's live
// jobs; before it writes, an unrestricted apply revives a retired finance job
// onto the row and commits. The sync must see the revived job and skip the
// definition, never rewrite the live finance job's trigger.
func TestPostgresAllowlistedSyncSeesRevivalOnSharedRow(t *testing.T) {
	db := openTenancyPostgres(t)
	f := &tenancyFixture{t: t, db: db, importer: NewImporter(db)}
	mkt, fin, row := sharedRowWithRetiredFinanceJob(t, f)
	revived := reviveDuringPeerRead(t, f, fin)

	result, err := f.importer.ApplyBatchWithOptions(context.Background(), []BatchPlan{
		{Def: f.def(mkt, "marketing", ""), Opts: &ApplyOptions{NamespaceAllowlist: []string{"marketing"}}},
	})
	require.NoError(t, err)
	require.True(t, *revived)
	require.Len(t, result.Refused, 1, "the sync saw the revived finance job on the row")
	require.Equal(t, NamespaceReasonTriggerShared, result.Refused[0].Err.Reason)
	job := f.job(fin)
	require.False(t, job.DeletedAt.Valid)
	require.Equal(t, row, job.TriggerID)
	require.Equal(t, fin, f.trigger(row).Alias, "the finance job's trigger is as its own apply wrote it")
}

// The same interleaving against a confined trigger PATCH.
func TestPostgresTriggerPatchSeesRevivalOnSharedRow(t *testing.T) {
	db := openTenancyPostgres(t)
	f := &tenancyFixture{t: t, db: db, importer: NewImporter(db)}
	_, fin, row := sharedRowWithRetiredFinanceJob(t, f)
	revived := reviveDuringPeerRead(t, f, fin)

	scope := NewTenancyScope([]string{"marketing"}, nil)
	err := WriteTx(context.Background(), db, nil, func(tx *gorm.DB) error {
		if err := CheckTriggerPatchTx(tx, TriggerPatch{TriggerID: row}, scope); err != nil {
			return err
		}
		return tx.Model(&models.Trigger{}).Where("id = ?", row).Update("alias", "patched").Error
	})
	require.True(t, *revived)
	requireTenancyRefusal(t, err, NamespaceReasonTriggerShared)
	require.Equal(t, fin, f.trigger(row).Alias, "the refused PATCH left the finance job's trigger alone")
}

// A run admitted after a confined batch's REPEATABLE READ snapshot, and
// committed before its prune locks the candidates, is still seen (PR #668
// review r3): run admission writes no job row, so only the prune's running
// check against the current committed state can catch it. The batch refuses
// and rolls back; the running job is not retired.
func TestPostgresConfinedPruneSeesRunAdmittedAfterSnapshot(t *testing.T) {
	db := openTenancyPostgres(t)
	f := &tenancyFixture{t: t, db: db, importer: NewImporter(db)}
	keep, busy, incoming := uniqueName("keep"), uniqueName("busy"), uniqueName("new")
	f.mustApply(keep, "marketing", "")
	busyJob := f.mustApply(busy, "marketing", "")
	admitted := false
	beforeConfinedPrune = func() {
		if admitted {
			return
		}
		admitted = true
		// Another connection, autocommit: the run commits now, after the
		// batch's snapshot and before its prune.
		require.NoError(t, db.Create(&models.JobRun{ID: uuid.New(), JobID: busyJob.ID, Namespace: "marketing", Status: "running"}).Error)
	}
	t.Cleanup(func() { beforeConfinedPrune = nil })

	opts := &ApplyOptions{NamespaceAllowlist: []string{"marketing"}}
	_, err := f.importer.ApplyConfinedBatch(context.Background(), []BatchPlan{
		{Def: f.def(keep, "marketing", ""), Opts: opts},
		{Def: f.def(incoming, "marketing", ""), Opts: opts},
	}, ConfinedBatchOptions{Prune: true, PruneNamespaces: []string{"marketing"}})
	require.True(t, admitted)
	require.ErrorIs(t, err, ErrJobRunning)
	require.False(t, f.job(busy).DeletedAt.Valid, "the running job is not retired")
	require.False(t, f.jobExists(incoming), "nothing from the batch was applied")
}

// A run admitted for a job a confined batch updates: the admission's insert
// (uncommitted, holding its foreign-key lock on the job row) is in flight when
// the batch takes its REPEATABLE READ snapshot and waits for that row; the
// admission commits; the batch gets the row (PR #668 review r4). Its
// in-snapshot check cannot see the run, so the current-state check must: the
// batch refuses and rolls back, and the running job is not rewritten.
func TestPostgresConfinedUpdateSeesRunAdmittedDuringItsLock(t *testing.T) {
	db := openTenancyPostgres(t)
	f := &tenancyFixture{t: t, db: db, importer: NewImporter(db)}
	busy := uniqueName("busy")
	busyJob := f.mustApply(busy, "marketing", "")
	before := f.job(busy).UpdatedAt
	def := f.def(busy, "marketing", "")
	opts := &ApplyOptions{NamespaceAllowlist: []string{"marketing"}}

	admission := db.Begin()
	require.NoError(t, admission.Error)
	require.NoError(t, admission.Create(&models.JobRun{ID: uuid.New(), JobID: busyJob.ID, Namespace: "marketing", Status: "running"}).Error)
	done := make(chan error, 1)
	go func() {
		_, err := f.importer.ApplyConfinedBatch(context.Background(), []BatchPlan{{Def: def, Opts: opts}}, ConfinedBatchOptions{})
		done <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int64
		require.NoError(t, db.Raw("SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock'").Scan(&waiting).Error)
		if waiting > 0 {
			break
		}
		require.True(t, time.Now().Before(deadline), "the batch never waited for the admission's row lock")
		time.Sleep(20 * time.Millisecond)
	}
	require.NoError(t, admission.Commit().Error)

	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrJobRunning)
	case <-time.After(30 * time.Second):
		t.Fatal("the batch did not finish")
	}
	require.Equal(t, before, f.job(busy).UpdatedAt, "the running job was not rewritten")
}

// With a writer pool of one connection, which the confined batch holds for
// its whole transaction, the running check still answers at once (PR #668
// review r5): an idle job is updated and an idle candidate pruned promptly,
// and a running one is still refused.
func TestPostgresConfinedRunCheckWithWriterPoolOfOne(t *testing.T) {
	db := openTenancyPostgresPool(t, 1)
	f := &tenancyFixture{t: t, db: db, importer: NewImporter(db)}
	idle, gone, busy := uniqueName("idle"), uniqueName("gone"), uniqueName("busy")
	f.mustApply(idle, "marketing", "")
	f.mustApply(gone, "marketing", "")
	busyJob := f.mustApply(busy, "finance", "")
	require.NoError(t, db.Create(&models.JobRun{ID: uuid.New(), JobID: busyJob.ID, Namespace: "finance", Status: "running"}).Error)
	ctx := context.Background()

	start := time.Now()
	mkt := &ApplyOptions{NamespaceAllowlist: []string{"marketing"}}
	result, err := f.importer.ApplyConfinedBatch(ctx, []BatchPlan{{Def: f.def(idle, "marketing", ""), Opts: mkt}},
		ConfinedBatchOptions{Prune: true, PruneNamespaces: []string{"marketing"}})
	require.NoError(t, err)
	require.Equal(t, ConfinedBatchResult{Applied: 1, Pruned: 1}, result)
	require.Less(t, time.Since(start), 5*time.Second, "the check never waits for a writer connection")
	require.True(t, f.job(gone).DeletedAt.Valid)

	start = time.Now()
	fin := &ApplyOptions{NamespaceAllowlist: []string{"finance"}}
	_, err = f.importer.ApplyConfinedBatch(ctx, []BatchPlan{{Def: f.def(busy, "finance", ""), Opts: fin}}, ConfinedBatchOptions{})
	require.ErrorIs(t, err, ErrJobRunning)
	require.Less(t, time.Since(start), 5*time.Second)
}

// Every writer connection busy: the batch holds one and two more writers of
// the same alias hold the rest while they wait for its advisory lock. The
// batch's running check still answers at once (it reads through its own side
// pool), the batch commits, and the waiting writers then apply in turn.
func TestPostgresConfinedRunCheckWithSaturatedWriterPool(t *testing.T) {
	const poolSize = 3
	db := openTenancyPostgresPool(t, poolSize)
	f := &tenancyFixture{t: t, db: db, importer: NewImporter(db)}
	alias := uniqueName("contested")
	f.mustApply(alias, "marketing", "")
	def := f.def(alias, "marketing", "")
	opts := &ApplyOptions{NamespaceAllowlist: []string{"marketing"}}
	apply := func() error {
		_, err := f.importer.ApplyConfinedBatch(context.Background(), []BatchPlan{{Def: def, Opts: opts}}, ConfinedBatchOptions{})
		return err
	}

	monitor, err := gorm.Open(postgres.Open(db.Dialector.(*postgres.Dialector).DSN), &gorm.Config{})
	require.NoError(t, err)
	monitorSQL, err := monitor.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = monitorSQL.Close() })

	waiters := make(chan error, poolSize-1)
	var once sync.Once
	lockSessionHook = func([]string) {
		once.Do(func() {
			for range poolSize - 1 {
				go func() { waiters <- apply() }()
			}
			deadline := time.Now().Add(10 * time.Second)
			for {
				var parked int64
				require.NoError(t, monitor.Raw("SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND wait_event = 'advisory'").Scan(&parked).Error)
				if parked >= poolSize-1 {
					return
				}
				require.True(t, time.Now().Before(deadline), "the other writers never parked on the advisory lock")
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
	t.Cleanup(func() { lockSessionHook = nil })

	start := time.Now()
	require.NoError(t, apply())
	require.Less(t, time.Since(start), 10*time.Second, "the check never waits for a writer connection")
	for range poolSize - 1 {
		select {
		case err := <-waiters:
			require.NoError(t, err)
		case <-time.After(30 * time.Second):
			t.Fatal("a parked writer never applied")
		}
	}
}

// An alias a confined writer may not write is refused alike whether or not
// its stored job is running (PR #668 review r6). The batch authorises every
// stored row (exact alias scope, requested and stored namespace) before its
// committed-state running check; the other way round, an existing running job
// outside the writer's scope answered 409 naming its raw alias and an idle one
// 403: a running-state oracle. Shapes: a key scoped to "payroll" naming a
// stored "payroll" with an ASCII or Unicode space, at "*" and
// namespace-confined, alone or after its own exact alias (itself running too);
// and a marketing operator naming a finance job. The writer's own running job
// is still refused with ErrJobRunning, naming only that job.
func TestPostgresConfinedBatchAuthorizesBeforeRunCheck(t *testing.T) {
	db := openTenancyPostgres(t)
	f := &tenancyFixture{t: t, db: db, importer: NewImporter(db)}
	ctx := context.Background()
	own := f.mustApply("payroll", "default", "")
	targets := make(map[string]*models.Job, len(nonCanonicalAliases))
	for _, alias := range nonCanonicalAliases {
		def := f.def("payroll", "default", "")
		def.Metadata.Alias = alias
		job, err := f.importer.ApplyWithOptions(ctx, def, nil)
		require.NoError(t, err)
		targets[alias] = job
	}
	finance := f.mustApply("payroll-finance", "finance", "")
	setRunning := func(job *models.Job, running bool) {
		t.Helper()
		if running {
			require.NoError(t, db.Create(&models.JobRun{ID: uuid.New(), JobID: job.ID, Namespace: job.Namespace, Status: "running"}).Error)
			return
		}
		require.NoError(t, db.Model(&models.JobRun{}).Where("job_id = ? AND status = ?", job.ID, "running").Update("status", "succeeded").Error)
	}

	type probe struct {
		name   string
		plans  []BatchPlan
		target *models.Job
		scope  *TenancyScope
		reason string
	}
	var probes []probe
	for _, name := range []string{"key at *", "namespace-confined"} {
		var namespaces []string
		if name == "namespace-confined" {
			namespaces = []string{"default"}
		}
		opts := &ApplyOptions{NamespaceAllowlist: namespaces, JobAliasScope: []string{"payroll"}}
		scope := NewTenancyScope(namespaces, []string{"payroll"})
		for _, alias := range nonCanonicalAliases {
			def := f.def("payroll", "default", "")
			def.Metadata.Alias = alias
			foreign := BatchPlan{Def: def, Opts: opts}
			exact := BatchPlan{Def: f.def("payroll", "default", ""), Opts: opts}
			probes = append(probes,
				probe{fmt.Sprintf("%s %q", name, alias), []BatchPlan{foreign}, targets[alias], scope, NamespaceReasonAliasScope},
				probe{fmt.Sprintf("%s own then %q", name, alias), []BatchPlan{exact, foreign}, targets[alias], scope, NamespaceReasonAliasScope})
		}
	}
	mkt := &ApplyOptions{NamespaceAllowlist: []string{"marketing"}}
	probes = append(probes, probe{"marketing operator naming a finance job",
		[]BatchPlan{{Def: f.def("payroll-finance", "marketing", ""), Opts: mkt}}, finance,
		NewTenancyScope([]string{"marketing"}, nil), NamespaceReasonStored})

	for _, p := range probes {
		t.Run(p.name, func(t *testing.T) {
			refuse := func() error {
				t.Helper()
				_, err := f.importer.ApplyConfinedBatch(ctx, p.plans, ConfinedBatchOptions{})
				requireTenancyRefusal(t, err, p.reason)
				require.NotErrorIs(t, err, ErrJobRunning)
				return err
			}
			before := f.job(p.target.Alias).UpdatedAt
			idle := refuse()
			setRunning(p.target, true)
			setRunning(own, true)
			running := refuse()
			setRunning(p.target, false)
			setRunning(own, false)

			require.Equal(t, idle.Error(), running.Error(), "the refusal does not depend on the target's run state")
			idleDenial, _ := WriteRefusal(idle, p.scope)
			runningDenial, _ := WriteRefusal(running, p.scope)
			require.NotNil(t, runningDenial)
			require.Equal(t, idleDenial, runningDenial, "the same 403")
			require.NotContains(t, runningDenial.Message, p.target.Alias)
			require.Equal(t, before, f.job(p.target.Alias).UpdatedAt, "the target was not rewritten")
		})
	}

	setRunning(own, true)
	_, err := f.importer.ApplyConfinedBatch(ctx, []BatchPlan{{Def: f.def("payroll", "default", ""), Opts: &ApplyOptions{JobAliasScope: []string{"payroll"}}}}, ConfinedBatchOptions{})
	require.ErrorIs(t, err, ErrJobRunning)
	require.Equal(t, fmt.Sprintf("%s: payroll", ErrJobRunning), err.Error(), "a 409 names only the writer's own job")
}

func requireOneSubscribingNamespace(t *testing.T, db *gorm.DB, path string) {
	t.Helper()
	var namespaces []string
	require.NoError(t, db.Model(&models.Job{}).Distinct("jobs.namespace").
		Joins("JOIN triggers ON triggers.id = jobs.trigger_id AND triggers.deleted_at IS NULL").
		Where("triggers.normalized_path = ?", path).Pluck("jobs.namespace", &namespaces).Error)
	require.Len(t, namespaces, 1, "path %q is subscribed from %v", path, namespaces)
}
