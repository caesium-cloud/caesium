package replay

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// replay_fanout_test.go covers quarantined replay of a baseline containing a
// fan-out group. The contract under test is narrow and load-bearing: the group
// is re-materialized from the partition list frozen on the PRODUCER's
// descriptor, and the producer is never re-run to rediscover it.

func threePartitions() []pkgtask.Partition {
	return []pkgtask.Partition{
		{Key: "alpha", Fingerprint: "sha256:" + strings.Repeat("a", 64)},
		{Key: "bravo", Fingerprint: "sha256:" + strings.Repeat("b", 64), Attributes: map[string]string{"region": "eu"}},
		{Key: "charlie", DependsOn: []string{"alpha"}},
	}
}

type fanOutSeedConfig struct {
	producer string
	group    string
	// partitions is what the baseline actually materialized as instance rows.
	partitions []pkgtask.Partition
	// recorded is what the producer's descriptor claims it emitted. Defaults to
	// partitions; set it separately to seed a disagreement.
	recorded []pkgtask.Partition
	// omitRecord leaves the producer's descriptor with no fanOut section at all,
	// which is what every descriptor written before that capture existed looks
	// like.
	omitRecord bool
	// entryPartitions overrides what the producer's cache entry
	// (cache.Entry.Partitions) records; it defaults to the recorded list, which
	// is what a real fanned run writes.
	entryPartitions []pkgtask.Partition
	// legacyEntry leaves the producer's cache entry with no partition list at
	// all, the shape of every entry written before that column existed.
	legacyEntry bool
	replaySafe  bool
}

type fanOutSeed struct {
	producerTaskID uuid.UUID
	groupTaskID    uuid.UUID
	instanceIDs    []uuid.UUID
	instanceHashes []string
}

// seedFanOutGroup writes a baseline exactly as a real fanned run leaves one: a
// producer row whose partitions column and descriptor both carry the emitted
// list, and N instance rows sharing the group's catalog task id, each with its
// own partition identity, identity hash and cache entry.
func (f replayFixture) seedFanOutGroup(t *testing.T, cfg fanOutSeedConfig) fanOutSeed {
	t.Helper()
	if cfg.producer == "" {
		cfg.producer = "discover"
	}
	if cfg.group == "" {
		cfg.group = "process-file"
	}
	if cfg.recorded == nil {
		cfg.recorded = cfg.partitions
	}

	producerTaskID := f.seedTask(t, seedTaskConfig{
		name:       cfg.producer,
		replaySafe: true,
		result:     "success",
		output:     map[string]string{"COUNT": "3"},
		position:   0,
	})
	groupTaskID := f.seedTask(t, seedTaskConfig{
		name:       cfg.group,
		replaySafe: cfg.replaySafe,
		result:     "success",
		position:   1,
	})
	f.linkDescriptors(t, producerTaskID, groupTaskID)

	var producerRow, template models.TaskRun
	require.NoError(t, f.db.First(&producerRow, "job_run_id = ? AND task_id = ?", f.runID, producerTaskID).Error)
	require.NoError(t, f.db.First(&template, "job_run_id = ? AND task_id = ?", f.runID, groupTaskID).Error)

	var groupDesc models.TaskExecutionDescriptor
	require.NoError(t, json.Unmarshal(template.ExecutionDescriptor, &groupDesc))
	predOutputs := map[string]map[string]string{cfg.producer: {"COUNT": "3"}}
	predHashes := []string{producerRow.Hash}

	// The template row becomes instance 0 in place, exactly as
	// run.Store.expandFanOutSuccessors rewrites it.
	require.NoError(t, f.db.Where("task_run_id = ?", template.ID).Delete(&models.TaskCache{}).Error)

	seed := fanOutSeed{producerTaskID: producerTaskID, groupTaskID: groupTaskID}
	for i, part := range cfg.partitions {
		hash, err := computeDescriptorInstanceHash(groupDesc, map[string]string{"mode": "baseline"}, predOutputs, predHashes, part)
		require.NoError(t, err)
		desc := groupDesc
		desc.Cache.ComputedHash = hash
		desc.Baseline.ComputedHash = hash

		attrs, err := encodePartitionAttributes(part.Attributes)
		require.NoError(t, err)
		deps, err := json.Marshal(part.DependsOn)
		require.NoError(t, err)

		row := template
		if i > 0 {
			row.ID = uuid.New()
		}
		row.PartitionValue = part.Key
		row.PartitionIndex = i
		row.PartitionCount = len(cfg.partitions)
		row.PartitionFingerprint = part.Fingerprint
		row.PartitionAttributes = attrs
		row.PartitionDependsOn = datatypes.JSON(deps)
		row.Hash = hash
		row.Output = mustJSON(t, map[string]string{"ROWS": part.Key})
		row.ExecutionDescriptor = mustJSON(t, desc)

		if i == 0 {
			require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", template.ID).Updates(map[string]any{
				"partition_value":       row.PartitionValue,
				"partition_index":       row.PartitionIndex,
				"partition_count":       row.PartitionCount,
				"partition_fingerprint": row.PartitionFingerprint,
				"partition_attributes":  row.PartitionAttributes,
				"partition_depends_on":  row.PartitionDependsOn,
				"hash":                  row.Hash,
				"output":                row.Output,
				"execution_descriptor":  row.ExecutionDescriptor,
			}).Error)
		} else {
			require.NoError(t, f.db.Create(&row).Error)
		}
		require.NoError(t, f.db.Create(&models.TaskCache{
			Hash:      hash,
			JobID:     f.jobID,
			TaskName:  cfg.group,
			Result:    "success",
			Output:    row.Output,
			RunID:     f.runID,
			TaskRunID: row.ID,
			CreatedAt: f.now,
		}).Error)
		seed.instanceIDs = append(seed.instanceIDs, row.ID)
		seed.instanceHashes = append(seed.instanceHashes, hash)
	}

	// Producer side: the partitions column, the descriptor's frozen copy, and the
	// list mirrored onto its cache entry.
	encoded, err := pkgtask.EncodePartitions(cfg.recorded)
	require.NoError(t, err)
	updates := map[string]any{"partitions": datatypes.JSON(encoded)}
	if !cfg.omitRecord {
		var producerDesc models.TaskExecutionDescriptor
		require.NoError(t, json.Unmarshal(producerRow.ExecutionDescriptor, &producerDesc))
		producerDesc.FanOut = &models.TaskExecutionFanOut{
			Partitions:         cfg.recorded,
			PartitionsRecorded: true,
			Groups:             []models.TaskExecutionFanOutGroup{{TaskID: groupTaskID, TaskName: cfg.group}},
		}
		updates["execution_descriptor"] = mustJSON(t, producerDesc)
	}
	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", producerRow.ID).Updates(updates).Error)

	if !cfg.legacyEntry {
		entryList := cfg.entryPartitions
		if entryList == nil {
			entryList = cfg.recorded
		}
		// Struct-tag encoding, not pkgtask.EncodePartitions — the symmetry
		// cache.entryToModel documents, so the read back in modelToEntry keeps the
		// attributes.
		entryJSON, err := json.Marshal(entryList)
		require.NoError(t, err)
		require.NoError(t, f.db.Model(&models.TaskCache{}).Where("task_run_id = ?", producerRow.ID).
			Update("partitions", datatypes.JSON(entryJSON)).Error)
	}
	return seed
}

func (f replayFixture) replayInstances(t *testing.T, runID, taskID uuid.UUID) []models.TaskRun {
	t.Helper()
	var rows []models.TaskRun
	require.NoError(t, f.db.Where("job_run_id = ? AND task_id = ?", runID, taskID).
		Order("partition_index ASC").Find(&rows).Error)
	return rows
}

// TestReplayReExpandsFannedGroupFromRecordedPartitions is the headline: a fanned
// baseline replays, the group comes back instance for instance, and the producer
// is resolved from cache rather than re-run to rediscover the list.
func TestReplayReExpandsFannedGroupFromRecordedPartitions(t *testing.T) {
	f := newReplayFixture(t)
	parts := threePartitions()
	seed := f.seedFanOutGroup(t, fanOutSeedConfig{partitions: parts, replaySafe: true})

	dispatcher := &recordingDispatcher{}
	result, err := New(f.store, dispatcher).Replay(context.Background(), Request{BaselineRunID: f.runID})
	require.NoError(t, err)
	require.Empty(t, dispatcher.calls, "an all-cached replay re-expands without executing anything")
	require.Equal(t, run.StatusSucceeded, result.Run.Status)

	rows := f.replayInstances(t, result.Run.ID, seed.groupTaskID)
	require.Len(t, rows, len(parts), "the group is re-materialized at its recorded width")
	for i, part := range parts {
		require.Equal(t, part.Key, rows[i].PartitionValue)
		require.Equal(t, i, rows[i].PartitionIndex)
		require.Equal(t, len(parts), rows[i].PartitionCount)
		require.Equal(t, part.Fingerprint, rows[i].PartitionFingerprint)
		require.Equal(t, seed.instanceHashes[i], rows[i].Hash, "each instance keeps its own per-partition identity")
		require.Equal(t, string(run.TaskStatusCached), rows[i].Status)
		require.True(t, rows[i].Quarantine)
	}
	require.JSONEq(t, `{"region":"eu"}`, string(rows[1].PartitionAttributes), "scalar attributes survive the round trip")
	require.JSONEq(t, `["alpha"]`, string(rows[2].PartitionDependsOn), "in-group ordering survives the round trip")

	var producer models.TaskRun
	require.NoError(t, f.db.First(&producer, "job_run_id = ? AND task_id = ?", result.Run.ID, seed.producerTaskID).Error)
	require.Equal(t, string(run.TaskStatusCached), producer.Status, "the producer is reused, never re-executed")
	require.NotEmpty(t, producer.Partitions, "the producer row records the list its group was expanded from")

	var decoded []map[string]any
	require.NoError(t, json.Unmarshal(producer.Partitions, &decoded))
	require.Len(t, decoded, len(parts))
	require.Equal(t, "alpha", decoded[0]["key"])
}

// TestReplayRefusesFannedBaselineWithoutRecordedPartitions pins the
// backward-compatibility contract: a baseline whose descriptors predate fan-out
// capture is refused exactly as it was before re-expansion existed.
func TestReplayRefusesFannedBaselineWithoutRecordedPartitions(t *testing.T) {
	f := newReplayFixture(t)
	f.seedFanOutGroup(t, fanOutSeedConfig{partitions: threePartitions(), replaySafe: true, omitRecord: true})

	_, err := New(f.store, &recordingDispatcher{}).Replay(context.Background(), Request{BaselineRunID: f.runID})
	require.ErrorIs(t, err, ErrFannedBaseline)
	require.Contains(t, err.Error(), "no partition list recorded")
}

// TestReplayRefusesFannedBaselineWhenRecordedListDisagrees keeps the recorded
// list honest: it is the authority only while it still describes the group the
// baseline actually materialized.
func TestReplayRefusesFannedBaselineWhenRecordedListDisagrees(t *testing.T) {
	f := newReplayFixture(t)
	parts := threePartitions()
	drifted := []pkgtask.Partition{parts[0], {Key: "delta"}, parts[2]}
	f.seedFanOutGroup(t, fanOutSeedConfig{partitions: parts, recorded: drifted, replaySafe: true})

	_, err := New(f.store, &recordingDispatcher{}).Replay(context.Background(), Request{BaselineRunID: f.runID})
	require.ErrorIs(t, err, ErrFannedBaseline)
	require.Contains(t, err.Error(), `is partition "bravo" but the recorded list has "delta"`)
}

// TestReplayRefusesFannedBaselineWhenRecordedWidthDisagrees covers the other
// half of the same check.
func TestReplayRefusesFannedBaselineWhenRecordedWidthDisagrees(t *testing.T) {
	f := newReplayFixture(t)
	parts := threePartitions()
	f.seedFanOutGroup(t, fanOutSeedConfig{partitions: parts, recorded: parts[:2], replaySafe: true})

	_, err := New(f.store, &recordingDispatcher{}).Replay(context.Background(), Request{BaselineRunID: f.runID})
	require.ErrorIs(t, err, ErrFannedBaseline)
	require.Contains(t, err.Error(), "recorded 2 partitions")
}

// TestReplayRefusesProducerWhoseReusedCacheEntryRecordedAnotherList is the F7
// thread the issue calls out: cacheSourceForUnchanged now carries
// entry.Partitions, and a reused entry that names a different group than the
// baseline's proves the producer is nondeterministic.
func TestReplayRefusesProducerWhoseReusedCacheEntryRecordedAnotherList(t *testing.T) {
	f := newReplayFixture(t)
	parts := threePartitions()
	f.seedFanOutGroup(t, fanOutSeedConfig{
		partitions:      parts,
		replaySafe:      true,
		entryPartitions: []pkgtask.Partition{{Key: "alpha"}, {Key: "bravo"}},
	})

	_, err := New(f.store, &recordingDispatcher{}).Replay(context.Background(), Request{BaselineRunID: f.runID})
	require.ErrorIs(t, err, ErrFannedBaseline)
	require.Contains(t, err.Error(), "not deterministic")
}

// TestReplayLegacyCacheEntryWithoutPartitionsStillReplays keeps the nil-versus-
// empty distinction meaningful: an entry written before the partitions column
// existed makes no claim about the group, so it is not evidence of drift and
// must not turn a replayable baseline into a 409.
func TestReplayLegacyCacheEntryWithoutPartitionsStillReplays(t *testing.T) {
	f := newReplayFixture(t)
	seed := f.seedFanOutGroup(t, fanOutSeedConfig{
		partitions:  threePartitions(),
		replaySafe:  true,
		legacyEntry: true,
	})

	var entry models.TaskCache
	require.NoError(t, f.db.First(&entry, "task_name = ?", "discover").Error)
	require.Empty(t, entry.Partitions, "precondition: the reused entry records no list")

	result, err := New(f.store, &recordingDispatcher{}).Replay(context.Background(), Request{BaselineRunID: f.runID})
	require.NoError(t, err)
	require.Len(t, f.replayInstances(t, result.Run.ID, seed.groupTaskID), 3)
}

// TestReplayFannedGroupOverrideSeedsInGroupOrdering drives the re-execution
// path. Every instance re-runs, and `charlie` — which the recorded list orders
// after `alpha` — waits for both its producer and its sibling, so the replayed
// group carries the baseline's in-group ordering rather than starting flat.
func TestReplayFannedGroupOverrideSeedsInGroupOrdering(t *testing.T) {
	f := newReplayFixture(t)
	parts := threePartitions()
	seed := f.seedFanOutGroup(t, fanOutSeedConfig{partitions: parts, replaySafe: true})

	dispatcher := &recordingDispatcher{}
	result, err := New(f.store, dispatcher).Replay(context.Background(), Request{
		BaselineRunID: f.runID,
		Set:           map[string]string{"mode": "what-if"},
	})
	require.NoError(t, err)
	require.Len(t, dispatcher.calls, 1, "a re-executing replay is handed to the dispatcher")

	rows := f.replayInstances(t, result.Run.ID, seed.groupTaskID)
	require.Len(t, rows, 3)
	for _, row := range rows {
		require.Equal(t, string(run.TaskStatusPending), row.Status)
	}
	require.Equal(t, 1, rows[0].OutstandingPredecessors, "alpha waits only on the producer")
	require.Equal(t, 1, rows[1].OutstandingPredecessors, "bravo has no in-group edges")
	require.Equal(t, 2, rows[2].OutstandingPredecessors, "charlie waits on the producer AND on alpha")

	byIndex := map[int]string{}
	for _, row := range rows {
		byIndex[row.PartitionIndex] = row.PartitionValue
		require.NotEqual(t, seed.instanceHashes[row.PartitionIndex], row.Hash,
			"an override changes every instance's identity, partition included")
	}
	require.Equal(t, map[int]string{0: "alpha", 1: "bravo", 2: "charlie"}, byIndex)
}

// TestReplayFannedGroupCachedSiblingDoesNotStrandItsDependent pins why in-group
// indegree counts only siblings that will actually re-execute: a sibling
// resolved from cache is written terminal at materialization and never completes,
// so it never decrements anything waiting on it.
func TestReplayFannedGroupCachedSiblingDoesNotStrandItsDependent(t *testing.T) {
	f := newReplayFixture(t)
	parts := []pkgtask.Partition{{Key: "alpha"}, {Key: "charlie", DependsOn: []string{"alpha"}}}
	seed := f.seedFanOutGroup(t, fanOutSeedConfig{partitions: parts, replaySafe: true})

	result, err := New(f.store, &recordingDispatcher{}).Replay(context.Background(), Request{BaselineRunID: f.runID})
	require.NoError(t, err)

	rows := f.replayInstances(t, result.Run.ID, seed.groupTaskID)
	require.Len(t, rows, 2)
	for _, row := range rows {
		require.Equal(t, string(run.TaskStatusCached), row.Status)
		require.Zero(t, row.OutstandingPredecessors,
			"a fully cached group leaves nothing waiting on a decrement that will never arrive")
	}
}

// TestReplayFannedGroupInstanceMustBeReplaySafeToReexecute keeps the replay-safe
// gate per instance rather than per catalog task.
func TestReplayFannedGroupInstanceMustBeReplaySafeToReexecute(t *testing.T) {
	f := newReplayFixture(t)
	f.seedFanOutGroup(t, fanOutSeedConfig{partitions: threePartitions(), replaySafe: false})

	_, err := New(f.store, &recordingDispatcher{}).Replay(context.Background(), Request{
		BaselineRunID: f.runID,
		Set:           map[string]string{"mode": "what-if"},
	})
	require.ErrorIs(t, err, ErrReplayUnsafe)
}
