package diff

import (
	"context"
	"testing"

	"github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/stretchr/testify/require"
)

func TestCompareProducesCreatesUpdatesDeletes(t *testing.T) {
	desired := map[string]JobSpec{
		"new": {
			Alias: "new",
		},
		"shared": {
			Alias:   "shared",
			Trigger: TriggerSpec{Type: "cron", Configuration: map[string]any{"cron": "* * * * *"}},
		},
	}
	actual := map[string]JobSpec{
		"shared": {
			Alias:   "shared",
			Trigger: TriggerSpec{Type: "cron", Configuration: map[string]any{"cron": "0 * * * *"}},
		},
		"stale": {Alias: "stale"},
	}

	diff := Compare(desired, actual)

	require.Len(t, diff.Creates, 1)
	require.Equal(t, "new", diff.Creates[0].Alias)

	require.Len(t, diff.Deletes, 1)
	require.Equal(t, "stale", diff.Deletes[0].Alias)

	require.Len(t, diff.Updates, 1)
	require.Equal(t, "shared", diff.Updates[0].Alias)
	require.NotEmpty(t, diff.Updates[0].Diff)
}

func TestLoadDatabaseSpecsMatchesDefinition(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)

	def, err := schema.Parse([]byte(testutil.SampleJob))
	require.NoError(t, err)

	importer := jobdef.NewImporter(db)
	_, err = importer.Apply(context.Background(), def)
	require.NoError(t, err)

	actual, err := LoadDatabaseSpecs(context.Background(), db)
	require.NoError(t, err)

	desired := map[string]JobSpec{def.Metadata.Alias: FromDefinition(def)}

	diff := Compare(desired, actual)
	require.True(t, diff.Empty())
}
