package run

import (
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func TestCollapsedSnapshotRetainsUnresolvedSiblingIdentity(t *testing.T) {
	taskID := uuid.New()
	verified := convertRunTaskModel(&models.TaskRun{ID: uuid.New(), TaskID: taskID})
	unknown := convertRunTaskModel(&models.TaskRun{ID: uuid.New(), TaskID: taskID, HashInputBlob: datatypes.JSON(`{"unresolvedImageIdentity":"legacy-attempt"}`)})
	collapsed := collapseFanOutGroups([]*TaskRun{verified, unknown})
	require.Len(t, collapsed, 1)
	require.True(t, collapsed[0].HasUnresolvedImageIdentity)
}
