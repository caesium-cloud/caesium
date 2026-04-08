package auth

import (
	"fmt"
	"net/http"
	"time"

	"github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/labstack/echo/v5"
)

type createKeyRequest struct {
	Description string           `json:"description"`
	Role        string           `json:"role"`
	Scope       *models.KeyScope `json:"scope,omitempty"`
	ExpiresIn   string           `json:"expires_in,omitempty"` // e.g. "90d", "24h"
}

type createKeyResponse struct {
	Key    string         `json:"key"`
	APIKey *models.APIKey `json:"api_key"`
}

func CreateKey(c *echo.Context) error {
	var req createKeyRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	if !models.ValidRole(req.Role) {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid role: %s (must be admin, operator, runner, or viewer)", req.Role))
	}

	var expiresAt *time.Time
	if req.ExpiresIn != "" {
		dur, err := parseDuration(req.ExpiresIn)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid expires_in").Wrap(err)
		}
		t := time.Now().UTC().Add(dur)
		expiresAt = &t
	}

	caller := middleware.GetAuthKey(c)
	createdBy := "unknown"
	if caller != nil {
		createdBy = caller.KeyPrefix
	}

	resp, err := Dependencies.Service.CreateKey(&auth.CreateKeyRequest{
		Description: req.Description,
		Role:        models.Role(req.Role),
		Scope:       req.Scope,
		CreatedBy:   createdBy,
		ExpiresAt:   expiresAt,
	})
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to create api key").Wrap(err)
	}

	logAuditFailure(Dependencies.Auditor.Log(auth.AuditEntry{
		Actor:        createdBy,
		Action:       auth.ActionKeyCreate,
		ResourceType: "api_key",
		ResourceID:   resp.Key.ID.String(),
		SourceIP:     c.RealIP(),
		Outcome:      auth.OutcomeSuccess,
		Metadata: map[string]interface{}{
			"role":        req.Role,
			"description": req.Description,
		},
	}))

	return c.JSON(http.StatusCreated, createKeyResponse{
		Key:    resp.Plaintext,
		APIKey: resp.Key,
	})
}

// parseDuration parses durations like "90d", "24h", "30m".
func parseDuration(s string) (time.Duration, error) {
	if len(s) < 2 {
		return 0, fmt.Errorf("duration too short: %s", s)
	}

	suffix := s[len(s)-1]
	value := s[:len(s)-1]

	switch suffix {
	case 'd':
		var days int
		if _, err := fmt.Sscanf(value, "%d", &days); err != nil {
			return 0, err
		}
		return time.Duration(days) * 24 * time.Hour, nil
	default:
		return time.ParseDuration(s)
	}
}
