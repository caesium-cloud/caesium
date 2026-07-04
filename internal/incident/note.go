package incident

import (
	"context"
	"encoding/json"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// AgentActionTypeNote is the AgentAction.Type used for free-text agent findings
// appended to the incident timeline. A note is a tier-0, actor=agent,
// status=executed row — it mutates nothing, it is evidence.
const AgentActionTypeNote = "note"

// noteParams is the JSON params shape for a note action.
type noteParams struct {
	Text string `json:"text"`
}

// RecordNote appends a free-text finding to the incident timeline as an
// AgentAction row. sessionID is optional (nil for a note recorded outside a
// session). It returns the recorded row.
func RecordNote(ctx context.Context, db *gorm.DB, incidentID uuid.UUID, sessionID *uuid.UUID, namespace *string, text string) (*models.AgentAction, error) {
	params, err := json.Marshal(noteParams{Text: text})
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	action := &models.AgentAction{
		ID:         uuid.New(),
		Namespace:  namespace,
		IncidentID: incidentID,
		SessionID:  sessionID,
		Type:       AgentActionTypeNote,
		Params:     datatypes.JSON(params),
		Tier:       0,
		Status:     models.AgentActionStatusExecuted,
		Actor:      models.AgentActionActorAgent,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := db.WithContext(ctx).Create(action).Error; err != nil {
		return nil, err
	}
	return action, nil
}

// noteTextFromParams extracts the note text from an AgentAction.Params blob.
func noteTextFromParams(raw datatypes.JSON) string {
	if len(raw) == 0 {
		return ""
	}
	var p noteParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	return p.Text
}
