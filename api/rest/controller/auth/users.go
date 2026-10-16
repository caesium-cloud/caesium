package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/api/internal/auditlog"
	"github.com/caesium-cloud/caesium/api/middleware"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

// User and session administration (identity-and-access E3, spec §11.2). Every
// route is cluster-admin only. Lists follow the run-list paging contract: the
// body is a bare array, the total and the continuation offset travel in
// response headers, and an out-of-range limit or offset is a 400.
const (
	defaultAdminPageSize = 100
	maxAdminPageSize     = 1000

	headerTotalCount = "X-Caesium-Total-Count"
	headerNextOffset = "X-Caesium-Next-Offset"
)

// UserAdminController serves the user and session administration routes.
type UserAdminController struct {
	admin   *iauth.UserAdmin
	auditor *iauth.AuditLogger
}

// NewUserAdmin constructs the user administration controller.
func NewUserAdmin(admin *iauth.UserAdmin, auditor *iauth.AuditLogger) *UserAdminController {
	return &UserAdminController{admin: admin, auditor: auditor}
}

// UserView is the administration view of a user. It is built field by field
// from models.User so that no credential column (the refresh grant, its
// lease or version) can ever reach a response.
type UserView struct {
	ID            uuid.UUID `json:"id"`
	Issuer        string    `json:"issuer"`
	Subject       string    `json:"subject"`
	Email         string    `json:"email"`
	EmailVerified bool      `json:"email_verified"`
	DisplayName   string    `json:"display_name"`
	Groups        []string  `json:"groups"`
	// LastLoginRole is the cluster role resolved at the user's last login.
	// It is a record, not a grant: requests resolve grants afresh.
	LastLoginRole models.Role `json:"last_login_role"`
	CreatedAt     time.Time   `json:"created_at"`
	LastLoginAt   *time.Time  `json:"last_login_at"`
	DisabledAt    *time.Time  `json:"disabled_at"`
}

// UserDetailView is a user with the newest of its active sessions (at most
// iauth.DetailSessionLimit) and their total; page the rest through
// GET /v1/auth/sessions?user_id=.
type UserDetailView struct {
	UserView
	Sessions      []SessionView `json:"sessions"`
	SessionsTotal int64         `json:"sessions_total"`
}

// UserChangeResponse is the body of the disable and enable routes.
type UserChangeResponse struct {
	User            UserView `json:"user"`
	Changed         bool     `json:"changed"`
	SessionsRevoked int64    `json:"sessions_revoked"`
}

// SessionsRevokedResponse is the body of POST /v1/auth/users/:id/sessions/revoke.
type SessionsRevokedResponse struct {
	UserID          uuid.UUID `json:"user_id"`
	SessionsRevoked int64     `json:"sessions_revoked"`
}

func newUserView(u *models.User) UserView {
	groups := []string{}
	if len(u.Groups) > 0 {
		var decoded []string
		if err := json.Unmarshal(u.Groups, &decoded); err == nil && decoded != nil {
			groups = decoded
		}
	}
	return UserView{
		ID:            u.ID,
		Issuer:        u.Issuer,
		Subject:       u.Subject,
		Email:         u.Email,
		EmailVerified: u.EmailVerified,
		DisplayName:   u.DisplayName,
		Groups:        groups,
		LastLoginRole: u.Role,
		CreatedAt:     u.CreatedAt,
		LastLoginAt:   u.LastLoginAt,
		DisabledAt:    u.DisabledAt,
	}
}

// ListUsers serves GET /v1/auth/users?issuer=&email=&disabled=&limit=&offset=.
func (ctrl *UserAdminController) ListUsers(c *echo.Context) error {
	limit, offset, err := adminPageBounds(c.QueryParam("limit"), c.QueryParam("offset"))
	if err != nil {
		return err
	}
	filter := iauth.UserFilter{
		Issuer: c.QueryParam("issuer"),
		Email:  c.QueryParam("email"),
		Limit:  limit,
		Offset: offset,
	}
	if raw := strings.TrimSpace(c.QueryParam("disabled")); raw != "" {
		disabled, err := strconv.ParseBool(raw)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "disabled must be true or false")
		}
		filter.Disabled = &disabled
	}
	page, err := ctrl.admin.ListUsers(c.Request().Context(), filter)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to list users").Wrap(err)
	}
	views := make([]UserView, 0, len(page.Users))
	for i := range page.Users {
		views = append(views, newUserView(&page.Users[i]))
	}
	setPageHeaders(c, page.Total, page.HasMore, offset, len(views))
	return c.JSON(http.StatusOK, views)
}

// GetUser serves GET /v1/auth/users/:id.
func (ctrl *UserAdminController) GetUser(c *echo.Context) error {
	id, err := parseUserID(c)
	if err != nil {
		return err
	}
	detail, err := ctrl.admin.GetUser(c.Request().Context(), id)
	if err != nil {
		return userAdminError(err, "failed to load user")
	}
	sessions := make([]SessionView, 0, len(detail.Sessions))
	for i := range detail.Sessions {
		sessions = append(sessions, newSessionView(&detail.Sessions[i]))
	}
	return c.JSON(http.StatusOK, UserDetailView{
		UserView:      newUserView(&detail.User),
		Sessions:      sessions,
		SessionsTotal: detail.SessionsTotal,
	})
}

// DisableUser serves POST /v1/auth/users/:id/disable: it disables the user
// and revokes every session in one transaction. Disabling a disabled user is
// a 200 with changed=false and writes no audit row. An admin cannot disable
// the user behind its own request (409), so a session cannot lock itself out
// by accident.
func (ctrl *UserAdminController) DisableUser(c *echo.Context) error {
	id, err := parseUserID(c)
	if err != nil {
		return err
	}
	if principal := middleware.GetPrincipal(c); principal != nil && principal.UserID != nil && *principal.UserID == id {
		return echo.NewHTTPError(http.StatusConflict, "cannot disable the user making this request")
	}
	change, err := ctrl.admin.DisableUser(c.Request().Context(), id)
	if err != nil {
		return userAdminError(err, "failed to disable user")
	}
	if change.Changed {
		ctrl.audit(c, iauth.ActionUserDisable, id, map[string]any{"sessions_revoked": change.SessionsRevoked})
		ctrl.auditSessionsRevoked(c, id, iauth.SessionsRevokedReasonUserDisabled, change.SessionsRevoked)
	}
	return c.JSON(http.StatusOK, UserChangeResponse{
		User:            newUserView(&change.User),
		Changed:         change.Changed,
		SessionsRevoked: change.SessionsRevoked,
	})
}

// EnableUser serves POST /v1/auth/users/:id/enable. Revoked sessions stay
// revoked; the user signs in again. Enabling an enabled user is a 200 with
// changed=false and writes no audit row.
func (ctrl *UserAdminController) EnableUser(c *echo.Context) error {
	id, err := parseUserID(c)
	if err != nil {
		return err
	}
	change, err := ctrl.admin.EnableUser(c.Request().Context(), id)
	if err != nil {
		return userAdminError(err, "failed to enable user")
	}
	if change.Changed {
		ctrl.audit(c, iauth.ActionUserEnable, id, nil)
		ctrl.auditSessionsRevoked(c, id, iauth.SessionsRevokedReasonUserEnabled, change.SessionsRevoked)
	}
	return c.JSON(http.StatusOK, UserChangeResponse{
		User:            newUserView(&change.User),
		Changed:         change.Changed,
		SessionsRevoked: change.SessionsRevoked,
	})
}

// RevokeUserSessions serves POST /v1/auth/users/:id/sessions/revoke ("log out
// everywhere"). Revoking the caller's own sessions is allowed.
func (ctrl *UserAdminController) RevokeUserSessions(c *echo.Context) error {
	id, err := parseUserID(c)
	if err != nil {
		return err
	}
	revoked, err := ctrl.admin.RevokeUserSessions(c.Request().Context(), id)
	if err != nil {
		return userAdminError(err, "failed to revoke user sessions")
	}
	ctrl.auditSessionsRevoked(c, id, iauth.SessionsRevokedReasonAdmin, revoked)
	return c.JSON(http.StatusOK, SessionsRevokedResponse{UserID: id, SessionsRevoked: revoked})
}

// auditSessionsRevoked writes auth.sessions_revoked when count > 0: a revoke
// of nothing changes no access, so it is not audited.
func (ctrl *UserAdminController) auditSessionsRevoked(c *echo.Context, userID uuid.UUID, reason string, count int64) {
	if count <= 0 {
		return
	}
	ctrl.audit(c, iauth.ActionSessionsRevoked, userID, map[string]any{"reason": reason, "count": count})
}

// audit records an administration action on a user, after the change has
// committed (best effort, like every other auth administration route). The
// routes are cluster-class, so the row's namespace is "*".
func (ctrl *UserAdminController) audit(c *echo.Context, action string, userID uuid.UUID, metadata map[string]any) {
	if ctrl.auditor == nil {
		return
	}
	actor := "unknown"
	if principal := middleware.GetPrincipal(c); principal != nil && principal.Subject != "" {
		actor = principal.Subject
	}
	auditlog.LogFailure(ctrl.auditor.Log(iauth.AuditEntry{
		Actor:        actor,
		Action:       action,
		ResourceType: "user",
		ResourceID:   userID.String(),
		SourceIP:     c.RealIP(),
		Outcome:      iauth.OutcomeSuccess,
		Metadata:     metadata,
		Namespace:    models.ClusterNamespace,
	}))
}

func parseUserID(c *echo.Context) (uuid.UUID, error) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return uuid.Nil, echo.NewHTTPError(http.StatusBadRequest, "invalid user id")
	}
	return id, nil
}

func userAdminError(err error, message string) error {
	if errors.Is(err, iauth.ErrUserNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, "user not found")
	}
	return echo.NewHTTPError(http.StatusInternalServerError, message).Wrap(err)
}

// adminPageBounds parses the page window: absent means the default; an
// unparseable or out-of-range value is a 400, never a silent clamp.
func adminPageBounds(limitParam, offsetParam string) (limit, offset int, err error) {
	limit = defaultAdminPageSize
	if raw := strings.TrimSpace(limitParam); raw != "" {
		parsed, convErr := strconv.Atoi(raw)
		if convErr != nil || parsed <= 0 || parsed > maxAdminPageSize {
			return 0, 0, echo.NewHTTPError(http.StatusBadRequest,
				fmt.Sprintf("limit must be an integer between 1 and %d", maxAdminPageSize))
		}
		limit = parsed
	}
	if raw := strings.TrimSpace(offsetParam); raw != "" {
		parsed, convErr := strconv.Atoi(raw)
		if convErr != nil || parsed < 0 {
			return 0, 0, echo.NewHTTPError(http.StatusBadRequest, "offset must be a non-negative integer")
		}
		offset = parsed
	}
	return limit, offset, nil
}

// setPageHeaders sets the total and, when the page query found more rows,
// the offset of the next page.
func setPageHeaders(c *echo.Context, total int64, hasMore bool, offset, n int) {
	c.Response().Header().Set(headerTotalCount, strconv.FormatInt(total, 10))
	if hasMore {
		c.Response().Header().Set(headerNextOffset, strconv.Itoa(offset+n))
	}
}
