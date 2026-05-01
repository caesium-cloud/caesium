package worker

import (
	"context"
	"errors"
	"maps"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/mattn/go-sqlite3"
	"gorm.io/gorm"
)

const defaultLeaseTTL = 5 * time.Minute

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

	err := withBusyRetry(ctx, c.busyRetryBackoffs, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}

		now := time.Now().UTC()
		leaseExpiry := now.Add(c.leaseTTL)
		claimed = nil
		attemptEvents := make([]event.Event, 0, 1)

		err := c.store.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			claimedTask, evt, err := c.claimNextTx(tx, now, leaseExpiry)
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
	}
	c.store.PublishEvents(pendingEvents...)

	return claimed, nil
}

func (c *Claimer) claimNextTx(tx *gorm.DB, now, leaseExpiry time.Time) (*models.TaskRun, *event.Event, error) {
	claimed, jobID, err := c.claimNextSingleStatementTx(tx, now, leaseExpiry)
	if err != nil {
		return nil, nil, err
	}
	if claimed == nil {
		return nil, nil, nil
	}
	evt, err := c.recordTaskClaimedEventTx(tx, claimed, jobID)
	return claimed, evt, err
}

func (c *Claimer) claimNextSingleStatementTx(tx *gorm.DB, now, leaseExpiry time.Time) (*models.TaskRun, uuid.UUID, error) {
	selectorSQL, selectorArgs, err := c.nodeSelectorPredicateSQL(tx.Name(), "tr")
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
	ORDER BY tr.created_at ASC
	LIMIT 1
)
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

type claimedTaskRunRow struct {
	models.TaskRun
	ClaimJobID uuid.UUID `gorm:"column:claim_job_id"`
}

func (c *Claimer) recordTaskClaimedEventTx(tx *gorm.DB, claimed *models.TaskRun, jobID uuid.UUID) (*event.Event, error) {
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
	err := withBusyRetry(ctx, c.busyRetryBackoffs, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}

		now := time.Now().UTC()
		attemptEvents := make([]event.Event, 0, 8)
		var attemptReclaimedCount int64

		err := c.store.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			runningRunIDs := tx.Model(&models.JobRun{}).
				Select("id").
				Where("status = ?", string(run.StatusRunning))

			var expired []models.TaskRun
			if err := tx.
				Where("job_run_id IN (?) AND status = ? AND claim_expires_at IS NOT NULL AND claim_expires_at < ?", runningRunIDs, string(run.TaskStatusRunning), now).
				Find(&expired).Error; err != nil {
				return err
			}

			result := tx.Model(&models.TaskRun{}).
				Where("job_run_id IN (?) AND status = ? AND claim_expires_at IS NOT NULL AND claim_expires_at < ?", runningRunIDs, string(run.TaskStatusRunning), now).
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

			for _, taskRun := range expired {
				var jobRun models.JobRun
				if err := tx.Select("job_id").First(&jobRun, "id = ?", taskRun.JobRunID).Error; err != nil {
					return err
				}
				leaseEvt := event.Event{
					Type:      event.TypeTaskLeaseExpired,
					JobID:     jobRun.JobID,
					RunID:     taskRun.JobRunID,
					TaskID:    taskRun.TaskID,
					Timestamp: time.Now().UTC(),
				}
				if err := c.store.RecordEventTx(tx, &leaseEvt); err != nil {
					return err
				}
				attemptEvents = append(attemptEvents, leaseEvt)

				readyEvt := event.Event{
					Type:      event.TypeTaskReady,
					JobID:     jobRun.JobID,
					RunID:     taskRun.JobRunID,
					TaskID:    taskRun.TaskID,
					Timestamp: time.Now().UTC(),
				}
				if err := c.store.RecordEventTx(tx, &readyEvt); err != nil {
					return err
				}
				attemptEvents = append(attemptEvents, readyEvt)
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
	if err == nil {
		return false
	}

	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.Code == sqlite3.ErrBusy || sqliteErr.Code == sqlite3.ErrLocked
	}

	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked") ||
		strings.Contains(msg, "database schema is locked") ||
		strings.Contains(msg, "sqlite_busy") ||
		strings.Contains(msg, "sqlite_locked")
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
