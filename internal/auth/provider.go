package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
)

// ErrLoginDenied is returned when an external identity is not allowed to log in.
var ErrLoginDenied = errors.New("login denied")

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
	auditor  *AuditLogger
}

// SSOServiceOption customizes the shared SSO login pipeline.
type SSOServiceOption func(*SSOService)

// WithSSOAuditLogger records SSO login audit entries during completion.
func WithSSOAuditLogger(auditor *AuditLogger) SSOServiceOption {
	return func(s *SSOService) {
		s.auditor = auditor
	}
}

// NewSSOService creates a shared SSO login pipeline.
func NewSSOService(users *UserStore, sessions *SessionStore, roles *RoleMapper, opts ...SSOServiceOption) *SSOService {
	s := &SSOService{users: users, sessions: sessions, roles: roles}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Complete turns an authenticated ExternalIdentity into a server-side session.
func (s *SSOService) Complete(ctx context.Context, ext *ExternalIdentity, method, ip, ua string) (string, *models.Session, error) {
	provider := metricProvider(method)
	start := time.Now()
	outcome := OutcomeError
	defer func() {
		metrics.SSOLoginsTotal.WithLabelValues(provider, outcome).Inc()
		metrics.SSOLoginDurationSeconds.WithLabelValues(provider).Observe(time.Since(start).Seconds())
	}()

	if ext == nil {
		outcome = OutcomeDenied
		s.auditLoginDenied(nil, method, ip, "missing_identity")
		return "", nil, ErrLoginDenied
	}

	role, ok := s.roles.Resolve(ext.Groups)
	if !ok {
		outcome = OutcomeDenied
		s.auditLoginDenied(ext, method, ip, "no_role_mapping")
		return "", nil, ErrLoginDenied
	}
	user, err := s.users.Upsert(ctx, ext, role)
	if err != nil {
		s.auditLoginError(ext, method, ip, "user_upsert_failed")
		return "", nil, err
	}
	if user.IsDisabled() {
		outcome = OutcomeDenied
		s.auditLoginDenied(ext, method, ip, "user_disabled")
		return "", nil, ErrLoginDenied
	}
	cookie, sess, err := s.sessions.Create(ctx, CreateSessionRequest{
		UserID:     user.ID,
		AuthMethod: method,
		SourceIP:   ip,
		UserAgent:  ua,
	})
	if err != nil {
		s.auditLoginError(ext, method, ip, "session_create_failed")
		return "", nil, err
	}

	outcome = OutcomeSuccess
	s.audit(AuditEntry{
		Actor:        auditActor(ext),
		Action:       ActionAuthLogin,
		ResourceType: "session",
		ResourceID:   sess.ID.String(),
		SourceIP:     ip,
		Outcome:      OutcomeSuccess,
		Metadata: map[string]interface{}{
			"provider": method,
			"role":     string(user.Role),
		},
	})
	return cookie, sess, nil
}

func (s *SSOService) auditLoginDenied(ext *ExternalIdentity, method, ip, reason string) {
	s.audit(AuditEntry{
		Actor:    auditActor(ext),
		Action:   ActionAuthLoginDenied,
		SourceIP: ip,
		Outcome:  OutcomeDenied,
		Metadata: map[string]interface{}{
			"provider": method,
			"reason":   reason,
		},
	})
}

func (s *SSOService) auditLoginError(ext *ExternalIdentity, method, ip, reason string) {
	s.audit(AuditEntry{
		Actor:    auditActor(ext),
		Action:   ActionAuthLogin,
		SourceIP: ip,
		Outcome:  OutcomeError,
		Metadata: map[string]interface{}{
			"provider": method,
			"reason":   reason,
		},
	})
}

func (s *SSOService) audit(entry AuditEntry) {
	if s == nil || s.auditor == nil {
		return
	}
	if err := s.auditor.Log(entry); err != nil {
		log.Warn("failed to write audit log", "error", err)
	}
}

func auditActor(ext *ExternalIdentity) string {
	if ext == nil {
		return "unknown"
	}
	if actor := strings.TrimSpace(ext.Email); actor != "" {
		return actor
	}
	if actor := strings.TrimSpace(ext.Subject); actor != "" {
		return actor
	}
	return "unknown"
}

func metricProvider(method string) string {
	if provider := strings.TrimSpace(method); provider != "" {
		return provider
	}
	return "unknown"
}
