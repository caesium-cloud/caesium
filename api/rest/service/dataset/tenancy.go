package dataset

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/freshness"
	"github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Dataset tenancy (identity-and-access design §5.1, §8.3, plan item B6).
//
// A dataset's tenancy namespaces are the current namespaces of the live jobs
// that declare they produce it; a dataset with no declared producer is
// cluster-only. The auth middleware authorises a namespace-confined caller on
// that set (any producer namespace for a read, every one for a write); this
// file shapes what such a caller is then shown:
//
//   - collections list only datasets with a producer in a namespace the caller
//     may read (filtered in SQL, so paging and totals describe what it sees);
//   - history rows are owned by the namespace that wrote them: a derivation by
//     its stamped JobNamespace, a metric sample by its run's persisted
//     namespace, a state's last run and a hold's holder, breach and clearing
//     run by those runs' persisted namespaces, the state's reason by the
//     namespace of the producer whose evaluation wrote it;
//   - anything another namespace owns is left out (no marker), and a hold's
//     impact cone, which names downstream jobs anywhere, is always left out.
//
// Every rule applies only to a restricted caller. An unrestricted one (a
// cluster grant, or no policy file, or auth off) runs exactly the queries and
// gets exactly the bodies it got before.

// viewer is the namespace restriction a read is shaped for.
type viewer struct {
	restricted bool
	allowed    []string
}

// viewerFrom reads the restriction the request context carries (a collection
// route's middleware scope, or authz.WithHistoryScope on a namespaced read).
func viewerFrom(ctx context.Context) viewer {
	allowed, restricted := authz.AllowedNamespaces(ctx)
	return viewer{restricted: restricted, allowed: allowed}
}

// viewerFor is the viewer-level restriction of p, for shaping a write's
// response (a write's context never carries a scope: B4's rule that work a
// write starts must not inherit the caller's narrowing).
func viewerFor(p *auth.Principal) viewer {
	if p == nil {
		return viewer{}
	}
	allowed := p.AllowedNamespaces()
	if allowed == nil {
		return viewer{}
	}
	return viewer{restricted: true, allowed: allowed}
}

// sees reports whether the viewer may read rows owned by namespace.
func (v viewer) sees(namespace string) bool {
	if !v.restricted {
		return true
	}
	return slices.Contains(v.allowed, models.NamespaceOrDefault(namespace))
}

// seesEvery reports whether the viewer may read every namespace in the
// (non-empty) set.
func (v viewer) seesEvery(namespaces []string) bool {
	if !v.restricted {
		return true
	}
	if len(namespaces) == 0 {
		return false
	}
	for _, namespace := range namespaces {
		if !v.sees(namespace) {
			return false
		}
	}
	return true
}

// producerVisible narrows q (a query on a table carrying a dataset identity
// in nsColumn/nameColumn) to datasets with a declared producer in a namespace
// the request may read. The dataset namespace is compared NULL-as-empty on
// both sides: declarations store NULL, states/holds/metrics store ”. The
// subquery's job namespace goes through authz.NamespaceFilter, which marks
// the collection filtered (and matches nothing when restricted to nothing).
// Unrestricted: q is returned unchanged.
func (s *Service) producerVisible(q *gorm.DB, nsColumn, nameColumn string) *gorm.DB {
	if !authz.Restricted(s.ctx) {
		return q
	}
	sub := s.db.WithContext(s.ctx).
		Table("dataset_declarations AS b6_pd").
		Select("1").
		Joins("JOIN jobs AS b6_pj ON b6_pj.id = b6_pd.job_id AND b6_pj.deleted_at IS NULL").
		Where("b6_pd.direction = ?", models.DatasetDirectionProduces).
		Where(fmt.Sprintf("b6_pd.name = %s AND COALESCE(b6_pd.namespace, '') = COALESCE(%s, '')", nameColumn, nsColumn))
	sub = authz.NamespaceFilter(sub, "b6_pj.namespace")
	return q.Where("EXISTS (?)", sub)
}

// runNamespaces returns the persisted tenancy namespace of every run in ids
// that still exists ("default" for a legacy blank, as B4's history filter
// reads it). A run missing from the result is unattributable.
func (s *Service) runNamespaces(ids []uuid.UUID) (map[uuid.UUID]string, error) {
	out := make(map[uuid.UUID]string, len(ids))
	unique := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if id != uuid.Nil && !slices.Contains(unique, id) {
			unique = append(unique, id)
		}
	}
	if len(unique) == 0 {
		return out, nil
	}
	var rows []struct {
		ID        uuid.UUID
		Namespace *string
	}
	if err := s.db.WithContext(s.ctx).Model(&models.JobRun{}).Select("id", "namespace").Where("id IN ?", unique).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		namespace := ""
		if row.Namespace != nil {
			namespace = *row.Namespace
		}
		out[row.ID] = models.NamespaceOrDefault(namespace)
	}
	return out, nil
}

// runVisible reports whether the run id (nil: nothing to show) is attributable
// to a namespace v may read.
func runVisible(v viewer, owners map[uuid.UUID]string, id *uuid.UUID) bool {
	if id == nil {
		return false
	}
	namespace, ok := owners[*id]
	return ok && v.sees(namespace)
}

// shapeStates drops from each state what another namespace owns: the last
// run, and the consumed snapshot, unless the run each belongs to is persisted
// in a readable namespace, and the evaluator's reason unless the namespace of the producer
// whose evaluation wrote it is readable (a NULL owner hides it).
func (s *Service) shapeStates(v viewer, states []*models.DatasetState) error {
	if !v.restricted || len(states) == 0 {
		return nil
	}
	var ids []uuid.UUID
	for _, state := range states {
		for _, id := range []*uuid.UUID{state.LastRunID, state.ConsumedRunID} {
			if id != nil {
				ids = append(ids, *id)
			}
		}
	}
	owners, err := s.runNamespaces(ids)
	if err != nil {
		return err
	}
	for _, state := range states {
		if !runVisible(v, owners, state.LastRunID) {
			state.LastRunID = nil
		}
		// The consumed snapshot belongs to the run that wrote it, which an
		// accepted observation without inputs (a manual advance, a producer
		// that dropped its inputs) leaves behind while last_run_id moves on.
		if !runVisible(v, owners, state.ConsumedRunID) {
			state.ConsumedWatermarks = nil
		}
		if state.ReasonNamespace == nil || !v.sees(*state.ReasonNamespace) {
			state.Reason = ""
		}
	}
	return nil
}

// shapeHolds drops from each hold what another namespace owns: the holder
// (job, run, task, step) unless the opening run's persisted namespace is
// readable; the latest breach (its run and violations, whose baseline was
// computed from the breaching run's namespace) unless that run's namespace is
// readable; the clearing run likewise; and always the impact cone.
func (s *Service) shapeHolds(v viewer, holds []*models.DatasetHold) error {
	if !v.restricted || len(holds) == 0 {
		return nil
	}
	var ids []uuid.UUID
	for _, hold := range holds {
		for _, id := range []*uuid.UUID{hold.HeldByRunID, hold.LastBreachRunID, hold.ReleaseRunID} {
			if id != nil {
				ids = append(ids, *id)
			}
		}
	}
	owners, err := s.runNamespaces(ids)
	if err != nil {
		return err
	}
	for _, hold := range holds {
		if !runVisible(v, owners, hold.LatestBreachRunID()) {
			hold.LastBreachRunID = nil
			hold.Violations = nil
		}
		if !runVisible(v, owners, hold.HeldByRunID) {
			hold.HeldByJobID = uuid.Nil
			hold.HeldByJobAlias = ""
			hold.HeldByRunID = nil
			hold.HeldByTaskID = nil
			hold.HeldByTaskRunID = nil
			hold.HeldByStepName = ""
		}
		if hold.ReleaseRunID != nil && !runVisible(v, owners, hold.ReleaseRunID) {
			hold.ReleaseRunID = nil
		}
		hold.Impact = nil
	}
	return nil
}

// derivationVisible narrows a derivation query to the rows a restricted
// caller may read: those stamped with a readable JobNamespace, plus the
// legacy rows (neither ProducerJobID nor JobNamespace stamped) when the
// caller may read every current producer namespace of the dataset. A
// partially stamped row (a producer deleted mid-decision) matches neither and
// is hidden. NULL ownership never counts as "default".
func (s *Service) derivationVisible(q *gorm.DB, v viewer, datasetNamespace, name string) (*gorm.DB, error) {
	if !v.restricted {
		return q, nil
	}
	authz.MarkFilteredContext(s.ctx)
	producers, err := freshness.ProducersTx(s.db.WithContext(s.ctx), datasetNamespace, name)
	if err != nil {
		return nil, err
	}
	if len(v.allowed) == 0 {
		return q.Where("1 = 0"), nil
	}
	if v.seesEvery(producerNamespaces(producers)) {
		return q.Where("(job_namespace IN ? OR (producer_job_id IS NULL AND job_namespace IS NULL))", v.allowed), nil
	}
	return q.Where("job_namespace IN ?", v.allowed), nil
}

// shapeDerivations drops a visible row's run id when that run is persisted in
// a namespace the caller may not read (a skipped_active_run row of a moved
// job can point at the old namespace's run).
func (s *Service) shapeDerivations(v viewer, rows []*models.DatasetDerivation) error {
	if !v.restricted || len(rows) == 0 {
		return nil
	}
	var ids []uuid.UUID
	for _, row := range rows {
		if row.RunID != nil {
			ids = append(ids, *row.RunID)
		}
	}
	owners, err := s.runNamespaces(ids)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.RunID != nil && !runVisible(v, owners, row.RunID) {
			row.RunID = nil
		}
	}
	return nil
}

func statePointers(rows []models.DatasetState) []*models.DatasetState {
	out := make([]*models.DatasetState, len(rows))
	for i := range rows {
		out[i] = &rows[i]
	}
	return out
}

func derivationPointers(rows []models.DatasetDerivation) []*models.DatasetDerivation {
	out := make([]*models.DatasetDerivation, len(rows))
	for i := range rows {
		out[i] = &rows[i]
	}
	return out
}

func producerNamespaces(producers []freshness.Producer) []string {
	out := make([]string, 0, len(producers))
	for _, producer := range producers {
		out = append(out, producer.Namespace)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// ErrProducersChanged is returned when a confined caller's dataset write saw
// the dataset's producer set change under it on every attempt (409).
var ErrProducersChanged = errors.New("dataset: the dataset's producers changed during the request; retry")

var errProducerSetMoved = errors.New("dataset: producer set changed since its locks were taken")

// beforeGuardedWrite, when set (tests only), runs after a confined write has
// read the producer set it locks and before it takes the locks: where a
// concurrent apply that moves a producer interleaves.
var beforeGuardedWrite func()

const guardedWriteAttempts = 3

// confined reports whether p's cluster role is below role, so the write's
// namespaces must be checked (the middleware's rule for confinement).
func confined(p *auth.Principal, role models.Role) bool {
	return p != nil && !auth.HasRole(p.ClusterRole(), role)
}

// guardedWrite runs write for a namespace-confined caller so the producer
// check and the write commit together (identity-and-access B6):
//
//  1. read the dataset's producers and take, on PostgreSQL, the advisory lock
//     every job write takes for each producer's alias (jobdef.AliasLockKey,
//     before BEGIN, as jobdef's writeTx does), so an apply that moves,
//     re-declares or retires a producer serialises with this write;
//  2. inside the write's transaction, re-read the producers FOR SHARE
//     (freshness.LockProducersTx) and require role in every one's namespace;
//     a producer that appeared since step 1 (whose alias was not locked)
//     restarts from step 1, at most three times.
//
// The share locks on the producers' job and declaration rows hold to commit,
// so a writer that bypasses the alias locks (DELETE /v1/jobs/:id, an apply's
// prune of a retired producer) either committed before the re-read (and is
// seen) or waits for this write. SQLite and dqlite serialise write
// transactions, so there the in-transaction re-read alone sees every
// committed write. The residual window, on PostgreSQL only: an apply that
// ADDS a producer under an alias this write did not lock and commits after
// the re-read. ValidateDatasetGraph refuses such an apply inside its own
// transaction while a locked producer still produces the dataset (one
// producer per dataset), and removing that producer needs the rows this
// write holds.
//
// An unconfined caller (and p == nil) gets write(s.db, nil): no locks, no
// guard, exactly as before.
func (s *Service) guardedWrite(p *auth.Principal, role models.Role, datasetNamespace, name string, write func(conn *gorm.DB, guard func(tx *gorm.DB) error) error) error {
	if !confined(p, role) {
		return write(s.db, nil)
	}
	for range guardedWriteAttempts {
		producers, err := freshness.ProducersTx(s.db.WithContext(s.ctx), datasetNamespace, name)
		if err != nil {
			return err
		}
		locked := make(map[uuid.UUID]struct{}, len(producers))
		keys := make([]string, 0, len(producers))
		for _, producer := range producers {
			locked[producer.JobID] = struct{}{}
			keys = append(keys, jobdef.AliasLockKey(producer.Alias))
		}
		slices.Sort(keys)
		keys = slices.Compact(keys)
		if beforeGuardedWrite != nil {
			beforeGuardedWrite()
		}
		guard := func(tx *gorm.DB) error {
			current, err := freshness.LockProducersTx(tx, datasetNamespace, name)
			if err != nil {
				return err
			}
			for _, producer := range current {
				if _, ok := locked[producer.JobID]; !ok {
					return errProducerSetMoved
				}
			}
			return requireEveryProducer(p, role, current)
		}
		err = jobdef.WithAdvisoryLocks(s.ctx, s.db, keys, func(conn *gorm.DB) error {
			return write(conn, guard)
		})
		if errors.Is(err, errProducerSetMoved) {
			continue
		}
		return err
	}
	return ErrProducersChanged
}

// requireEveryProducer is the middleware's every-of rule, applied to the
// producers read inside the write's transaction. No producer (the dataset
// became cluster-only) refuses a confined caller.
func requireEveryProducer(p *auth.Principal, role models.Role, producers []freshness.Producer) error {
	namespaces := producerNamespaces(producers)
	denial := &authz.Denial{Reason: "insufficient_role", Required: role, Namespaces: namespaces, Message: "insufficient permissions"}
	if len(namespaces) == 0 {
		return denial
	}
	for _, namespace := range namespaces {
		if !auth.HasRole(p.RoleIn(namespace), role) {
			return denial
		}
	}
	return nil
}
