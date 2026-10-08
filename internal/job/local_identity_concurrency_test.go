package job

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/caesium-cloud/caesium/internal/cache"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func TestLocalParallelRootsPublishCompletePredecessorIdentity(t *testing.T) {
	const roots = 24
	edges := make([][2]int, roots)
	for i := range roots {
		edges[i] = [2]int{i, roots}
	}
	j, store, engine, tasks := characterizationJob(t, roots+1, edges)
	j.envVariables = func() env.Environment { return env.Environment{ExecutionMode: executionModeLocal, MaxParallelTasks: 8} }
	for i, task := range tasks {
		task.Name = fmt.Sprintf("step-%02d", i)
		task.CacheConfig = datatypes.JSON("true")
		require.NoError(t, store.DB().Model(&models.Task{}).Where("id = ?", task.ID).Updates(map[string]any{"name": task.Name, "cache_config": task.CacheConfig}).Error)
		engine.logsByName[task.ID.String()] = fmt.Sprintf("##caesium::output {\"key\":%q}\n", task.Name)
	}
	require.NoError(t, j.Run(t.Context()))
	snapshot := latestRunSnapshot(t, store, j.id)
	var consumer models.TaskRun
	require.NoError(t, store.DB().Where("job_run_id = ? AND task_id = ?", snapshot.ID, tasks[roots].ID).First(&consumer).Error)
	var blob cache.HashInputBlob
	require.NoError(t, json.Unmarshal(consumer.HashInputBlob, &blob))
	expectedHashes := make([]string, roots)
	expectedOutputs := make(map[string]map[string]string, roots)
	for i, task := range tasks[:roots] {
		var row models.TaskRun
		require.NoError(t, store.DB().Where("job_run_id = ? AND task_id = ?", snapshot.ID, task.ID).First(&row).Error)
		require.NotEmpty(t, row.Hash)
		expectedHashes[i] = row.Hash
		expectedOutputs[task.Name] = map[string]string{"key": task.Name}
	}
	require.ElementsMatch(t, expectedHashes, blob.PredecessorHashes)
	require.Equal(t, expectedOutputs, blob.PredecessorOutputs)
}

func TestLocalIdentityPublicationCopiesOutputMaps(t *testing.T) {
	id := uuid.New()
	local := &localRun{taskOutputs: make(map[uuid.UUID]map[string]string), taskHashes: make(map[uuid.UUID]string)}
	output := map[string]string{"key": "original"}
	local.setTaskOutput(id, output)
	output["key"] = "mutated source"
	read, ok := local.taskOutput(id)
	require.True(t, ok)
	require.Equal(t, "original", read["key"])
	read["key"] = "mutated reader"
	again, ok := local.taskOutput(id)
	require.True(t, ok)
	require.Equal(t, "original", again["key"])
}
