package auth

import (
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
)

// AgentSessionKeyRole is the RBAC role an agent-session credential is minted
// with. It is the minimum role that satisfies the /v1/agent/* route policy
// (read context at viewer level, propose/execute actions and append notes at
// runner level). The credential is additionally scope-locked to a single
// incident's agent routes by its AgentClaim, so the role alone never grants
// reach beyond that incident — RBAC and scope both gate every request.
const AgentSessionKeyRole = models.RoleRunner

// ErrAgentKeyIncidentRequired is returned when a mint is attempted without a
// bound incident.
var ErrAgentKeyIncidentRequired = errors.New("mint agent session key: incident id required")

// MintAgentSessionKey creates a scoped, short-lived API key bound to a single
// incident's /v1/agent/* tool surface. It is the credential-minting site the
// incident manager (an unscoped, server-side principal) calls once per agent
// session: the caller supplies the FROZEN job allowlist snapshotted at incident
// open, and the returned key can never widen it. The plaintext is returned once
// (to be injected into the agent container) and only its hash is persisted.
//
// The key:
//   - carries an AgentClaim, so the deny-by-default route-scope switch treats it
//     as valid ONLY for this incident's agent routes and 403s everything else;
//   - is minted at the runner role (the minimum the agent routes require);
//   - expires after ttl, so the credential dies with the session even if the
//     supervisor never gets to revoke it explicitly.
func (s *Service) MintAgentSessionKey(incidentID uuid.UUID, allowlist []string, ttl time.Duration) (*CreateKeyResponse, error) {
	if incidentID == uuid.Nil {
		return nil, ErrAgentKeyIncidentRequired
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	expiry := s.nowUTC().Add(ttl)

	scope := &models.KeyScope{
		Agent: &models.AgentClaim{
			IncidentID: incidentID,
			Jobs:       normalizeAllowlist(allowlist),
		},
	}

	return s.CreateKey(&CreateKeyRequest{
		Description: "agent session " + incidentID.String(),
		Role:        AgentSessionKeyRole,
		Scope:       scope,
		CreatedBy:   "incident-manager",
		ExpiresAt:   &expiry,
	})
}

// normalizeAllowlist trims, de-duplicates, and sorts the frozen job allowlist so
// the persisted claim is deterministic.
func normalizeAllowlist(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, a := range in {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if _, ok := seen[a]; ok {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}
