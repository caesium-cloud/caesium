package run

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestLocalTaskTimeoutLoadsExactInstanceAndPreservesDefaultCompatibility(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, catalogID := uuid.New(), uuid.New()
	rows := make([]models.TaskRun, 2)
	for i, timeout := range []time.Duration{0, 2 * time.Second} {
		descriptor, err := json.Marshal(models.TaskExecutionDescriptor{
			SchemaVersion: models.TaskExecutionDescriptorSchemaVersion,
			Timing:        models.TaskExecutionTiming{TaskTimeout: timeout},
		})
		require.NoError(t, err)
		rows[i] = models.TaskRun{ID: uuid.New(), JobRunID: runID, TaskID: catalogID, PartitionIndex: i, PartitionCount: 2, ExecutionDescriptor: descriptor}
	}
	require.NoError(t, db.Create(&rows).Error)
	for i, row := range rows {
		got, err := store.LocalTaskExecutionTimeout(t.Context(), runID, row.ID)
		require.NoError(t, err)
		require.Equal(t, time.Duration(i)*2*time.Second, got, "sibling timing must not come from the collapsed group head")
	}
	_, err := store.LocalTaskExecutionTimeout(t.Context(), runID, catalogID)
	require.ErrorIs(t, err, ErrAmbiguousTaskRun)
	for _, descriptor := range [][]byte{nil, []byte(`{invalid`), []byte(`{"schemaVersion":999,"timing":{"taskTimeout":5}}`), []byte(`{"schemaVersion":1,"timing":{"taskTimeout":5},"containerSpec":{"env":{"invalid":5}}}`)} {
		require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", rows[0].ID).Update("execution_descriptor", descriptor).Error)
		got, err := store.LocalTaskExecutionTimeout(t.Context(), runID, rows[0].ID)
		require.NoError(t, err)
		require.Zero(t, got, "retain the existing local zero/default fallback")
	}
}

func TestConvertRunTaskModelDoesNotDecodeExecutionDescriptor(t *testing.T) {
	row := &models.TaskRun{ID: uuid.New()}
	var converted *TaskRun
	baseline := testing.AllocsPerRun(100, func() { converted = convertRunTaskModel(row) })
	var descriptor models.TaskExecutionDescriptor
	descriptor.SchemaVersion = models.TaskExecutionDescriptorSchemaVersion
	descriptor.Timing.TaskTimeout = time.Second
	descriptor.ContainerSpec.Env = make(map[string]string, 100)
	for i := 0; i < 100; i++ {
		descriptor.ContainerSpec.Env[uuid.NewString()] = "normal environment value"
	}
	encoded, err := json.Marshal(descriptor)
	require.NoError(t, err)
	row.ExecutionDescriptor = encoded
	withDescriptor := testing.AllocsPerRun(100, func() { converted = convertRunTaskModel(row) })
	require.Equal(t, baseline, withDescriptor, "run-detail/event conversion must not allocate a decoded runtime recipe")
	require.Equal(t, row.ID, converted.ID)
}
