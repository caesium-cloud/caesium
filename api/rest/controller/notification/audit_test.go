package notification

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	svc "github.com/caesium-cloud/caesium/api/rest/service/notification"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// fakeNotificationService is a hand-rolled svc.Service the controller tests
// drive directly, so audit-log behaviour can be verified without a live
// database connection (the real service reads db.Connection(), a package
// singleton the controller tests have no way to redirect).
type fakeNotificationService struct {
	channel    *models.NotificationChannel
	channelErr error
	deleteErr  error

	policy       *models.NotificationPolicy
	policyErr    error
	deletePolErr error
}

func (f *fakeNotificationService) ListChannels(*svc.ListRequest) ([]models.NotificationChannel, error) {
	return nil, nil
}
func (f *fakeNotificationService) GetChannel(uuid.UUID) (*models.NotificationChannel, error) {
	return f.channel, f.channelErr
}
func (f *fakeNotificationService) CreateChannel(*svc.CreateChannelRequest) (*models.NotificationChannel, error) {
	return f.channel, f.channelErr
}
func (f *fakeNotificationService) UpdateChannel(uuid.UUID, *svc.UpdateChannelRequest) (*models.NotificationChannel, error) {
	return f.channel, f.channelErr
}
func (f *fakeNotificationService) DeleteChannel(uuid.UUID) error {
	return f.deleteErr
}

func (f *fakeNotificationService) ListPolicies(*svc.ListRequest) ([]models.NotificationPolicy, error) {
	return nil, nil
}
func (f *fakeNotificationService) GetPolicy(uuid.UUID) (*models.NotificationPolicy, error) {
	return f.policy, f.policyErr
}
func (f *fakeNotificationService) CreatePolicy(*svc.CreatePolicyRequest) (*models.NotificationPolicy, error) {
	return f.policy, f.policyErr
}
func (f *fakeNotificationService) UpdatePolicy(uuid.UUID, *svc.UpdatePolicyRequest) (*models.NotificationPolicy, error) {
	return f.policy, f.policyErr
}
func (f *fakeNotificationService) DeletePolicy(uuid.UUID) error {
	return f.deletePolErr
}

// withFakeService swaps the package-level service constructor for the
// duration of the test, restoring the real one on cleanup.
func withFakeService(t *testing.T, fake *fakeNotificationService) {
	t.Helper()
	prev := newService
	newService = func(context.Context) svc.Service { return fake }
	t.Cleanup(func() { newService = prev })
}

// setupNotificationControllerDeps builds a Controller backed by a real,
// in-memory audit logger — the same "in-memory audit logger" pattern
// api/rest/controller/auth/auth_test.go uses for its own Controller.
func setupNotificationControllerDeps(t *testing.T) (*Controller, *iauth.AuditLogger) {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	auditor := iauth.NewAuditLogger(db)
	return New(auditor), auditor
}

// newNotificationContext mirrors auth_test.go's newAuthContext helper.
func newNotificationContext(t *testing.T, method, target, body string, id string, key *models.APIKey) (*echo.Context, *httptest.ResponseRecorder) {
	t.Helper()

	e := echo.New()
	req := httptest.NewRequestWithContext(context.Background(), method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	}
	req.RemoteAddr = "198.51.100.9:1234"
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if id != "" {
		c.SetPathValues(echo.PathValues{{Name: "id", Value: id}})
	}
	if key != nil {
		c.Set(authmw.ContextKeyAuth, key)
	}
	return c, rec
}

func TestCreateChannelAudits(t *testing.T) {
	ctrl, auditor := setupNotificationControllerDeps(t)

	chID := uuid.New()
	cfg, err := json.Marshal(map[string]any{"url": "https://hooks.example.com/super-secret-path"})
	require.NoError(t, err)
	withFakeService(t, &fakeNotificationService{
		channel: &models.NotificationChannel{
			ID:      chID,
			Name:    "pager-primary",
			Type:    models.ChannelType("webhook"),
			Config:  datatypes.JSON(cfg),
			Enabled: true,
		},
	})

	c, rec := newNotificationContext(t, http.MethodPost, "/v1/notifications/channels",
		`{"name":"pager-primary","type":"webhook","config":{"url":"https://hooks.example.com/super-secret-path"}}`,
		"", &models.APIKey{KeyPrefix: "csk_live_admin"})

	require.NoError(t, ctrl.CreateChannel(c))
	require.Equal(t, http.StatusCreated, rec.Code)

	entries, err := auditor.Query(&iauth.AuditQueryRequest{Action: iauth.ActionNotificationChannelCreate})
	require.NoError(t, err)
	require.Len(t, entries, 1)

	entry := entries[0]
	require.Equal(t, "csk_live_admin", entry.Actor)
	require.Equal(t, "notification_channel", entry.ResourceType)
	require.Equal(t, chID.String(), entry.ResourceID)
	require.Equal(t, iauth.OutcomeSuccess, entry.Outcome)
	require.NotEmpty(t, entry.SourceIP)

	var meta map[string]any
	require.NoError(t, json.Unmarshal(entry.Metadata, &meta))
	require.Equal(t, "pager-primary", meta["name"])
	require.Equal(t, "webhook", meta["type"])
	// The channel config (which may hold secrets — webhook URLs, tokens) must
	// never be copied into the audit trail: that would defeat the API's own
	// config redaction (see redactChannel).
	require.NotContains(t, string(entry.Metadata), "super-secret-path")
}

func TestCreateChannelDefaultsActorToUnknown(t *testing.T) {
	ctrl, auditor := setupNotificationControllerDeps(t)

	withFakeService(t, &fakeNotificationService{
		channel: &models.NotificationChannel{ID: uuid.New(), Name: "c", Type: models.ChannelType("webhook")},
	})

	c, rec := newNotificationContext(t, http.MethodPost, "/v1/notifications/channels",
		`{"name":"c","type":"webhook"}`, "", nil)

	require.NoError(t, ctrl.CreateChannel(c))
	require.Equal(t, http.StatusCreated, rec.Code)

	entries, err := auditor.Query(&iauth.AuditQueryRequest{Action: iauth.ActionNotificationChannelCreate})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "unknown", entries[0].Actor)
}

func TestUpdateChannelAudits(t *testing.T) {
	ctrl, auditor := setupNotificationControllerDeps(t)

	chID := uuid.New()
	withFakeService(t, &fakeNotificationService{
		channel: &models.NotificationChannel{ID: chID, Name: "renamed", Type: models.ChannelType("slack"), Enabled: false},
	})

	c, rec := newNotificationContext(t, http.MethodPatch, "/v1/notifications/channels/"+chID.String(),
		`{"name":"renamed","enabled":false}`, chID.String(), &models.APIKey{KeyPrefix: "csk_live_op"})

	require.NoError(t, ctrl.UpdateChannel(c))
	require.Equal(t, http.StatusOK, rec.Code)

	entries, err := auditor.Query(&iauth.AuditQueryRequest{Action: iauth.ActionNotificationChannelUpdate})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, chID.String(), entries[0].ResourceID)
	require.Equal(t, "csk_live_op", entries[0].Actor)

	var meta map[string]any
	require.NoError(t, json.Unmarshal(entries[0].Metadata, &meta))
	require.Equal(t, "renamed", meta["name"])
	require.Equal(t, false, meta["enabled"])
}

func TestDeleteChannelAudits(t *testing.T) {
	ctrl, auditor := setupNotificationControllerDeps(t)

	chID := uuid.New()
	withFakeService(t, &fakeNotificationService{})

	c, rec := newNotificationContext(t, http.MethodDelete, "/v1/notifications/channels/"+chID.String(),
		"", chID.String(), &models.APIKey{KeyPrefix: "csk_live_admin"})

	require.NoError(t, ctrl.DeleteChannel(c))
	require.Equal(t, http.StatusNoContent, rec.Code)

	entries, err := auditor.Query(&iauth.AuditQueryRequest{Action: iauth.ActionNotificationChannelDelete})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, chID.String(), entries[0].ResourceID)
	require.Equal(t, "notification_channel", entries[0].ResourceType)
	require.Equal(t, iauth.OutcomeSuccess, entries[0].Outcome)
}

func TestChannelReadsDoNotAudit(t *testing.T) {
	ctrl, auditor := setupNotificationControllerDeps(t)

	chID := uuid.New()
	withFakeService(t, &fakeNotificationService{
		channel: &models.NotificationChannel{ID: chID, Name: "c", Type: models.ChannelType("webhook")},
	})

	c, rec := newNotificationContext(t, http.MethodGet, "/v1/notifications/channels/"+chID.String(), "", chID.String(), nil)
	require.NoError(t, ctrl.GetChannel(c))
	require.Equal(t, http.StatusOK, rec.Code)

	c, rec = newNotificationContext(t, http.MethodGet, "/v1/notifications/channels", "", "", nil)
	require.NoError(t, ctrl.ListChannels(c))
	require.Equal(t, http.StatusOK, rec.Code)

	entries, err := auditor.Query(&iauth.AuditQueryRequest{Limit: 100})
	require.NoError(t, err)
	require.Empty(t, entries, "reads must not write audit entries")
}

func TestCreatePolicyAudits(t *testing.T) {
	ctrl, auditor := setupNotificationControllerDeps(t)

	polID := uuid.New()
	chanID := uuid.New()
	eventTypesJSON, err := json.Marshal([]string{"run_failed"})
	require.NoError(t, err)
	withFakeService(t, &fakeNotificationService{
		policy: &models.NotificationPolicy{
			ID:         polID,
			Name:       "page-oncall",
			ChannelID:  chanID,
			EventTypes: datatypes.JSON(eventTypesJSON),
			Enabled:    true,
		},
	})

	c, rec := newNotificationContext(t, http.MethodPost, "/v1/notifications/policies",
		`{"name":"page-oncall","channel_id":"`+chanID.String()+`","event_types":["run_failed"]}`,
		"", &models.APIKey{KeyPrefix: "csk_live_admin"})

	require.NoError(t, ctrl.CreatePolicy(c))
	require.Equal(t, http.StatusCreated, rec.Code)

	entries, err := auditor.Query(&iauth.AuditQueryRequest{Action: iauth.ActionNotificationPolicyCreate})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, polID.String(), entries[0].ResourceID)
	require.Equal(t, "notification_policy", entries[0].ResourceType)

	var meta map[string]any
	require.NoError(t, json.Unmarshal(entries[0].Metadata, &meta))
	require.Equal(t, "page-oncall", meta["name"])
	require.Equal(t, chanID.String(), meta["channel_id"])
}

func TestUpdatePolicyAudits(t *testing.T) {
	ctrl, auditor := setupNotificationControllerDeps(t)

	polID := uuid.New()
	chanID := uuid.New()
	eventTypesJSON, err := json.Marshal([]string{"run_failed", "run_completed"})
	require.NoError(t, err)
	withFakeService(t, &fakeNotificationService{
		policy: &models.NotificationPolicy{
			ID:         polID,
			Name:       "page-oncall",
			ChannelID:  chanID,
			EventTypes: datatypes.JSON(eventTypesJSON),
			Enabled:    false,
		},
	})

	c, rec := newNotificationContext(t, http.MethodPatch, "/v1/notifications/policies/"+polID.String(),
		`{"enabled":false}`, polID.String(), &models.APIKey{KeyPrefix: "csk_live_op"})

	require.NoError(t, ctrl.UpdatePolicy(c))
	require.Equal(t, http.StatusOK, rec.Code)

	entries, err := auditor.Query(&iauth.AuditQueryRequest{Action: iauth.ActionNotificationPolicyUpdate})
	require.NoError(t, err)
	require.Len(t, entries, 1)

	var meta map[string]any
	require.NoError(t, json.Unmarshal(entries[0].Metadata, &meta))
	require.Equal(t, false, meta["enabled"])
	eventTypes, ok := meta["event_types"].([]any)
	require.True(t, ok)
	require.ElementsMatch(t, []any{"run_failed", "run_completed"}, eventTypes)
}

func TestDeletePolicyAudits(t *testing.T) {
	ctrl, auditor := setupNotificationControllerDeps(t)

	polID := uuid.New()
	withFakeService(t, &fakeNotificationService{})

	c, rec := newNotificationContext(t, http.MethodDelete, "/v1/notifications/policies/"+polID.String(),
		"", polID.String(), &models.APIKey{KeyPrefix: "csk_live_admin"})

	require.NoError(t, ctrl.DeletePolicy(c))
	require.Equal(t, http.StatusNoContent, rec.Code)

	entries, err := auditor.Query(&iauth.AuditQueryRequest{Action: iauth.ActionNotificationPolicyDelete})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, polID.String(), entries[0].ResourceID)
	require.Equal(t, "notification_policy", entries[0].ResourceType)
}

func TestMutationFailureDoesNotAudit(t *testing.T) {
	ctrl, auditor := setupNotificationControllerDeps(t)

	withFakeService(t, &fakeNotificationService{channelErr: svc.ErrInvalidChannel})

	c, _ := newNotificationContext(t, http.MethodPost, "/v1/notifications/channels",
		`{"name":"","type":"webhook"}`, "", &models.APIKey{KeyPrefix: "csk_live_admin"})

	err := ctrl.CreateChannel(c)
	require.Error(t, err)

	entries, err := auditor.Query(&iauth.AuditQueryRequest{Limit: 100})
	require.NoError(t, err)
	require.Empty(t, entries, "a failed create must not write a success audit entry")
}
