// Package receipt exposes the reproducibility-receipt REST endpoints: emit a
// content-addressed receipt for a run, and verify a committed receipt against a
// run's current persisted state (drift detection).
package receipt

import (
	"errors"
	"net/http"

	rsvc "github.com/caesium-cloud/caesium/api/rest/service/receipt"
	ireceipt "github.com/caesium-cloud/caesium/internal/receipt"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

// Get handles GET /jobs/:id/runs/:run_id/receipt.
//
// It re-derives the run's reproducibility receipt from persisted state and
// returns it as JSON — a small, content-addressed artifact intended to be
// committed to git alongside the pipeline. The receipt's `degraded` flag is
// honest: if any task ran on an unpinned, mutable image tag, the receipt cannot
// attest reproducibility and says so.
func Get(c *echo.Context) error {
	// Parse and validate BOTH path params up-front. The job ID must be a valid
	// UUID so the ownership cross-check below cannot be silently bypassed by a
	// malformed `id` (a non-UUID would otherwise skip the check and leak the
	// receipt for any run_id under any path).
	jobID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}
	runID, err := uuid.Parse(c.Param("run_id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	ctx := c.Request().Context()

	r, err := rsvc.New(ctx).Build(runID)
	if err != nil {
		if errors.Is(err, ireceipt.ErrRunNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	// Cross-check the run belongs to the job in the path so the route is not a
	// way to read receipts for arbitrary runs under any job ID.
	if r.JobID != jobID {
		return echo.ErrNotFound
	}

	return c.JSON(http.StatusOK, r)
}

// Verify handles POST /jobs/:id/runs/:run_id/receipt/verify.
//
// The request body is a previously-emitted (committed) receipt. The handler
// re-derives the receipt from the run's current persisted state and returns a
// drift report: whether the run still matches, and if not, every divergence
// ("image tag mutated: digest mismatch", "manifest changed", …). A run whose
// tasks were not digest-pinned is reported as degraded and never as a clean
// match.
func Verify(c *echo.Context) error {
	// Parse and validate BOTH path params up-front (see Get) so the ownership
	// cross-check below cannot be bypassed by a malformed `id`.
	jobID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}
	runID, err := uuid.Parse(c.Param("run_id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	var committed ireceipt.Receipt
	if err := c.Bind(&committed); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	// The run in the path is authoritative; ignore/override a mismatched RunID
	// in the body so a caller cannot verify run A's state against run B's
	// committed receipt by accident.
	committed.RunID = runID

	ctx := c.Request().Context()

	result, err := rsvc.New(ctx).Verify(&committed)
	if err != nil {
		if errors.Is(err, ireceipt.ErrRunNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	if result.Rederived != nil && result.Rederived.JobID != jobID {
		return echo.ErrNotFound
	}

	return c.JSON(http.StatusOK, result)
}
