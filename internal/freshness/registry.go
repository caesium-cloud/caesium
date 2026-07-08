// Package freshness holds the data-freshness scheduling substrate: the declared
// dataset registry (this file) and — in later streams — the dataset state store
// and the leader-gated evaluator. The declared registry is the complement to
// the observed lineage graph: it is rebuilt from job manifests on every apply
// and requires no OpenLineage.
package freshness

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// Registry is the typed store over the declared dataset graph
// (dataset_declarations). It is a projection of the applied manifest set — the
// importer rebuilds a job's rows on every apply and prunes them on retire — so
// callers read it as the authoritative declared graph.
type Registry struct {
	db *gorm.DB
}

// NewRegistry constructs a Registry over the provided connection.
func NewRegistry(db *gorm.DB) *Registry {
	return &Registry{db: db}
}

// BuildDeclarations projects a validated job definition into the declaration
// rows that represent its slice of the declared graph: one row per declared
// source, produced dataset, and consumed dataset. The definition must have
// passed schema.Definition.Validate first. jobID/jobAlias identify the owning
// job (jobAlias is denormalized onto each row for cross-job lint).
func BuildDeclarations(def *schema.Definition, jobID uuid.UUID, jobAlias string) ([]models.DatasetDeclaration, error) {
	if def == nil {
		return nil, nil
	}
	alias := strings.TrimSpace(jobAlias)
	if alias == "" {
		alias = strings.TrimSpace(def.Metadata.Alias)
	}

	decls := make([]models.DatasetDeclaration, 0)
	skipWhenFresh := schema.SkipWhenFreshEnabled(def.Metadata.Datasets)
	skipWhenFreshPtr := boolPtr(skipWhenFresh)

	if def.Metadata.Datasets != nil {
		for i := range def.Metadata.Datasets.Sources {
			src := &def.Metadata.Datasets.Sources[i]
			name := strings.TrimSpace(src.Name)
			if name == "" {
				continue
			}
			binding, err := marshalArrivalBinding(src.Arrival)
			if err != nil {
				return nil, err
			}
			decls = append(decls, models.DatasetDeclaration{
				ID:             uuid.New(),
				JobID:          jobID,
				JobAlias:       alias,
				Name:           name,
				Direction:      models.DatasetDirectionSource,
				ExpectedEvery:  strings.TrimSpace(src.ExpectedEvery),
				External:       src.External,
				ArrivalBinding: binding,
				SkipWhenFresh:  skipWhenFreshPtr,
			})
		}
	}

	for i := range def.Steps {
		step := &def.Steps[i]
		if step.Datasets == nil {
			continue
		}
		for j := range step.Datasets.Produces {
			p := &step.Datasets.Produces[j]
			name := strings.TrimSpace(p.Name)
			if name == "" {
				continue
			}
			schemaJSON, err := marshalInlineSchema(p.Schema)
			if err != nil {
				return nil, fmt.Errorf("steps[%d].datasets.produces[%d].schema: %w", i, j, err)
			}
			watermarkKey := ""
			if p.Watermark != nil {
				watermarkKey = strings.TrimSpace(p.Watermark.Key)
			}
			decls = append(decls, models.DatasetDeclaration{
				ID:            uuid.New(),
				JobID:         jobID,
				JobAlias:      alias,
				StepName:      step.Name,
				Name:          name,
				Direction:     models.DatasetDirectionProduces,
				SchemaJSON:    schemaJSON,
				SchemaFrom:    strings.TrimSpace(p.SchemaFrom),
				SchemaVersion: p.Version,
				Freshness:     strings.TrimSpace(p.Freshness),
				MaxStaleness:  strings.TrimSpace(p.MaxStaleness),
				WatermarkKey:  watermarkKey,
				SkipWhenFresh: skipWhenFreshPtr,
			})
		}
		for j := range step.Datasets.Consumes {
			consumed := &step.Datasets.Consumes[j]
			name := strings.TrimSpace(consumed.Name)
			if name == "" {
				continue
			}
			schemaJSON, err := marshalInlineSchema(consumed.Schema)
			if err != nil {
				return nil, fmt.Errorf("steps[%d].datasets.consumes[%d].schema: %w", i, j, err)
			}
			decls = append(decls, models.DatasetDeclaration{
				ID:            uuid.New(),
				JobID:         jobID,
				JobAlias:      alias,
				StepName:      step.Name,
				Name:          name,
				Direction:     models.DatasetDirectionConsumes,
				SchemaJSON:    schemaJSON,
				SkipWhenFresh: skipWhenFreshPtr,
			})
		}
	}

	return decls, nil
}

func marshalInlineSchema(value map[string]any) (string, error) {
	if len(value) == 0 {
		return "", nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func boolPtr(value bool) *bool {
	v := value
	return &v
}

func marshalArrivalBinding(arrival *schema.Arrival) (datatypes.JSON, error) {
	if arrival == nil {
		return nil, nil
	}
	data, err := json.Marshal(arrival)
	if err != nil {
		return nil, err
	}
	return datatypes.JSON(data), nil
}

// ReplaceForJobTx rebuilds a single job's declarations inside an existing
// transaction: it hard-deletes the job's current rows and inserts the supplied
// set. This is the per-apply upsert seam — rebuilding from the manifest means a
// declaration removed from the manifest is pruned. Passing an empty slice
// clears the job's declarations (a job that dropped its datasets surface).
func ReplaceForJobTx(tx *gorm.DB, jobID uuid.UUID, decls []models.DatasetDeclaration) error {
	if err := tx.Where("job_id = ?", jobID).Delete(&models.DatasetDeclaration{}).Error; err != nil {
		return err
	}
	if len(decls) == 0 {
		return nil
	}
	return tx.Create(&decls).Error
}

// DeleteForJobsTx removes all declarations for the given jobs inside an existing
// transaction. Used when jobs are retired/pruned so the declared graph never
// references a job that no longer exists.
func DeleteForJobsTx(tx *gorm.DB, jobIDs []uuid.UUID) error {
	if len(jobIDs) == 0 {
		return nil
	}
	return tx.Where("job_id IN ?", jobIDs).Delete(&models.DatasetDeclaration{}).Error
}

// ListAll returns every declaration in the registry.
func (r *Registry) ListAll(ctx context.Context) ([]models.DatasetDeclaration, error) {
	var out []models.DatasetDeclaration
	if err := r.db.WithContext(ctx).Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// ListArrivalSources returns source declarations that carry an arrival binding.
func (r *Registry) ListArrivalSources(ctx context.Context) ([]models.DatasetDeclaration, error) {
	var out []models.DatasetDeclaration
	if err := r.db.WithContext(ctx).
		Where("direction = ? AND arrival_binding IS NOT NULL", models.DatasetDirectionSource).
		Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// ListByJob returns the declarations owned by a single job.
func (r *Registry) ListByJob(ctx context.Context, jobID uuid.UUID) ([]models.DatasetDeclaration, error) {
	var out []models.DatasetDeclaration
	if err := r.db.WithContext(ctx).Where("job_id = ?", jobID).Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}
