package incident

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/baselineevidence"
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/lineage"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// data_quality_hold incidents (data-circuit-breaker F1). Every other incident
// starts from a failure and ends on a green run. This one starts from a green
// run, because the breaker fails the dataset rather than the task, and ends when
// the hold is released:
//
//   - One incident per hold. The hold id is in the dedupe key, so two datasets
//     held by one job stay apart, and a new hold after a release opens a new
//     incident however recently the last one closed: cooldown is keyed on the
//     dedupe key, so it cannot reach a different hold.
//   - A success event never closes one (Store.OpenForJobTask). dataset_released
//     does, because a release: manual hold stays active across any number of
//     green runs.
//   - dataset_held is emitted once per hold, so a repeat breach does not reach
//     here: the occurrence count lives on the hold.
//   - Neither event is guaranteed to reach this subscriber, so ReconcileHolds
//     (hold_reconcile.go) converges incidents on hold state. The events are the
//     fast path; the hold row decides.

// evidenceImpactCap bounds the cone embedded in an incident's evidence. The cone
// has a node per lineage row, so it grows with the history of every downstream
// job; breadth-first order means the nodes cut are the furthest away.
const evidenceImpactCap = 100

// holdEvent is the part of run.DatasetHoldEvent this package reads. It is a
// local mirror because internal/run imports this package; TestHoldEventOpensIncident
// in internal/run pins the two together.
type holdEvent struct {
	HoldID           uuid.UUID       `json:"hold_id"`
	Namespace        string          `json:"namespace"`
	Dataset          string          `json:"dataset"`
	Reason           string          `json:"reason"`
	JobAlias         string          `json:"job_alias"`
	Step             string          `json:"step"`
	TaskID           *uuid.UUID      `json:"task_id"`
	TaskRunID        *uuid.UUID      `json:"task_run_id"`
	Violations       json.RawMessage `json:"violations"`
	ImpactedDatasets int             `json:"impacted_datasets"`
	ReleaseReason    string          `json:"release_reason"`
	ReleasedBy       string          `json:"released_by"`
}

const holdSubjectPrefix = "hold:"

// holdSubject is the OpenParams.Subject of a hold's incident.
func holdSubject(holdID uuid.UUID) string {
	return holdSubjectPrefix + holdID.String()
}

func decodeHoldEvent(evt event.Event) (holdEvent, bool) {
	var held holdEvent
	if err := json.Unmarshal(evt.Payload, &held); err != nil {
		log.Warn("incident: could not decode dataset hold event", "type", evt.Type, "error", err)
		return holdEvent{}, false
	}
	if held.HoldID == uuid.Nil || evt.JobID == uuid.Nil {
		log.Warn("incident: dataset hold event names no hold or job", "type", evt.Type, "hold_id", held.HoldID, "job_id", evt.JobID)
		return holdEvent{}, false
	}
	return held, true
}

// handleHold opens the incident for a dataset hold. Everything but the impact
// cone comes from the event, so the task and the run are the producing ones
// named there, and the failed-instance attribution in resolveContext never
// applies: the task succeeded.
func (s *Subscriber) handleHold(ctx context.Context, evt event.Event) {
	if !env.Variables().DataAssertionsEnabled {
		return
	}
	held, ok := decodeHoldEvent(evt)
	if !ok {
		return
	}

	// A redelivered dataset_held must not count as an occurrence, and once the
	// incident has closed it must not open a second one. The unique active key
	// backstops the first case if this read fails.
	if existing, err := s.store.incidentForHold(ctx, evt.JobID, held.HoldID); err != nil {
		log.Warn("incident: could not check for a hold's incident; opening", "hold_id", held.HoldID, "error", err)
	} else if existing != nil {
		log.Debug("incident: dataset hold already has an incident", "hold_id", held.HoldID, "incident_id", existing.ID)
		return
	}

	// A hold released before this event arrived (a delayed redelivery) needs no
	// remediation, and its release may already have been handled, which would
	// leave the incident open for good.
	impact, released := s.holdImpact(ctx, held.HoldID)
	if released {
		log.Info("incident: dataset hold released before its incident opened; skipping", "hold_id", held.HoldID)
		return
	}

	s.openHoldIncident(ctx, evt.JobID, evt.RunID, held, impact)
}

// openHoldIncident opens the incident for a hold, from its event or from its row.
// untrusted names breaching runs other than runID.
func (s *Subscriber) openHoldIncident(ctx context.Context, jobID, runID uuid.UUID, held holdEvent, impact datatypes.JSON, untrusted ...uuid.UUID) {
	// Run-level on purpose: with a TaskID, resolveContext would classify from
	// the task's rows. A hold whose run is gone falls back to the job's
	// namespace, which is also what Store.OpenOrAppend does with no run.
	fc := s.resolveContext(ctx, event.Event{JobID: jobID, RunID: runID})
	if fc.namespace == "" {
		fc.namespace = s.resolveContext(ctx, event.Event{JobID: jobID}).namespace
	}
	namespace := models.NamespaceOrDefault(fc.namespace)

	var runRef *uuid.UUID
	if runID != uuid.Nil {
		runRef = &runID
	}

	class := s.classifier.Classify(Signal{EventType: string(event.TypeDatasetHeld)})
	violations, summary := shapeHoldVerdict(held)
	s.recordIncident(ctx, OpenParams{
		Namespace: namespace,
		JobID:     jobID,
		// The breaching run: FreezeAllowlist excludes its outputs from the seeds
		// of the impact walk, so a poisoned run cannot widen the allowlist.
		RunID:                  runRef,
		UntrustedRunIDs:        untrusted,
		TaskID:                 held.TaskID,
		TaskName:               held.Step,
		Class:                  class,
		Subject:                holdSubject(held.HoldID),
		LastError:              summary,
		Evidence:               buildHoldEvidence(ctx, class, namespace, held, violations, impact),
		BackfillID:             fc.backfillID,
		RemediationTargetRunID: runRef,
	})
}

// holdImpact reads the impact cone frozen on the hold when it opened, and
// whether the hold has been released. The cone does not travel in the event
// because the event is served to /v1/events and notification channels, and the
// cone names other namespaces' datasets; it does not change after open, so this
// read cannot see a moved value. Best-effort: a hold with no readable cone is
// still a hold, so the incident opens without one.
func (s *Subscriber) holdImpact(ctx context.Context, holdID uuid.UUID) (datatypes.JSON, bool) {
	var hold models.DatasetHold
	err := s.db.WithContext(ctx).Select("id", "status", "impact").First(&hold, "id = ?", holdID).Error
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			log.Warn("incident: could not read a dataset hold's impact cone", "hold_id", holdID, "error", err)
		}
		return nil, false
	}
	return hold.Impact, hold.Status == models.DatasetHoldStatusReleased
}

// shapeHoldVerdict returns the verdicts and the summary as an incident may store
// them. An incident's evidence is served as stored to every reader of it and to
// its agent, so a verdict with an unowned baseline (one recorded before baselines
// were namespace-owned, still pending or on the hold row) loses its baseline
// evidence here rather than on read. Verdicts that do not decode are left out.
func shapeHoldVerdict(held holdEvent) (any, string) {
	payload := map[string]any{"error": holdSummary(held)}
	var violations any
	if err := json.Unmarshal(held.Violations, &violations); err == nil {
		payload["data_violations"] = violations
	}
	baselineevidence.WithholdJSON(payload)
	summary, _ := payload["error"].(string)
	return payload["data_violations"], summary
}

// holdSummary renders the first violation for the incident feed, in the shape
// run.DataViolation.String gives a failed task. It quotes the verdict's own
// message, so a confined read withholds it exactly as it withholds that text on
// the task row.
func holdSummary(held holdEvent) string {
	var violations []struct {
		Metric    string `json:"metric"`
		Assertion string `json:"assertion"`
		Message   string `json:"message"`
	}
	if err := json.Unmarshal(held.Violations, &violations); err != nil || len(violations) == 0 {
		return fmt.Sprintf("dataset %q held: %s", held.Dataset, held.Reason)
	}
	first := violations[0]
	summary := fmt.Sprintf("dataset %q metric %q assertion %q: %s", held.Dataset, first.Metric, first.Assertion, first.Message)
	if more := len(violations) - 1; more > 0 {
		summary += fmt.Sprintf(" (+%d more)", more)
	}
	return summary
}

// buildHoldEvidence renders the incident's evidence: what broke, by how much,
// against what baseline, and how far it reaches. The dataset's namespace is a
// data axis, unrelated to the tenant namespace the incident belongs to.
func buildHoldEvidence(ctx context.Context, class FailureClass, namespace string, held holdEvent, violations any, impact datatypes.JSON) datatypes.JSON {
	m := map[string]any{
		"class":             string(class),
		"hold_id":           held.HoldID.String(),
		"dataset_namespace": held.Namespace,
		"dataset":           held.Dataset,
		"reason":            held.Reason,
		"impacted_datasets": held.ImpactedDatasets,
	}
	if held.TaskRunID != nil {
		m["task_run_id"] = held.TaskRunID.String()
	}
	if violations != nil {
		m["violations"] = violations
	}
	if cone := redactedHoldImpact(ctx, namespace, impact); cone != nil {
		m["impact"] = cone
		if held.ImpactedDatasets == 0 {
			m["impacted_datasets"] = cone.DownstreamTotal
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		log.Warn("incident: could not encode hold evidence", "hold_id", held.HoldID, "error", err)
		return nil
	}
	return datatypes.JSON(b)
}

// holdEvidenceImpact is a cone as an incident's evidence carries it.
type holdEvidenceImpact struct {
	lineage.RedactedImpactResult
	// DownstreamTotal counts the redacted nodes before evidenceImpactCap.
	DownstreamTotal int `json:"downstream_total"`
}

// redactedHoldImpact shapes a hold's stored cone for its incident. Evidence is
// written once and served to every reader of the incident and to its agent, so
// the reader it is redacted for is the incident's own namespace: nodes owned by
// any other collapse to placeholders, as they do for a confined caller of
// GET /v1/lineage/impact. A cone that cannot be read is left out rather than
// shown unredacted.
func redactedHoldImpact(ctx context.Context, namespace string, impact datatypes.JSON) *holdEvidenceImpact {
	if len(impact) == 0 {
		return nil
	}
	redactor := authz.NewRedactor(authz.WithAllowedNamespaces(ctx, []string{namespace}))
	cone, err := lineage.RedactStoredImpact(redactor, impact)
	if err != nil {
		log.Warn("incident: could not redact a dataset hold's impact cone", "error", err)
		return nil
	}
	out := &holdEvidenceImpact{RedactedImpactResult: *cone, DownstreamTotal: len(cone.Downstream)}
	if len(out.Downstream) > evidenceImpactCap {
		out.Downstream = out.Downstream[:evidenceImpactCap]
	}
	return out
}

// handleHoldRelease closes a hold's incident.
func (s *Subscriber) handleHoldRelease(ctx context.Context, evt event.Event) {
	held, ok := decodeHoldEvent(evt)
	if !ok {
		return
	}
	inc, err := s.store.incidentForHold(ctx, evt.JobID, held.HoldID)
	if err != nil {
		log.Warn("incident: could not load a released hold's incident", "hold_id", held.HoldID, "error", err)
		return
	}
	if inc == nil || inc.Status.IsTerminal() {
		return
	}
	s.closeHoldIncident(ctx, inc, holdReleaseSummary(held.ReleaseReason, held.ReleasedBy))
}

func holdReleaseSummary(reason, by string) string {
	return fmt.Sprintf("dataset hold released: %s by %s", reason, by)
}

// closeHoldIncident closes an incident whose hold is over. An escalated incident
// has handed the problem to a human and cannot be remediated, but the problem is
// gone, so it closes without being remediated. The status is not read first: an
// operator can reject the last approval between such a read and the write, so
// the status machine decides, under the row lock each transition takes. An
// incident that is already terminal refuses both, which is the idempotent case.
func (s *Subscriber) closeHoldIncident(ctx context.Context, inc *models.Incident, summary string) {
	err := s.remediate(ctx, inc, summary)
	if errors.Is(err, ErrInvalidTransition) {
		_, err = s.store.Transition(ctx, inc.ID, models.IncidentStatusClosed, summary)
	}
	if err != nil && !errors.Is(err, ErrInvalidTransition) {
		log.Warn("incident: could not close a hold's incident", "incident_id", inc.ID, "error", err)
	}
}
