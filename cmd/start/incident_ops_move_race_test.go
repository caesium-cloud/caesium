package start

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/incident"
	internaljobdef "github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// TestApplyJobdefPatchRefusesAJobMovedDuringTheRender (identity-and-access
// A8): a move that commits while the diff is rendered is caught by the
// ownership re-read, so neither route returns or applies a diff of the new
// owner's definition.
func TestApplyJobdefPatchRefusesAJobMovedDuringTheRender(t *testing.T) {
	previous := policy.ProcessHolder()
	t.Cleanup(func() { policy.SetProcessHolder(previous) })
	policy.SetProcessHolder(nil)
	for _, dryRun := range []bool{true, false} {
		db := testutil.OpenTestDB(t)
		ctx := context.Background()
		const alias = "render-race"
		encoded, err := json.Marshal(patchDefinition(alias, "marketing"))
		require.NoError(t, err)
		var applied schema.Definition
		require.NoError(t, json.Unmarshal(encoded, &applied))
		_, err = internaljobdef.NewImporter(db).Apply(ctx, &applied)
		require.NoError(t, err)
		var job models.Job
		require.NoError(t, db.Where("alias = ?", alias).First(&job).Error)

		moved := false
		require.NoError(t, db.Callback().Query().After("gorm:query").Register("a8-move-during-render", func(tx *gorm.DB) {
			if moved || tx.Statement.Table != "tasks" {
				return
			}
			moved = true
			require.NoError(t, tx.Session(&gorm.Session{NewDB: true}).Exec(`UPDATE jobs SET namespace = 'finance' WHERE id = ?`, job.ID).Error)
		}))

		_, err = newIncidentActionOps(db, nil, nil).ApplyJobdefPatch(ctx, job.ID, "marketing", encoded, dryRun)
		require.True(t, moved, "dryRun=%v: the move ran during the render", dryRun)
		require.ErrorIs(t, err, incident.ErrNamespaceMoved, "dryRun=%v", dryRun)
		testutil.CloseDB(db)
	}
}
