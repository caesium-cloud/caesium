package auth

import (
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func TestPrincipalFromKey(t *testing.T) {
	id := uuid.New()
	key := &models.APIKey{
		ID:        id,
		KeyPrefix: "csk_live_ab",
		Role:      models.RoleOperator,
		Scope:     []byte(`{"jobs":["x"]}`),
	}
	p := PrincipalFromKey(key)
	assert.Equal(t, PrincipalAPIKey, p.Kind)
	assert.Equal(t, models.RoleOperator, p.Role)
	assert.Equal(t, "csk_live_ab", p.Subject)
	assert.Equal(t, []byte(`{"jobs":["x"]}`), p.Scope)
	assert.Equal(t, &id, p.KeyID)
	assert.Nil(t, p.UserID)
}

func TestPrincipalFromUser(t *testing.T) {
	id := uuid.New()
	u := &models.User{ID: id, Email: "a@b.com", Role: models.RoleViewer}
	p := PrincipalFromUser(u)
	assert.Equal(t, PrincipalUser, p.Kind)
	assert.Equal(t, models.RoleViewer, p.Role)
	assert.Equal(t, "a@b.com", p.Subject)
	assert.Equal(t, &id, p.UserID)
	assert.Nil(t, p.Scope)
	assert.Nil(t, p.KeyID)
}
