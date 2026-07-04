package start

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
)

// errIncidentOpNotWired marks a tier-1/2 action whose concrete server-side
// operation is not yet wired in this phase. The live Phase-0 path only exercises
// RetryFromFailure (the deterministic auto_retry_backoff rule and the
// snooze_retry timer both retry a run); the remaining catalog operations get
// their concrete wiring when Stream C lands the /v1/agent/* endpoint, and no
// surface reaches them until then.
var errIncidentOpNotWired = errors.New("incident: action operation not wired in this phase")

// incidentActionOps is the Phase-0 server-side implementation of
// incident.ActionOps. It backs the durable snooze_retry timer and the
// deterministic auto_retry_backoff rule with the admit-aware retry entry point
// (run.Store.RetryFromFailureAdmitted — the retry safety valves), so the
// autonomous Phase-0 path actually runs. The other methods are unreachable until
// Stream C wires the agent tool surface and return errIncidentOpNotWired.
type incidentActionOps struct {
	runStore *run.Store
}

func newIncidentActionOps(runStore *run.Store) *incidentActionOps {
	return &incidentActionOps{runStore: runStore}
}

func (o *incidentActionOps) RetryFromFailure(_ context.Context, runID uuid.UUID) error {
	_, err := o.runStore.RetryFromFailureAdmitted(runID)
	return err
}

func (o *incidentActionOps) RetryCallbacks(_ context.Context, _ uuid.UUID) error {
	return errIncidentOpNotWired
}

func (o *incidentActionOps) RerunWithParams(_ context.Context, _ uuid.UUID, _ map[string]string) (uuid.UUID, error) {
	return uuid.Nil, errIncidentOpNotWired
}

func (o *incidentActionOps) QuarantineReplay(_ context.Context, _ uuid.UUID, _ map[string]string) (json.RawMessage, error) {
	return nil, errIncidentOpNotWired
}

func (o *incidentActionOps) Notify(_ context.Context, _, _ string) error {
	return errIncidentOpNotWired
}

func (o *incidentActionOps) Escalate(_ context.Context, _ uuid.UUID, _, _ string) error {
	return errIncidentOpNotWired
}

func (o *incidentActionOps) SetJobPaused(_ context.Context, _ uuid.UUID, _ bool) error {
	return errIncidentOpNotWired
}

func (o *incidentActionOps) ClearCacheEntry(_ context.Context, _ uuid.UUID, _ string) error {
	return errIncidentOpNotWired
}

func (o *incidentActionOps) SuppressDownstreamAlerts(_ context.Context, _ uuid.UUID, _ time.Time) error {
	return errIncidentOpNotWired
}

func (o *incidentActionOps) ExtendSLAOnce(_ context.Context, _ uuid.UUID, _ time.Duration) error {
	return errIncidentOpNotWired
}
