package auth

import (
	"encoding/json"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
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
	Grants  policy.Grants // namespace -> role; "*" is cluster-wide. Constructors never leave it nil.
	Scope   []byte        // raw KeyScope JSON; nil/empty == unrestricted.
	Subject string        // audit actor: key prefix or user email.
	UserID  *uuid.UUID
	KeyID   *uuid.UUID
}

// RoleIn returns the principal's effective role in namespace ns.
func (p *Principal) RoleIn(ns string) models.Role {
	if p == nil {
		return ""
	}
	return p.Grants.RoleIn(ns)
}

// ClusterRole returns the principal's cluster-wide role, or "" when it has none.
func (p *Principal) ClusterRole() models.Role {
	if p == nil {
		return ""
	}
	return p.Grants.ClusterRole()
}

// AllowedNamespaces returns nil when the principal is unrestricted, otherwise
// the namespaces it may read. A nil principal may read none.
func (p *Principal) AllowedNamespaces() []string {
	if p == nil {
		return []string{}
	}
	return p.Grants.AllowedNamespaces()
}

// PrincipalFromKey builds a Principal from a validated API key. A key with SQL
// NULL namespaces holds its role cluster-wide; any other unusable namespaces
// value grants nothing.
func PrincipalFromKey(k *models.APIKey) *Principal {
	id := k.ID
	return &Principal{
		Kind:    PrincipalAPIKey,
		Grants:  keyGrants(k),
		Scope:   k.Scope,
		Subject: k.KeyPrefix,
		KeyID:   &id,
	}
}

func keyGrants(k *models.APIKey) policy.Grants {
	if len(k.Namespaces) == 0 {
		return policy.Grants{policy.ClusterNamespace: k.Role}
	}
	var namespaces []string
	if err := json.Unmarshal(k.Namespaces, &namespaces); err != nil || len(namespaces) == 0 {
		log.Warn("api key namespaces are unusable; granting nothing", "key_prefix", k.KeyPrefix, "error", err)
		return policy.Grants{}
	}
	grants := make(policy.Grants, len(namespaces))
	for _, ns := range namespaces {
		if ns != "" {
			grants[ns] = k.Role
		}
	}
	return grants
}

// PrincipalFromUser builds a Principal from an authenticated user, resolving
// its grants from the stored groups and, only when the provider verified it,
// the stored email. Grants are resolved on every call, so a changed policy or
// changed groups apply without a new login. SSO users are unscoped (nil Scope)
// in v1.
func PrincipalFromUser(u *models.User, src policy.Source) *Principal {
	id := u.ID
	return &Principal{
		Kind:    PrincipalUser,
		Grants:  resolveUserGrants(u, src),
		Subject: u.Email,
		UserID:  &id,
	}
}

func resolveUserGrants(u *models.User, src policy.Source) policy.Grants {
	if u == nil || src == nil {
		return policy.Grants{}
	}
	var groups []string
	if len(u.Groups) > 0 {
		if err := json.Unmarshal(u.Groups, &groups); err != nil {
			log.Warn("stored user groups are malformed; granting nothing", "user_id", u.ID, "error", err)
			return policy.Grants{}
		}
	}
	email := ""
	if u.EmailVerified {
		email = u.Email
	}
	grants := src.Resolve(groups, email)
	if grants == nil {
		return policy.Grants{}
	}
	return grants
}
