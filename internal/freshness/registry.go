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
	"github.com/caesium-cloud/caesium/pkg/ptr"
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
	skipWhenFreshPtr := ptr.Of(skipWhenFresh)

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
			// The data-circuit-breaker spec rides the SAME registry row as the
			// freshness SLO (docs/design/data-circuit-breaker.md: one registry, no
			// private copy), so it is rebuilt from the manifest on every apply
			// exactly like the schema and SLO columns.
			assertionsJSON, err := marshalAssertions(p.Assertions)
			if err != nil {
				return nil, fmt.Errorf("steps[%d].datasets.produces[%d].assertions: %w", i, j, err)
			}
			decls = append(decls, models.DatasetDeclaration{
				ID:             uuid.New(),
				JobID:          jobID,
				JobAlias:       alias,
				StepName:       step.Name,
				Name:           name,
				Direction:      models.DatasetDirectionProduces,
				SchemaJSON:     schemaJSON,
				SchemaFrom:     strings.TrimSpace(p.SchemaFrom),
				SchemaVersion:  p.Version,
				Freshness:      strings.TrimSpace(p.Freshness),
				MaxStaleness:   strings.TrimSpace(p.MaxStaleness),
				WatermarkKey:   watermarkKey,
				AssertionsJSON: assertionsJSON,
				OnViolation:    strings.TrimSpace(p.OnViolation),
				Release:        strings.TrimSpace(p.Release),
				SkipWhenFresh:  skipWhenFreshPtr,
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

// marshalAssertions renders the declared assertion block for the registry
// column. An empty block stores the empty string (not "null"), so a reader can
// treat "no assertions" as a plain empty check.
func marshalAssertions(value *schema.DatasetAssertions) (string, error) {
	if value.IsEmpty() {
		return "", nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(data), nil
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

// ListAll returns every declaration in the registry, each carrying its job's
// tenancy namespace as of the same read (see listWithOwners).
func (r *Registry) ListAll(ctx context.Context) ([]models.DatasetDeclaration, error) {
	return listWithOwners(r.db.WithContext(ctx))
}

// declarationWithOwner is a declaration row plus its job's namespace, read in
// one statement.
type declarationWithOwner struct {
	models.DatasetDeclaration
	OwnerJobID     *uuid.UUID `gorm:"column:owner_job_id"`
	OwnerNamespace *string    `gorm:"column:owner_namespace"`
}

// listWithOwners reads the declarations q selects together with their live
// job's namespace in ONE statement, and records it on each declaration's Job
// (ID and Namespace only): the owner of everything an evaluation computes from
// that snapshot (identity-and-access B6). Reading the namespace later would
// let an apply that moves and re-declares the producer in between stamp the
// old declarations' reason and inputs as the new namespace's. A declaration
// whose job is gone keeps a zero Job, and its decisions carry no namespace.
func listWithOwners(q *gorm.DB) ([]models.DatasetDeclaration, error) {
	var rows []declarationWithOwner
	if err := q.Table("dataset_declarations").
		Select("dataset_declarations.*, jobs.id AS owner_job_id, jobs.namespace AS owner_namespace").
		Joins("LEFT JOIN jobs ON jobs.id = dataset_declarations.job_id AND jobs.deleted_at IS NULL").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]models.DatasetDeclaration, len(rows))
	for i, row := range rows {
		out[i] = row.DatasetDeclaration
		// Every declaration read here is marked as snapshotted (Job.ID set);
		// one whose job was not live at the read is marked ownerless, so a
		// job revived in another namespace afterwards cannot claim it.
		out[i].Job = models.Job{ID: row.JobID, Namespace: ownerlessSnapshot}
		if row.OwnerJobID != nil && *row.OwnerJobID == row.JobID && row.OwnerNamespace != nil {
			out[i].Job.Namespace = *row.OwnerNamespace
		}
	}
	return out, nil
}

// ownerlessSnapshot marks a declaration listWithOwners read while its job was
// not live. It is not a valid namespace name.
const ownerlessSnapshot = "\x00ownerless"

// snapshotOwner reports the tenancy namespace listWithOwners recorded on decl.
// snapshotted is false for a declaration not read through it (the caller then
// resolves the owner itself); owner is nil for one read while its job was not
// live, whose decisions carry no namespace and admit no run.
func snapshotOwner(decl models.DatasetDeclaration) (owner *string, snapshotted bool) {
	if decl.JobID == uuid.Nil || decl.Job.ID != decl.JobID {
		return nil, false
	}
	if decl.Job.Namespace == ownerlessSnapshot {
		return nil, true
	}
	namespace := models.NamespaceOrDefault(decl.Job.Namespace)
	return &namespace, true
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
	return listWithOwners(r.db.WithContext(ctx).Where("dataset_declarations.job_id = ?", jobID))
}
