package models

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm/schema"
)

func TestDescriptorTombstoneSchema(t *testing.T) {
	tombstone := &DescriptorTombstone{}
	require.Contains(t, All, tombstone)
	table, err := schema.Parse(tombstone, &sync.Map{}, schema.NamingStrategy{})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"job_run_id", "task_id", "task_name", "pruned_at"}, table.DBNames)
	require.ElementsMatch(t, []string{"job_run_id", "task_id"}, table.PrimaryFieldDBNames)
}
