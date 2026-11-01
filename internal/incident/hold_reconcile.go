package incident

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
)

// Hold reconciliation. The incident subscriber is leader-gated, but each node
// has its own bus and any node can publish a pending event and mark it
// dispatched. So dataset_held or dataset_released can be consumed by a follower,
// which drops it, and never reach the leader. A lost dataset_released would
// strand the incident for good: a green run never closes this class, and a second
// release is refused. Leadership changes and dropped events end up in the same
// place. The sweep below therefore converges incidents on the hold rows.
//
// Ordinary failure events have the same gap, with no row to converge on.

const defaultHoldReconcileInterval = 30 * time.Second

// holdIDFromDedupeKey reads the hold id back out of a hold incident's dedupe key,
// where the subject is the last segment.
func holdIDFromDedupeKey(key string) (uuid.UUID, bool) {
	i := strings.LastIndex(key, "|"+holdSubjectPrefix)
	if i < 0 {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(key[i+len("|"+holdSubjectPrefix):])
	return id, err == nil
}

// RunHoldReconciler sweeps until ctx is cancelled. Like ApprovalRedriver it
// sweeps once before the ticker: a restart is one of the ways an event is lost.
// The interval is also the grace an open hold gets before the sweep steps in.
func (s *Subscriber) RunHoldReconciler(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultHoldReconcileInterval
	}
	sweep := func() {
		if err := s.ReconcileHolds(ctx, interval); err != nil && ctx.Err() == nil {
			log.Error("incident hold reconcile sweep failed", "error", err)
		}
	}
	sweep()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

// ReconcileHolds closes the incident of every hold that is no longer active and
// opens the missing incident of every active hold opened more than grace ago.
// grace leaves a fresh hold to its dataset_held event. It is leader-gated like
// the subscriber, and idempotent: the hold id in the dedupe key means a hold has
// one incident, and a hold whose incident has closed is not opened again.
func (s *Subscriber) ReconcileHolds(ctx context.Context, grace time.Duration) error {
	if !s.isLeader(ctx) {
		return nil
	}
	return errors.Join(
		s.closeIncidentsOfEndedHolds(ctx),
		s.openIncidentsOfUncoveredHolds(ctx, time.Now().UTC().Add(-grace)),
	)
}

func (s *Subscriber) closeIncidentsOfEndedHolds(ctx context.Context) error {
	var incidents []models.Incident
	if err := s.db.WithContext(ctx).
		Select("id", "job_id", "class", "task_name", "dedupe_key", "opened_at").
		Where("class = ? AND active_dedupe_key IS NOT NULL", string(ClassDataQualityHold)).
		Find(&incidents).Error; err != nil {
		return err
	}
	if len(incidents) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(incidents))
	for i := range incidents {
		if id, ok := holdIDFromDedupeKey(incidents[i].DedupeKey); ok {
			ids = append(ids, id)
		}
	}
	var rows []models.DatasetHold
	if err := s.db.WithContext(ctx).
		Select("id", "status", "release_reason", "released_by").
		Where("id IN ?", ids).
		Find(&rows).Error; err != nil {
		return err
	}
	holds := make(map[uuid.UUID]*models.DatasetHold, len(rows))
	for i := range rows {
		holds[rows[i].ID] = &rows[i]
	}
	for i := range incidents {
		id, ok := holdIDFromDedupeKey(incidents[i].DedupeKey)
		if !ok {
			continue
		}
		hold, found := holds[id]
		switch {
		case !found:
			s.closeHoldIncident(ctx, &incidents[i], "dataset hold no longer exists")
		case hold.Status != models.DatasetHoldStatusActive:
			log.Info("incident: closing a hold's incident whose release was not delivered", "incident_id", incidents[i].ID, "hold_id", id)
			s.closeHoldIncident(ctx, &incidents[i], holdReleaseSummary(hold.ReleaseReason, hold.ReleasedBy))
		}
	}
	return nil
}

func (s *Subscriber) openIncidentsOfUncoveredHolds(ctx context.Context, openedBefore time.Time) error {
	if !env.Variables().DataAssertionsEnabled {
		return nil
	}
	var active []models.DatasetHold
	if err := s.db.WithContext(ctx).
		Select("id", "held_by_job_id").
		Where("status = ? AND opened_at < ?", models.DatasetHoldStatusActive, openedBefore).
		Find(&active).Error; err != nil {
		return err
	}
	if len(active) == 0 {
		return nil
	}
	jobIDs := make([]uuid.UUID, 0, len(active))
	for i := range active {
		jobIDs = append(jobIDs, active[i].HeldByJobID)
	}
	// Every status counts: a hold whose incident closed stays closed.
	var keys []string
	if err := s.db.WithContext(ctx).
		Model(&models.Incident{}).
		Where("class = ? AND job_id IN ?", string(ClassDataQualityHold), jobIDs).
		Pluck("dedupe_key", &keys).Error; err != nil {
		return err
	}
	covered := make(map[uuid.UUID]struct{}, len(keys))
	for _, key := range keys {
		if id, ok := holdIDFromDedupeKey(key); ok {
			covered[id] = struct{}{}
		}
	}
	var missing []uuid.UUID
	for i := range active {
		if _, ok := covered[active[i].ID]; !ok {
			missing = append(missing, active[i].ID)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	var holds []models.DatasetHold
	if err := s.db.WithContext(ctx).Where("id IN ?", missing).Find(&holds).Error; err != nil {
		return err
	}
	for i := range holds {
		log.Info("incident: opening a hold's incident whose dataset_held was not delivered", "hold_id", holds[i].ID)
		s.openIncidentFromHold(ctx, &holds[i])
	}
	return nil
}

// openIncidentFromHold opens a hold's incident from its row. The row's verdicts
// are the latest breach's, and any later breach's run is as untrusted as the
// opening one.
func (s *Subscriber) openIncidentFromHold(ctx context.Context, hold *models.DatasetHold) {
	held := holdEvent{
		HoldID:     hold.ID,
		Namespace:  hold.Namespace,
		Dataset:    hold.Name,
		Reason:     hold.Reason,
		JobAlias:   hold.HeldByJobAlias,
		Step:       hold.HeldByStepName,
		TaskID:     hold.HeldByTaskID,
		TaskRunID:  hold.HeldByTaskRunID,
		Violations: []byte(hold.Violations),
	}
	var runID uuid.UUID
	if hold.HeldByRunID != nil {
		runID = *hold.HeldByRunID
	}
	var untrusted []uuid.UUID
	if hold.LastBreachRunID != nil {
		untrusted = append(untrusted, *hold.LastBreachRunID)
	}
	s.openHoldIncident(ctx, hold.HeldByJobID, runID, held, hold.Impact, untrusted...)
}
