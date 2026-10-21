package run

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Per-namespace run quota (identity-and-access D1, design §10.3).
//
// A namespace's quotas.maxConcurrentRuns caps how many of its runs may be
// `running` at once. The cap is ONE transactional gate, namespaceCapacityTx,
// called on every path that makes a run running: admission (admit), retry
// re-admission (readmitRetryTx, which whole-run, agent and partition retries
// all reach) and the quarantined-replay insert (internal/replay). What the
// caller does at cap depends on the run kind:
//
//   - trigger-originated and manual starts are parked in run_queue with
//     held_reason namespace_quota, whatever the job's own queue policy, and the
//     dequeuer drains them round-robin across namespaces and jobs;
//   - retries, partition retries and replays are refused (HTTP 409
//     namespace_quota with Retry-After) — they are on-demand actions;
//   - backfill children are never queued: the backfill driver waits and
//     retries the date.
//
// The settings are read from policy.ProcessHolder() at the point of use, so a
// policy reload takes effect on the next admission. With no policy file every
// namespace is unlimited and the gate issues no SQL at all.

var (
	// ErrNamespaceQuota is matched by every *NamespaceQuotaError.
	ErrNamespaceQuota = errors.New("run: namespace concurrent-run quota reached")
	// ErrNamespaceUndeclared is matched by every *NamespaceUndeclaredError.
	ErrNamespaceUndeclared = errors.New("run: namespace is not declared by the access policy")
	// ErrNamespacePolicyPending refuses a start while the server's configured
	// access policy file is not loaded yet (see ExpectNamespacePolicy). It is
	// transient: the file is installed during startup or the server exits.
	ErrNamespacePolicyPending = errors.New("run: the access policy file is not loaded yet; namespace quotas cannot be checked")
	// ErrRunQueuedByQuota is returned by a trigger-originated start the
	// namespace quota parked in run_queue. It wraps ErrRunQueued, so every
	// caller that treats a queued start as "nothing to execute" keeps doing so.
	ErrRunQueuedByQuota = fmt.Errorf("%w: its namespace is at its concurrent-run quota", ErrRunQueued)
	// ErrQueuedRunNamespaceMoved is matched by every
	// *QueuedRunNamespaceMovedError.
	ErrQueuedRunNamespaceMoved = errors.New("run: queued run belongs to a namespace its job has left")
)

// NamespaceQuotaRetryAfter is the Retry-After a refused retry or replay
// advertises. Capacity frees when some run in the namespace finishes, which
// the server cannot predict; five seconds is long enough not to spin a client
// and short enough to catch a typical slot.
const NamespaceQuotaRetryAfter = 5 * time.Second

// Reasons a queued run is dropped instead of promoted.
const (
	QueueDropReasonNamespaceMoved      = "namespace_moved"
	QueueDropReasonNamespaceUndeclared = "namespace_undeclared"
)

// NamespaceQuotaError refuses a run because its namespace already has Limit
// runs running.
type NamespaceQuotaError struct {
	Namespace string
	Limit     int
}

func (e *NamespaceQuotaError) Error() string {
	return fmt.Sprintf("run: namespace %q is at its concurrent-run quota (maxConcurrentRuns %d)", e.Namespace, e.Limit)
}

// Is makes errors.Is(err, ErrNamespaceQuota) match.
func (e *NamespaceQuotaError) Is(target error) bool { return target == ErrNamespaceQuota }

// NamespaceUndeclaredError refuses a run in a namespace the loaded access
// policy file does not declare. Apply already rejects such a namespace; a run
// reaches this only when a reload removed the namespace from the policy, and
// an undeclared namespace has no settings to enforce — so it is refused,
// never treated as unlimited.
type NamespaceUndeclaredError struct {
	Namespace string
}

func (e *NamespaceUndeclaredError) Error() string {
	return fmt.Sprintf("run: namespace %q is not declared by the access policy", e.Namespace)
}

// Is makes errors.Is(err, ErrNamespaceUndeclared) match.
func (e *NamespaceUndeclaredError) Is(target error) bool { return target == ErrNamespaceUndeclared }

// QueuedRunNamespaceMovedError refuses to promote a queued run recorded in
// one namespace after its job moved to another: the request belongs to the
// old owner and must never start under the new owner's secrets, identity or
// quota.
type QueuedRunNamespaceMovedError struct {
	QueueNamespace string
	JobNamespace   string
}

func (e *QueuedRunNamespaceMovedError) Error() string {
	return fmt.Sprintf("run: queued run belongs to namespace %q but its job is now in namespace %q", e.QueueNamespace, e.JobNamespace)
}

// Is makes errors.Is(err, ErrQueuedRunNamespaceMoved) match.
func (e *QueuedRunNamespaceMovedError) Is(target error) bool {
	return target == ErrQueuedRunNamespaceMoved
}

// namespacePolicyExpected records that this process serves an access policy
// file (ExpectNamespacePolicy). Until that file is installed in
// policy.ProcessHolder(), the namespace gate refuses rather than treating
// every namespace as declared and unlimited.
var namespacePolicyExpected atomic.Bool

// ExpectNamespacePolicy tells the namespace gate whether the server is
// configured with an access policy file. The server calls it before it starts
// anything that admits runs (the run-queue dequeuer, the freshness evaluator),
// which run before the auth setup loads and installs the file: in that window
// a start is refused with ErrNamespacePolicyPending — and a queued run left
// queued — instead of escaping its namespace's quota. Other processes
// (`caesium dev`, tests) never call it and keep the no-policy behaviour.
func ExpectNamespacePolicy(expected bool) {
	namespacePolicyExpected.Store(expected)
}

// NamespacePolicyPending reports whether the server is configured with an
// access policy file that is not installed yet.
func NamespacePolicyPending() bool {
	return namespacePolicyExpected.Load() && !policy.ProcessHolder().FileBacked()
}

// NamespaceRunLimit returns namespace's maxConcurrentRuns (0 is unlimited)
// under the access policy being served right now. Without a policy file every
// namespace is unlimited. With one, a namespace the current revision does not
// declare is refused with a *NamespaceUndeclaredError; before a configured
// file is installed, every namespace is refused with ErrNamespacePolicyPending.
func NamespaceRunLimit(namespace string) (int, error) {
	holder := policy.ProcessHolder()
	if !holder.FileBacked() {
		if namespacePolicyExpected.Load() {
			return 0, ErrNamespacePolicyPending
		}
		return 0, nil
	}
	namespace = models.NamespaceOrDefault(strings.TrimSpace(namespace))
	settings, declared := holder.Namespace(namespace)
	if !declared {
		return 0, &NamespaceUndeclaredError{Namespace: namespace}
	}
	if settings.Quotas == nil || settings.Quotas.MaxConcurrentRuns <= 0 {
		return 0, nil
	}
	return settings.Quotas.MaxConcurrentRuns, nil
}

// CheckNamespaceDeclared refuses a namespace the loaded policy file does not
// declare. Without a policy file it always passes.
func CheckNamespaceDeclared(namespace string) error {
	_, err := NamespaceRunLimit(namespace)
	return err
}

// QuotaNamespaces returns every namespace the served policy declares with a
// positive maxConcurrentRuns, mapped to that limit. It is empty without a
// policy file.
func QuotaNamespaces() map[string]int {
	holder := policy.ProcessHolder()
	if !holder.FileBacked() {
		return nil
	}
	rev := holder.Current()
	if rev == nil || rev.Policy == nil {
		return nil
	}
	out := make(map[string]int)
	for name, ns := range rev.Policy.Namespaces {
		if ns.Quotas != nil && ns.Quotas.MaxConcurrentRuns > 0 {
			out[name] = ns.Quotas.MaxConcurrentRuns
		}
	}
	return out
}

// CheckNamespaceCapacityTx is namespaceCapacityTx for callers outside this
// package (the quarantined-replay insert).
func CheckNamespaceCapacityTx(tx *gorm.DB, namespace string) error {
	return namespaceCapacityTx(tx, namespace)
}

// namespaceCapacityTx is the namespace quota gate. It returns nil when one
// more run may become running in namespace inside tx, a
// *NamespaceQuotaError at cap, a *NamespaceUndeclaredError for a namespace
// the policy file does not declare, and any database error unchanged (an
// error never allows).
//
// Every running run counts — quarantined replays and backfill children
// included: they are real compute (design §10.3).
//
// Race freedom. On dqlite and SQLite every read-write transaction runs on
// the single serialized writer, and a transaction whose snapshot went stale
// cannot upgrade to a write (SQLITE_BUSY, retried by the store), so the count
// and the caller's write commit atomically. On PostgreSQL (READ COMMITTED) a
// transaction-scoped advisory lock keyed on the namespace is taken first: the
// next gate in the namespace waits for this transaction to commit and its
// count then sees the row this one wrote.
func namespaceCapacityTx(tx *gorm.DB, namespace string) error {
	namespace = models.NamespaceOrDefault(strings.TrimSpace(namespace))
	limit, err := NamespaceRunLimit(namespace)
	if err != nil {
		return err
	}
	return namespaceCapacityLimitTx(tx, namespace, limit)
}

// namespaceCapacityLimitTx is namespaceCapacityTx with the limit already
// read (admit reads it once, before anything is written).
func namespaceCapacityLimitTx(tx *gorm.DB, namespace string, limit int) error {
	if limit <= 0 {
		return nil
	}
	if err := lockNamespaceQuotaTx(tx, namespace); err != nil {
		return err
	}
	running, err := countRunningInNamespaceTx(tx, namespace)
	if err != nil {
		return err
	}
	if running >= int64(limit) {
		return &NamespaceQuotaError{Namespace: namespace, Limit: limit}
	}
	return nil
}

func countRunningInNamespaceTx(tx *gorm.DB, namespace string) (int64, error) {
	query := tx.Model(&models.JobRun{}).Where("status = ?", string(StatusRunning))
	if namespace == models.DefaultNamespace {
		// A legacy empty namespace is "default", exactly as the namespace
		// filters treat it.
		query = query.Where("(namespace = ? OR namespace = '' OR namespace IS NULL)", namespace)
	} else {
		query = query.Where("namespace = ?", namespace)
	}
	var running int64
	if err := query.Count(&running).Error; err != nil {
		return 0, err
	}
	return running, nil
}

func lockNamespaceQuotaTx(tx *gorm.DB, namespace string) error {
	if tx == nil || tx.Dialector == nil {
		return errors.New("run: namespace quota requires a database dialect")
	}
	stmt, err := namespaceQuotaLockSQL(tx.Name())
	if err != nil {
		return err
	}
	if stmt == "" {
		return nil
	}
	return tx.Exec(stmt, namespaceQuotaLockKey(namespace)).Error
}

func namespaceQuotaLockSQL(dialect string) (string, error) {
	switch dialect {
	case "postgres":
		return "SELECT pg_advisory_xact_lock(?)", nil
	case "dqlite", "sqlite", "sqlite3":
		// Writers are already serialized; see namespaceCapacityTx.
		return "", nil
	default:
		return "", fmt.Errorf("run: unsupported dialect %q for the namespace quota lock", dialect)
	}
}

// namespaceQuotaLockKey is the advisory-lock key for namespace. A collision
// only serializes two namespaces' gates, never admits more runs.
func namespaceQuotaLockKey(namespace string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("caesium/namespace-quota/" + namespace))
	return int64(h.Sum64()) //nolint:gosec // a lock key; wrapping is intended
}

// RecordNamespaceQuotaDeferral counts one deferral when err is a namespace
// quota refusal, and reports whether it was.
func RecordNamespaceQuotaDeferral(err error) bool {
	quota, ok := errors.AsType[*NamespaceQuotaError](err)
	if !ok {
		return false
	}
	RecordNamespaceQuotaDeferrals(quota.Namespace, 1)
	return true
}

// RecordNamespaceQuotaDeferrals adds n deferrals for namespace. Callers pass
// only namespaces a quota refused, which the policy declares with a quota, so
// the label stays bounded.
func RecordNamespaceQuotaDeferrals(namespace string, n int) {
	if n <= 0 {
		return
	}
	metrics.NamespaceQuotaDeferralsTotal.WithLabelValues(models.NamespaceOrDefault(namespace)).Add(float64(n))
}

// RunningByNamespace counts running job runs per namespace (a legacy empty
// namespace counts as "default").
func (s *Store) RunningByNamespace(ctx context.Context) (map[string]int64, error) {
	var rows []struct {
		Namespace string
		Running   int64
	}
	if err := s.db.WithContext(ctx).Model(&models.JobRun{}).
		Select("namespace, count(*) AS running").
		Where("status = ?", string(StatusRunning)).
		Group("namespace").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(rows))
	for _, row := range rows {
		out[models.NamespaceOrDefault(strings.TrimSpace(row.Namespace))] += row.Running
	}
	return out, nil
}

// ReleaseQueuedRunHeldByQuota releases a claimed row whose promotion the
// namespace run quota refused, recording that it now waits on the quota. The
// reason is sticky: it is set here and on a quota enqueue, and never cleared,
// because a quota-held row of a job without a queue policy is drained only
// because of it. It reports whether the row was not quota-held before.
func (s *Store) ReleaseQueuedRunHeldByQuota(ctx context.Context, queued *models.RunQueue, claimedBy string) (bool, error) {
	if queued == nil {
		return false, nil
	}
	result := s.db.WithContext(ctx).
		Model(&models.RunQueue{}).
		Where("id = ? AND claimed_by = ?", queued.ID, claimedBy).
		Updates(map[string]any{
			"claimed_by":  "",
			"claimed_at":  nil,
			"held_reason": models.RunQueueHeldReasonNamespaceQuota,
		})
	if result.Error != nil {
		return false, result.Error
	}
	if err := s.observeRunQueueDepth(queued.JobID); err != nil {
		log.Warn("run queue: failed to observe depth after release", "job_id", queued.JobID, "error", err)
	}
	return result.RowsAffected > 0 && !queued.IsQuotaHeld(), nil
}

// AnnotateQuotaHeldRuns marks the next `slots` unclaimed rows of a job with
// a queue policy as held by the namespace run quota, when the job itself has
// those free slots but its namespace is full: these rows would start now but
// for the quota. It writes only when a row is not yet marked, and returns how
// many rows changed.
func (s *Store) AnnotateQuotaHeldRuns(ctx context.Context, jobID uuid.UUID, slots int) (int64, error) {
	if slots <= 0 {
		return 0, nil
	}
	var head []models.RunQueue
	if err := s.db.WithContext(ctx).
		Select("id", "held_reason").
		Where("job_id = ? AND claimed_by = ''", jobID).
		Order("priority DESC").
		Order("created_at ASC").
		Limit(slots).
		Find(&head).Error; err != nil {
		return 0, err
	}
	ids := make([]uuid.UUID, 0, len(head))
	for _, row := range head {
		if !row.IsQuotaHeld() {
			ids = append(ids, row.ID)
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	result := s.db.WithContext(ctx).
		Model(&models.RunQueue{}).
		Where("id IN ? AND claimed_by = '' AND held_reason IS NULL", ids).
		Update("held_reason", models.RunQueueHeldReasonNamespaceQuota)
	return result.RowsAffected, result.Error
}

// DropQueuedRun removes a claimed row that must never be promoted — its job
// moved to another namespace, or its namespace is no longer declared — and
// resolves an idempotent start waiting on it as dropped with reason, in one
// transaction.
func (s *Store) DropQueuedRun(ctx context.Context, queued *models.RunQueue, reason string) error {
	if queued == nil {
		return nil
	}
	err := withStoreBusyRetryContext(ctx, func() error {
		return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := tx.Delete(&models.RunQueue{}, "id = ?", queued.ID).Error; err != nil {
				return err
			}
			return tx.Model(&models.RunStartIdempotency{}).
				Where("queue_id = ? AND outcome = ?", queued.ID, string(StartOutcomeQueued)).
				Updates(map[string]any{
					"outcome":    string(StartOutcomeDropped),
					"reason":     reason,
					"updated_at": time.Now().UTC(),
				}).Error
		})
	})
	if err != nil {
		return err
	}
	if err := s.observeRunQueueDepth(queued.JobID); err != nil {
		log.Warn("run queue: failed to observe depth after drop", "job_id", queued.JobID, "error", err)
	}
	return nil
}
