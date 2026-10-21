package job

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type QueueItem struct {
	ID         uuid.UUID         `json:"id"`
	Position   int               `json:"position"`
	Priority   int               `json:"priority"`
	Params     map[string]string `json:"params,omitempty"`
	EnqueuedAt time.Time         `json:"enqueued_at"`
	// ClaimState is `pending`, `claimed` or `stale` (see models.RunQueueClaimState*).
	ClaimState string `json:"claim_state"`
	// Stale is the one-bit form of ClaimState == "stale", for callers that only
	// need "is this row stuck?".
	Stale bool `json:"stale"`
	// ClaimedBy/ClaimedAt name the dequeuer holding the row and when it took
	// it. Both are empty on a pending row; on a stale one they say which node
	// died and how long ago.
	ClaimedBy string     `json:"claimed_by,omitempty"`
	ClaimedAt *time.Time `json:"claimed_at,omitempty"`
	// HeldReason says why the run waits beyond the job's own policy:
	// `namespace_quota` when its namespace is at quotas.maxConcurrentRuns
	// (identity-and-access D2). Omitted otherwise.
	HeldReason string `json:"held_reason,omitempty"`
}

// Queue lists every queued run for a job, claimed rows included.
//
// Claimed rows used to be filtered out entirely, the query matching only rows
// with an empty claim. That reads fine while claims last milliseconds, but a
// dequeuer that dies between claiming a row and starting it leaves the row
// claimed until the leader's reaper releases it — up to
// CAESIUM_RUN_QUEUE_CLAIM_STALE_AFTER later. Hiding it for that whole window
// understates queue depth and, worse, makes the one row an operator most needs
// to see — a stuck one — the only invisible row in the queue. So every row is
// returned and annotated with its claim state instead.
//
// A queued request belongs to the namespace it was enqueued in, which is not
// necessarily its job's current one (identity-and-access D1). Under a history
// scope (authz.WithHistoryScope) only rows recorded in a namespace the caller
// may read are returned, renumbered among themselves; a row from before the
// column existed records no namespace and is withheld from every confined
// caller. Unrestricted callers see every row exactly as before.
func (j *jobService) Queue(id uuid.UUID) ([]QueueItem, error) {
	var rows []models.RunQueue
	if err := j.db.WithContext(j.ctx).
		Where("job_id = ?", id).
		Order("priority DESC").
		Order("created_at ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}

	staleCutoff := models.RunQueueStaleCutoff(time.Now(), env.Variables().RunQueueClaimStaleAfter)

	restricted := authz.Restricted(j.ctx)
	if restricted {
		authz.MarkFilteredContext(j.ctx)
	}
	items := make([]QueueItem, 0, len(rows))
	for idx := range rows {
		if restricted && !queueRowVisible(j.ctx, rows[idx]) {
			continue
		}
		params, err := decodeQueueParams(rows[idx].Params)
		if err != nil {
			return nil, err
		}
		state := rows[idx].ClaimState(staleCutoff)
		item := QueueItem{
			ID:         rows[idx].ID,
			Position:   len(items) + 1,
			Priority:   rows[idx].Priority,
			Params:     params,
			EnqueuedAt: rows[idx].CreatedAt,
			ClaimState: state,
			Stale:      state == models.RunQueueClaimStateStale,
			ClaimedBy:  rows[idx].ClaimedBy,
			ClaimedAt:  rows[idx].ClaimedAt,
		}
		if rows[idx].HeldReason != nil {
			item.HeldReason = *rows[idx].HeldReason
		}
		items = append(items, item)
	}
	return items, nil
}

// queueRowVisible reports whether a restricted caller may see row: only a
// row recorded in a namespace the caller may read.
func queueRowVisible(ctx context.Context, row models.RunQueue) bool {
	return row.Namespace != nil && authz.NamespaceAllowed(ctx, *row.Namespace)
}

// queuedRunCancelCheckKey carries the check CancelQueuedRun applies to the
// row's recorded namespace.
type queuedRunCancelCheckKey struct{}

// WithQueuedRunCancelCheck returns ctx carrying check, which CancelQueuedRun
// calls with the queued row's recorded namespace (nil for a row queued before
// the column existed) before it deletes anything; a non-nil error is returned
// unchanged and nothing is deleted. A queued request belongs to the namespace
// it was queued in (identity-and-access D1), which can differ from its job's
// current one, so the caller authorises the cancel against it.
func WithQueuedRunCancelCheck(ctx context.Context, check func(namespace *string) error) context.Context {
	return context.WithValue(ctx, queuedRunCancelCheckKey{}, check)
}

// CancelQueuedRun drops a queued run, after the check ctx carries (see
// WithQueuedRunCancelCheck), if any, accepts the row's recorded namespace.
func (j *jobService) CancelQueuedRun(jobID, queueID uuid.UUID) error {
	if check, ok := j.ctx.Value(queuedRunCancelCheckKey{}).(func(*string) error); ok && check != nil {
		var row models.RunQueue
		err := j.db.WithContext(j.ctx).
			Select("id", "namespace").
			Where("id = ? AND job_id = ?", queueID, jobID).
			Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return runstorage.ErrQueuedRunNotFound
		}
		if err != nil {
			return err
		}
		if err := check(row.Namespace); err != nil {
			return err
		}
	}
	return runstorage.NewStore(j.db.WithContext(j.ctx)).CancelQueuedRun(j.ctx, jobID, queueID)
}

func decodeQueueParams(raw []byte) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var params map[string]string
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, err
	}
	return params, nil
}
