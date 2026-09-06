package freshness

import (
	"context"
	"maps"
	"strings"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// enricherNamespace is the namespace the consumed view is read under. v1 always
// keys dataset identity on name alone, matching Capturer.namespace.
var enricherNamespace *string

// EnrichStartParams is the run-store start-params hook that freezes a run's
// consumed-input watermarks onto the run row AT CREATION, under
// _consumed_watermarks — the same param, in the same format, the evaluator
// already writes for a freshness-derived run.
//
// It exists because the consumed view has to be the one the run actually began
// with. Capturing it from an asynchronous run_started subscriber cannot promise
// that: the event only queues work, so the read can land after the run is
// already executing (an input that advanced in between is then credited to a run
// that never read it), and a full subscriber buffer drops the event outright, at
// which point there is no view at all. Enriching the params inside the creation
// path makes the view part of the INSERT: it is there or the run is not.
//
// Every read here goes through the db handle the run store hands in, never a
// captured connection. Runs are created by stores built over an open
// transaction (internal/trigger/event/router.go), and reading a second
// connection underneath one of those deadlocks the whole database.
//
// The result is read back at completion by Capturer.consumedForRun.
//
// On the queued-concurrency path a run is admitted twice: once when it is
// enqueued (the view rides the run_queue row) and again when the dequeuer
// promotes it, which is when the run actually begins. fromQueue marks that
// second call and makes it RE-read: a run that waited in the queue while its
// inputs advanced began on the newer view, and keeping the admission-time one
// would attribute its output to inputs it never read — the same misattribution
// this capture exists to prevent, only in the other direction (freshness would
// then see the output as behind and derive redundant work).
//
// Refreshing on promotion is correct for a freshness-derived run too, even
// though its param is the evaluator's decision-time view: that view is
// separately durable on the dataset_derivations row (evaluator.recordDerivation
// writes consumed_watermarks there), so overwriting the run param loses nothing
// and makes the run row say what the run truly consumed.
//
// Register it from the server bootstrap under CAESIUM_FRESHNESS_ENABLED. It is a
// plain func rather than a run.StartParamsEnricher so this package keeps no
// dependency on internal/run — the dependency runs the other way.
func EnrichStartParams(ctx context.Context, db *gorm.DB, jobID uuid.UUID, params map[string]string, fromQueue bool) (map[string]string, error) {
	if db == nil || jobID == uuid.Nil {
		return params, nil
	}
	// A freshness-derived run already carries the evaluator's view of exactly
	// the inputs its derivation decision was made on. That is the authoritative
	// view for such a run; never overwrite it — except on queue promotion, where
	// the run is starting now and the decision-time view is no longer what it
	// consumes.
	if raw, ok := params[freshnessConsumedWatermarksParam]; ok && !fromQueue && strings.TrimSpace(raw) != "" {
		return params, nil
	}

	// One indexed read per run creation, which is the whole cost for the common
	// case of a job that declares no dataset.
	var decls []models.DatasetDeclaration
	if err := db.WithContext(ctx).Where("job_id = ?", jobID).Find(&decls).Error; err != nil {
		return params, err
	}

	produces := false
	consumedNames := make([]string, 0, len(decls))
	for i := range decls {
		switch decls[i].Direction {
		case models.DatasetDirectionProduces:
			produces = true
		case models.DatasetDirectionConsumes:
			consumedNames = append(consumedNames, decls[i].Name)
		}
	}
	// Nothing produced means the completion handler returns before it ever wants
	// a snapshot; nothing consumed means there is no input view to freeze.
	if !produces || len(consumedNames) == 0 {
		return params, nil
	}

	// A failed read means the view is UNKNOWN, which is not the same answer as
	// an empty one. Returning the error omits the param (run.enrichedStartParams
	// logs at warn and keeps the caller's params), so completion falls back to
	// its current-watermark read — degraded, but honest. Writing {} here would
	// instead record "this run consumed nothing" as fact, and completion would
	// believe it. It is deliberately not fatal to run creation: freshness is an
	// optional subsystem and a run must still start when its read fails.
	snapshot, err := consumedSnapshot(ctx, db, enricherNamespace, consumedNames)
	if err != nil {
		return params, err
	}

	// Stamp even an EMPTY view: "no input had a watermark when this run was
	// created" is an authoritative answer, and omitting the param would send the
	// completion path back to the current-watermark read this exists to avoid.
	out := maps.Clone(params)
	if out == nil {
		out = make(map[string]string, 1)
	}
	out[freshnessConsumedWatermarksParam] = string(canonicalConsumedJSON(snapshot))
	return out, nil
}
