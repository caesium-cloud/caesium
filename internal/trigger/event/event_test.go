package event

import (
	"context"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestFireWithParamsSkipAndErrorOutcomes(t *testing.T) {
	t.Parallel()

	triggerID := uuid.New()
	triggerModel := &models.Trigger{
		ID:            triggerID,
		Type:          models.TriggerTypeEvent,
		Configuration: `{"events":[{"type":"webhook.*"}]}`,
	}

	t.Run("no jobs", func(t *testing.T) {
		trig, err := New(triggerModel, WithListJobs(func(context.Context, string) (models.Jobs, error) {
			return nil, nil
		}))
		require.NoError(t, err)

		outcomes, err := trig.FireWithParams(context.Background(), map[string]string{"branch": "main"})
		require.NoError(t, err)
		require.Len(t, outcomes, 1)
		require.True(t, outcomes[0].Skipped)
		require.Equal(t, "no jobs registered for trigger", outcomes[0].SkipReason)
	})

	t.Run("nil and paused jobs", func(t *testing.T) {
		paused := &models.Job{ID: uuid.New(), TriggerID: triggerID, Paused: true}
		trig, err := New(triggerModel, WithListJobs(func(context.Context, string) (models.Jobs, error) {
			return models.Jobs{nil, paused}, nil
		}))
		require.NoError(t, err)

		outcomes, err := trig.FireWithParams(context.Background(), nil)
		require.NoError(t, err)
		require.Len(t, outcomes, 2)
		require.True(t, outcomes[0].Skipped)
		require.Equal(t, "nil job", outcomes[0].SkipReason)
		require.True(t, outcomes[1].Skipped)
		require.Equal(t, paused.ID, outcomes[1].JobID)
		require.Equal(t, "job paused", outcomes[1].SkipReason)
	})

	t.Run("run start error", func(t *testing.T) {
		db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
		require.NoError(t, err)
		sqlDB, err := db.DB()
		require.NoError(t, err)
		sqlDB.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = sqlDB.Close() })

		job := &models.Job{ID: uuid.New(), TriggerID: triggerID}
		trig, err := New(triggerModel,
			WithListJobs(func(context.Context, string) (models.Jobs, error) {
				return models.Jobs{job}, nil
			}),
			WithRunStoreFactory(func() *runstorage.Store {
				return runstorage.NewStore(db)
			}),
		)
		require.NoError(t, err)

		outcomes, err := trig.FireWithParams(context.Background(), nil)
		require.NoError(t, err)
		require.Len(t, outcomes, 1)
		require.Equal(t, job.ID, outcomes[0].JobID)
		require.NotEmpty(t, outcomes[0].Error)
		require.Equal(t, uuid.Nil, outcomes[0].RunID)
	})
}

func TestExtractParamsJSONPath(t *testing.T) {
	t.Parallel()

	data := datatypes.JSON(`{
		"ref":"refs/heads/main",
		"repository":{"full_name":"caesium-cloud/caesium"},
		"list":[{"name":"first"},{"name":"second"}],
		"delivery":{"attempt":2}
	}`)
	params := extractParams(data, map[string]string{
		"ref":         "$.ref",
		"repo":        "$.repository.full_name",
		"first":       "$.list[0].name",
		"attempt":     "$.delivery.attempt",
		"whole":       "$",
		"missing":     "$.repository.missing",
		"bad_bracket": "$.list[abc]",
	})

	require.Equal(t, "refs/heads/main", params["ref"])
	require.Equal(t, "caesium-cloud/caesium", params["repo"])
	require.Equal(t, "first", params["first"])
	require.Equal(t, "2", params["attempt"])
	require.JSONEq(t, string(data), params["whole"])
	require.NotContains(t, params, "missing")
	require.NotContains(t, params, "bad_bracket")
}
