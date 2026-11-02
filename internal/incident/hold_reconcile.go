package incident

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
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

const (
	defaultHoldReconcileInterval = 30 * time.Second
	// holdReconcileBatch bounds every query a sweep makes.
	holdReconcileBatch = 100
)

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
// opens the missing incident of active holds opened more than grace ago. grace
// leaves a fresh hold to its dataset_held event. It is leader-gated like the
// subscriber, and idempotent: holdOpenGuard gives a hold one incident for its
// lifetime, so a hold whose incident has closed is not opened again.
func (s *Subscriber) ReconcileHolds(ctx context.Context, grace time.Duration) error {
	if !s.isLeader(ctx) {
		return nil
	}
	return errors.Join(
		s.closeIncidentsOfEndedHolds(ctx),
		s.openIncidentsOfUncoveredHolds(ctx, time.Now().UTC().Add(-grace)),
	)
}

// closeIncidentsOfEndedHolds pages through the open hold incidents by id. Each
// page is bounded, and paging cannot starve an incident behind ones whose holds
// are still active.
func (s *Subscriber) closeIncidentsOfEndedHolds(ctx context.Context) error {
	after := uuid.Nil
	for {
		var incidents []models.Incident
		if err := s.db.WithContext(ctx).
			Select("id", "job_id", "class", "task_name", "dedupe_key", "opened_at").
			Where("class = ? AND active_dedupe_key IS NOT NULL AND id > ?", string(ClassDataQualityHold), after).
			Order("id ASC").
			Limit(holdReconcileBatch).
			Find(&incidents).Error; err != nil {
			return err
		}
		if len(incidents) == 0 {
			return nil
		}
		if err := s.closeEndedPage(ctx, incidents); err != nil {
			return err
		}
		if len(incidents) < holdReconcileBatch {
			return nil
		}
		after = incidents[len(incidents)-1].ID
	}
}

func (s *Subscriber) closeEndedPage(ctx context.Context, incidents []models.Incident) error {
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

// openIncidentsOfUncoveredHolds opens at most holdReconcileBatch incidents per
// sweep. The anti-join keeps a covered hold out of the page, and holdScanAfter
// moves the page on each sweep, so an uncovered hold whose open keeps failing
// takes its turn and cannot hold a place at the head. Each hold is read and opened
// on its own for the same reason: one bad row fails only itself. Every incident
// status counts, so a hold whose incident closed stays closed.
func (s *Subscriber) openIncidentsOfUncoveredHolds(ctx context.Context, openedBefore time.Time) error {
	if !env.Variables().DataAssertionsEnabled {
		return nil
	}
	var uncovered []uuid.UUID
	if err := s.db.WithContext(ctx).
		Model(&models.DatasetHold{}).
		Where("status = ? AND opened_at < ? AND id > ?", models.DatasetHoldStatusActive, openedBefore, s.holdScanAfter).
		Where(`NOT EXISTS (SELECT 1 FROM incidents
			WHERE incidents.job_id = dataset_holds.held_by_job_id AND incidents.class = ?
			AND incidents.dedupe_key LIKE '%|' || ? || CAST(dataset_holds.id AS TEXT))`,
			string(ClassDataQualityHold), holdSubjectPrefix).
		Order("id ASC").
		Limit(holdReconcileBatch).
		Pluck("id", &uncovered).Error; err != nil {
		return err
	}
	s.holdScanAfter = uuid.Nil
	if len(uncovered) == holdReconcileBatch {
		s.holdScanAfter = uncovered[len(uncovered)-1]
	}
	if s.afterHoldScan != nil {
		s.afterHoldScan()
	}
	var failures []error
	for _, id := range uncovered {
		var hold models.DatasetHold
		err := s.db.WithContext(ctx).First(&hold, "id = ?", id).Error
		if err == nil {
			log.Info("incident: opening a hold's incident whose dataset_held was not delivered", "hold_id", id)
			err = s.openIncidentFromHold(ctx, &hold)
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("hold %s: %w", id, err))
		}
	}
	return errors.Join(failures...)
}

// openIncidentFromHold opens a hold's incident from its row. Unlike the event,
// the row's verdicts are the latest breach's, and that breach can belong to
// another producer of the dataset in another namespace: its observations and its
// baseline, however well scoped, are then not this incident's to store. They are
// left out on the rule shapeHolds applies to a read: verdicts are served only
// when the run that wrote them is attributable to the reader's namespace.
func (s *Subscriber) openIncidentFromHold(ctx context.Context, hold *models.DatasetHold) error {
	var runID uuid.UUID
	if hold.HeldByRunID != nil {
		runID = *hold.HeldByRunID
	}
	scope, err := s.holdScope(ctx, hold.HeldByJobID, runID)
	if err != nil {
		return err
	}
	held := holdEvent{
		HoldID:    hold.ID,
		Namespace: hold.Namespace,
		Dataset:   hold.Name,
		Reason:    hold.Reason,
		JobAlias:  hold.HeldByJobAlias,
		Step:      hold.HeldByStepName,
		TaskID:    hold.HeldByTaskID,
		TaskRunID: hold.HeldByTaskRunID,
	}
	owned, err := s.runInNamespace(ctx, hold.LatestBreachRunID(), scope.namespace)
	if err != nil {
		return err
	}
	if owned {
		held.Violations = []byte(hold.Violations)
	}
	s.openHoldIncident(ctx, hold.HeldByJobID, runID, scope, held, hold.Impact, hold.OpenedAt)
	return nil
}

// runInNamespace reports whether the run is attributable to namespace. No run,
// or one that no longer exists, is attributable to nobody.
func (s *Subscriber) runInNamespace(ctx context.Context, runID *uuid.UUID, namespace string) (bool, error) {
	if runID == nil {
		return false, nil
	}
	var run models.JobRun
	err := s.db.WithContext(ctx).Select("id", "namespace").First(&run, "id = ?", *runID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return models.NamespaceOrDefault(run.Namespace) == namespace, nil
}
