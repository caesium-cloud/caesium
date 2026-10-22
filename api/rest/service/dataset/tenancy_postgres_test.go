package dataset

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/internal/models"
	runstore "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// The PostgreSQL race of a confined dataset write (identity-and-access B6),
// against a real server: opt-in with CAESIUM_TEST_POSTGRES_DSN, each test in a
// fresh schema. A concurrent apply that moves the producer holds the alias
// advisory lock every job write takes; the confined write must wait for it
// and then see the move, not commit against the namespace it read first.

func openDatasetPostgres(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("CAESIUM_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("CAESIUM_TEST_POSTGRES_DSN is not set; PostgreSQL dataset tenancy race tests are opt-in")
	}
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	adminSQL, err := admin.DB()
	require.NoError(t, err)
	schemaName := "caesium_b6_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	require.NoError(t, admin.Exec("CREATE SCHEMA "+schemaName).Error)
	scopedDSN := dsn + " search_path=" + schemaName
	if strings.Contains(dsn, "://") {
		parsed, err := url.Parse(dsn)
		require.NoError(t, err)
		query := parsed.Query()
		query.Set("search_path", schemaName)
		parsed.RawQuery = query.Encode()
		scopedDSN = parsed.String()
	}
	db, err := gorm.Open(postgres.Open(scopedDSN), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(8)
	t.Cleanup(func() {
		require.NoError(t, sqlDB.Close())
		require.NoError(t, admin.Exec("DROP SCHEMA "+schemaName+" CASCADE").Error)
		require.NoError(t, adminSQL.Close())
	})
	var all []any
	for _, model := range models.All {
		switch model.(type) {
		case *models.InternalCAGeneration, *models.InternalNodeEnrollment, *models.RunCheckpoint:
			continue
		}
		all = append(all, model)
	}
	require.NoError(t, db.AutoMigrate(all...))
	return db
}

// holdAliasLock takes the advisory lock a job write on alias takes, on its own
// connection, until the returned release func runs (after applying move).
func holdAliasLock(t *testing.T, db *gorm.DB, alias string) (release func(move func())) {
	t.Helper()
	sqlDB, err := db.DB()
	require.NoError(t, err)
	conn, err := sqlDB.Conn(context.Background())
	require.NoError(t, err)
	_, err = conn.ExecContext(context.Background(), "SELECT pg_advisory_lock(hashtextextended($1, 0))", jobdef.AliasLockKey(alias))
	require.NoError(t, err)
	return func(move func()) {
		move()
		_, err := conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock_all()")
		require.NoError(t, err)
		require.NoError(t, conn.Close())
	}
}

func TestPostgresConfinedDatasetWritesSerialiseWithAProducerMove(t *testing.T) {
	db := openDatasetPostgres(t)
	prev := storeFor
	storeFor = runstore.NewStore
	t.Cleanup(func() { storeFor = prev })

	trigger := uuid.New()
	require.NoError(t, db.Create(&models.Trigger{ID: trigger, Type: models.TriggerTypeCron}).Error)
	jobID := uuid.New()
	require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: "pg-producer", Namespace: "marketing", TriggerID: trigger}).Error)
	now := time.Now().UTC()
	for _, name := range []string{"orders", "ledger"} {
		require.NoError(t, db.Create(&models.DatasetDeclaration{ID: uuid.New(), JobID: jobID, JobAlias: "pg-producer", Name: name,
			Direction: models.DatasetDirectionProduces, CreatedAt: now, UpdatedAt: now}).Error)
	}
	key := "/orders"
	holdID := uuid.New()
	require.NoError(t, db.Create(&models.DatasetHold{ID: holdID, Name: "orders", Status: models.DatasetHoldStatusActive, ActiveKey: &key,
		HeldByJobID: jobID, OpenedAt: now, CreatedAt: now, UpdatedAt: now}).Error)
	svc := NewWithDatabase(context.Background(), db)
	move := func(namespace string) func() {
		return func() {
			require.NoError(t, db.Model(&models.Job{}).Where("id = ?", jobID).Update("namespace", namespace).Error)
		}
	}

	type outcome struct{ err error }
	waitBlocked := func(done <-chan outcome) {
		t.Helper()
		select {
		case got := <-done:
			t.Fatalf("the confined write finished while a job write held its producer's lock: %v", got.err)
		case <-time.After(400 * time.Millisecond):
		}
	}

	// Release: the operator is confined to marketing; the producer moves to
	// finance while the release waits on the alias lock.
	operator := confinedPrincipal(policy.Grants{"marketing": models.RoleOperator})
	release := holdAliasLock(t, db, "pg-producer")
	done := make(chan outcome, 1)
	go func() {
		_, err := svc.ReleaseHold(ReleaseHoldParams{HoldID: holdID, Principal: "op", Reason: "ack", Caller: operator})
		done <- outcome{err}
	}()
	waitBlocked(done)
	release(move("finance"))
	got := <-done
	var denial *authz.Denial
	require.ErrorAs(t, got.err, &denial, "the release must see the move it waited for")
	var hold models.DatasetHold
	require.NoError(t, db.Take(&hold, "id = ?", holdID).Error)
	require.Equal(t, models.DatasetHoldStatusActive, hold.Status)

	// Advance: same shape, the runner confined to finance; the producer moves
	// back to marketing while the advance waits.
	runner := confinedPrincipal(policy.Grants{"finance": models.RoleRunner})
	release = holdAliasLock(t, db, "pg-producer")
	done = make(chan outcome, 1)
	go func() {
		_, err := svc.Advance(AdvanceParams{Name: "ledger", Watermark: "7", Principal: runner})
		done <- outcome{err}
	}()
	waitBlocked(done)
	release(move("marketing"))
	got = <-done
	require.ErrorAs(t, got.err, &denial)
	var states int64
	require.NoError(t, db.Model(&models.DatasetState{}).Where("name = ?", "ledger").Count(&states).Error)
	require.Zero(t, states, "a refused advance writes nothing")

	// A delete takes no alias lock: it holds the producer's job row in an
	// open transaction. The confined write's FOR SHARE re-read waits for it
	// and then sees the producer gone, instead of reading the committed
	// version and advancing a dataset nobody it may write owns.
	sqlDB, err := db.DB()
	require.NoError(t, err)
	deleter, err := sqlDB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	_, err = deleter.ExecContext(context.Background(), "UPDATE jobs SET deleted_at = now() WHERE id = $1", jobID)
	require.NoError(t, err)
	done = make(chan outcome, 1)
	go func() {
		_, err := svc.Advance(AdvanceParams{Name: "ledger", Watermark: "9", Principal: confinedPrincipal(policy.Grants{"marketing": models.RoleRunner})})
		done <- outcome{err}
	}()
	waitBlocked(done)
	require.NoError(t, deleter.Commit())
	got = <-done
	require.ErrorAs(t, got.err, &denial, "the producer was deleted under the write")
	require.NoError(t, db.Exec("UPDATE jobs SET deleted_at = NULL WHERE id = ?", jobID).Error)

	// Without a concurrent writer the confined writes go through.
	_, err = svc.Advance(AdvanceParams{Name: "ledger", Watermark: "8", Principal: confinedPrincipal(policy.Grants{"marketing": models.RoleRunner})})
	require.NoError(t, err)
	_, err = svc.ReleaseHold(ReleaseHoldParams{HoldID: holdID, Principal: "op", Reason: "ack", Caller: operator})
	require.NoError(t, err)
}
