package lineage

import (
	"context"

	"github.com/caesium-cloud/caesium/internal/authz"
	jobdiff "github.com/caesium-cloud/caesium/internal/jobdef/diff"
	illineage "github.com/caesium-cloud/caesium/internal/lineage"
	"github.com/caesium-cloud/caesium/internal/models"
	"gorm.io/gorm"
)

// RedactedImpactResult is GET /v1/lineage/impact for a namespace-confined
// caller (identity-and-access design §8.3, plan item B5). Downstream holds
// each node the caller may read exactly as an unrestricted caller sees it
// (an illineage.ImpactNode), and every other node as an ExternalImpactNode.
type RedactedImpactResult struct {
	RootNamespace string `json:"root_namespace"`
	RootName      string `json:"root_name"`
	Downstream    []any  `json:"downstream"`
}

// ExternalImpactNode is a downstream node collapsed for a confined caller:
// alias, namespace, dataset, job id, producing step, provenance and last-seen
// time removed. Depth is kept: it is the only structure an impact list has,
// and the collapsed topology already implies it.
type ExternalImpactNode struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Depth int    `json:"depth"`
}

// ConfinedImpact is Impact computed over the whole lineage graph and then
// redacted for r's caller. A root the caller may not read answers exactly as a
// root with no recorded lineage does (no existence oracle). Everything is read
// from one read-only snapshot, so the root decision and every node agree.
func (s *Service) ConfinedImpact(r *authz.Redactor, namespace, name string, maxDepth int) (*RedactedImpactResult, error) {
	out := &RedactedImpactResult{RootNamespace: namespace, RootName: name, Downstream: []any{}}
	err := jobdiff.ReadSnapshot(s.context(), s.db, func(tx *gorm.DB) error {
		// A retry can find the root unreadable, and that path leaves Downstream
		// alone: it must not still hold the nodes of an attempt whose COMMIT
		// failed. The caller would be reading a root it may no longer read.
		out.Downstream = []any{}
		visible, err := rootVisible(s.context(), tx, r, namespace, name)
		if err != nil || !visible {
			return err
		}
		result, err := illineage.QueryImpact(s.context(), tx, namespace, name, maxDepth)
		if err != nil {
			return err
		}
		out.Downstream = redactImpactNodes(r, result.Downstream)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) context() context.Context {
	if s.ctx == nil {
		return context.Background()
	}
	return s.ctx
}

// redactImpactNodes keeps each node whose producing job's current namespace
// and whose run's persisted namespace are both readable, and collapses every
// other node. Placeholders are de-duplicated response-wide by (id, depth):
// repeat runs of one foreign producer are one node, not a run count.
func redactImpactNodes(r *authz.Redactor, nodes []illineage.ImpactNode) []any {
	out := make([]any, 0, len(nodes))
	seen := map[ExternalImpactNode]struct{}{}
	for _, node := range nodes {
		if r.Visible(node.JobNamespace, node.RunNamespace) {
			out = append(out, node)
			continue
		}
		placeholder := ExternalImpactNode{
			ID:    r.ExternalID("impact\x00" + node.DatasetNamespace + "\x00" + node.DatasetName + "\x00" + node.JobID.String()),
			Kind:  authz.ExternalKind,
			Depth: node.Depth,
		}
		if _, dup := seen[placeholder]; dup {
			continue
		}
		seen[placeholder] = struct{}{}
		out = append(out, placeholder)
	}
	return out
}

// rootVisible reports whether the caller may read the impact root. A consumer
// never grants ownership of a dataset:
//   - a dataset declared in the registry (the empty OpenLineage namespace)
//     with declared producers is owned by those producers' jobs (current
//     namespace); the caller must read one of them;
//   - otherwise (observed only) the caller must read a run that PRODUCED it:
//     an output lineage row whose job's current namespace and run's persisted
//     namespace are both allowed.
//
// With neither, the root is cluster-only.
func rootVisible(ctx context.Context, tx *gorm.DB, r *authz.Redactor, namespace, name string) (bool, error) {
	if namespace == "" {
		var owners []string
		if err := tx.WithContext(ctx).
			Table("dataset_declarations AS dd").
			Joins("JOIN jobs j ON j.id = dd.job_id AND j.deleted_at IS NULL").
			Where("dd.direction = ? AND dd.name = ? AND (dd.namespace IS NULL OR dd.namespace = '')", models.DatasetDirectionProduces, name).
			Pluck("j.namespace", &owners).Error; err != nil {
			return false, err
		}
		if len(owners) > 0 {
			for _, owner := range owners {
				if r.Visible(owner) {
					return true, nil
				}
			}
			return false, nil
		}
	}
	var producers []struct {
		JobNamespace string
		RunNamespace string
	}
	if err := tx.WithContext(ctx).
		Table("lineage_datasets AS ld").
		Select("j.namespace AS job_namespace, jr.namespace AS run_namespace").
		Joins("JOIN task_runs tr ON tr.id = ld.task_run_id").
		Joins("JOIN job_runs jr ON jr.id = tr.job_run_id").
		Joins("JOIN jobs j ON j.id = jr.job_id").
		Where("ld.direction = ? AND ld.namespace = ? AND ld.name = ?", "output", namespace, name).
		Group("j.namespace, jr.namespace").
		Scan(&producers).Error; err != nil {
		return false, err
	}
	for _, producer := range producers {
		if r.Visible(producer.JobNamespace, producer.RunNamespace) {
			return true, nil
		}
	}
	return false, nil
}
