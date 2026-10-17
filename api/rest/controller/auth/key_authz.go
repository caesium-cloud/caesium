package auth

import (
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/models"
)

// Key administration authorisation (identity-and-access A5, design §7.2 and
// §8.1). The auth middleware already requires admin in every namespace a key
// route resolves; these checks repeat the decision at write time against the
// principal (and, for revoke and rotate, against the stored key row read in
// the write transaction), so a handler never acts on the middleware's word
// alone. A refusal is an *authz.Denial, which the middleware audits as a 403.

// higherPrivilegeMessage is the 403 body for a key wider than its minter.
const higherPrivilegeMessage = "cannot create api key with higher privilege than caller"

// authorizeKeyMint applies the minting constraint to a new key with role
// confined to namespaces ("*" for cluster-wide): p holds at least role in
// every namespace (the key's grants are a subset of p's), and admin in every
// namespace (key administration).
func authorizeKeyMint(p *iauth.Principal, role models.Role, namespaces []string) error {
	if err := keyScopeDenial(p, namespaces); err != nil {
		return err
	}
	if err := keyRoleDenial(p, role, namespaces, higherPrivilegeMessage); err != nil {
		return err
	}
	return keyRoleDenial(p, models.RoleAdmin, namespaces, "insufficient permissions")
}

// storedKeyAuthorizer authorises revoke and rotate against the stored key: p
// needs admin in every namespace the key is confined to and, since a rotation
// mints a replacement, at least the key's role there. A key whose stored
// namespaces are unusable is a cluster-only target.
func storedKeyAuthorizer(p *iauth.Principal) iauth.KeyAuthorizer {
	return func(key *models.APIKey) error {
		namespaces, err := iauth.StoredKeyNamespaces(key.Namespaces)
		if err != nil {
			namespaces = []string{models.ClusterNamespace}
		}
		if err := keyScopeDenial(p, namespaces); err != nil {
			return err
		}
		if err := keyRoleDenial(p, models.RoleAdmin, namespaces, "insufficient permissions"); err != nil {
			return err
		}
		return keyRoleDenial(p, key.Role, namespaces, higherPrivilegeMessage)
	}
}

// keyScopeDenial refuses a principal narrowed by a job allow-list or an
// agent-session claim: it administers no key.
func keyScopeDenial(p *iauth.Principal, namespaces []string) error {
	if p != nil && !iauth.ScopeRestricts(p.Scope) {
		return nil
	}
	return &authz.Denial{
		Reason:     "insufficient_scope",
		Required:   models.RoleAdmin,
		Namespaces: append([]string(nil), namespaces...),
		Message:    "insufficient permissions",
	}
}

// keyRoleDenial refuses p unless it holds role in every namespace.
func keyRoleDenial(p *iauth.Principal, role models.Role, namespaces []string, message string) error {
	short := iauth.KeyGrantShortfall(p, role, namespaces)
	if len(short) == 0 {
		return nil
	}
	return &authz.Denial{
		Reason:     "insufficient_role",
		Required:   role,
		Namespaces: short,
		Message:    message,
	}
}

// keyAuditMetadata adds a confined key's namespaces to its audit metadata. A
// cluster key's row is unchanged.
func keyAuditMetadata(metadata map[string]any, key *models.APIKey) map[string]any {
	if key == nil || len(key.Namespaces) == 0 {
		return metadata
	}
	namespaces, err := iauth.StoredKeyNamespaces(key.Namespaces)
	if err != nil || (len(namespaces) == 1 && namespaces[0] == models.ClusterNamespace) {
		return metadata
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata["namespaces"] = namespaces
	return metadata
}
