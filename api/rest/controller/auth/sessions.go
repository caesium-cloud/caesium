package auth

import (
	"net/http"
	"strings"
	"time"

	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

// SessionView is the administration view of an active session. It never
// carries the token hash or the CSRF token. IdleExpiresAt and LastSeenAt
// include activity this node has buffered but not yet written.
type SessionView struct {
	ID                uuid.UUID  `json:"id"`
	UserID            uuid.UUID  `json:"user_id"`
	AuthMethod        string     `json:"auth_method"`
	CreatedAt         time.Time  `json:"created_at"`
	LastSeenAt        *time.Time `json:"last_seen_at"`
	IdleExpiresAt     time.Time  `json:"idle_expires_at"`
	AbsoluteExpiresAt time.Time  `json:"absolute_expires_at"`
	SourceIP          string     `json:"source_ip"`
	UserAgent         string     `json:"user_agent"`
}

func newSessionView(s *models.Session) SessionView {
	return SessionView{
		ID:                s.ID,
		UserID:            s.UserID,
		AuthMethod:        s.AuthMethod,
		CreatedAt:         s.CreatedAt,
		LastSeenAt:        s.LastSeenAt,
		IdleExpiresAt:     s.IdleExpiresAt,
		AbsoluteExpiresAt: s.AbsoluteExpiresAt,
		SourceIP:          s.SourceIP,
		UserAgent:         s.UserAgent,
	}
}

// ListSessions serves GET /v1/auth/sessions?user_id=&limit=&offset=: active
// sessions, newest first; without user_id, every user's.
func (ctrl *UserAdminController) ListSessions(c *echo.Context) error {
	limit, offset, err := adminPageBounds(c.QueryParam("limit"), c.QueryParam("offset"))
	if err != nil {
		return err
	}
	filter := iauth.SessionFilter{Limit: limit, Offset: offset}
	if raw := strings.TrimSpace(c.QueryParam("user_id")); raw != "" {
		userID, err := uuid.Parse(raw)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid user_id")
		}
		filter.UserID = &userID
	}
	page, err := ctrl.admin.ListSessions(c.Request().Context(), filter)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to list sessions").Wrap(err)
	}
	views := make([]SessionView, 0, len(page.Sessions))
	for i := range page.Sessions {
		views = append(views, newSessionView(&page.Sessions[i]))
	}
	setPageHeaders(c, page.Total, page.HasMore, offset, len(views))
	return c.JSON(http.StatusOK, views)
}
