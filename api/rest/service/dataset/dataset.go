// Package dataset exposes the freshness dataset read model and manual advance
// operation used by the REST controller and operator CLI.
package dataset

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/freshness"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

const (
	defaultListLimit = 50
	maxListLimit     = 200
)

var declarationDirections = []string{
	models.DatasetDirectionProduces,
	models.DatasetDirectionSource,
}

// Service wraps read-side dataset queries and the manual advance path.
type Service struct {
	ctx context.Context
	db  *gorm.DB
}

// New creates a Service backed by the default DB connection.
func New(ctx context.Context) *Service {
	return &Service{ctx: ctx, db: db.Connection()}
}

// WithDatabase returns a copy of the Service backed by conn; used by tests.
func (s *Service) WithDatabase(conn *gorm.DB) *Service {
	if conn == nil {
		return s
	}
	return &Service{ctx: s.ctx, db: conn}
}

// NamespaceFromPath maps the REST path convention "_" to the v1 empty
// namespace. All other values are trimmed and used literally.
func NamespaceFromPath(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "_" {
		return ""
	}
	return raw
}

// ListParams filters and paginates the dataset feed.
type ListParams struct {
	Status string
	Limit  int
	Offset int
}

// ListResult is the paginated dataset state response.
type ListResult struct {
	Datasets []models.DatasetState `json:"datasets"`
	Total    int64                 `json:"total"`
	Limit    int                   `json:"limit"`
	Offset   int                   `json:"offset"`
}

// SLO summarizes the declaration-level freshness contract for a dataset.
type SLO struct {
	Freshness     string `json:"freshness,omitempty"`
	MaxStaleness  string `json:"max_staleness,omitempty"`
	ExpectedEvery string `json:"expected_every,omitempty"`
}

// ProducingJob identifies the Caesium job and step that produce a dataset.
type ProducingJob struct {
	ID       uuid.UUID `json:"id"`
	Alias    string    `json:"alias"`
	StepName string    `json:"step_name,omitempty"`
}

// Detail returns the state row plus the declaration metadata operators need to
// understand the SLO and producer.
type Detail struct {
	State        models.DatasetState        `json:"state"`
	Declaration  *models.DatasetDeclaration `json:"declaration,omitempty"`
	SLO          *SLO                       `json:"slo,omitempty"`
	Producing    *ProducingJob              `json:"producing_job,omitempty"`
	LastDecision *models.DatasetDerivation  `json:"last_decision,omitempty"`
}

// DerivationsParams filters and paginates the append-only derivation audit.
type DerivationsParams struct {
	Namespace string
	Name      string
	Limit     int
	Offset    int
}

// DerivationsResult is the paginated derivation audit response.
type DerivationsResult struct {
	Derivations []models.DatasetDerivation `json:"derivations"`
	Total       int64                      `json:"total"`
	Limit       int                        `json:"limit"`
	Offset      int                        `json:"offset"`
}

// AdvanceParams carries a manual dataset arrival.
type AdvanceParams struct {
	Namespace string
	Name      string
	Watermark string
}

// AdvanceResult is the manual advance outcome plus the resulting state row.
type AdvanceResult struct {
	Outcome freshness.Outcome   `json:"outcome"`
	State   models.DatasetState `json:"state"`
}

// List returns a bounded, paginated, filtered slice of dataset states
// newest-first. Declared-but-unobserved produced/source datasets are surfaced as
// synthetic unknown state so operators can see the declared graph before a run
// advances anything.
func (s *Service) List(p ListParams) (*ListResult, error) {
	limit, offset := normalizePagination(p.Limit, p.Offset)
	status := strings.TrimSpace(p.Status)

	if status != "" && status != models.DatasetStatusUnknown {
		rows, total, err := s.observedStates(status, limit, offset)
		if err != nil {
			return nil, err
		}
		return &ListResult{
			Datasets: rows,
			Total:    total,
			Limit:    limit,
			Offset:   offset,
		}, nil
	}

	fetchLimit := limit + offset
	rows, observedTotal, err := s.observedStates(status, fetchLimit, 0)
	if err != nil {
		return nil, err
	}

	declRows, declTotal, err := s.declarationOnlyStates(fetchLimit)
	if err != nil {
		return nil, err
	}
	rows = append(rows, declRows...)
	sortStates(rows)

	rows = paginateStates(rows, limit, offset)
	return &ListResult{
		Datasets: rows,
		Total:    observedTotal + declTotal,
		Limit:    limit,
		Offset:   offset,
	}, nil
}

// Get returns one dataset's state plus declaration metadata. A declared dataset
// with no state row is served as unknown rather than 404.
func (s *Service) Get(namespace, name string) (*Detail, error) {
	name = strings.TrimSpace(name)
	state, foundState, err := s.getState(namespace, name)
	if err != nil {
		return nil, err
	}

	decl, foundDecl, err := s.getDeclaration(namespace, name)
	if err != nil {
		return nil, err
	}
	if !foundState && !foundDecl {
		return nil, gorm.ErrRecordNotFound
	}
	if !foundState {
		state = unknownState(namespace, name, decl.CreatedAt, decl.UpdatedAt)
	}

	detail := &Detail{State: state}
	if foundDecl {
		detail.Declaration = &decl
		detail.SLO = &SLO{
			Freshness:     decl.Freshness,
			MaxStaleness:  decl.MaxStaleness,
			ExpectedEvery: decl.ExpectedEvery,
		}
		if decl.Direction == models.DatasetDirectionProduces {
			detail.Producing = &ProducingJob{
				ID:       decl.JobID,
				Alias:    decl.JobAlias,
				StepName: decl.StepName,
			}
		}
	}
	derivation, foundDerivation, err := s.latestDerivation(namespace, name)
	if err != nil {
		return nil, err
	}
	if foundDerivation {
		detail.LastDecision = &derivation
	}
	return detail, nil
}

// Derivations returns the append-only derivation audit newest-first.
func (s *Service) Derivations(p DerivationsParams) (*DerivationsResult, error) {
	limit, offset := normalizePagination(p.Limit, p.Offset)
	exists, err := s.exists(p.Namespace, p.Name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, gorm.ErrRecordNotFound
	}

	q := s.db.WithContext(s.ctx).
		Model(&models.DatasetDerivation{}).
		Where("name = ?", strings.TrimSpace(p.Name))
	q = withDerivationNamespace(q, p.Namespace)

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, err
	}

	var rows []models.DatasetDerivation
	if err := q.
		Order("created_at DESC").
		Order("id DESC").
		Limit(limit).
		Offset(offset).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return &DerivationsResult{
		Derivations: rows,
		Total:       total,
		Limit:       limit,
		Offset:      offset,
	}, nil
}

// Advance applies a manual arrival through the freshness.Store contract.
func (s *Service) Advance(p AdvanceParams) (*AdvanceResult, error) {
	now := time.Now().UTC()
	result, err := freshness.NewStore(s.db).Advance(s.ctx, freshness.AdvanceInput{
		Namespace:   namespacePtr(p.Namespace),
		Name:        strings.TrimSpace(p.Name),
		Watermark:   strings.TrimSpace(p.Watermark),
		RunID:       uuid.Nil,
		CompletedAt: now,
		RunOrder:    now,
	})
	if err != nil {
		return nil, err
	}
	return &AdvanceResult{Outcome: result.Outcome, State: result.State}, nil
}

func (s *Service) observedStates(status string, limit, offset int) ([]models.DatasetState, int64, error) {
	q := s.db.WithContext(s.ctx).Model(&models.DatasetState{})
	if status != "" {
		q = q.Where("status = ?", status)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var rows []models.DatasetState
	if err := q.
		Order("updated_at DESC").
		Limit(limit).
		Offset(offset).
		Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

func normalizePagination(rawLimit, rawOffset int) (int, int) {
	limit := rawLimit
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	offset := rawOffset
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

func (s *Service) declarationOnlyStates(limit int) ([]models.DatasetState, int64, error) {
	// A dataset is commonly declared by multiple jobs; we want distinct
	// (namespace, name) datasets so the LIMIT and total reflect datasets rather
	// than raw declaration rows. We dedup in Go rather than via a SQL GROUP BY:
	// dqlite cannot return computed/aggregate result columns (MAX(...),
	// COALESCE(...)) and fails such a query with "unknown data type". The
	// declared-but-stateless set is bounded (one row per job/dataset
	// declaration), so fetching plain columns and deduping in-process is cheap.
	var decls []models.DatasetDeclaration
	if err := s.db.WithContext(s.ctx).
		Model(&models.DatasetDeclaration{}).
		Where("direction IN ?", declarationDirections).
		Where("NOT EXISTS (SELECT 1 FROM dataset_states ds WHERE ds.name = dataset_declarations.name AND ds.namespace = COALESCE(dataset_declarations.namespace, ''))").
		Order("updated_at DESC").
		Find(&decls).Error; err != nil {
		return nil, 0, err
	}

	seen := make(map[string]struct{}, len(decls))
	out := make([]models.DatasetState, 0)
	for _, decl := range decls {
		ns := declNamespaceValue(decl.Namespace)
		key := ns + "\x00" + decl.Name
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		// updated_at DESC ordering means the first row for a (ns, name) is the
		// most recently touched declaration — mirrors the old MAX(updated_at).
		if len(out) < limit {
			out = append(out, unknownState(ns, decl.Name, decl.CreatedAt, decl.UpdatedAt))
		}
	}
	return out, int64(len(seen)), nil
}

func (s *Service) getState(namespace, name string) (models.DatasetState, bool, error) {
	var state models.DatasetState
	res := s.db.WithContext(s.ctx).
		Where("namespace = ? AND name = ?", strings.TrimSpace(namespace), strings.TrimSpace(name)).
		Limit(1).
		Find(&state)
	if res.Error != nil {
		return models.DatasetState{}, false, res.Error
	}
	return state, res.RowsAffected > 0, nil
}

func (s *Service) getDeclaration(namespace, name string) (models.DatasetDeclaration, bool, error) {
	name = strings.TrimSpace(name)
	for _, direction := range declarationDirections {
		var decl models.DatasetDeclaration
		q := s.db.WithContext(s.ctx).
			Where("name = ? AND direction = ?", name, direction).
			Limit(1)
		q = withDeclarationNamespace(q, namespace)
		res := q.Find(&decl)
		if res.Error != nil {
			return models.DatasetDeclaration{}, false, res.Error
		}
		if res.RowsAffected > 0 {
			return decl, true, nil
		}
	}
	return models.DatasetDeclaration{}, false, nil
}

func (s *Service) latestDerivation(namespace, name string) (models.DatasetDerivation, bool, error) {
	var derivation models.DatasetDerivation
	q := s.db.WithContext(s.ctx).
		Where("name = ?", strings.TrimSpace(name)).
		Order("created_at DESC").
		Order("id DESC").
		Limit(1)
	q = withDerivationNamespace(q, namespace)
	res := q.Find(&derivation)
	if res.Error != nil {
		return models.DatasetDerivation{}, false, res.Error
	}
	return derivation, res.RowsAffected > 0, nil
}

func (s *Service) exists(namespace, name string) (bool, error) {
	if _, ok, err := s.getState(namespace, name); err != nil || ok {
		return ok, err
	}
	_, ok, err := s.getDeclaration(namespace, name)
	return ok, err
}

func withDeclarationNamespace(q *gorm.DB, namespace string) *gorm.DB {
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return q.Where("(namespace IS NULL OR namespace = '')")
	}
	return q.Where("namespace = ?", namespace)
}

func withDerivationNamespace(q *gorm.DB, namespace string) *gorm.DB {
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return q.Where("(namespace IS NULL OR namespace = '')")
	}
	return q.Where("namespace = ?", namespace)
}

func unknownState(namespace, name string, createdAt, updatedAt time.Time) models.DatasetState {
	return models.DatasetState{
		Namespace: strings.TrimSpace(namespace),
		Name:      strings.TrimSpace(name),
		Status:    models.DatasetStatusUnknown,
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}
}

func namespacePtr(namespace string) *string {
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return nil
	}
	return &namespace
}

func declNamespaceValue(namespace *string) string {
	if namespace == nil {
		return ""
	}
	return strings.TrimSpace(*namespace)
}

func paginateStates(rows []models.DatasetState, limit, offset int) []models.DatasetState {
	if offset >= len(rows) {
		return []models.DatasetState{}
	}
	end := offset + limit
	if end > len(rows) {
		end = len(rows)
	}
	return rows[offset:end]
}

func sortStates(rows []models.DatasetState) {
	sort.SliceStable(rows, func(i, j int) bool {
		left := rows[i].UpdatedAt
		right := rows[j].UpdatedAt
		if left.Equal(right) {
			if rows[i].Namespace == rows[j].Namespace {
				return rows[i].Name < rows[j].Name
			}
			return rows[i].Namespace < rows[j].Namespace
		}
		return left.After(right)
	})
}
