package run

import (
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func TestPredecessorProjectionDistinctCacheAndDescriptorFilters(t *testing.T) {
	f := newFanOutFixture(t, nil)
	rows := []models.TaskRun{
		{ID: uuid.New(), TaskID: f.producer.ID, Hash: "", EffectiveHash: "effective-only", Status: string(TaskStatusSucceeded), PartitionValue: "a", PartitionIndex: 0, PartitionCount: 4, Output: datatypes.JSON(`{"v":"one"}`)},
		{ID: uuid.New(), TaskID: f.producer.ID, Hash: "new", EffectiveHash: "old", Status: string(TaskStatusCached), PartitionValue: "b", PartitionIndex: 1, PartitionCount: 4},
		{ID: uuid.New(), TaskID: f.producer.ID, Hash: "failed", Status: string(TaskStatusFailed), PartitionValue: "c", PartitionIndex: 2, PartitionCount: 4, Output: datatypes.JSON(`{"unused":9}`)},
		{ID: uuid.New(), TaskID: f.producer.ID, Hash: "skipped", Status: string(TaskStatusSkipped), PartitionValue: "d", PartitionIndex: 3, PartitionCount: 4, Output: datatypes.JSON(`{"unused":9}`)},
	}
	require.NoError(t, f.db.Where("job_run_id = ? AND task_id = ?", f.runID, f.producer.ID).Delete(&models.TaskRun{}).Error)
	for i := range rows {
		rows[i].JobRunID = f.runID
		rows[i].AtomID = f.producer.AtomID
		require.NoError(t, f.db.Create(&rows[i]).Error)
	}
	outputs, err := f.store.PredecessorOutputs(f.runID, f.consumer.ID)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"v": `{"a":"one"}`, "PARTITION_COUNT": "1", "SUCCEEDED": "2", "FAILED": "1"}, outputs["discover"])
	hashes, err := f.store.PredecessorHashes(f.runID, f.consumer.ID)
	require.NoError(t, err)
	require.Equal(t, []string{GroupIdentityHash([]string{"old"})}, hashes)
	descriptorOutputs, descriptorHashes, err := f.store.PredecessorDescriptorInputs(f.runID, f.consumer.ID)
	require.NoError(t, err)
	require.Equal(t, outputs["discover"], descriptorOutputs[f.producer.ID])
	require.Equal(t, GroupIdentityHash([]string{"effective-only", "old"}), descriptorHashes[f.producer.ID])
}

func TestPredecessorAbsentOutputsAndNoEdgesAreValid(t *testing.T) {
	f := newFanOutFixture(t, nil)
	output, err := f.store.PredecessorOutputs(f.runID, f.consumer.ID)
	require.NoError(t, err)
	require.Nil(t, output)
	output, err = f.store.PredecessorOutputs(f.runID, f.producer.ID)
	require.NoError(t, err)
	require.Nil(t, output)
	hashes, err := f.store.PredecessorHashes(f.runID, f.producer.ID)
	require.NoError(t, err)
	require.Nil(t, hashes)
	descriptorOutputs, descriptorHashes, err := f.store.PredecessorDescriptorInputs(f.runID, f.producer.ID)
	require.NoError(t, err)
	require.Nil(t, descriptorOutputs)
	require.Nil(t, descriptorHashes)
}
