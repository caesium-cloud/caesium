package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
)

// ErrLoginDenied is returned when an external identity is not allowed to log in.
var ErrLoginDenied = errors.New("login denied")

// ErrInvalidExternalIdentity is returned when a provider returns an unusable identity.
var ErrInvalidExternalIdentity = errors.New("invalid external identity")

// ExternalIdentity is the normalized identity every provider produces.
type ExternalIdentity struct {
	Issuer      string
	Subject     string
	Email       string
	DisplayName string
	Groups      []string
	// EmailVerified reports that the provider verified Email or is trusted to
	// assert it. Only then may Email match policy users: bindings. The zero
	// value is untrusted.
	EmailVerified bool
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

// SSOService is the shared tail: resolve grants, provision the user, mint a session.
type SSOService struct {
	users    *UserStore
	sessions *SessionStore
	policy   policy.Source
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

// NewSSOService creates a shared SSO login pipeline that resolves grants
// through src. A nil src denies every login.
func NewSSOService(users *UserStore, sessions *SessionStore, src policy.Source, opts ...SSOServiceOption) *SSOService {
	s := &SSOService{users: users, sessions: sessions, policy: src}
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

	if ext == nil || strings.TrimSpace(ext.Issuer) == "" || strings.TrimSpace(ext.Subject) == "" {
		s.auditLoginError(ext, method, ip, "invalid_external_identity")
		return "", nil, ErrInvalidExternalIdentity
	}

	email := ""
	if ext.EmailVerified {
		email = ext.Email
	}
	var grants policy.Grants
	if s.policy != nil {
		grants = s.policy.Resolve(ext.Groups, email)
	}
	if len(grants) == 0 {
		outcome = OutcomeDenied
		s.auditLoginDenied(ext, method, ip, "no_binding")
		return "", nil, ErrLoginDenied
	}
	// User.Role keeps only the cluster role at last login (possibly "" for a
	// confined user); requests resolve their grants afresh.
	user, created, err := s.users.upsert(ctx, ext, grants.ClusterRole())
	if err != nil {
		s.auditLoginError(ext, method, ip, "user_upsert_failed")
		return "", nil, err
	}
	if created {
		s.auditUserProvisioned(ext, method, ip, user)
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
		Metadata: map[string]any{
			"provider": method,
			"role":     string(user.Role),
		},
	})
	return cookie, sess, nil
}

func (s *SSOService) auditUserProvisioned(ext *ExternalIdentity, method, ip string, user *models.User) {
	if user == nil {
		return
	}
	s.audit(AuditEntry{
		Actor:        auditActor(ext),
		Action:       ActionUserProvisioned,
		ResourceType: "user",
		ResourceID:   user.ID.String(),
		SourceIP:     ip,
		Outcome:      OutcomeSuccess,
		Metadata: map[string]any{
			"provider": method,
			"issuer":   ext.Issuer,
			"role":     string(user.Role),
		},
	})
}

func (s *SSOService) auditLoginDenied(ext *ExternalIdentity, method, ip, reason string) {
	s.audit(AuditEntry{
		Actor:    auditActor(ext),
		Action:   ActionAuthLoginDenied,
		SourceIP: ip,
		Outcome:  OutcomeDenied,
		Metadata: map[string]any{
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
		Metadata: map[string]any{
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
