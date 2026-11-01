// Package dataset exposes dataset freshness, circuit-breaker reads, and operator
// actions used by the REST controller and CLI.
package dataset

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/freshness"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/caesium-cloud/caesium/pkg/env"
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

// NewWithDatabase creates a Service backed by conn; used by tests.
func NewWithDatabase(ctx context.Context, conn *gorm.DB) *Service {
	return &Service{ctx: ctx, db: conn}
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
	Datasets []State `json:"datasets"`
	Total    int64   `json:"total"`
	Limit    int     `json:"limit"`
	Offset   int     `json:"offset"`
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
	HoldStatus   string                     `json:"hold_status,omitempty"`
	Hold         *models.DatasetHold        `json:"hold,omitempty"`
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
	// Principal is the authenticated caller (nil when auth is off). A
	// namespace-confined one is re-checked inside the write (B6).
	Principal *auth.Principal
}

// AdvanceRole and ReleaseRole are the roles the manual advance and the hold
// release require (internal/auth's route table); the in-transaction producer
// re-check of a confined caller requires them in every producer namespace.
const (
	AdvanceRole = models.RoleRunner
	ReleaseRole = models.RoleOperator
)

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

	// Declared-but-unobserved datasets (no state row) are merged in for the
	// unfiltered feed and for `unknown`; for `held` too while the circuit breaker
	// is on, because a hold can outlive - or precede - the state row indefinitely
	// (assertions on, freshness scheduling off: nothing ever creates one).
	mergeDeclared := status == "" || status == models.DatasetStatusUnknown ||
		(status == models.DatasetStatusHeld && env.Variables().DataAssertionsEnabled)
	if !mergeDeclared {
		rows, total, err := s.observedStates(status, limit, offset)
		if err != nil {
			return nil, err
		}
		states, err := s.withHolds(rows)
		if err != nil {
			return nil, err
		}
		return &ListResult{
			Datasets: states,
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

	declRows, declTotal, err := s.declarationOnlyStates(status, fetchLimit)
	if err != nil {
		return nil, err
	}
	rows = append(rows, declRows...)
	sortStates(rows)

	rows = paginateStates(rows, limit, offset)
	states, err := s.withHolds(rows)
	if err != nil {
		return nil, err
	}
	return &ListResult{
		Datasets: states,
		Total:    observedTotal + declTotal,
		Limit:    limit,
		Offset:   offset,
	}, nil
}

// Get returns one dataset's state plus declaration metadata. A declared dataset
// with no state row is served as unknown rather than 404.
func (s *Service) Get(namespace, name string) (*Detail, error) {
	return s.GetWithOptions(namespace, name, GetOptions{IncludeHold: true})
}

// GetOptions controls optional detail evidence. Metadata polling can omit the
// hold lookup because the corresponding list row already carries its summary.
type GetOptions struct {
	IncludeHold bool
}

// GetWithOptions returns dataset detail with optional active hold evidence.
func (s *Service) GetWithOptions(namespace, name string, options GetOptions) (*Detail, error) {
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
	v := viewerFrom(s.ctx)
	if err := s.shapeStates(v, []*models.DatasetState{&state}); err != nil {
		return nil, err
	}

	detail := &Detail{State: state}
	if !env.Variables().DataAssertionsEnabled {
		unstrandHeld(&detail.State)
	}
	if options.IncludeHold && env.Variables().DataAssertionsEnabled {
		var hold models.DatasetHold
		err := s.db.WithContext(s.ctx).
			Where("namespace = ? AND name = ? AND status = ?", state.Namespace, state.Name, models.DatasetHoldStatusActive).
			Take(&hold).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
		if err == nil {
			if err := s.shapeHolds(v, []*models.DatasetHold{&hold}); err != nil {
				return nil, err
			}
			detail.HoldStatus, detail.Hold = hold.Status, &hold
			// held => not fresh: the freshness status itself reports the hold, not
			// just the adjacent badge.
			detail.State.Status = models.DatasetStatusHeld
			detail.State.Reason = freshness.HeldReason(state.Namespace, state.Name, hold.ID)
		}
	}
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
	derivation, foundDerivation, err := s.latestDerivation(v, namespace, name)
	if err != nil {
		return nil, err
	}
	if foundDerivation {
		if err := s.shapeDerivations(v, []*models.DatasetDerivation{&derivation}); err != nil {
			return nil, err
		}
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
	v := viewerFrom(s.ctx)
	q, err = s.derivationVisible(q, v, p.Namespace, p.Name)
	if err != nil {
		return nil, err
	}

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
	if err := s.shapeDerivations(v, derivationPointers(rows)); err != nil {
		return nil, err
	}
	return &DerivationsResult{
		Derivations: rows,
		Total:       total,
		Limit:       limit,
		Offset:      offset,
	}, nil
}

// Advance applies a manual arrival through the freshness.Store contract. A
// namespace-confined principal's advance re-checks the dataset's producer
// namespaces inside the advance transaction (guardedWrite), and its response
// is shaped for what it may read.
func (s *Service) Advance(p AdvanceParams) (*AdvanceResult, error) {
	now := time.Now().UTC()
	var result freshness.AdvanceResult
	err := s.guardedWrite(p.Principal, AdvanceRole, p.Namespace, p.Name, func(conn *gorm.DB, guard func(tx *gorm.DB) error) error {
		var err error
		result, err = freshness.NewStore(conn).Advance(s.ctx, freshness.AdvanceInput{
			Namespace:   namespacePtr(p.Namespace),
			Name:        strings.TrimSpace(p.Name),
			Watermark:   strings.TrimSpace(p.Watermark),
			RunID:       uuid.Nil,
			CompletedAt: now,
			RunOrder:    now,
			Guard:       guard,
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	state := result.State
	if err := s.shapeStates(viewerFor(p.Principal), []*models.DatasetState{&state}); err != nil {
		return nil, err
	}
	return &AdvanceResult{Outcome: result.Outcome, State: state}, nil
}

func (s *Service) observedStates(status string, limit, offset int) ([]models.DatasetState, int64, error) {
	q := s.db.WithContext(s.ctx).Model(&models.DatasetState{})
	q = withEffectiveStatus(q, status)
	q = s.producerVisible(q, "dataset_states.namespace", "dataset_states.name")

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var rows []models.DatasetState
	// ONE total order (updated_at, namespace, name) everywhere a feed is cut:
	// here, in declarationOnlyStates and in sortStates. Ordering by updated_at
	// alone leaves ties to the engine, so a page cut here and a merge that breaks
	// ties by identity can skip or repeat a row.
	if err := q.
		Order("dataset_states.updated_at DESC").
		Order("dataset_states.namespace ASC").
		Order("dataset_states.name ASC").
		Limit(limit).
		Offset(offset).
		Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// activeHoldExists is a correlated EXISTS over the circuit breaker's active
// holds, matched on the exact (namespace, name) pair of the outer row.
const activeHoldExists = "EXISTS (SELECT 1 FROM dataset_holds WHERE dataset_holds.status = '" +
	models.DatasetHoldStatusActive + "' AND dataset_holds.namespace = %[1]s AND dataset_holds.name = %[2]s)"

// withEffectiveStatus applies the `status` filter to dataset_states against the
// status List reports, which is "held" for a dataset with an active hold (see
// State). With the data circuit breaker off no hold counts, so the filter is the
// persisted column alone.
//
//   - held: an active hold, or a row the evaluator already persisted as held;
//   - any other value: persisted AND no active hold, so a held dataset does not
//     list as fresh (or unknown) from a stale persisted row while its hold is
//     open.
func withEffectiveStatus(q *gorm.DB, status string) *gorm.DB {
	if status == "" {
		return q
	}
	if !env.Variables().DataAssertionsEnabled {
		// No hold counts with the breaker off, so a `held` row persisted while it
		// was on is stranded: it reads as unknown (see unstrandHeld), never held.
		switch status {
		case models.DatasetStatusHeld:
			return q.Where("1 = 0")
		case models.DatasetStatusUnknown:
			return q.Where("status IN ?", []string{models.DatasetStatusUnknown, models.DatasetStatusHeld})
		}
		return q.Where("status = ?", status)
	}
	exists := fmt.Sprintf(activeHoldExists, "dataset_states.namespace", "dataset_states.name")
	if status == models.DatasetStatusHeld {
		return q.Where("(status = ? OR "+exists+")", status)
	}
	return q.Where("status = ?", status).Where("NOT " + exists)
}

func normalizePagination(rawLimit, rawOffset int) (int, int) {
	limit := rawLimit
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	offset := max(rawOffset, 0)
	return limit, offset
}

func (s *Service) declarationOnlyStates(status string, limit int) ([]models.DatasetState, int64, error) {
	// A dataset is commonly declared by multiple jobs; we want distinct
	// (namespace, name) datasets so the LIMIT and total reflect datasets rather
	// than raw declaration rows. We dedup in Go rather than via a SQL GROUP BY:
	// dqlite cannot return computed/aggregate result columns (MAX(...),
	// COALESCE(...)) and fails such a query with "unknown data type". The
	// declared-but-stateless set is bounded (one row per job/dataset
	// declaration), so fetching plain columns and deduping in-process is cheap.
	var decls []models.DatasetDeclaration
	q := s.db.WithContext(s.ctx).
		Model(&models.DatasetDeclaration{}).
		Where("direction IN ?", declarationDirections).
		Where("NOT EXISTS (SELECT 1 FROM dataset_states ds WHERE ds.name = dataset_declarations.name AND ds.namespace = COALESCE(dataset_declarations.namespace, ''))")
	if status == models.DatasetStatusHeld {
		// Only the declared datasets an active hold is on (List calls this for
		// `held` only with the circuit breaker on).
		q = q.Where(fmt.Sprintf(activeHoldExists, "COALESCE(dataset_declarations.namespace, '')", "dataset_declarations.name"))
	}
	if status == models.DatasetStatusUnknown && env.Variables().DataAssertionsEnabled {
		// A declared dataset with an active hold but no state row yet (the hold
		// opens before the producing run's watermark is captured) reports held,
		// so it does not satisfy a `status=unknown` filter.
		q = q.Where("NOT " + fmt.Sprintf(activeHoldExists, "COALESCE(dataset_declarations.namespace, '')", "dataset_declarations.name"))
	}
	q = s.producerVisible(q, "dataset_declarations.namespace", "dataset_declarations.name")
	// Same total order as observedStates / sortStates; the declaration's namespace
	// is nullable and a NULL means "" everywhere else, hence the COALESCE.
	if err := q.
		Order("dataset_declarations.updated_at DESC").
		Order("COALESCE(dataset_declarations.namespace, '') ASC").
		Order("dataset_declarations.name ASC").
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
	if authz.Restricted(s.ctx) {
		// A restricted caller is shown a producer it may read, never another
		// namespace's (B6). The middleware let it in on such a producer; a
		// source-only or consumer declaration owns nothing and is not shown.
		var decl models.DatasetDeclaration
		q := s.db.WithContext(s.ctx).
			Model(&models.DatasetDeclaration{}).
			Joins("JOIN jobs ON jobs.id = dataset_declarations.job_id AND jobs.deleted_at IS NULL").
			Where("dataset_declarations.name = ? AND dataset_declarations.direction = ?", name, models.DatasetDirectionProduces).
			Order("dataset_declarations.job_alias").
			Limit(1)
		if ns := strings.TrimSpace(namespace); ns == "" {
			q = q.Where("(dataset_declarations.namespace IS NULL OR dataset_declarations.namespace = '')")
		} else {
			q = q.Where("dataset_declarations.namespace = ?", ns)
		}
		res := authz.NamespaceFilter(q, "jobs.namespace").Select("dataset_declarations.*").Find(&decl)
		if res.Error != nil {
			return models.DatasetDeclaration{}, false, res.Error
		}
		return decl, res.RowsAffected > 0, nil
	}
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

func (s *Service) latestDerivation(v viewer, namespace, name string) (models.DatasetDerivation, bool, error) {
	var derivation models.DatasetDerivation
	q := s.db.WithContext(s.ctx).
		Where("name = ?", strings.TrimSpace(name)).
		Order("created_at DESC").
		Order("id DESC").
		Limit(1)
	q = withDerivationNamespace(q, namespace)
	q, err := s.derivationVisible(q, v, namespace, name)
	if err != nil {
		return models.DatasetDerivation{}, false, err
	}
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
	end := min(offset+limit, len(rows))
	return rows[offset:end]
}

func sortStates(rows []models.DatasetState) {
	slices.SortStableFunc(rows, func(a, b models.DatasetState) int {
		if order := b.UpdatedAt.Compare(a.UpdatedAt); order != 0 {
			return order
		}
		if order := cmp.Compare(a.Namespace, b.Namespace); order != 0 {
			return order
		}
		return cmp.Compare(a.Name, b.Name)
	})
}
