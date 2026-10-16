package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// Namespaces of API keys (identity-and-access A5, design §7.2).
//
// A key's namespaces column is SQL NULL (cluster-wide, the state of every key
// minted before namespaces existed) or a JSON list of namespace names. Minting
// stores the list canonically (trimmed, validated, de-duplicated, sorted) and
// stores NULL, never ["*"], for a cluster key. Administration (revoke, rotate,
// list) reads stored values strictly: anything that is not NULL, a list naming
// "*", or a list of valid namespace names is unusable, and an unusable key is a
// cluster-only target.

var (
	// ErrInvalidKeyNamespaces reports a create request whose namespaces cannot
	// be stored. The wrapped message is safe to return to the client.
	ErrInvalidKeyNamespaces = errors.New("invalid api key namespaces")
	// ErrKeyNamespacesUnusable reports a stored namespaces value that is not
	// NULL, a list naming "*", or a list of valid namespace names.
	ErrKeyNamespacesUnusable = errors.New("api key namespaces are unusable")
)

// UndeclaredNamespacesError reports namespaces the served policy file does not
// declare.
type UndeclaredNamespacesError struct {
	Namespaces []string
}

func (e *UndeclaredNamespacesError) Error() string {
	if len(e.Namespaces) == 1 {
		return fmt.Sprintf("namespace %q is not declared in the access policy", e.Namespaces[0])
	}
	quoted := make([]string, len(e.Namespaces))
	for i, ns := range e.Namespaces {
		quoted[i] = fmt.Sprintf("%q", ns)
	}
	return fmt.Sprintf("namespaces %s are not declared in the access policy", strings.Join(quoted, ", "))
}

// KeyAuthorizer decides, inside the write transaction, whether the caller may
// administer the stored key it is given. A non-nil error aborts the operation
// unchanged and is returned to the caller as is.
type KeyAuthorizer func(key *models.APIKey) error

// NormalizeKeyNamespaces validates the namespaces a create request asks for.
// requested == nil (omitted or JSON null) is a cluster key: stored NULL,
// effective ["*"]. Otherwise each entry is trimmed and must be "*" or a
// namespace name; the list must name at least one and may not combine "*"
// with other namespaces. It returns the value to store (nil for NULL) and the
// effective, sorted namespaces.
func NormalizeKeyNamespaces(requested *[]string) (datatypes.JSON, []string, error) {
	if requested == nil {
		return nil, []string{models.ClusterNamespace}, nil
	}
	if len(*requested) == 0 {
		return nil, nil, fmt.Errorf("%w: namespaces must name at least one namespace; omit it for a cluster-wide key", ErrInvalidKeyNamespaces)
	}
	namespaces := make([]string, 0, len(*requested))
	for _, raw := range *requested {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			return nil, nil, fmt.Errorf("%w: a namespace must not be empty", ErrInvalidKeyNamespaces)
		}
		namespace, err := models.NormalizeObjectNamespace(trimmed)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %s", ErrInvalidKeyNamespaces, err.Error())
		}
		namespaces = append(namespaces, namespace)
	}
	slices.Sort(namespaces)
	namespaces = slices.Compact(namespaces)
	if slices.Contains(namespaces, models.ClusterNamespace) {
		if len(namespaces) > 1 {
			return nil, nil, fmt.Errorf("%w: %q cannot be combined with other namespaces", ErrInvalidKeyNamespaces, models.ClusterNamespace)
		}
		return nil, []string{models.ClusterNamespace}, nil
	}
	stored, err := json.Marshal(namespaces)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal api key namespaces: %w", err)
	}
	return datatypes.JSON(stored), namespaces, nil
}

// StoredKeyNamespaces returns the namespaces a stored key is confined to:
// ["*"] for SQL NULL or a list naming "*", otherwise the sorted list. Any
// other value is ErrKeyNamespacesUnusable; it is never read as "*".
func StoredKeyNamespaces(raw datatypes.JSON) ([]string, error) {
	if len(raw) == 0 {
		return []string{models.ClusterNamespace}, nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil || len(list) == 0 {
		return nil, ErrKeyNamespacesUnusable
	}
	for _, namespace := range list {
		normalized, err := models.NormalizeObjectNamespace(namespace)
		if err != nil || namespace == "" || normalized != namespace {
			return nil, ErrKeyNamespacesUnusable
		}
	}
	if slices.Contains(list, models.ClusterNamespace) {
		return []string{models.ClusterNamespace}, nil
	}
	slices.Sort(list)
	return slices.Compact(list), nil
}

// KeyVisibleTo reports whether a namespace-restricted reader whose allowed
// namespaces are allowed may see a key with the stored namespaces: the key
// must be confined, usably, to namespaces all inside allowed. Cluster keys
// and unusable values are never visible to a restricted reader.
func KeyVisibleTo(raw datatypes.JSON, allowed []string) bool {
	namespaces, err := StoredKeyNamespaces(raw)
	if err != nil {
		return false
	}
	for _, namespace := range namespaces {
		if namespace == models.ClusterNamespace || !slices.Contains(allowed, namespace) {
			return false
		}
	}
	return true
}

// KeyGrantShortfall returns the namespaces among namespaces ("*" included) in
// which p holds less than role; empty when p holds role in every one. A nil
// principal holds nothing.
func KeyGrantShortfall(p *Principal, role models.Role, namespaces []string) []string {
	var short []string
	for _, namespace := range namespaces {
		if !HasRole(p.RoleIn(namespace), role) {
			short = append(short, namespace)
		}
	}
	return short
}

// UndeclaredKeyNamespaces returns the namespaces (never "*") the process
// policy file does not declare. With no policy file every namespace counts as
// declared, as for job namespaces (B2).
func UndeclaredKeyNamespaces(namespaces []string) []string {
	holder := policy.ProcessHolder()
	if !holder.FileBacked() {
		return nil
	}
	var undeclared []string
	for _, namespace := range namespaces {
		if namespace == models.ClusterNamespace {
			continue
		}
		if _, declared := holder.Namespace(namespace); !declared {
			undeclared = append(undeclared, namespace)
		}
	}
	return undeclared
}

// ScopeRestricts reports whether a key scope narrows its holder below its
// grants: a job allow-list, an agent-session claim, or an undecodable value.
func ScopeRestricts(scopeJSON []byte) bool {
	claim, err := DecodeAgentClaim(scopeJSON)
	if err != nil || claim != nil {
		return true
	}
	jobs, err := ScopeJobs(scopeJSON)
	return err != nil || len(jobs) > 0
}

// KeyNamespacesByID returns the namespaces administering the stored key id
// requires a grant in, for route authorisation: the namespaces it is confined
// to, and ["*"] for a cluster key and for an unusable value (which only a
// cluster principal may then administer; the value never grants anything).
// A missing key is gorm.ErrRecordNotFound.
func (s *Service) KeyNamespacesByID(ctx context.Context, id uuid.UUID) ([]string, error) {
	conn, err := s.readDB(ctx)
	if err != nil {
		return nil, err
	}
	var key models.APIKey
	if err := s.withReadRetry(func() error {
		return conn.Select("id", "namespaces").First(&key, "id = ?", id).Error
	}); err != nil {
		return nil, err
	}
	namespaces, err := StoredKeyNamespaces(key.Namespaces)
	if err != nil {
		return []string{models.ClusterNamespace}, nil
	}
	return namespaces, nil
}
