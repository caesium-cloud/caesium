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
	sqlDB.SetMaxOpenConns(8)
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

func requireOneSubscribingNamespace(t *testing.T, db *gorm.DB, path string) {
	t.Helper()
	var namespaces []string
	require.NoError(t, db.Model(&models.Job{}).Distinct("jobs.namespace").
		Joins("JOIN triggers ON triggers.id = jobs.trigger_id AND triggers.deleted_at IS NULL").
		Where("triggers.normalized_path = ?", path).Pluck("jobs.namespace", &namespaces).Error)
	require.Len(t, namespaces, 1, "path %q is subscribed from %v", path, namespaces)
}
