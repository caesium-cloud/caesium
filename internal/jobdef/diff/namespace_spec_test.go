package diff

import (
	"context"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/stretchr/testify/require"
)

func TestCompareNamespaceOnlyChangesFromDatabase(t *testing.T) {
	for _, tc := range []struct {
		name, actual, desired string
		wantUpdate            bool
	}{
		{"move", "marketing", "finance", true},
		{"omission_moves_to_default", "finance", "", true},
		{"omission_matches_default", "default", "", false},
		{"legacy_empty_matches_default", "", "default", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openDiffTestDB(t)
			defer closeDiffTestDB(db)
			definition := schema.Definition{
				Metadata: schema.Metadata{Alias: "namespace-diff", Namespace: tc.actual},
				Trigger:  schema.Trigger{Type: schema.TriggerCron, Configuration: map[string]any{"cron": "0 0 31 2 *"}},
				Steps:    []schema.Step{{Name: "task", Engine: schema.EngineDocker, Image: "alpine:3.23", Command: []string{"true"}}},
			}
			insertDiffDefinition(t, db, definition)
			if tc.actual == "" {
				require.NoError(t, db.Model(&models.Job{}).Where("alias = ?", definition.Metadata.Alias).UpdateColumn("namespace", "").Error)
			}
			actual, err := LoadDatabaseSpecs(context.Background(), db)
			require.NoError(t, err)
			require.Equal(t, models.NamespaceOrDefault(tc.actual), actual[definition.Metadata.Alias].Namespace)
			definition.Metadata.Namespace = tc.desired
			desired := FromDefinition(&definition)
			require.Equal(t, models.NamespaceOrDefault(tc.desired), desired.Namespace)
			result := Compare(map[string]JobSpec{definition.Metadata.Alias: desired}, actual)
			require.Empty(t, result.Creates)
			require.Empty(t, result.Deletes)
			if tc.wantUpdate {
				require.Len(t, result.Updates, 1)
				require.Equal(t, definition.Metadata.Alias, result.Updates[0].Alias)
				require.Contains(t, result.Updates[0].Diff, "Namespace")
				require.Contains(t, result.Updates[0].Diff, models.NamespaceOrDefault(tc.actual))
				require.Contains(t, result.Updates[0].Diff, models.NamespaceOrDefault(tc.desired))
			} else {
				require.True(t, result.Empty(), "omitted/legacy namespaces must compare as default")
			}
		})
	}
}
