package freshness

import (
	"errors"
	"slices"
	"strings"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Producer is one live job that declares it produces a dataset: the owner of
// the dataset's tenancy (identity-and-access design §5.1, plan item B6).
type Producer struct {
	JobID     uuid.UUID
	Alias     string
	Namespace string
}

// ProducersTx returns the live jobs that declare they produce the dataset
// (datasetNamespace, name), read on db (a caller's transaction or a plain
// connection), ordered by alias. The dataset identity is matched with a blank
// dataset namespace equal to NULL or the empty string, exactly as the auth
// middleware's DatasetProducerNamespaces matches it. Namespace is the job's
// current tenancy namespace ("default" for a legacy blank).
func ProducersTx(db *gorm.DB, datasetNamespace, name string) ([]Producer, error) {
	return producersTx(db, datasetNamespace, name, false)
}

// LockProducersTx is ProducersTx inside a write transaction: on PostgreSQL the
// producers' declaration and job rows are read FOR SHARE, so a concurrent
// delete, prune or move of a producer either committed before the read or
// waits for the caller's commit (identity-and-access B6). SQLite and dqlite
// serialise write transactions and take no row locks.
func LockProducersTx(tx *gorm.DB, datasetNamespace, name string) ([]Producer, error) {
	return producersTx(tx, datasetNamespace, name, tx.Name() == "postgres")
}

func producersTx(db *gorm.DB, datasetNamespace, name string, share bool) ([]Producer, error) {
	var rows []struct {
		JobID     uuid.UUID
		Alias     string
		Namespace string
	}
	q := db.Table("dataset_declarations").
		Select("jobs.id AS job_id, jobs.alias AS alias, jobs.namespace AS namespace").
		Joins("JOIN jobs ON jobs.id = dataset_declarations.job_id AND jobs.deleted_at IS NULL").
		Where("dataset_declarations.name = ? AND dataset_declarations.direction = ?", strings.TrimSpace(name), models.DatasetDirectionProduces)
	if ns := strings.TrimSpace(datasetNamespace); ns == "" {
		q = q.Where("(dataset_declarations.namespace IS NULL OR dataset_declarations.namespace = '')")
	} else {
		q = q.Where("dataset_declarations.namespace = ?", ns)
	}
	if share {
		q = q.Clauses(clause.Locking{Strength: clause.LockingStrengthShare})
	}
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]Producer, 0, len(rows))
	seen := make(map[uuid.UUID]struct{}, len(rows))
	for _, row := range rows {
		if _, dup := seen[row.JobID]; dup {
			continue
		}
		seen[row.JobID] = struct{}{}
		out = append(out, Producer{JobID: row.JobID, Alias: row.Alias, Namespace: models.NamespaceOrDefault(row.Namespace)})
	}
	slices.SortFunc(out, func(a, b Producer) int { return strings.Compare(a.Alias, b.Alias) })
	return out, nil
}

// jobNamespaceTx returns the current tenancy namespace of a live job, or nil
// when the job no longer exists (a decision about a job deleted mid-tick,
// whose record then carries no namespace and is hidden from every
// namespace-confined reader).
func jobNamespaceTx(db *gorm.DB, jobID uuid.UUID) (*string, error) {
	if jobID == uuid.Nil {
		return nil, nil
	}
	var rows []string
	if err := db.Model(&models.Job{}).Where("id = ?", jobID).Limit(1).Pluck("namespace", &rows).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	namespace := models.NamespaceOrDefault(rows[0])
	return &namespace, nil
}
