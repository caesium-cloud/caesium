package job

import (
	"context"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/jsonmap"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.Job{}))
	return db
}

func TestCreatePersistsMetadata(t *testing.T) {
	db := openTestDB(t)
	svc := &jobService{ctx: context.Background(), db: db}

	req := &CreateRequest{
		TriggerID:   uuid.New(),
		Alias:       "job-meta",
		Labels:      map[string]string{"team": "data"},
		Annotations: map[string]string{"owner": "qa"},
	}

	job, err := svc.Create(req)
	require.NoError(t, err)
	require.NotNil(t, job)

	var stored models.Job
	require.NoError(t, db.First(&stored, "id = ?", job.ID).Error)
	require.Equal(t, "data", stored.Labels["team"])
	require.Equal(t, "qa", stored.Annotations["owner"])
}

func TestJSONMapFromStringMapHandlesNil(t *testing.T) {
	val := jsonmap.FromStringMap(nil)
	require.Empty(t, val)
}
