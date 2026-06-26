// Package replay implements the quarantined replay REST endpoint.
package replay

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	replaysvc "github.com/caesium-cloud/caesium/api/rest/service/replay"
	runsvc "github.com/caesium-cloud/caesium/api/rest/service/run"
	replaycore "github.com/caesium-cloud/caesium/internal/replay"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

const (
	maxReplayRequestBodyBytes = 64 * 1024
	maxReplaySetEntries       = 256
	maxReplaySetKeyBytes      = 1024
	maxReplaySetValueBytes    = 1024
)

// PostRequest is the only accepted body for POST /v1/jobs/:id/runs/:run_id/replay.
type PostRequest struct {
	Set map[string]string `json:"set,omitempty"`
}

// PostResponse returns the materialized replay run id.
type PostResponse struct {
	ID         uuid.UUID `json:"id"`
	RunID      uuid.UUID `json:"run_id"`
	Status     string    `json:"status"`
	Quarantine bool      `json:"quarantine"`
}

// Post handles POST /v1/jobs/:id/runs/:run_id/replay.
func Post(c *echo.Context) error {
	ctx := c.Request().Context()

	jobID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}
	baselineRunID, err := uuid.Parse(c.Param("run_id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	idempotencyKey := strings.TrimSpace(c.Request().Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "Idempotency-Key header is required")
	}

	req, err := decodePostRequest(c)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return echo.NewHTTPError(http.StatusRequestEntityTooLarge, "request body too large").Wrap(err)
		}
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	if _, err = jsvc.Service(ctx).Get(jobID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	baseline, err := runsvc.New(ctx).Get(baselineRunID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	if baseline.JobID != jobID {
		return echo.ErrNotFound
	}

	result, err := replaysvc.New(ctx).Replay(replaysvc.Request{
		JobID:          jobID,
		BaselineRunID:  baselineRunID,
		Set:            req.Set,
		IdempotencyKey: idempotencyKey,
		Principal:      authmw.GetPrincipal(c),
	})
	if err != nil {
		return replayError(err)
	}

	return c.JSON(http.StatusAccepted, PostResponse{
		ID:         result.Run.ID,
		RunID:      result.Run.ID,
		Status:     string(result.Run.Status),
		Quarantine: result.Run.Quarantine,
	})
}

func decodePostRequest(c *echo.Context) (PostRequest, error) {
	var req PostRequest
	body := c.Request().Body
	if body == nil {
		return req, nil
	}

	body = http.MaxBytesReader(c.Response(), body, maxReplayRequestBodyBytes)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		if errors.Is(err, io.EOF) {
			return req, nil
		}
		return req, err
	}

	var extra struct{}
	if err := dec.Decode(&extra); err != nil {
		if errors.Is(err, io.EOF) {
			return req, validatePostRequest(req)
		}
		return req, err
	}
	return req, errors.New("request body must contain a single JSON object")
}

func validatePostRequest(req PostRequest) error {
	if len(req.Set) > maxReplaySetEntries {
		return fmt.Errorf("set may contain at most %d entries", maxReplaySetEntries)
	}
	for key, value := range req.Set {
		if strings.TrimSpace(key) == "" {
			return errors.New("set keys must be non-empty")
		}
		if len([]byte(key)) > maxReplaySetKeyBytes {
			return fmt.Errorf("set key %q exceeds %d bytes", key, maxReplaySetKeyBytes)
		}
		if len([]byte(value)) > maxReplaySetValueBytes {
			return fmt.Errorf("set value for %q exceeds %d bytes", key, maxReplaySetValueBytes)
		}
	}
	return nil
}

func replayError(err error) error {
	switch {
	case errors.Is(err, replaysvc.ErrMissingIdempotencyKey):
		return echo.NewHTTPError(http.StatusBadRequest, "Idempotency-Key header is required")
	case errors.Is(err, replaysvc.ErrReplayRequiresDistributedMode):
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	case errors.Is(err, gorm.ErrRecordNotFound):
		return echo.ErrNotFound
	case errors.Is(err, replaycore.ErrReplayUnsafe):
		return echo.NewHTTPError(http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, replaycore.ErrUnavailableBaselineProof):
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	case errors.Is(err, replaycore.ErrMissingDescriptor):
		return echo.NewHTTPError(http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, replaycore.ErrUnsupportedDescriptor):
		return echo.NewHTTPError(http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, replaycore.ErrQuarantinedBaseline):
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	case errors.Is(err, replaycore.ErrBaselineNotTerminal):
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	case errors.Is(err, replaycore.ErrSecretIdentity):
		return echo.NewHTTPError(http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, replaycore.ErrDispatchRequired):
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	default:
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(fmt.Errorf("replay: %w", err))
	}
}
