package auth

import (
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
)

// PrincipalKind distinguishes the credential type behind an authenticated request.
type PrincipalKind string

const (
	PrincipalAPIKey PrincipalKind = "api_key"
	PrincipalUser   PrincipalKind = "user"
)

// Principal is the unified authenticated identity used by RBAC, scope checks,
// and audit. It is produced from either an API key or a user session.
type Principal struct {
	Kind    PrincipalKind
	Role    models.Role
	Scope   []byte // raw KeyScope JSON; nil/empty == unrestricted.
	Subject string // audit actor: key prefix or user email.
	UserID  *uuid.UUID
	KeyID   *uuid.UUID
}

// PrincipalFromKey builds a Principal from a validated API key.
func PrincipalFromKey(k *models.APIKey) *Principal {
	id := k.ID
	return &Principal{
		Kind:    PrincipalAPIKey,
		Role:    k.Role,
		Scope:   k.Scope,
		Subject: k.KeyPrefix,
		KeyID:   &id,
	}
}

// PrincipalFromUser builds a Principal from an authenticated user. SSO users are
// unscoped (nil Scope) in v1.
func PrincipalFromUser(u *models.User) *Principal {
	id := u.ID
	return &Principal{
		Kind:    PrincipalUser,
		Role:    u.Role,
		Subject: u.Email,
		UserID:  &id,
	}
}
