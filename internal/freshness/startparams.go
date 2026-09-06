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
// On the queued-concurrency path the view is frozen when the run is first
// admitted and rides the run_queue row to the eventual start, so a queued run
// records a view no NEWER than the one it read. That errs toward reporting an
// output as behind its inputs, which is the safe direction — the failure this
// capture exists to prevent is the opposite one.
//
// Register it from the server bootstrap under CAESIUM_FRESHNESS_ENABLED. It is a
// plain func rather than a run.StartParamsEnricher so this package keeps no
// dependency on internal/run — the dependency runs the other way.
func EnrichStartParams(ctx context.Context, db *gorm.DB, jobID uuid.UUID, params map[string]string) (map[string]string, error) {
	if db == nil || jobID == uuid.Nil {
		return params, nil
	}
	// A freshness-derived run already carries the evaluator's view of exactly
	// the inputs its derivation decision was made on. That is the authoritative
	// view for such a run; never overwrite it.
	if raw, ok := params[freshnessConsumedWatermarksParam]; ok && strings.TrimSpace(raw) != "" {
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

	// Stamp even an EMPTY view: "no input had a watermark when this run was
	// created" is an authoritative answer, and omitting the param would send the
	// completion path back to the current-watermark read this exists to avoid.
	out := maps.Clone(params)
	if out == nil {
		out = make(map[string]string, 1)
	}
	out[freshnessConsumedWatermarksParam] = string(canonicalConsumedJSON(
		consumedSnapshot(ctx, db, enricherNamespace, consumedNames),
	))
	return out, nil
}
