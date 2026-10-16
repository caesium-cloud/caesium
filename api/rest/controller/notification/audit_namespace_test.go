package notification

import (
	"errors"
	"net/http"
	"testing"

	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// The notification controllers write their own audit rows; each row carries
// the namespace of the object it acted on (identity-and-access A3, D6), so a
// namespace admin's `caesium auth audit --namespace` sees them.

func auditNamespaceOf(t *testing.T, auditor *iauth.AuditLogger, action string) string {
	t.Helper()
	entries, err := auditor.Query(&iauth.AuditQueryRequest{Action: action})
	require.NoError(t, err)
	require.Len(t, entries, 1, action)
	return entries[0].Namespace
}

func TestChannelAuditRowsCarryNamespace(t *testing.T) {
	for _, tc := range []struct{ stored, want string }{
		{"marketing", "marketing"},
		{"*", "*"},
		{"", "default"},
	} {
		ctrl, auditor := setupNotificationControllerDeps(t)
		chID := uuid.New()
		ch := &models.NotificationChannel{ID: chID, Namespace: tc.stored, Name: "c", Type: models.ChannelType("webhook"), Config: datatypes.JSON(`{}`)}
		withFakeService(t, &fakeNotificationService{channel: ch})
		key := &models.APIKey{KeyPrefix: "csk_live_op"}

		c, rec := newNotificationContext(t, http.MethodPost, "/v1/notifications/channels", `{"name":"c","type":"webhook"}`, "", key)
		require.NoError(t, ctrl.CreateChannel(c))
		require.Equal(t, http.StatusCreated, rec.Code)
		require.Equal(t, tc.want, auditNamespaceOf(t, auditor, iauth.ActionNotificationChannelCreate), "create %q", tc.stored)

		c, _ = newNotificationContext(t, http.MethodPatch, "/v1/notifications/channels/"+chID.String(), `{"name":"c"}`, chID.String(), key)
		require.NoError(t, ctrl.UpdateChannel(c))
		require.Equal(t, tc.want, auditNamespaceOf(t, auditor, iauth.ActionNotificationChannelUpdate), "update %q", tc.stored)

		c, rec = newNotificationContext(t, http.MethodDelete, "/v1/notifications/channels/"+chID.String(), "", chID.String(), key)
		require.NoError(t, ctrl.DeleteChannel(c))
		require.Equal(t, http.StatusNoContent, rec.Code)
		require.Equal(t, tc.want, auditNamespaceOf(t, auditor, iauth.ActionNotificationChannelDelete), "delete %q", tc.stored)
	}
}

func TestPolicyAuditRowsCarryNamespace(t *testing.T) {
	for _, tc := range []struct{ stored, want string }{
		{"finance", "finance"},
		{"*", "*"},
	} {
		ctrl, auditor := setupNotificationControllerDeps(t)
		polID := uuid.New()
		p := &models.NotificationPolicy{ID: polID, Namespace: tc.stored, Name: "p", ChannelID: uuid.New(), EventTypes: datatypes.JSON(`["run_failed"]`)}
		withFakeService(t, &fakeNotificationService{policy: p})
		key := &models.APIKey{KeyPrefix: "csk_live_op"}

		c, _ := newNotificationContext(t, http.MethodPost, "/v1/notifications/policies", `{"name":"p"}`, "", key)
		require.NoError(t, ctrl.CreatePolicy(c))
		require.Equal(t, tc.want, auditNamespaceOf(t, auditor, iauth.ActionNotificationPolicyCreate))

		c, _ = newNotificationContext(t, http.MethodPatch, "/v1/notifications/policies/"+polID.String(), `{"enabled":true}`, polID.String(), key)
		require.NoError(t, ctrl.UpdatePolicy(c))
		require.Equal(t, tc.want, auditNamespaceOf(t, auditor, iauth.ActionNotificationPolicyUpdate))

		c, _ = newNotificationContext(t, http.MethodDelete, "/v1/notifications/policies/"+polID.String(), "", polID.String(), key)
		require.NoError(t, ctrl.DeletePolicy(c))
		require.Equal(t, tc.want, auditNamespaceOf(t, auditor, iauth.ActionNotificationPolicyDelete))
	}
}

// TestDeleteAuditNamespaceIsBestEffort: the pre-delete read never changes the
// response. A missing object (idempotent delete) and a failed read both
// delete as before and record an empty namespace.
func TestDeleteAuditNamespaceIsBestEffort(t *testing.T) {
	ctrl, auditor := setupNotificationControllerDeps(t)
	chID := uuid.New()
	withFakeService(t, &fakeNotificationService{channelErr: errors.New("read failed")})
	c, rec := newNotificationContext(t, http.MethodDelete, "/v1/notifications/channels/"+chID.String(), "", chID.String(), nil)
	require.NoError(t, ctrl.DeleteChannel(c))
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(t, "", auditNamespaceOf(t, auditor, iauth.ActionNotificationChannelDelete))
}
