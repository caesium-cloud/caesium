package run

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sync/atomic"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// StartParamsFence re-checks the enriched start params inside the admission
// transaction, once the run's tenancy namespace is frozen and before any path
// writes the run (or its run_queue row).
//
// It exists because the StartParamsEnricher runs before that transaction: an
// apply that moves the job to another namespace (and changes its
// declarations) can land between the enricher's read and the freeze, and the
// run would then be persisted in the new namespace carrying a view read
// through the old namespace's declarations (identity-and-access B6). The
// fence is handed the admission transaction, the frozen namespace and the
// enriched params; it returns the params to persist.
//
// Every read MUST go through tx, for the same reason the enricher reads on
// the store's handle. A failure is not fatal to run creation: the fence
// returns the params it can stand behind alongside the error (nil means the
// params it was handed).
type StartParamsFence func(ctx context.Context, tx *gorm.DB, jobID uuid.UUID, namespace string, params map[string]string) (map[string]string, error)

var startParamsFence atomic.Pointer[StartParamsFence]

// SetStartParamsFence registers the admission fence for enriched start params,
// or clears it when fn is nil. Register it together with the enricher it
// fences (cmd/start does both under CAESIUM_FRESHNESS_ENABLED).
func SetStartParamsFence(fn StartParamsFence) {
	if fn == nil {
		startParamsFence.Store(nil)
		return
	}
	startParamsFence.Store(&fn)
}

// fenceStartParamsTx applies the registered fence to the enriched params (as
// startRun built them, before any attempt) and encodes the result onto
// model.Params exactly as newStartRunModel does. It starts from params, not
// from model.Params, on every attempt: a busy-retried admission re-freezes the
// namespace, and a previous attempt's result was bound to the namespace that
// attempt froze.
func fenceStartParamsTx(tx *gorm.DB, model *models.JobRun, params map[string]string) error {
	fn := startParamsFence.Load()
	if fn == nil || *fn == nil || len(params) == 0 {
		return nil
	}
	out, err := (*fn)(tx.Statement.Context, tx, model.JobID, model.Namespace, maps.Clone(params))
	if err != nil {
		log.Warn("run: start params fence failed; starting run with degraded params",
			"job_id", model.JobID, "error", err)
	}
	if out == nil {
		// Nothing to correct (the StartParamsEnricher convention).
		out = params
	}
	if len(out) == 0 {
		model.Params = nil
		return nil
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("run: failed to marshal fenced params: %w", err)
	}
	model.Params = encoded
	return nil
}
