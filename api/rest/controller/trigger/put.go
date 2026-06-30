package trigger

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	codes "net/http"
	"strings"

	triggersvc "github.com/caesium-cloud/caesium/api/rest/service/trigger"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	triggerhttp "github.com/caesium-cloud/caesium/internal/trigger/http"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

var (
	triggerServiceFactory = triggersvc.Service
	fireHTTPTrigger       = fireTrigger
)

type FireRequest struct {
	Params   map[string]string `json:"params,omitempty"`
	Priority string            `json:"priority,omitempty"`
}

func Fire(c *echo.Context) error {
	ctx := c.Request().Context()

	if err := requireManualTriggerAPIKey(c); err != nil {
		return err
	}

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(codes.StatusBadRequest, "bad request").Wrap(err)
	}

	fireReq, err := parseOptionalFireRequest(c.Request().Body)
	if err != nil {
		return echo.NewHTTPError(codes.StatusBadRequest, "bad request").Wrap(err)
	}
	if strings.TrimSpace(fireReq.Priority) != "" {
		if _, err := runstorage.PriorityValue(fireReq.Priority); err != nil {
			return echo.NewHTTPError(codes.StatusBadRequest, "bad request").Wrap(err)
		}
	}

	trig, err := triggerServiceFactory(ctx).Get(id)
	switch {
	case err != nil && !errors.Is(err, gorm.ErrRecordNotFound):
		return echo.NewHTTPError(codes.StatusInternalServerError, "internal server error").Wrap(err)
	case errors.Is(err, gorm.ErrRecordNotFound):
		return echo.ErrNotFound
	case trig == nil:
		return echo.ErrNotFound
	case trig.Type != models.TriggerTypeHTTP:
		return echo.NewHTTPError(codes.StatusBadRequest, "bad request").Wrap(
			fmt.Errorf(
				"trigger: '%v' is type: '%v', not '%v'",
				trig.ID,
				trig.Type,
				models.TriggerTypeHTTP,
			),
		)
	}

	if err := fireHTTPTrigger(ctx, trig, fireReq.Params, fireReq.Priority); err != nil {
		return echo.NewHTTPError(codes.StatusInternalServerError, "internal server error").Wrap(err)
	}

	return c.JSON(codes.StatusAccepted, nil)
}

func parseOptionalFireRequest(body io.Reader) (FireRequest, error) {
	if body == nil {
		return FireRequest{}, nil
	}

	data, err := io.ReadAll(body)
	if err != nil {
		return FireRequest{}, err
	}
	if strings.TrimSpace(string(data)) == "" {
		return FireRequest{}, nil
	}

	var req FireRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return FireRequest{}, err
	}
	return req, nil
}

func fireTrigger(ctx context.Context, trig *models.Trigger, params map[string]string, priority string) error {
	httpTrigger, err := triggerhttp.New(trig)
	if err != nil {
		return err
	}
	return httpTrigger.FireWithParams(ctx, params, triggerhttp.WithPriority(priority))
}

func requireManualTriggerAPIKey(c *echo.Context) error {
	expected := strings.TrimSpace(env.Variables().ManualTriggerAPIKey)
	if expected == "" {
		return echo.NewHTTPError(codes.StatusServiceUnavailable, "manual trigger fire is disabled")
	}

	provided := strings.TrimSpace(c.Request().Header.Get("X-Caesium-API-Key"))
	if provided == "" {
		auth := strings.TrimSpace(c.Request().Header.Get("Authorization"))
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			provided = strings.TrimSpace(auth[7:])
		}
	}

	if subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
		return echo.NewHTTPError(codes.StatusUnauthorized, "invalid api key")
	}
	return nil
}
