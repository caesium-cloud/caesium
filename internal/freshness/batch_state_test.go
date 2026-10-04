package freshness

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func batchTestStore(t *testing.T) (*Store, *gorm.DB) {
	t.Helper()
	conn := openRegistryDB(t)
	sqlDB, err := conn.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	return NewStore(conn), conn
}

func TestGetManyPreservesNamespacePresenceAndFreshAt(t *testing.T) {
	store, conn := batchTestStore(t)
	states := []models.DatasetState{
		{ID: uuid.New(), Namespace: "", Name: "orders", Watermark: "", VerifiedAt: &t0},
		{ID: uuid.New(), Namespace: "team", Name: "orders", Watermark: "12", AdvancedAt: &t0},
	}
	require.NoError(t, conn.Create(&states).Error)
	reads := 0
	require.NoError(t, conn.Callback().Query().After("gorm:query").Register("test:batch-presence", func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*[]models.DatasetState); ok {
			reads++
		}
	}))
	got, err := store.getMany(context.Background(), []datasetIdentity{{"", " orders "}, {"", "orders"}, {" team ", "orders"}, {"", "missing"}, {"", " "}})
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, 2, reads)
	empty, exists := got[datasetIdentity{"", "orders"}]
	require.True(t, exists)
	require.Empty(t, empty.Watermark)
	at, seen := FreshAt(empty)
	require.True(t, seen)
	require.Equal(t, t0, at)
	require.Equal(t, "12", got[datasetIdentity{"team", "orders"}].Watermark)
	_, exists = got[datasetIdentity{"", "missing"}]
	require.False(t, exists)
}

func TestGetManyBoundsChunksAndDiscardsPartialReads(t *testing.T) {
	for _, failSecond := range []bool{false, true} {
		t.Run(fmt.Sprint(failSecond), func(t *testing.T) {
			store, conn := batchTestStore(t)
			ids := make([]datasetIdentity, 401)
			states := make([]models.DatasetState, len(ids))
			for i := range ids {
				ids[i] = datasetIdentity{"", fmt.Sprintf("input-%03d", i)}
				states[i] = models.DatasetState{ID: uuid.New(), Name: ids[i].name, Watermark: fmt.Sprint(i)}
			}
			require.NoError(t, conn.CreateInBatches(&states, 100).Error)
			fault := errors.New("second input chunk unavailable")
			reads := 0
			require.NoError(t, conn.Callback().Query().Before("gorm:query").Register("test:chunk-fault", func(tx *gorm.DB) {
				if _, ok := tx.Statement.Dest.(*[]models.DatasetState); !ok {
					return
				}
				reads++
				if failSecond && reads == 2 {
					tx.AddError(fault)
				}
			}))
			require.NoError(t, conn.Callback().Query().After("gorm:query").Register("test:chunk-limit", func(tx *gorm.DB) {
				if _, ok := tx.Statement.Dest.(*[]models.DatasetState); ok && tx.Error == nil {
					require.LessOrEqual(t, len(tx.Statement.Vars), 401, "namespace plus at most 400 names")
				}
			}))
			got, err := store.getMany(context.Background(), append(ids, ids[0]))
			require.Equal(t, 2, reads)
			if failSecond {
				require.ErrorIs(t, err, fault)
				require.Nil(t, got)
			} else {
				require.NoError(t, err)
				require.Len(t, got, 401)
				require.Equal(t, "400", got[ids[400]].Watermark)
			}
		})
	}
}

func TestConsumedSnapshotNilAndPresentEmptyWatermark(t *testing.T) {
	_, conn := batchTestStore(t)
	for _, names := range [][]string{nil, {"missing"}, {" "}} {
		got, err := consumedSnapshot(context.Background(), conn, nil, names)
		require.NoError(t, err)
		require.Nil(t, got)
	}
	require.NoError(t, conn.Create(&models.DatasetState{ID: uuid.New(), Name: "orders", Watermark: "", VerifiedAt: &t0}).Error)
	got, err := consumedSnapshot(context.Background(), conn, nil, []string{"orders", " orders ", "missing"})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"orders": ""}, got)
}

func TestBatchedReadinessKeepsObservationAndFatalErrors(t *testing.T) {
	store, conn := batchTestStore(t)
	require.NoError(t, conn.Create(&models.DatasetState{ID: uuid.New(), Name: "observed", VerifiedAt: &t0}).Error)
	e := &Evaluator{store: store}
	declarations := []models.DatasetDeclaration{{Name: "observed"}, {Name: " observed "}}
	ready, current, _, err := e.upstreamReady(context.Background(), models.DatasetState{}, declarations)
	require.NoError(t, err)
	require.True(t, ready)
	require.Equal(t, map[string]string{"observed": ""}, current)
	ready, _, _, err = e.upstreamReady(context.Background(), models.DatasetState{}, []models.DatasetDeclaration{{Name: "missing"}})
	require.NoError(t, err)
	require.False(t, ready)
	fault := errors.New("consumed state unavailable")
	require.NoError(t, conn.Callback().Query().Before("gorm:query").Register("test:readiness-fault", func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*[]models.DatasetState); ok {
			tx.AddError(fault)
		}
	}))
	ready, current, _, err = e.upstreamReady(context.Background(), models.DatasetState{}, declarations)
	require.ErrorIs(t, err, fault)
	require.False(t, ready)
	require.Nil(t, current)
	blocked, current, _, err := e.consumedWatermarksBlockSkip(context.Background(), models.DatasetState{}, declarations)
	require.ErrorIs(t, err, fault)
	require.False(t, blocked)
	require.Nil(t, current)
	snapshot, err := consumedSnapshot(context.Background(), conn, nil, []string{"observed"})
	require.ErrorIs(t, err, fault)
	require.Nil(t, snapshot)
}
