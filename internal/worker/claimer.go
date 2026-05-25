package worker

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/dqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

const defaultLeaseTTL = 5 * time.Minute

// dbWriteCounts accumulates per-category DB write counts during a single retry
// attempt. Must be reset() at the start of each retry closure and commit()'d
// only after the retry returns nil; otherwise transactions retried due to
// busy/locked errors will over-count. Each category tracks both rows and
// stmts; see internal/run/store.go for the equivalent type with full prose.
type dbWriteCounts struct {
	taskRunStatusRows  int
	taskRunStatusStmts int
	eventInsertRows    int
	eventInsertStmts   int
}

func (c *dbWriteCounts) reset() { *c = dbWriteCounts{} }

func (c *dbWriteCounts) addTaskRunStatus(rows int) {
	if rows <= 0 {
		return
	}
	c.taskRunStatusRows += rows
	c.taskRunStatusStmts++
}

func (c *dbWriteCounts) addEventInsert(rows int) {
	if rows <= 0 {
		return
	}
	c.eventInsertRows += rows
	c.eventInsertStmts++
}

func (c *dbWriteCounts) commit() {
	emit := func(category string, rows, stmts int) {
		if rows > 0 {
			metrics.DBWritesTotal.WithLabelValues(category).Add(float64(rows))
		}
		if stmts > 0 {
			metrics.DBStatementsTotal.WithLabelValues(category).Add(float64(stmts))
		}
	}
	emit(metrics.DBWriteCategoryTaskRunStatus, c.taskRunStatusRows, c.taskRunStatusStmts)
	emit(metrics.DBWriteCategoryEventInsert, c.eventInsertRows, c.eventInsertStmts)
}

type Claimer struct {
	nodeID            string
	nodeLabels        map[string]string
	store             *run.Store
	leaseTTL          time.Duration
	busyRetryBackoffs []time.Duration
}

func NewClaimer(nodeID string, store *run.Store, leaseTTL time.Duration, nodeLabels ...map[string]string) *Claimer {
	if store == nil {
		panic("worker claimer requires run store")
	}
	if strings.TrimSpace(nodeID) == "" {
		nodeID = "unknown-node"
	}
	if leaseTTL <= 0 {
		leaseTTL = defaultLeaseTTL
	}

	labels := map[string]string{}
	if len(nodeLabels) > 0 {
		labels = maps.Clone(nodeLabels[0])
		if labels == nil {
			labels = map[string]string{}
		}
	}

	return &Claimer{
		nodeID:            nodeID,
		nodeLabels:        labels,
		store:             store,
		leaseTTL:          leaseTTL,
		busyRetryBackoffs: defaultBusyRetryBackoffSchedule(),
	}
}

func defaultBusyRetryBackoffSchedule() []time.Duration {
	return []time.Duration{
		10 * time.Millisecond,
		20 * time.Millisecond,
		40 * time.Millisecond,
		80 * time.Millisecond,
		160 * time.Millisecond,
	}
}

// ClaimNext claims one ready task, or returns nil when no tasks are available.
func (c *Claimer) ClaimNext(ctx context.Context) (*models.TaskRun, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var claimed *models.TaskRun
	pendingEvents := make([]event.Event, 0, 1)
	var counts dbWriteCounts

	err := withBusyRetry(ctx, c.busyRetryBackoffs, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}

		now := time.Now().UTC()
		leaseExpiry := now.Add(c.leaseTTL)
		claimed = nil
		counts.reset()
		attemptEvents := make([]event.Event, 0, 1)

		err := c.store.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			claimedTask, evt, err := c.claimNextTx(tx, now, leaseExpiry, &counts)
			if err != nil {
				return err
			}
			claimed = claimedTask
			if evt != nil {
				attemptEvents = append(attemptEvents, *evt)
			}
			return nil
		})
		if err == nil {
			pendingEvents = attemptEvents
		}
		return err
	}, c.observeBusyRetry)
	if err != nil {
		return nil, err
	}
	if claimed != nil {
		metrics.WorkerClaimsTotal.WithLabelValues(c.nodeID).Inc()
		metrics.DBWritesTotal.WithLabelValues(metrics.DBWriteCategoryTaskRunStatus).Inc()
		metrics.DBStatementsTotal.WithLabelValues(metrics.DBWriteCategoryTaskRunStatus).Inc()
		counts.commit()
	}
	c.store.PublishEvents(pendingEvents...)

	return claimed, nil
}

func (c *Claimer) claimNextTx(tx *gorm.DB, now, leaseExpiry time.Time, counts *dbWriteCounts) (*models.TaskRun, *event.Event, error) {
	claimed, jobID, err := c.claimNextSingleStatementTx(tx, now, leaseExpiry)
	if err != nil {
		return nil, nil, err
	}
	if claimed == nil {
		return nil, nil, nil
	}
	evt, err := c.recordTaskClaimedEventTx(tx, claimed, jobID, counts)
	return claimed, evt, err
}

func (c *Claimer) claimNextSingleStatementTx(tx *gorm.DB, now, leaseExpiry time.Time) (*models.TaskRun, uuid.UUID, error) {
	selectorSQL, selectorArgs, err := c.nodeSelectorPredicateSQL(tx.Name(), "tr")
	if err != nil {
		return nil, uuid.Nil, err
	}

	// liveLeaseGuard returns SQL that is true only when the candidate task's run
	// does NOT have a live (non-expired) run_leases row.  When no row exists
	// (owner mode off, pre-owner runs) NOT EXISTS is always true, so ClaimNext
	// behaves byte-identically to the no-owner-mode path.
	//
	// The job_run_id column is stored as UUID on Postgres but as TEXT on
	// SQLite/dqlite.  run_leases.run_id is always TEXT.  On Postgres we must
	// cast to avoid a type-mismatch error; on SQLite the comparison works
	// without any cast.
	liveLeaseGuard, err := c.liveLeaseGuardSQL(tx.Name(), "tr")
	if err != nil {
		return nil, uuid.Nil, err
	}

	sql := `
UPDATE task_runs
SET claimed_by = ?, claim_expires_at = ?, claim_attempt = claim_attempt + 1, status = ?, updated_at = ?
WHERE id = (
	SELECT tr.id
	FROM task_runs AS tr
	JOIN job_runs AS jr ON jr.id = tr.job_run_id
	WHERE jr.status = ?
		AND tr.status = ?
		AND tr.outstanding_predecessors = ?
		AND (tr.claimed_by = '' OR tr.claim_expires_at IS NULL OR tr.claim_expires_at < ?)
		AND ` + selectorSQL + `
		AND ` + liveLeaseGuard + `
	ORDER BY tr.created_at ASC
	LIMIT 1
)
AND status = ?
AND outstanding_predecessors = ?
AND (claimed_by = '' OR claim_expires_at IS NULL OR claim_expires_at < ?)
AND job_run_id IN (SELECT id FROM job_runs WHERE status = ?)
RETURNING *, (SELECT job_id FROM job_runs WHERE id = task_runs.job_run_id) AS claim_job_id`

	args := []interface{}{
		c.nodeID,
		leaseExpiry,
		string(run.TaskStatusRunning),
		now,
		string(run.StatusRunning),
		string(run.TaskStatusPending),
		0,
		now,
	}
	args = append(args, selectorArgs...)
	// liveLeaseGuard binds one parameter: now (the live-lease expiry cutoff).
	args = append(args, now)
	args = append(args, string(run.TaskStatusPending), 0, now, string(run.StatusRunning))

	var claimed claimedTaskRunRow
	result := tx.Raw(sql, args...).Scan(&claimed)
	if result.Error != nil {
		return nil, uuid.Nil, result.Error
	}
	if result.RowsAffected == 0 || claimed.ID == uuid.Nil {
		return nil, uuid.Nil, nil
	}
	return &claimed.TaskRun, claimed.ClaimJobID, nil
}

// liveLeaseGuardSQL returns a NOT EXISTS predicate that is true when the
// candidate task's run does NOT have a live (non-expired) run_leases row.
// The single bound parameter is now (UTC), which must be appended to the args
// slice immediately after the selector args and before any post-subquery args.
//
// Semantics:
//   - No run_leases row           → NOT EXISTS true → claimable (owner mode off).
//   - Live lease (expires_at > ?) → NOT EXISTS false → defer to owner's dispatch loop.
//   - Expired lease               → NOT EXISTS true → claimable (recovery path).
func (c *Claimer) liveLeaseGuardSQL(dialect, tableAlias string) (string, error) {
	// job_run_id is a native UUID column on Postgres; a text column on
	// SQLite/dqlite.  run_leases.run_id is always TEXT.  Cast only on Postgres.
	// Mirror nodeSelectorPredicateSQL: reject unknown dialects rather than
	// silently defaulting, so a future backend that needs different casting
	// surfaces the misconfiguration at claim time instead of producing wrong SQL.
	var jobRunIDExpr string
	switch dialect {
	case "dqlite", "sqlite":
		jobRunIDExpr = tableAlias + ".job_run_id"
	case "postgres":
		jobRunIDExpr = "CAST(" + tableAlias + ".job_run_id AS TEXT)"
	default:
		return "", fmt.Errorf("worker claimer: unsupported dialect %q for live-lease guard", dialect)
	}
	return "NOT EXISTS (" +
		"SELECT 1 FROM run_leases rl " +
		"WHERE rl.run_id = " + jobRunIDExpr + " " +
		"AND rl.lease_expires_at > ?" +
		")", nil
}

type claimedTaskRunRow struct {
	models.TaskRun
	ClaimJobID uuid.UUID `gorm:"column:claim_job_id"`
}

func (c *Claimer) recordTaskClaimedEventTx(tx *gorm.DB, claimed *models.TaskRun, jobID uuid.UUID, counts *dbWriteCounts) (*event.Event, error) {
	if claimed == nil {
		return nil, nil
	}

	evt := event.Event{
		Type:      event.TypeTaskClaimed,
		JobID:     jobID,
		RunID:     claimed.JobRunID,
		TaskID:    claimed.TaskID,
		Timestamp: time.Now().UTC(),
	}
	if err := c.store.RecordEventTx(tx, &evt); err != nil {
		return nil, err
	}
	counts.addEventInsert(1)
	return &evt, nil
}

func (c *Claimer) nodeSelectorPredicateSQL(dialect, tableAlias string) (string, []interface{}, error) {
	column := tableAlias + ".node_selector"
	jsonExpr, valueExpr, keyExpr := sqliteNodeSelectorJSONExprs(column)
	switch dialect {
	case "dqlite", "sqlite":
	case "postgres":
		jsonExpr = "COALESCE(" + column + ", '{}'::json)"
		valueExpr = "BTRIM(ns.value)"
		keyExpr = "ns.key"
	default:
		return "", nil, errors.New("worker: unsupported claim database dialect: " + dialect)
	}

	iteratorSQL := nodeSelectorIteratorSQL(dialect, jsonExpr)
	if len(c.nodeLabels) == 0 {
		return "NOT EXISTS (SELECT 1 FROM " + iteratorSQL + " WHERE " + valueExpr + " <> '')", nil, nil
	}

	keys := make([]string, 0, len(c.nodeLabels))
	for key := range c.nodeLabels {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	args := make([]interface{}, 0, len(keys)*3)
	inPlaceholders := make([]string, 0, len(keys))
	caseParts := make([]string, 0, len(keys))
	for _, key := range keys {
		inPlaceholders = append(inPlaceholders, "?")
		args = append(args, key)
	}
	for _, key := range keys {
		caseParts = append(caseParts, "WHEN ? THEN ?")
		args = append(args, key, c.nodeLabels[key])
	}

	sql := "NOT EXISTS (SELECT 1 FROM " + iteratorSQL + " WHERE " + valueExpr + " <> '' AND (" + keyExpr + " NOT IN (" +
		strings.Join(inPlaceholders, ",") + ") OR " + valueExpr + " <> CASE " + keyExpr + " " +
		strings.Join(caseParts, " ") + " ELSE NULL END))"
	return sql, args, nil
}

func sqliteNodeSelectorJSONExprs(column string) (jsonExpr, valueExpr, keyExpr string) {
	jsonExpr = "CASE WHEN " + column + " IS NULL OR " + column + " = '' THEN '{}' ELSE " + column + " END"
	valueExpr = "TRIM(CAST(ns.value AS TEXT))"
	keyExpr = "CAST(ns.key AS TEXT)"
	return jsonExpr, valueExpr, keyExpr
}

func nodeSelectorIteratorSQL(dialect, jsonExpr string) string {
	if dialect == "postgres" {
		return "json_each_text(" + jsonExpr + ") AS ns(key, value)"
	}
	return "json_each(" + jsonExpr + ") AS ns"
}

func (c *Claimer) ReclaimExpired(ctx context.Context) error {
	start := time.Now()
	defer func() {
		metrics.ReclaimDurationSeconds.Observe(time.Since(start).Seconds())
	}()

	pendingEvents := make([]event.Event, 0, 8)
	var reclaimedCount int64
	var counts dbWriteCounts
	err := withBusyRetry(ctx, c.busyRetryBackoffs, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}

		now := time.Now().UTC()
		counts.reset()
		attemptEvents := make([]event.Event, 0, 8)
		var attemptReclaimedCount int64

		err := c.store.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			runningRunIDs := tx.Model(&models.JobRun{}).
				Select("id").
				Where("status = ?", string(run.StatusRunning))

			// liveLeaseGuard skips tasks belonging to a live-owned run — the
			// owner's dispatch loop is responsible for re-dispatching them after a
			// worker crash.  Resetting such a task here would race the owner,
			// risking double-execution.
			//
			// liveLeaseGuardSQL generates "NOT EXISTS (SELECT 1 FROM run_leases rl
			// WHERE rl.run_id = <job_run_id_expr> AND rl.lease_expires_at > ?)"
			// and handles the Postgres UUID→TEXT cast internally via tableAlias.
			// One bound parameter (now) is appended via liveLeaseArgs.
			liveLeaseGuard, err := c.liveLeaseGuardSQL(tx.Name(), "task_runs")
			if err != nil {
				return err
			}

			// Shared between the Find (to collect events) and the Updates (to
			// reset claims) so the expiry criteria can't drift between them.
			// liveLeaseGuard binds one parameter (now); it trails the three
			// static args.
			expiredWhere := "job_run_id IN (?) AND status = ? AND claim_expires_at IS NOT NULL AND claim_expires_at < ? AND " + liveLeaseGuard
			expiredArgs := []interface{}{runningRunIDs, string(run.TaskStatusRunning), now, now}

			var expired []models.TaskRun
			if err := tx.Where(expiredWhere, expiredArgs...).Find(&expired).Error; err != nil {
				return err
			}

			result := tx.Model(&models.TaskRun{}).
				Where(expiredWhere, expiredArgs...).
				Updates(map[string]interface{}{
					"status":           string(run.TaskStatusPending),
					"claimed_by":       "",
					"claim_expires_at": nil,
					"runtime_id":       "",
					"started_at":       nil,
				})
			if result.Error != nil {
				return result.Error
			}
			counts.addTaskRunStatus(int(result.RowsAffected))

			// Batch all lease-expired + task-ready events into a single INSERT.
			if len(expired) > 0 {
				// Collect unique job IDs we need.
				jobRunByID := make(map[uuid.UUID]models.JobRun, len(expired))
				for _, taskRun := range expired {
					if _, ok := jobRunByID[taskRun.JobRunID]; ok {
						continue
					}
					var jobRun models.JobRun
					if err := tx.Select("job_id").First(&jobRun, "id = ?", taskRun.JobRunID).Error; err != nil {
						return err
					}
					jobRunByID[taskRun.JobRunID] = jobRun
				}

				now := time.Now().UTC()
				eventRecords := make([]models.ExecutionEvent, 0, len(expired)*2)
				for _, taskRun := range expired {
					jobRun := jobRunByID[taskRun.JobRunID]
					jobIDPtr := &jobRun.JobID
					runIDCopy := taskRun.JobRunID
					taskIDCopy := taskRun.TaskID
					eventRecords = append(eventRecords,
						models.ExecutionEvent{
							Type:               string(event.TypeTaskLeaseExpired),
							JobID:              jobIDPtr,
							RunID:              &runIDCopy,
							TaskID:             &taskIDCopy,
							BusDispatchPending: true,
							CreatedAt:          now,
						},
						models.ExecutionEvent{
							Type:               string(event.TypeTaskReady),
							JobID:              jobIDPtr,
							RunID:              &runIDCopy,
							TaskID:             &taskIDCopy,
							BusDispatchPending: true,
							CreatedAt:          now,
						},
					)
				}
				if err := tx.Create(&eventRecords).Error; err != nil {
					return err
				}
				counts.addEventInsert(len(eventRecords))

				// Build bus-dispatch events from the inserted records.
				for i := range eventRecords {
					rec := &eventRecords[i]
					attemptEvents = append(attemptEvents, event.Event{
						Sequence:  rec.Sequence,
						Type:      event.Type(rec.Type),
						JobID:     derefUUID(rec.JobID),
						RunID:     derefUUID(rec.RunID),
						TaskID:    derefUUID(rec.TaskID),
						Timestamp: rec.CreatedAt,
					})
				}
			}
			attemptReclaimedCount = result.RowsAffected
			return nil
		})
		if err == nil {
			pendingEvents = attemptEvents
			reclaimedCount = attemptReclaimedCount
		}
		return err
	}, c.observeBusyRetry)
	if err == nil {
		counts.commit()
		if reclaimedCount > 0 {
			metrics.WorkerLeaseExpirationsTotal.WithLabelValues(c.nodeID).Add(float64(reclaimedCount))
		}
		c.store.PublishEvents(pendingEvents...)
	}
	return err
}

func (c *Claimer) observeBusyRetry(error) {
	metrics.WorkerClaimContentionTotal.WithLabelValues(c.nodeID).Inc()
}

func withBusyRetry(ctx context.Context, backoffs []time.Duration, fn func() error, onRetry func(error)) error {
	var err error
	for attempt := 0; ; attempt++ {
		err = fn()
		if err == nil || !isClaimContentionErr(err) {
			return err
		}
		if attempt >= len(backoffs) {
			return err
		}

		metrics.DBBusyRetriesTotal.Inc()
		if onRetry != nil {
			onRetry(err)
		}
		if sleepErr := sleepBusyRetry(ctx, backoffs[attempt]); sleepErr != nil {
			return sleepErr
		}
	}
}

func sleepBusyRetry(ctx context.Context, base time.Duration) error {
	timer := time.NewTimer(jitterBusyRetryBackoff(base))
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func jitterBusyRetryBackoff(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}

	maxJitter := int64(base / 5)
	if maxJitter <= 0 {
		return base
	}
	return base - time.Duration(rand.Int64N(maxJitter+1))
}

func isClaimContentionErr(err error) bool {
	// Delegate to the single shared classifier so the matched error strings
	// live in exactly one place (pkg/dqlite). This helper retries whole
	// transaction closures; the pkg/db connection-pool retry covers single
	// autocommit statements.
	return dqlite.IsContentionError(err)
}

func derefUUID(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.Nil
	}
	return *id
}

func ParseNodeLabels(raw string) map[string]string {
	values := map[string]string{}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		parts := strings.SplitN(entry, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		if key == "" || value == "" {
			continue
		}
		values[key] = value
	}
	return values
}
