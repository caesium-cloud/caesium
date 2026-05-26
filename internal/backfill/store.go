package backfill

import (
	"encoding/json"
	"errors"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/caesium-cloud/caesium/pkg/dqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Store provides CRUD operations for Backfill records.
type Store struct {
	db *gorm.DB
}

var (
	defaultStore     *Store
	defaultStoreOnce sync.Once
)

// busyRetryBackoffs aliases the shared contention-retry schedule so backfill
// store ops back off on the same budget as the other retry layers; see
// db.BusyRetryBackoffs.
var busyRetryBackoffs = db.BusyRetryBackoffs

func NewStore(conn *gorm.DB) *Store {
	if conn == nil {
		panic("backfill store requires database connection")
	}
	return &Store{db: conn}
}

func Default() *Store {
	defaultStoreOnce.Do(func() {
		defaultStore = NewStore(db.Connection())
	})
	return defaultStore
}

func (s *Store) Create(b *models.Backfill) error {
	return s.withBusyRetry(func() error {
		return s.db.Create(b).Error
	})
}

func (s *Store) Get(id uuid.UUID) (*models.Backfill, error) {
	// Single autocommit read: contention retry is handled globally by the
	// connection-pool busy-retry in pkg/db, so no per-call-site wrap is needed.
	var b models.Backfill
	if err := s.db.First(&b, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *Store) List(jobID uuid.UUID) ([]*models.Backfill, error) {
	var backfills []*models.Backfill
	if err := s.db.Where("job_id = ?", jobID).Order("created_at desc").Find(&backfills).Error; err != nil {
		return nil, err
	}
	return backfills, nil
}

// RequestCancel persists a cancellation request for a running backfill.
func (s *Store) RequestCancel(id uuid.UUID) error {
	now := time.Now().UTC()
	return s.withBusyRetry(func() error {
		return s.db.Model(&models.Backfill{}).
			Where("id = ? AND status = ?", id, string(models.BackfillStatusRunning)).
			Updates(map[string]interface{}{
				"cancel_requested_at": now,
			}).Error
	})
}

// MarkCancelled marks a running backfill as fully cancelled after in-flight
// work has drained.
func (s *Store) MarkCancelled(id uuid.UUID) error {
	now := time.Now().UTC()
	return s.withBusyRetry(func() error {
		return s.db.Model(&models.Backfill{}).
			Where("id = ? AND status = ?", id, string(models.BackfillStatusRunning)).
			Updates(map[string]interface{}{
				"status":       string(models.BackfillStatusCancelled),
				"completed_at": now,
			}).Error
	})
}

func (s *Store) Complete(id uuid.UUID, failed bool) error {
	now := time.Now().UTC()
	status := models.BackfillStatusSucceeded
	if failed {
		status = models.BackfillStatusFailed
	}
	return s.withBusyRetry(func() error {
		return s.db.Model(&models.Backfill{}).
			Where("id = ? AND status = ?", id, string(models.BackfillStatusRunning)).
			Updates(map[string]interface{}{
				"status":       string(status),
				"completed_at": now,
			}).Error
	})
}

func (s *Store) IncrementCompleted(id uuid.UUID) error {
	return s.withBusyRetry(func() error {
		return s.db.Model(&models.Backfill{}).
			Where("id = ?", id).
			UpdateColumn("completed_runs", gorm.Expr("completed_runs + 1")).Error
	})
}

func (s *Store) IncrementFailed(id uuid.UUID) error {
	return s.withBusyRetry(func() error {
		return s.db.Model(&models.Backfill{}).
			Where("id = ?", id).
			UpdateColumn("failed_runs", gorm.Expr("failed_runs + 1")).Error
	})
}

// SetTotalRuns updates the total_runs counter on a backfill.
func (s *Store) SetTotalRuns(id uuid.UUID, total int) error {
	return s.withBusyRetry(func() error {
		return s.db.Model(&models.Backfill{}).
			Where("id = ?", id).
			UpdateColumn("total_runs", total).Error
	})
}

// IsRunning returns true if a backfill with the given ID is in the running state.
func (s *Store) IsRunning(id uuid.UUID) (bool, error) {
	var b models.Backfill
	err := s.db.Select("status").First(&b, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return b.Status == string(models.BackfillStatusRunning), nil
}

// IsCancelRequested returns true if a running backfill has a persisted cancel
// request.
func (s *Store) IsCancelRequested(id uuid.UUID) (bool, error) {
	var b models.Backfill
	err := s.db.Select("status", "cancel_requested_at").First(&b, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return b.Status == string(models.BackfillStatusRunning) && b.CancelRequestedAt != nil, nil
}

// LatestRunForLogicalDate returns the status of the most recent run for a job
// whose params contain the given logical_date value, or "" if none exists.
// It uses Go-side filtering to remain portable across SQLite and Postgres.
func (s *Store) LatestRunForLogicalDate(jobID uuid.UUID, logicalDate string) (string, error) {
	type runRow struct {
		Status string
		Params []byte
	}
	var rows []runRow
	err := s.db.Raw(
		`SELECT status, params FROM job_runs WHERE job_id = ? AND params IS NOT NULL ORDER BY created_at DESC`,
		jobID,
	).Scan(&rows).Error
	if err != nil {
		return "", err
	}
	for _, row := range rows {
		var p map[string]string
		if err := json.Unmarshal(row.Params, &p); err != nil {
			continue
		}
		if p["logical_date"] == logicalDate {
			return row.Status, nil
		}
	}
	return "", nil
}

// withBusyRetry retries the given operation when it fails with a transient
// dqlite/SQLite contention or connection-state error. See isContentionErr for
// the recognised classes.
//
// On the connection-state poisoning case (a `cannot start a transaction within
// a transaction` error left over from a prior failed rollback), the helper
// also issues a best-effort `ROLLBACK` against the global pool before sleeping.
// On a poisoned connection that ROLLBACK clears the leftover BEGIN; on a clean
// connection it errors out harmlessly with "no transaction is active". This
// follows the pattern from canonical/lxd/lxd/db/query/transaction.go and
// dramatically improves per-retry success when one of the pooled connections
// has been poisoned by a prior `checkpoint in progress` error.
func (s *Store) withBusyRetry(fn func() error) error {
	var err error
	for attempt := 0; ; attempt++ {
		err = fn()
		if err == nil || !isContentionErr(err) {
			return err
		}
		if attempt >= len(busyRetryBackoffs) {
			return err
		}

		if isPoisonedConnErr(err) {
			// Errors from this Exec are intentionally ignored — see comment above.
			_ = s.db.Exec("ROLLBACK").Error
		}

		metrics.DBBusyRetriesTotal.Inc()
		time.Sleep(jitterBackoff(busyRetryBackoffs[attempt]))
	}
}

// isPoisonedConnErr matches the narrow case where a pooled connection has
// been left with a stale active transaction. Distinct from isContentionErr,
// which also covers transient busy/locked errors that don't require active
// recovery. Delegates to the shared classifier in pkg/dqlite.
func isPoisonedConnErr(err error) bool {
	return dqlite.IsConnPoolPoisonedError(err)
}

func jitterBackoff(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	maxJitter := int64(base / 5)
	if maxJitter <= 0 {
		return base
	}
	return base - time.Duration(rand.Int64N(maxJitter+1))
}

// isContentionErr classifies an error as a transient dqlite/SQLite contention
// or connection-state error worth retrying on a fresh pooled connection.
//
// Two distinct classes are recognised:
//
//   - Direct contention (`database is locked`, `checkpoint in progress`, etc.):
//     a simple retry is likely to find the database in a non-busy state.
//   - Connection-state poisoning (`cannot start a transaction within a
//     transaction`): a previous transaction on the same pooled connection
//     failed mid-flight (typically because the implicit ROLLBACK after a
//     `checkpoint in progress` itself failed) and left the connection with
//     a still-active SQLite transaction handle. The next caller's BEGIN on
//     that connection then fails. With multiple pooled connections, retries
//     are likely to draw a clean one. Without retry the poisoned connection
//     persists for the lifetime of the process and breaks every caller that
//     happens to receive it — exactly the cascade observed in #155 / #156.
//
// Delegates to the single shared classifier in pkg/dqlite.
func isContentionErr(err error) bool {
	return dqlite.IsContentionError(err)
}
