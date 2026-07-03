package jobdef

import (
	"context"
	"fmt"
	"strings"

	"github.com/caesium-cloud/caesium/internal/freshness"
	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ValidateDatasetGraph runs the cross-job declared-graph lint over the incoming
// definitions PLUS the persisted declarations of jobs that are not being
// replaced: exactly one job produces a given dataset, every consumed dataset
// resolves to a produced dataset or a declared source across the whole set, and
// the declared graph is acyclic across jobs. It is the batch counterpart to the
// per-definition dataset validation in pkg/jobdef (which sees one job at a time
// and cannot prove a cross-job cycle or a duplicate producer).
//
// conn may be nil (e.g. `caesium job lint` with no server), in which case only
// the incoming set is checked. The pure graph algorithm lives in
// internal/freshness so it is unit-testable without a database.
func ValidateDatasetGraph(ctx context.Context, conn *gorm.DB, defs []schema.Definition) error {
	if ctx == nil {
		ctx = context.Background()
	}

	incomingAliases := make(map[string]struct{}, len(defs))
	decls := make([]models.DatasetDeclaration, 0)
	for i := range defs {
		alias := strings.TrimSpace(defs[i].Metadata.Alias)
		if alias == "" {
			return fmt.Errorf("definition %d: metadata.alias is required", i)
		}
		incomingAliases[alias] = struct{}{}
		built, err := freshness.BuildDeclarations(&defs[i], uuid.Nil, alias)
		if err != nil {
			return err
		}
		decls = append(decls, built...)
	}

	existing, err := existingDatasetDeclarations(ctx, conn, incomingAliases)
	if err != nil {
		return err
	}
	decls = append(decls, existing...)

	return freshness.ValidateGraph(decls)
}

// existingDatasetDeclarations loads persisted declarations for live jobs,
// excluding any job whose alias (or job id) appears in the incoming set — those
// declarations are being replaced by this apply and must not double-count as a
// second producer or a stale edge, mirroring the trigger-chain exclusion.
func existingDatasetDeclarations(ctx context.Context, conn *gorm.DB, incomingAliases map[string]struct{}) ([]models.DatasetDeclaration, error) {
	if conn == nil {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	incomingJobIDs, err := existingJobIDsByAlias(ctx, conn, incomingAliases)
	if err != nil {
		return nil, err
	}
	incomingJobIDSet := make(map[uuid.UUID]struct{}, len(incomingJobIDs))
	for _, id := range incomingJobIDs {
		incomingJobIDSet[id] = struct{}{}
	}

	var rows []models.DatasetDeclaration
	err = conn.WithContext(ctx).
		Where("job_id IN (SELECT id FROM jobs WHERE deleted_at IS NULL)").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}

	out := make([]models.DatasetDeclaration, 0, len(rows))
	for idx := range rows {
		row := rows[idx]
		if _, replaced := incomingAliases[strings.TrimSpace(row.JobAlias)]; replaced {
			continue
		}
		if _, replaced := incomingJobIDSet[row.JobID]; replaced {
			continue
		}
		out = append(out, row)
	}
	return out, nil
}
