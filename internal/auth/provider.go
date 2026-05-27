package auth

import (
	"context"
	"errors"
	"net/http"

	"github.com/caesium-cloud/caesium/internal/models"
)

// ErrLoginDenied is returned when an external identity maps to no role.
var ErrLoginDenied = errors.New("login denied: no role mapping")

// ExternalIdentity is the normalized identity every provider produces.
type ExternalIdentity struct {
	Issuer      string
	Subject     string
	Email       string
	DisplayName string
	Groups      []string
}

// RedirectAuthenticator is implemented by browser-redirect providers.
type RedirectAuthenticator interface {
	Name() string
	Begin(w http.ResponseWriter, r *http.Request, returnTo string) (redirectURL string, err error)
	Complete(r *http.Request) (*ExternalIdentity, error)
}

// CredentialAuthenticator is implemented by credential providers.
type CredentialAuthenticator interface {
	Name() string
	Authenticate(ctx context.Context, username, password string) (*ExternalIdentity, error)
}

// SSOService is the shared tail: provision the user, map a role, mint a session.
type SSOService struct {
	users    *UserStore
	sessions *SessionStore
	roles    *RoleMapper
}

// NewSSOService creates a shared SSO login pipeline.
func NewSSOService(users *UserStore, sessions *SessionStore, roles *RoleMapper) *SSOService {
	return &SSOService{users: users, sessions: sessions, roles: roles}
}

// Complete turns an authenticated ExternalIdentity into a server-side session.
func (s *SSOService) Complete(ctx context.Context, ext *ExternalIdentity, method, ip, ua string) (string, *models.Session, error) {
	role, ok := s.roles.Resolve(ext.Groups)
	if !ok {
		return "", nil, ErrLoginDenied
	}
	user, err := s.users.Upsert(ctx, ext, role)
	if err != nil {
		return "", nil, err
	}
	return s.sessions.Create(ctx, CreateSessionRequest{
		UserID:     user.ID,
		AuthMethod: method,
		SourceIP:   ip,
		UserAgent:  ua,
	})
}
