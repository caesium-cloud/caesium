package freshness

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// The `held` write's hold check must be a LOCKED read on PostgreSQL: a plain
// COUNT takes no row lock under READ COMMITTED, so a release could commit
// between the check and the upsert. A dry run on the postgres dialector builds
// the SQL without a server (this asserts the statement, it does not run
// PostgreSQL), and the SQLite dialector - the dqlite path - drops the clause.
func TestHeldWriteHoldCheckTakesARowLockOnPostgres(t *testing.T) {
	pg, err := gorm.Open(postgres.New(postgres.Config{DSN: "host=127.0.0.1 port=1 user=caesium dbname=caesium sslmode=disable"}),
		&gorm.Config{DryRun: true, DisableAutomaticPing: true})
	require.NoError(t, err)
	var statements []string
	require.NoError(t, pg.Callback().Query().After("gorm:query").Register("lock:capture", func(tx *gorm.DB) {
		statements = append(statements, tx.Statement.SQL.String())
	}))
	_, err = lockActiveHold(pg, "", "warehouse/orders")
	require.NoError(t, err)
	require.Len(t, statements, 1)
	require.True(t, strings.HasSuffix(statements[0], " FOR UPDATE"), statements[0])
	require.Contains(t, statements[0], "dataset_holds")
	require.Contains(t, statements[0], "namespace")
	require.Contains(t, statements[0], "status")
	require.Contains(t, statements[0], "LIMIT")

	// SQLite / dqlite: same query, no locking clause, still correct.
	lite := openRegistryDB(t)
	setDataAssertions(t, true)
	seedActiveHold(t, lite, "", "present")
	active, err := lockActiveHold(lite, "", "present")
	require.NoError(t, err)
	require.True(t, active)
	active, err = lockActiveHold(lite, "", "absent")
	require.NoError(t, err)
	require.False(t, active)
	active, err = lockActiveHold(lite, "other", "present")
	require.NoError(t, err)
	require.False(t, active, "the identity is the exact (namespace, name)")
}

// On a real PostgreSQL (opt-in: CAESIUM_TEST_POSTGRES_DSN, like the watermark
// race tests; NOT exercised by the default lanes): a release attempted between
// the in-transaction hold check and the `held` upsert must wait for the row
// lock, then reset the row, so `held` never survives the release.
func TestPostgresReleaseBetweenHoldCheckAndHeldWriteDoesNotStrandHeld(t *testing.T) {
	db, monitor := openStatePostgres(t)
	require.NoError(t, db.AutoMigrate(&models.DatasetHold{}))
	setDataAssertions(t, true)
	ctx := context.Background()

	now := time.Now().UTC()
	key := models.DatasetHoldKey("", "out")
	hold := models.DatasetHold{ID: uuid.New(), Name: "out", Status: models.DatasetHoldStatusActive, ActiveKey: &key, Reason: "min",
		HeldByJobID: uuid.New(), OccurrenceCount: 1, OpenedAt: now, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(&hold).Error)

	var fired atomic.Bool
	released := make(chan error, 1)
	afterHeldHoldLocked = func() {
		if !fired.CompareAndSwap(false, true) {
			return
		}
		go func() {
			_, err := runstorage.NewStore(db).ReleaseHold(ctx, hold.ID, runstorage.ReleaseHoldParams{ReleasedBy: "op"})
			released <- err
		}()
		// Wait until the release is parked on the hold row lock (the fix) or has
		// already finished (the race: it did not wait).
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			var parked int64
			require.NoError(t, monitor.Raw("SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock'").Scan(&parked).Error)
			if parked > 0 {
				return
			}
			select {
			case err := <-released:
				released <- err
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
		t.Error("the release neither finished nor waited for the hold row lock")
	}
	t.Cleanup(func() { afterHeldHoldLocked = nil })

	jobID := uuid.New()
	decl := produceDecl(jobID, "out", "1h", "")
	// A registry-snapshotted declaration, so the write does not read `jobs`.
	decl.Job = models.Job{ID: jobID, Namespace: "default"}
	e := &Evaluator{db: db, now: func() time.Time { return now }}
	require.NoError(t, e.updateStatusTx(ctx, db, decl, models.DatasetStatusHeld, "dataset_hold:/out"))
	require.True(t, fired.Load(), "precondition: the held write found the active hold")
	select {
	case err := <-released:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("the release never finished")
	}

	var state models.DatasetState
	require.NoError(t, db.Where("namespace = ? AND name = ?", "", "out").Take(&state).Error)
	require.NotEqual(t, models.DatasetStatusHeld, state.Status, "held survived the release")
}
