package auth

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// Namespace lookups for route authorisation (identity-and-access A3). Each one
// reads the authoritative row the handler acts on, on the read connection,
// and returns its namespace normalised with models.NamespaceOrDefault (a
// legacy empty value means "default"; "" is never returned, because
// Principal.RoleIn("") is the cluster role). A missing row is
// gorm.ErrRecordNotFound; any other error is a lookup failure the caller must
// treat as a denial.

// ErrNoAuthDatabase reports a lookup on a Service built without a database.
var ErrNoAuthDatabase = errors.New("auth service has no database")

// SharedObjectKind names a table of shared objects (design §10.4).
type SharedObjectKind string

const (
	SharedObjectNotificationChannel SharedObjectKind = "notification_channel"
	SharedObjectNotificationPolicy  SharedObjectKind = "notification_policy"
	SharedObjectAgentProfile        SharedObjectKind = "agent_profile"
)

func (s *Service) readDB(ctx context.Context) (*gorm.DB, error) {
	if s == nil || s.db == nil {
		return nil, ErrNoAuthDatabase
	}
	return s.db.WithContext(ctx), nil
}

// JobNamespace returns the namespace of a live (not soft-deleted) job.
func (s *Service) JobNamespace(ctx context.Context, id uuid.UUID) (string, error) {
	conn, err := s.readDB(ctx)
	if err != nil {
		return "", err
	}
	var job models.Job
	err = s.withReadRetry(func() error {
		return conn.Select("id", "namespace").First(&job, "id = ?", id).Error
	})
	if err != nil {
		return "", err
	}
	return models.NamespaceOrDefault(job.Namespace), nil
}

// JobNamespaceByIDPrefix resolves a job by full ID or unique canonical ID
// prefix with models.FindJobByIDPrefix (the lookup the job handlers use) and
// returns its ID and namespace. ErrInvalidJobIDPrefix and
// ErrAmbiguousJobIDPrefix pass through unchanged.
func (s *Service) JobNamespaceByIDPrefix(ctx context.Context, raw string) (uuid.UUID, string, error) {
	conn, err := s.readDB(ctx)
	if err != nil {
		return uuid.Nil, "", err
	}
	var job *models.Job
	err = s.withReadRetry(func() error {
		var lookupErr error
		job, lookupErr = models.FindJobByIDPrefix(conn, raw)
		return lookupErr
	})
	if err != nil {
		return uuid.Nil, "", err
	}
	return job.ID, models.NamespaceOrDefault(job.Namespace), nil
}

// RunNamespace returns the job a run belongs to and the run's persisted
// namespace (which stays put when the job later moves).
func (s *Service) RunNamespace(ctx context.Context, id uuid.UUID) (uuid.UUID, string, error) {
	conn, err := s.readDB(ctx)
	if err != nil {
		return uuid.Nil, "", err
	}
	var run models.JobRun
	err = s.withReadRetry(func() error {
		return conn.Select("id", "job_id", "namespace").First(&run, "id = ?", id).Error
	})
	if err != nil {
		return uuid.Nil, "", err
	}
	return run.JobID, models.NamespaceOrDefault(run.Namespace), nil
}

// BackfillNamespace returns the job a backfill belongs to and the backfill's
// persisted namespace.
func (s *Service) BackfillNamespace(ctx context.Context, id uuid.UUID) (uuid.UUID, string, error) {
	conn, err := s.readDB(ctx)
	if err != nil {
		return uuid.Nil, "", err
	}
	var backfill models.Backfill
	err = s.withReadRetry(func() error {
		return conn.Select("id", "job_id", "namespace").First(&backfill, "id = ?", id).Error
	})
	if err != nil {
		return uuid.Nil, "", err
	}
	return backfill.JobID, models.NamespaceOrDefault(backfill.Namespace), nil
}

// TriggerJobNamespaces returns the sorted namespaces of every live job the
// trigger serves. A trigger that exists but serves no job returns an empty,
// non-nil slice; a missing trigger is gorm.ErrRecordNotFound.
func (s *Service) TriggerJobNamespaces(ctx context.Context, id uuid.UUID) ([]string, error) {
	conn, err := s.readDB(ctx)
	if err != nil {
		return nil, err
	}
	var namespaces []string
	err = s.withReadRetry(func() error {
		var trigger models.Trigger
		if err := conn.Select("id").First(&trigger, "id = ?", id).Error; err != nil {
			return err
		}
		namespaces = nil
		return conn.Model(&models.Job{}).Where("trigger_id = ?", id).Pluck("namespace", &namespaces).Error
	})
	if err != nil {
		return nil, err
	}
	return normalizeNamespaceSet(namespaces), nil
}

// IncidentNamespace returns an incident's namespace (the failing run's
// ownership, fixed when the incident opened).
func (s *Service) IncidentNamespace(ctx context.Context, id uuid.UUID) (string, error) {
	conn, err := s.readDB(ctx)
	if err != nil {
		return "", err
	}
	var incident models.Incident
	err = s.withReadRetry(func() error {
		return conn.Select("id", "namespace").First(&incident, "id = ?", id).Error
	})
	if err != nil {
		return "", err
	}
	return models.NamespaceOrDefault(incident.Namespace), nil
}

// SharedObjectNamespace returns a live shared object's namespace;
// models.ClusterNamespace ("*") marks a cluster-shared object.
func (s *Service) SharedObjectNamespace(ctx context.Context, kind SharedObjectKind, id uuid.UUID) (string, error) {
	conn, err := s.readDB(ctx)
	if err != nil {
		return "", err
	}
	var model any
	switch kind {
	case SharedObjectNotificationChannel:
		model = &models.NotificationChannel{}
	case SharedObjectNotificationPolicy:
		model = &models.NotificationPolicy{}
	case SharedObjectAgentProfile:
		model = &models.AgentProfile{}
	default:
		return "", errors.New("unknown shared object kind " + string(kind))
	}
	var namespaces []string
	err = s.withReadRetry(func() error {
		namespaces = nil
		return conn.Model(model).Where("id = ?", id).Limit(1).Pluck("namespace", &namespaces).Error
	})
	if err != nil {
		return "", err
	}
	if len(namespaces) == 0 {
		return "", gorm.ErrRecordNotFound
	}
	return models.NamespaceOrDefault(namespaces[0]), nil
}

// DatasetProducerNamespaces returns the sorted namespaces of the live jobs that
// declare they produce the dataset (datasetNamespace, name): the dataset's
// tenancy (design §5.1). A consumer or a job declaring the dataset as an
// external source does not own it, so a dataset with no declared producer
// returns an empty, non-nil slice (cluster-only). The dataset identity is
// matched the way the dataset service matches a declaration: a blank dataset
// namespace matches NULL or the empty string.
func (s *Service) DatasetProducerNamespaces(ctx context.Context, datasetNamespace, name string) ([]string, error) {
	conn, err := s.readDB(ctx)
	if err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	datasetNamespace = strings.TrimSpace(datasetNamespace)
	var namespaces []string
	err = s.withReadRetry(func() error {
		namespaces = nil
		q := conn.Model(&models.DatasetDeclaration{}).
			Joins("JOIN jobs ON jobs.id = dataset_declarations.job_id AND jobs.deleted_at IS NULL").
			Where("dataset_declarations.name = ? AND dataset_declarations.direction = ?", name, models.DatasetDirectionProduces)
		if datasetNamespace == "" {
			q = q.Where("(dataset_declarations.namespace IS NULL OR dataset_declarations.namespace = '')")
		} else {
			q = q.Where("dataset_declarations.namespace = ?", datasetNamespace)
		}
		return q.Pluck("jobs.namespace", &namespaces).Error
	})
	if err != nil {
		return nil, err
	}
	return normalizeNamespaceSet(namespaces), nil
}

// HoldProducerNamespaces returns the producer namespaces (DatasetProducerNamespaces) of
// the dataset a hold was opened on. A missing hold is gorm.ErrRecordNotFound.
func (s *Service) HoldProducerNamespaces(ctx context.Context, holdID uuid.UUID) ([]string, error) {
	conn, err := s.readDB(ctx)
	if err != nil {
		return nil, err
	}
	var hold models.DatasetHold
	err = s.withReadRetry(func() error {
		return conn.Select("id", "namespace", "name").First(&hold, "id = ?", holdID).Error
	})
	if err != nil {
		return nil, err
	}
	return s.DatasetProducerNamespaces(ctx, hold.Namespace, hold.Name)
}

// KeyAuditNamespace returns the audit namespace of a key: "*" for a
// cluster-wide key (SQL NULL namespaces, or a list naming "*"), the namespace
// for a key confined to exactly one, and "*" for anything spanning several or
// unreadable, so no namespace-confined reader sees a key that reaches beyond
// its namespace.
func KeyAuditNamespace(namespaces datatypes.JSON) string {
	if len(namespaces) == 0 {
		return models.ClusterNamespace
	}
	var list []string
	if err := json.Unmarshal(namespaces, &list); err != nil {
		return models.ClusterNamespace
	}
	set := normalizeNamespaceSet(list)
	if len(set) == 1 && set[0] != models.ClusterNamespace {
		return set[0]
	}
	return models.ClusterNamespace
}

// KeyAuditNamespaceByID is KeyAuditNamespace for a stored key, best-effort:
// a lookup failure returns "" so the caller's audit row is still written.
func (s *Service) KeyAuditNamespaceByID(ctx context.Context, id uuid.UUID) string {
	conn, err := s.readDB(ctx)
	if err != nil {
		return ""
	}
	var key models.APIKey
	if err := s.withReadRetry(func() error {
		return conn.Select("id", "namespaces").First(&key, "id = ?", id).Error
	}); err != nil {
		return ""
	}
	return KeyAuditNamespace(key.Namespaces)
}

// normalizeNamespaceSet maps legacy empty values to the default namespace and
// returns the sorted, de-duplicated set (never nil).
func normalizeNamespaceSet(namespaces []string) []string {
	out := make([]string, 0, len(namespaces))
	for _, namespace := range namespaces {
		out = append(out, models.NamespaceOrDefault(strings.TrimSpace(namespace)))
	}
	slices.Sort(out)
	return slices.Compact(out)
}
