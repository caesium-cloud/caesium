package run

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

func TestPredecessorExecutionInputsOneReadMatchesLegacyProjections(t *testing.T) {
	f := newFanOutFixture(t, nil)
	row := f.producerRow(t)
	require.NoError(t, f.db.Model(row).Updates(map[string]any{"output": datatypes.JSON(`{"value":"one"}`), "status": string(TaskStatusSucceeded), "hash": "new", "effective_hash": "old"}).Error)
	outputs, err := f.store.PredecessorOutputs(f.runID, f.consumer.ID)
	require.NoError(t, err)
	hashes, err := f.store.PredecessorHashes(f.runID, f.consumer.ID)
	require.NoError(t, err)
	descriptorOutputs, descriptorHashes, err := f.store.PredecessorDescriptorInputs(f.runID, f.consumer.ID)
	require.NoError(t, err)
	reads := 0
	require.NoError(t, f.db.Callback().Query().Before("gorm:query").Register("test:count_predecessor_rows", func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*[]models.TaskRun); ok {
			reads++
		}
	}))
	t.Cleanup(func() { _ = f.db.Callback().Query().Remove("test:count_predecessor_rows") })
	got, err := f.store.PredecessorExecutionInputs(context.Background(), f.runID, f.consumer.ID)
	require.NoError(t, err)
	require.Equal(t, 1, reads)
	require.Equal(t, outputs, got.OutputsByName)
	require.Equal(t, hashes, got.Hashes)
	require.Equal(t, descriptorOutputs, got.DescriptorOutputs)
	require.Equal(t, descriptorHashes, got.DescriptorHashes)
}

func TestPredecessorExecutionInputsDistinctFanInHashFilters(t *testing.T) {
	f := newFanOutFixture(t, nil)
	row := f.producerRow(t)
	require.NoError(t, f.db.Model(row).Updates(map[string]any{"output": datatypes.JSON(`{"v":"one"}`), "status": string(TaskStatusSucceeded), "effective_hash": "effective-only", "partition_count": 2, "partition_value": "a"}).Error)
	sibling := &models.TaskRun{ID: uuid.New(), JobRunID: f.runID, TaskID: f.producer.ID, AtomID: f.producer.AtomID, Status: string(TaskStatusCached), Hash: "new", EffectiveHash: "old", PartitionValue: "b", PartitionCount: 2, PartitionIndex: 1}
	require.NoError(t, f.db.Create(sibling).Error)
	got, err := f.store.PredecessorExecutionInputs(context.Background(), f.runID, f.consumer.ID)
	require.NoError(t, err)
	require.Equal(t, []string{GroupIdentityHash([]string{"old"})}, got.Hashes)
	require.Equal(t, GroupIdentityHash([]string{"effective-only", "old"}), got.DescriptorHashes[f.producer.ID])
	require.Equal(t, got.OutputsByName["discover"], got.DescriptorOutputs[f.producer.ID])
	require.Equal(t, "2", got.OutputsByName["discover"]["SUCCEEDED"])
	require.Equal(t, "1", got.OutputsByName["discover"]["PARTITION_COUNT"])
}

func TestPredecessorExecutionInputsNoEdgesAndAbsentOutput(t *testing.T) {
	f := newFanOutFixture(t, nil)
	got, err := f.store.PredecessorExecutionInputs(context.Background(), f.runID, f.producer.ID)
	require.NoError(t, err)
	require.Equal(t, PredecessorInputs{}, got)
	got, err = f.store.PredecessorExecutionInputs(context.Background(), f.runID, f.consumer.ID)
	require.NoError(t, err)
	require.Nil(t, got.OutputsByName)
	require.Empty(t, got.Hashes)
	require.Empty(t, got.DescriptorOutputs)
}

func TestPredecessorExecutionInputsQueryFailuresReturnZero(t *testing.T) {
	for _, stage := range []string{"refs", "names", "rows"} {
		t.Run(stage, func(t *testing.T) {
			f := newFanOutFixture(t, nil)
			injected := errors.New("injected predecessor query failure")
			require.NoError(t, f.db.Callback().Query().Before("gorm:query").Register("test:predecessor_fault", func(tx *gorm.DB) {
				fail := false
				switch stage {
				case "refs":
					_, fail = tx.Statement.Dest.(*[]models.TaskEdge)
				case "names":
					_, fail = tx.Statement.Dest.(*[]models.Task)
				case "rows":
					_, fail = tx.Statement.Dest.(*[]models.TaskRun)
				}
				if fail {
					tx.AddError(injected)
				}
			}))
			t.Cleanup(func() { _ = f.db.Callback().Query().Remove("test:predecessor_fault") })
			got, err := f.store.PredecessorExecutionInputs(context.Background(), f.runID, f.consumer.ID)
			require.ErrorIs(t, err, injected)
			require.Equal(t, PredecessorInputs{}, got)
		})
	}
}

func TestPredecessorExecutionInputsMissingLiveName(t *testing.T) {
	f := newFanOutFixture(t, nil)
	// The query succeeds but the nonempty live reference cannot be resolved.
	require.NoError(t, f.db.Callback().Query().After("gorm:query").Register("test:missing_name", func(tx *gorm.DB) {
		if tasks, ok := tx.Statement.Dest.(*[]models.Task); ok {
			*tasks = nil
		}
	}))
	t.Cleanup(func() { _ = f.db.Callback().Query().Remove("test:missing_name") })
	got, err := f.store.PredecessorExecutionInputs(context.Background(), f.runID, f.consumer.ID)
	require.ErrorContains(t, err, "missing from the live catalog")
	require.Equal(t, PredecessorInputs{}, got)
	// The legacy output path retains its best-effort name policy.
	output, err := f.store.PredecessorOutputs(f.runID, f.consumer.ID)
	require.NoError(t, err)
	require.Nil(t, output)
}

func TestPredecessorExecutionInputsDecodePolicy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  TaskStatus
		fanned  bool
		invalid bool
	}{
		{"scalar", TaskStatusSucceeded, false, true},
		{"scalar failed still consumed", TaskStatusFailed, false, true},
		{"fanned success", TaskStatusSucceeded, true, true},
		{"fanned cached", TaskStatusCached, true, true},
		{"fanned failed unused", TaskStatusFailed, true, false},
		{"fanned skipped unused", TaskStatusSkipped, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFanOutFixture(t, nil)
			updates := map[string]any{"status": string(tc.status), "output": datatypes.JSON(`{"value":9}`)}
			if tc.fanned {
				updates["partition_count"] = 1
				updates["partition_value"] = "a"
			}
			require.NoError(t, f.db.Model(f.producerRow(t)).Updates(updates).Error)
			got, err := f.store.PredecessorExecutionInputs(context.Background(), f.runID, f.consumer.ID)
			if tc.invalid {
				var decodeErr *json.UnmarshalTypeError
				require.ErrorAs(t, err, &decodeErr)
				require.Equal(t, PredecessorInputs{}, got)
			} else {
				require.NoError(t, err)
				require.Equal(t, "0", got.OutputsByName["discover"]["SUCCEEDED"])
			}
		})
	}
}

func TestPredecessorExecutionInputsAggregateFailureReturnsZero(t *testing.T) {
	f := newFanOutFixture(t, nil)
	output, err := json.Marshal(map[string]string{"payload": strings.Repeat("x", pkgtask.MaxOutputBytes)})
	require.NoError(t, err)
	require.NoError(t, f.db.Model(f.producerRow(t)).Updates(map[string]any{"status": string(TaskStatusSucceeded), "output": datatypes.JSON(output), "partition_count": 1, "partition_value": "a"}).Error)
	got, err := f.store.PredecessorExecutionInputs(context.Background(), f.runID, f.consumer.ID)
	require.ErrorIs(t, err, pkgtask.ErrFanInAggregateTooLarge)
	require.Equal(t, PredecessorInputs{}, got)
}

func TestPredecessorExecutionInputsCancellationReturnsZero(t *testing.T) {
	for _, duringRead := range []bool{false, true} {
		t.Run(map[bool]string{false: "before acquisition", true: "after rows"}[duringRead], func(t *testing.T) {
			f := newFanOutFixture(t, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if duringRead {
				require.NoError(t, f.db.Callback().Query().After("gorm:query").Register("test:cancel_inputs", func(tx *gorm.DB) {
					if _, ok := tx.Statement.Dest.(*[]models.TaskRun); ok {
						cancel()
					}
				}))
				t.Cleanup(func() { _ = f.db.Callback().Query().Remove("test:cancel_inputs") })
			} else {
				cancel()
			}
			got, err := f.store.PredecessorExecutionInputs(ctx, f.runID, f.consumer.ID)
			require.ErrorIs(t, err, context.Canceled)
			require.Equal(t, PredecessorInputs{}, got)
		})
	}
}

func TestPredecessorExecutionInputsReplayUsesFrozenNamesAndRefs(t *testing.T) {
	f := newFanOutFixture(t, nil)
	require.NoError(t, f.db.Model(f.producerRow(t)).Updates(map[string]any{"status": string(TaskStatusSucceeded), "output": datatypes.JSON(`{"v":"frozen"}`), "hash": "old"}).Error)
	descriptor, err := json.Marshal(models.TaskExecutionDescriptor{SchemaVersion: models.TaskExecutionDescriptorSchemaVersion, DAG: models.TaskExecutionDAG{Predecessors: []models.TaskExecutionEdgeRef{{TaskID: f.producer.ID, TaskName: "baseline_name"}}}})
	require.NoError(t, err)
	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("job_run_id = ? AND task_id = ?", f.runID, f.consumer.ID).Updates(map[string]any{"quarantine": true, "execution_descriptor": datatypes.JSON(descriptor)}).Error)
	require.NoError(t, f.db.Model(f.producer).Update("name", "renamed").Error)
	require.NoError(t, f.db.Where("to_task_id = ?", f.consumer.ID).Delete(&models.TaskEdge{}).Error)
	got, err := f.store.PredecessorExecutionInputs(context.Background(), f.runID, f.consumer.ID)
	require.NoError(t, err)
	require.Equal(t, map[string]map[string]string{"baseline_name": {"v": "frozen"}}, got.OutputsByName)
	require.Equal(t, "old", got.DescriptorHashes[f.producer.ID])
}
