package start

import (
	"testing"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// DELETE /v1/jobs/:id allows a run in flight and only soft-deletes the job, so
// recovery has to read past the delete or the run can never be adopted.
func TestRetainedJobReadsADeletedJob(t *testing.T) {
	conn := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(conn) })
	jobID := uuid.New()
	require.NoError(t, conn.Create(&models.Job{ID: jobID, Alias: "deleted-with-a-run"}).Error)
	require.NoError(t, conn.Delete(&models.Job{ID: jobID}).Error)
	require.ErrorIs(t, conn.First(&models.Job{}, "id = ?", jobID).Error, gorm.ErrRecordNotFound)

	j, err := retainedJob(t.Context(), conn, jobID)
	require.NoError(t, err)
	require.Equal(t, jobID, j.ID)

	_, err = retainedJob(t.Context(), conn, uuid.New())
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
}
