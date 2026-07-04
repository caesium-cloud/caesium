package incident

import (
	"context"
	"sort"

	"github.com/caesium-cloud/caesium/internal/lineage"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// FreezeAllowlist computes the static job allowlist that scopes an agent's read
// surface for one incident. It is the failing job itself PLUS every job
// transitively downstream of the failing job's output datasets in the lineage
// graph — but the seed datasets deliberately EXCLUDE any output rows produced by
// the failing run itself. That exclusion is the security property: a failing
// task can emit arbitrary `##caesium::output` markers, which materialize as
// lineage dataset rows; if those poisoned edges seeded the impact walk, an
// attacker who controls the failing task could widen the agent's read scope to
// jobs it should never see. Seeding only from the job's TRUSTED historical
// outputs (other runs) closes that hole.
//
// The incident manager (an unscoped, server-side principal) calls this at
// incident open; the result is frozen onto the incident and every agent-session
// token minted for the incident carries a copy. The agent can never widen it.
//
// FreezeAllowlist is best-effort with respect to lineage availability: if the
// lineage graph is empty or unavailable (e.g. OpenLineage disabled), it returns
// just the failing job's own alias rather than failing incident open.
func FreezeAllowlist(ctx context.Context, db *gorm.DB, jobID uuid.UUID, failingRunID *uuid.UUID) []string {
	allow := map[string]struct{}{}

	// The incident's own job is always in scope.
	var job models.Job
	if err := db.WithContext(ctx).Select("alias").First(&job, "id = ?", jobID).Error; err == nil {
		if job.Alias != "" {
			allow[job.Alias] = struct{}{}
		}
	} else {
		log.Debug("incident: freeze allowlist could not resolve job alias", "job_id", jobID, "error", err)
	}

	// Seed the impact walk from the failing job's TRUSTED output datasets —
	// every output row for the job EXCEPT those produced by the failing run.
	seeds, err := trustedOutputDatasets(ctx, db, jobID, failingRunID)
	if err != nil {
		// Lineage tables may be absent/empty when OpenLineage is disabled; degrade
		// to the job's own alias rather than blocking incident open.
		log.Debug("incident: freeze allowlist could not read trusted output datasets", "job_id", jobID, "error", err)
		return sortedKeys(allow)
	}

	for _, seed := range seeds {
		impact, err := lineage.QueryImpact(ctx, db, seed.namespace, seed.name, 0)
		if err != nil {
			log.Debug("incident: freeze allowlist impact query failed", "dataset", seed.name, "error", err)
			continue
		}
		for _, node := range impact.Downstream {
			if node.JobAlias != "" {
				allow[node.JobAlias] = struct{}{}
			}
		}
	}

	return sortedKeys(allow)
}

type datasetSeed struct {
	namespace string
	name      string
}

// trustedOutputDatasets returns the distinct (namespace, name) output datasets
// the job has produced across its runs, EXCLUDING any produced by failingRunID.
func trustedOutputDatasets(ctx context.Context, db *gorm.DB, jobID uuid.UUID, failingRunID *uuid.UUID) ([]datasetSeed, error) {
	type row struct {
		Namespace string
		Name      string
	}
	var rows []row

	q := db.WithContext(ctx).
		Table("lineage_datasets ld").
		Select("DISTINCT ld.namespace, ld.name").
		Joins("JOIN task_runs tr ON tr.id = ld.task_run_id").
		Joins("JOIN job_runs jr ON jr.id = tr.job_run_id").
		Where("jr.job_id = ? AND ld.direction = ?", jobID, "output")
	if failingRunID != nil {
		q = q.Where("jr.id <> ?", *failingRunID)
	}
	if err := q.Scan(&rows).Error; err != nil {
		return nil, err
	}

	seeds := make([]datasetSeed, 0, len(rows))
	for _, r := range rows {
		seeds = append(seeds, datasetSeed{namespace: r.Namespace, name: r.Name})
	}
	return seeds, nil
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
