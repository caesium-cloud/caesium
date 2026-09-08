package lineage

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"gorm.io/gorm"
)

// CheckDeclaredDatasetsObserved reports declared produced datasets that no task
// run has ever been observed emitting. It is a lint WARNING, never an error:
// a dataset declared today and first produced tonight is legitimate, and so is
// a job applied before its producer image was built.
//
// It exists because the declared registry and the observed lineage graph are
// two different sources of truth for "what datasets exist"
// (design-data-circuit-breaker.md § "Dataset identity"): a hold hangs off the
// DECLARED name, so a typo there is a hold nobody can ever open and an impact
// cone that resolves to nothing. Comparing the two catches the typo at lint
// time instead of at 3am.
//
// Observation lives in lineage_datasets, which is only populated when
// OpenLineage capture is on — so the caller must skip this check when it is
// off, otherwise every declaration is "never observed". See
// api/rest/controller/jobdef.Lint for the gating.
//
// Matching is on the dataset NAME only: v1 keys identity on name (namespace is
// reserved on both models), and observed rows carry the configured OpenLineage
// namespace rather than a declared one.
func CheckDeclaredDatasetsObserved(ctx context.Context, conn *gorm.DB, defs []schema.Definition) ([]string, error) {
	if conn == nil || len(defs) == 0 {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	// declarants maps a declared dataset name to the "alias/step" sites that
	// declare it, so the warning names where to go fix the typo.
	declarants := make(map[string][]string)
	for i := range defs {
		alias := strings.TrimSpace(defs[i].Metadata.Alias)
		for s := range defs[i].Steps {
			step := &defs[i].Steps[s]
			if step.Datasets == nil {
				continue
			}
			for p := range step.Datasets.Produces {
				name := strings.TrimSpace(step.Datasets.Produces[p].Name)
				if name == "" {
					continue
				}
				declarants[name] = append(declarants[name], fmt.Sprintf("%s/%s", alias, step.Name))
			}
		}
	}
	if len(declarants) == 0 {
		return nil, nil
	}

	names := make([]string, 0, len(declarants))
	for name := range declarants {
		names = append(names, name)
	}
	sort.Strings(names)

	var observed []string
	if err := conn.WithContext(ctx).
		Model(&models.LineageDataset{}).
		Where("name IN ?", names).
		Distinct("name").
		Pluck("name", &observed).Error; err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(observed))
	for _, name := range observed {
		seen[name] = struct{}{}
	}

	warnings := make([]string, 0)
	for _, name := range names {
		if _, ok := seen[name]; ok {
			continue
		}
		sites := declarants[name]
		sort.Strings(sites)
		warnings = append(warnings, fmt.Sprintf(
			"dataset %q is declared by %s but has never been observed in lineage; holds and impact queries key on the declared name, so check it for a typo",
			name, strings.Join(sites, ", ")))
	}
	if len(warnings) == 0 {
		return nil, nil
	}
	return warnings, nil
}
