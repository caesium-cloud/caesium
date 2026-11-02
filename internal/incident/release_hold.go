package incident

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// release_hold (data-circuit-breaker F2). A release through an incident is the
// operator release of POST /v1/datasets/holds/:id/release, reached through the
// action pipeline: ActionOps calls that endpoint's service, and this file decides
// only whether the incident may ask.
//
// The executor never moves the incident. The release emits dataset_released, and
// the subscriber or, failing delivery, the reconcile sweep closes the incident
// through closeHoldIncident. That is one writer of the outcome, whichever of the
// two gets there first, and the summary is the same either way. The final status
// is closed, by way of remediated unless the incident was already escalated.

// maxReleaseTolerance bounds one tolerance window an agent may propose. The REST
// release has no bound: an operator states a window there, but here a human only
// reviews one.
const maxReleaseTolerance = 30 * 24 * time.Hour

// releaseToleranceCaveat is recorded on a release that carries tolerance windows,
// so the approver's record says what they do and do not do.
const releaseToleranceCaveat = "tolerance windows are recorded on the release, not enforced: a breach inside one re-opens the hold"

// ErrHoldNotActive is ActionOps.ReleaseHold's answer when the hold ended first.
// The executor treats it as done: what the action asked for is already true.
var ErrHoldNotActive = errors.New("incident: dataset hold is not active")

// ErrInvalidTolerance refuses a tolerance window that is malformed, out of
// bounds, or names an assertion the hold did not breach.
var ErrInvalidTolerance = errors.New("incident: invalid tolerance window")

// ErrReleaseReasonRequired refuses a release_hold without a reason.
var ErrReleaseReasonRequired = errors.New("incident: release_hold requires a reason")

// HoldRelease is one permitted release, as ActionOps.ReleaseHold carries it out.
type HoldRelease struct {
	HoldID uuid.UUID
	// Principal is recorded as the releaser and the audit actor: the deciding
	// human, or the agent session a playbook let act alone.
	Principal string
	Reason    string
	// Tolerate holds validated windows in the form the proposal stated them,
	// which is what the REST release stores.
	Tolerate map[string]string
	// Namespace is the audit row's namespace: the incident's.
	Namespace string
	// Guard re-runs the boundary inside the release transaction, so a job that
	// moves or a producer that changes after dispatch checked cannot slip through.
	Guard func(tx *gorm.DB) error
}

// resolveHoldID picks the params hold, falling back to the hold a
// data_quality_hold incident was opened for. That one is read from the dedupe key,
// which the server wrote, and is checked like a named hold.
func resolveHoldID(inc *models.Incident, params ActionParams) (uuid.UUID, error) {
	if params.HoldID != nil && *params.HoldID != uuid.Nil {
		return *params.HoldID, nil
	}
	if FailureClass(inc.Class) == ClassDataQualityHold {
		if id, ok := holdIDFromDedupeKey(inc.DedupeKey); ok {
			return id, nil
		}
	}
	return uuid.Nil, errors.New("incident: release_hold requires a hold id (none in params or incident)")
}

// verifyHoldBoundary is the security boundary of release_hold, read from the hold
// row and the live declarations and never from anything the caller supplied. The
// hold must have been opened by the incident's job, in the incident's namespace,
// and while it is active the dataset must still be produced by that job, with no
// producer outside the namespace: a confined operator needs the role in every
// producer's namespace to release over REST, and approving here needs it in the
// incident's alone. A hold that has ended is returned without the producer check,
// because releasing it changes nothing.
//
// Every ownership refusal reads the same, so it does not tell an agent whether a
// foreign hold id exists.
func verifyHoldBoundary(ctx context.Context, db *gorm.DB, inc *models.Incident, holdID uuid.UUID) (*models.DatasetHold, error) {
	refused := fmt.Errorf("%w: hold %s is not a hold of a dataset produced by incident job %s", ErrCrossBoundaryTarget, holdID, inc.JobID)

	var hold models.DatasetHold
	if err := db.WithContext(ctx).First(&hold, "id = ?", holdID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, refused
		}
		return nil, fmt.Errorf("incident: load dataset hold: %w", err)
	}
	if hold.HeldByJobID != inc.JobID {
		return nil, refused
	}
	var openingRun uuid.UUID
	if hold.HeldByRunID != nil {
		openingRun = *hold.HeldByRunID
	}
	if err := verifyNamespaceBoundary(ctx, db, inc, openingRun); err != nil {
		return nil, err
	}
	if hold.Status != models.DatasetHoldStatusActive {
		return &hold, nil
	}

	produces, err := holdWithinIncidentProducers(ctx, db, inc, &hold)
	if err != nil {
		return nil, err
	}
	if !produces {
		return nil, refused
	}
	return &hold, nil
}

// holdWithinIncidentProducers reports whether the incident's job is a live
// declared producer of the held dataset and every other producer shares the
// incident's namespace. It mirrors freshness.LockProducersTx, which this package
// cannot import (internal/freshness imports internal/run, which imports this one):
// on PostgreSQL the rows are read FOR SHARE, so a concurrent move or prune of a
// producer waits for the release transaction this guards.
func holdWithinIncidentProducers(ctx context.Context, db *gorm.DB, inc *models.Incident, hold *models.DatasetHold) (bool, error) {
	var producers []struct {
		JobID     uuid.UUID
		Namespace string
	}
	q := db.WithContext(ctx).Table("dataset_declarations").
		Select("jobs.id AS job_id, jobs.namespace AS namespace").
		Joins("JOIN jobs ON jobs.id = dataset_declarations.job_id AND jobs.deleted_at IS NULL").
		Where("dataset_declarations.name = ? AND dataset_declarations.direction = ?", hold.Name, models.DatasetDirectionProduces).
		Where("COALESCE(dataset_declarations.namespace, '') = ?", hold.Namespace)
	if db.Name() == "postgres" {
		q = q.Clauses(clause.Locking{Strength: clause.LockingStrengthShare})
	}
	if err := q.Find(&producers).Error; err != nil {
		return false, fmt.Errorf("incident: load dataset producers: %w", err)
	}
	found := false
	for i := range producers {
		if models.NamespaceOrDefault(producers[i].Namespace) != IncidentNamespace(inc) {
			return false, nil
		}
		found = found || producers[i].JobID == inc.JobID
	}
	return found, nil
}

// validateTolerate parses and bounds each window. ActionOps applies the REST
// grammar on top.
func validateTolerate(raw map[string]string) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(raw))
	for assertion, window := range raw {
		assertion, window = strings.TrimSpace(assertion), strings.TrimSpace(window)
		d, err := time.ParseDuration(window)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %q is not a duration (e.g. 24h)", ErrInvalidTolerance, assertion, window)
		}
		if d <= 0 || d > maxReleaseTolerance {
			return nil, fmt.Errorf("%w: %s: %q must be positive and at most %s", ErrInvalidTolerance, assertion, window, maxReleaseTolerance)
		}
		out[assertion] = window
	}
	return out, nil
}

// requireBreached refuses a window on an assertion the hold did not breach, so
// each one means something on the record an approver signs. It applies to a
// proposal only: the row keeps the latest breach's verdicts, so the assertion a
// human approved may have left them by the time the action runs.
func requireBreached(hold *models.DatasetHold, tolerate map[string]string) error {
	if len(tolerate) == 0 {
		return nil
	}
	var violations []struct {
		Assertion string `json:"assertion"`
	}
	if len(hold.Violations) > 0 {
		if err := json.Unmarshal(hold.Violations, &violations); err != nil {
			return fmt.Errorf("%w: the hold's violations cannot be read", ErrInvalidTolerance)
		}
	}
	breached := make(map[string]struct{}, len(violations))
	for i := range violations {
		breached[violations[i].Assertion] = struct{}{}
	}
	for assertion := range tolerate {
		if _, ok := breached[assertion]; !ok {
			return fmt.Errorf("%w: %q is not an assertion this hold breached", ErrInvalidTolerance, assertion)
		}
	}
	return nil
}

// checkedRelease is a release_hold that has passed every check short of running.
type checkedRelease struct {
	hold     *models.DatasetHold
	reason   string
	tolerate map[string]string
}

// checkRelease applies every check a release_hold must pass, at proposal and again
// at dispatch. A hold that has ended passes with nothing left to do: its reason and
// windows no longer describe anything.
func (e *Executor) checkRelease(ctx context.Context, inc *models.Incident, params ActionParams) (*checkedRelease, error) {
	holdID, err := resolveHoldID(inc, params)
	if err != nil {
		return nil, err
	}
	hold, err := verifyHoldBoundary(ctx, e.store.DB(), inc, holdID)
	if err != nil {
		return nil, err
	}
	release := &checkedRelease{hold: hold}
	if hold.Status != models.DatasetHoldStatusActive {
		return release, nil
	}
	release.reason = strings.TrimSpace(params.Reason)
	if release.reason == "" {
		return nil, ErrReleaseReasonRequired
	}
	release.tolerate, err = validateTolerate(params.Tolerate)
	if err != nil {
		return nil, err
	}
	return release, nil
}

// settleReleaseProposal answers a release_hold bound for the approval gate when no
// human should be asked: a proposal that fails its checks is recorded failed, and
// one whose hold has already ended is recorded executed. It reports whether it
// finished the action. An approval on a foreign hold would put that hold in front
// of a human, and one on an ended hold could change nothing.
func (e *Executor) settleReleaseProposal(ctx context.Context, inc *models.Incident, action *models.AgentAction, params ActionParams) (bool, error) {
	release, err := e.checkRelease(ctx, inc, params)
	if err == nil {
		if release.hold.Status != models.DatasetHoldStatusActive {
			e.finish(ctx, action, models.AgentActionStatusExecuted, holdAlreadyReleased(release.hold))
			return true, nil
		}
		err = requireBreached(release.hold, release.tolerate)
	}
	if err != nil {
		e.finish(ctx, action, models.AgentActionStatusFailed, map[string]any{"error": err.Error()})
		return true, err
	}
	return false, nil
}

// dispatchReleaseHold executes a release_hold. A replay and a release that lost to
// another one end the same way: the hold is over, so the action says so and
// succeeds, with no second release.
func (e *Executor) dispatchReleaseHold(ctx context.Context, inc *models.Incident, action *models.AgentAction, params ActionParams) (map[string]any, error) {
	release, err := e.checkRelease(ctx, inc, params)
	if err != nil {
		return nil, err
	}
	hold := release.hold
	if hold.Status != models.DatasetHoldStatusActive {
		return holdAlreadyReleased(hold), nil
	}
	if e.ops == nil {
		return nil, errNoOps
	}
	principal := e.releasePrincipal(ctx, inc, action)
	err = e.ops.ReleaseHold(ctx, HoldRelease{
		HoldID:    hold.ID,
		Principal: principal,
		Reason:    release.reason,
		Tolerate:  release.tolerate,
		Namespace: IncidentNamespace(inc),
		Guard: func(tx *gorm.DB) error {
			_, err := verifyHoldBoundary(ctx, tx, inc, hold.ID)
			return err
		},
	})
	if errors.Is(err, ErrHoldNotActive) {
		var ended models.DatasetHold
		if err := e.store.DB().WithContext(ctx).First(&ended, "id = ?", hold.ID).Error; err != nil {
			return nil, fmt.Errorf("incident: load released dataset hold: %w", err)
		}
		return holdAlreadyReleased(&ended), nil
	}
	if err != nil {
		return nil, err
	}
	result := map[string]any{
		"hold_id":           hold.ID.String(),
		"dataset_namespace": hold.Namespace,
		"dataset":           hold.Name,
		"reason":            release.reason,
		"released_by":       principal,
	}
	if len(release.tolerate) > 0 {
		result["tolerate"] = release.tolerate
		result["caveat"] = releaseToleranceCaveat
	}
	return result, nil
}

func holdAlreadyReleased(hold *models.DatasetHold) map[string]any {
	return map[string]any{
		"hold_id":          hold.ID.String(),
		"already_released": true,
		"release_reason":   hold.ReleaseReason,
		"released_by":      hold.ReleasedBy,
	}
}

// releasePrincipal names who is releasing: the human whose decision is on the
// approval row, or the agent session a playbook let act alone. ExecuteApproved is
// the only path that dispatches a claimed row.
func (e *Executor) releasePrincipal(ctx context.Context, inc *models.Incident, action *models.AgentAction) string {
	if action.Status == models.AgentActionStatusExecuting {
		decider, _ := e.approvalDecision(ctx, action.ID)
		return decider
	}
	if action.SessionID != nil {
		return "agent:session:" + action.SessionID.String()
	}
	return "agent:incident:" + inc.ID.String()
}
