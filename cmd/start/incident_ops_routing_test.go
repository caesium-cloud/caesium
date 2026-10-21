package start

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

func seedRoutingPolicy(t *testing.T, db *gorm.DB, namespace, channelNamespace string, channelEnabled bool) {
	t.Helper()
	channel := models.NotificationChannel{ID: uuid.New(), Namespace: channelNamespace, Name: "ch-" + uuid.NewString(), Type: models.ChannelTypeWebhook, Config: datatypes.JSON(`{}`), Enabled: true}
	require.NoError(t, db.Create(&channel).Error)
	if !channelEnabled {
		require.NoError(t, db.Model(&models.NotificationChannel{}).Where("id = ?", channel.ID).Update("enabled", false).Error)
	}
	require.NoError(t, db.Create(&models.NotificationPolicy{ID: uuid.New(), Namespace: namespace, Name: "pol-" + uuid.NewString(), ChannelID: channel.ID,
		EventTypes: datatypes.JSON(`["` + string(event.TypeIncidentEscalated) + `"]`), Enabled: true}).Error)
}

// TestEscalateRoutedMatchesDelivery: Escalate reports routed exactly when the
// subscriber would deliver (identity-and-access D4): a policy whose channel is
// disabled or lies outside its namespace never makes an escalation look
// delivered; with a policy file, neither does a policy in another namespace,
// while a marketing policy does, even after the incident's job moved to
// finance.
func TestEscalateRoutedMatchesDelivery(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ctx := context.Background()
	incidentID, _ := seedEscalationIncident(t, db)
	require.NoError(t, db.Model(&models.Incident{}).Where("id = ?", incidentID).Update("namespace", "marketing").Error)
	var inc models.Incident
	require.NoError(t, db.First(&inc, "id = ?", incidentID).Error)
	require.NoError(t, db.Model(&models.Job{}).Where("id = ?", inc.JobID).Update("namespace", "finance").Error)
	ops := newIncidentActionOps(db, &recordingBus{}, event.NewStore(db))

	escalate := func() bool {
		routed, err := ops.Escalate(ctx, incidentID, "oncall", "summary")
		require.NoError(t, err)
		return routed
	}

	seedRoutingPolicy(t, db, "finance", "finance", true)
	seedRoutingPolicy(t, db, "marketing", "marketing", false)
	seedRoutingPolicy(t, db, "marketing", "finance", true)

	// Without a policy file (design §§1/15) any enabled policy on a usable
	// channel matches, whatever its namespace, as before D4.
	policy.SetProcessHolder(nil)
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	require.True(t, escalate(), "no policy file: the finance policy routes the escalation, as before")

	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte("apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  marketing: {}\n  finance: {}\n"), 0o600))
	holder, err := policy.LoadFile(t.Context(), policy.HolderOptions{Path: path})
	require.NoError(t, err)
	policy.SetProcessHolder(holder)
	require.False(t, escalate(), "no policy that would deliver: the escalation is not routed")

	seedRoutingPolicy(t, db, "marketing", models.ClusterNamespace, true)
	require.True(t, escalate(), "a marketing policy on a shared channel delivers the marketing incident's escalation")
}
