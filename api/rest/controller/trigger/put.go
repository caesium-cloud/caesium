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

func Fire(c *echo.Context) error {
	ctx := c.Request().Context()

	if err := requireManualTriggerAPIKey(c); err != nil {
		return err
	}

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(codes.StatusBadRequest, "bad request").Wrap(err)
	}

	params, err := parseOptionalParams(c.Request().Body)
	if err != nil {
		return echo.NewHTTPError(codes.StatusBadRequest, "bad request").Wrap(err)
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

	if err := fireHTTPTrigger(ctx, trig, params); err != nil {
		return echo.NewHTTPError(codes.StatusInternalServerError, "internal server error").Wrap(err)
	}

	return c.JSON(codes.StatusAccepted, nil)
}

func parseOptionalParams(body io.Reader) (map[string]string, error) {
	if body == nil {
		return nil, nil
	}

	data, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	rawParams, ok := payload["params"]
	if !ok {
		return nil, fmt.Errorf("invalid json body: missing params object")
	}

	var params map[string]string
	if err := json.Unmarshal(rawParams, &params); err != nil {
		return nil, fmt.Errorf("invalid json body: %w", err)
	}
	return params, nil
}

func fireTrigger(ctx context.Context, trig *models.Trigger, params map[string]string) error {
	httpTrigger, err := triggerhttp.New(trig)
	if err != nil {
		return err
	}
	return httpTrigger.FireWithParams(ctx, params)
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
