package incident

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// TestGetForReaderConfinesTheDetail (identity-and-access A8): a confined
// reader gets the frozen allowlist minus jobs that moved out of the
// incident's namespace and no content a pre-A8 agent session wrote; an
// unconfined reader gets the rows exactly as stored.
func TestGetForReaderConfinesTheDetail(t *testing.T) {
	svc, db := newTestService(t)
	now := time.Now().UTC()
	jobs := map[string]string{"mkt-own": "marketing", "mkt-down": "marketing", "moved-away": "finance"}
	ids := map[string]uuid.UUID{}
	for alias, namespace := range jobs {
		id := uuid.New()
		ids[alias] = id
		require.NoError(t, db.Create(&models.Job{ID: id, Alias: alias, Namespace: namespace, TriggerID: uuid.New(), CreatedAt: now, UpdatedAt: now}).Error)
	}
	incID := uuid.New()
	require.NoError(t, db.Create(&models.Incident{
		ID: incID, Namespace: "marketing", JobID: ids["mkt-own"], Class: "unknown", Status: models.IncidentStatusOpen,
		DedupeKey: incID.String(), AllowedJobs: datatypes.JSON(`["mkt-down","mkt-own","moved-away"]`),
		OpenedAt: now, CreatedAt: now, UpdatedAt: now,
	}).Error)
	ns := "marketing"
	mkSession := func(log string, confined bool) {
		require.NoError(t, db.Create(&models.AgentSession{ID: uuid.New(), Namespace: &ns, IncidentID: incID, State: models.AgentSessionStateSucceeded,
			SessionLog: log, NamespaceConfined: confined, CreatedAt: now, UpdatedAt: now}).Error)
	}
	mkSession("legacy transcript quoting moved-away", false)
	mkSession("confined transcript", true)
	mkAction := func(actor models.AgentActionActor, params string, confined bool) {
		require.NoError(t, db.Create(&models.AgentAction{ID: uuid.New(), Namespace: &ns, IncidentID: incID, Type: "note",
			Params: datatypes.JSON(params), Status: models.AgentActionStatusExecuted, Actor: actor, NamespaceConfined: confined,
			CreatedAt: now, UpdatedAt: now}).Error)
	}
	mkAction(models.AgentActionActorAgent, `{"text":"legacy note quoting moved-away"}`, false)
	mkAction(models.AgentActionActorAgent, `{"text":"confined note"}`, true)
	mkAction(models.AgentActionActorPolicy, `{"text":"policy row"}`, false)

	all, err := svc.GetForReader(incID, false)
	require.NoError(t, err)
	raw, err := json.Marshal(all)
	require.NoError(t, err)
	require.Contains(t, string(raw), "moved-away", "an unconfined reader sees the rows as stored")
	require.NotContains(t, string(raw), "withheld")

	confined, err := svc.GetForReader(incID, true)
	require.NoError(t, err)
	require.JSONEq(t, `["mkt-down","mkt-own"]`, string(confined.Incident.AllowedJobs))
	raw, err = json.Marshal(confined)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "moved-away")
	require.Contains(t, string(raw), "confined transcript")
	require.Contains(t, string(raw), "confined note")
	require.Contains(t, string(raw), "policy row")
	require.Contains(t, string(raw), `"session_log_withheld":true`)
	require.Contains(t, string(raw), `"content_withheld":true`)

	// The persisted rows are unchanged.
	var stored models.Incident
	require.NoError(t, db.First(&stored, "id = ?", incID).Error)
	require.Contains(t, string(stored.AllowedJobs), "moved-away")
}
