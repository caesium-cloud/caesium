package run

import (
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func TestRehydrateInGroupEdgesRejectsMalformedDependenciesAtomically(t *testing.T) {
	for _, raw := range []string{`[`, `["a",9]`, `{}`, `"a"`} {
		t.Run(raw, func(t *testing.T) {
			b := newTopoBuilder()
			group := b.task("")
			rs := NewRunState(b.build(), 0)
			before := rs.Clone()
			invalidID := uuid.New()
			rows := []models.TaskRun{
				{ID: uuid.New(), TaskID: group, PartitionValue: "a", PartitionCount: 2, PartitionDependsOn: datatypes.JSON(`[]`)},
				{ID: invalidID, TaskID: group, PartitionValue: "b", PartitionCount: 2, PartitionDependsOn: datatypes.JSON(raw)},
			}
			err := rs.RehydrateInGroupEdges(rows, []models.Task{{ID: group, Name: "group", FanOutConfig: datatypes.JSON(`{"maxParallel":3}`)}})
			require.ErrorContains(t, err, invalidID.String())
			require.Equal(t, before, rs.Clone(), "failed reconstruction must not mutate any state")
			recovered, result, err := RecoverRunStateWithFanOut(b.build(), nil, []models.TaskRun{{TaskID: group, Status: string(TaskStatusSucceeded), TerminalSequence: 1}}, rows, nil)
			require.ErrorContains(t, err, invalidID.String())
			require.Nil(t, recovered)
			require.Equal(t, RecoveryResult{}, result, "recovery must abort before terminal replay")
		})
	}
}

func TestRehydrateInGroupEdgesIgnoresUnfannedDependencyBytes(t *testing.T) {
	b := newTopoBuilder()
	id := b.task("")
	rs := NewRunState(b.build(), 0)
	require.NoError(t, rs.RehydrateInGroupEdges([]models.TaskRun{{ID: uuid.New(), TaskID: id, PartitionDependsOn: datatypes.JSON(`bad-json`)}}, nil))
	require.Empty(t, rs.instancesOf)
}
