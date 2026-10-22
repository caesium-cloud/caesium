package jobdef

import (
	"context"
	"testing"

	"github.com/caesium-cloud/caesium/internal/freshness"
	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/stretchr/testify/require"
)

// producingDef is the fixture's one-step job, producing dataset.
func (f *tenancyFixture) producingDef(alias, namespace, dataset string) *schema.Definition {
	f.t.Helper()
	def := f.def(alias, namespace, "")
	def.Steps[0].Datasets = &schema.StepDatasets{Produces: []schema.ProducedDataset{{Name: dataset}}}
	return def
}

// A legacy job stored as "payroll " (before Validate refused surrounding
// whitespace) produces ledger. Its canonical twin "payroll" is a different
// job: applying it with the same output is a second producer, refused by the
// pre-write validation and the post-write re-validation alike, with nothing
// written (identity-and-access W7-ε). Declarations store the trimmed alias,
// so excluding persisted rows by trimmed alias dropped the legacy producer
// and keying producers by alias folded the two into one.
func TestCanonicalTwinOfALegacyProducerIsASecondProducer(t *testing.T) {
	ctx := context.Background()
	f := newTenancyFixture(t, false)
	legacy, err := f.importer.ApplyWithOptions(ctx, f.producingDef("payroll-seed", "finance", "ledger"), nil)
	require.NoError(t, err)
	require.NoError(t, f.db.Model(&models.Job{}).Where("id = ?", legacy.ID).Update("alias", "payroll ").Error)
	var stored []models.DatasetDeclaration
	require.NoError(t, f.db.Where("job_id = ?", legacy.ID).Find(&stored).Error)
	require.NotEmpty(t, stored)
	// As BuildDeclarations stores a legacy job's rows: its alias trimmed.
	require.NoError(t, f.db.Model(&models.DatasetDeclaration{}).Where("job_id = ?", legacy.ID).Update("job_alias", "payroll").Error)

	twin := f.producingDef("payroll", "marketing", "ledger")
	require.ErrorIs(t, f.importer.ValidateBatch(ctx, []schema.Definition{*twin}), freshness.ErrDatasetMultipleProducers)

	// The batch apply re-validates the written graph inside its transaction.
	_, err = f.importer.ApplyBatchWithOptions(ctx, []BatchPlan{{Def: twin}})
	require.ErrorIs(t, err, freshness.ErrDatasetMultipleProducers)
	var count int64
	require.NoError(t, f.db.Model(&models.Job{}).Where("alias = ?", "payroll").Count(&count).Error)
	require.Zero(t, count, "the refused twin was written")

	// The stored graph alone (the legacy job, one producer) still validates.
	require.NoError(t, ValidateDatasetGraph(ctx, f.db, nil))
}
