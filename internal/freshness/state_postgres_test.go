package freshness

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// openStatePostgres opens a fresh schema on the PostgreSQL server named by
// CAESIUM_TEST_POSTGRES_DSN (the repo's opt-in convention) holding only the
// dataset_states table.
func openStatePostgres(t *testing.T) (db, monitor *gorm.DB) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("CAESIUM_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("CAESIUM_TEST_POSTGRES_DSN is not set; PostgreSQL watermark race tests are opt-in")
	}
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	adminSQL, err := admin.DB()
	require.NoError(t, err)
	schemaName := "caesium_freshness_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	db, err = gorm.Open(postgres.Open(scoped), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(4)
	t.Cleanup(func() {
		require.NoError(t, sqlDB.Close())
		require.NoError(t, admin.Exec("DROP SCHEMA "+schemaName+" CASCADE").Error)
		require.NoError(t, adminSQL.Close())
	})
	require.NoError(t, db.AutoMigrate(&models.DatasetState{}))
	return db, admin
}

// TestPostgresConcurrentAdvancesNeverRegressTheWatermark: on PostgreSQL the
// conflicting INSERT ... ON CONFLICT DO NOTHING locks nothing, so two advances
// of one dataset both re-read the same watermark and, without a row lock on
// that re-read, the one that commits last wins even when its value is smaller.
// The hook starts a larger advance while the smaller one sits between its
// re-read and its write, and lets the smaller one continue once the larger one
// has either finished (no lock: the race) or parked on the row lock.
func TestPostgresConcurrentAdvancesNeverRegressTheWatermark(t *testing.T) {
	db, monitor := openStatePostgres(t)
	ctx := context.Background()
	store := NewStore(db)
	_, err := store.Advance(ctx, AdvanceInput{Name: "staging.orders", Watermark: "1", RunID: uuid.New(), CompletedAt: t0})
	require.NoError(t, err)

	var fired atomic.Bool
	larger := make(chan error, 1)
	prev := afterAdvanceReread
	afterAdvanceReread = func(*gorm.DB) {
		if !fired.CompareAndSwap(false, true) {
			return
		}
		go func() {
			res, err := store.Advance(ctx, AdvanceInput{Name: "staging.orders", Watermark: "3", RunID: uuid.New(), CompletedAt: t0.Add(2 * time.Minute)})
			if err == nil && res.Outcome != OutcomeAdvanced {
				err = fmt.Errorf("larger advance: outcome %s", res.Outcome)
			}
			larger <- err
		}()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			var parked int64
			require.NoError(t, monitor.Raw("SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock'").Scan(&parked).Error)
			if parked > 0 {
				return
			}
			select {
			case err := <-larger:
				larger <- err
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
		t.Error("the larger advance neither finished nor waited for the row lock")
	}
	t.Cleanup(func() { afterAdvanceReread = prev })

	_, err = store.Advance(ctx, AdvanceInput{Name: "staging.orders", Watermark: "2", RunID: uuid.New(), CompletedAt: t0.Add(time.Minute)})
	require.NoError(t, err)
	require.True(t, fired.Load(), "precondition: the advance took the conflict path")
	select {
	case err := <-larger:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("the larger advance never finished")
	}

	var row models.DatasetState
	require.NoError(t, db.Where("name = ?", "staging.orders").Take(&row).Error)
	require.Equal(t, "3", row.Watermark, "the smaller advance overwrote the larger one")
}
