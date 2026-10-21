package start

import (
	"context"
	"testing"

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
// subscriber would deliver (identity-and-access D4): a policy in another
// namespace, one whose channel is disabled, and one whose channel lies outside
// its namespace never make an escalation look delivered; a marketing policy
// does, even after the incident's job moved to finance.
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
	require.False(t, escalate(), "no policy that would deliver: the escalation is not routed")

	seedRoutingPolicy(t, db, "marketing", models.ClusterNamespace, true)
	require.True(t, escalate(), "a marketing policy on a shared channel delivers the marketing incident's escalation")
}
