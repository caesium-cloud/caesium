package trigger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	codes "net/http"

	triggersvc "github.com/caesium-cloud/caesium/api/rest/service/trigger"
	"github.com/caesium-cloud/caesium/internal/models"
	triggerhttp "github.com/caesium-cloud/caesium/internal/trigger/http"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

var (
	triggerServiceFactory = triggersvc.Service
	fireHTTPTrigger       = fireTrigger
)

func Put(c *echo.Context) error {
	ctx := c.Request().Context()

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
	case err != nil, trig == nil:
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
	if rawParams, ok := payload["params"]; ok {
		var params map[string]string
		if err := json.Unmarshal(rawParams, &params); err != nil {
			return nil, fmt.Errorf("invalid json body")
		}
		return params, nil
	}

	params := make(map[string]string, len(payload))
	for key, rawValue := range payload {
		var value string
		if err := json.Unmarshal(rawValue, &value); err != nil {
			return nil, fmt.Errorf("invalid json body")
		}
		params[key] = value
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
