package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
)

// ErrLoginDenied is returned when an external identity is not allowed to log in.
var ErrLoginDenied = errors.New("login denied")

// ErrNoBinding is the login denial for a valid identity that no policy binding
// (or fallback role) grants any role. It wraps ErrLoginDenied, so existing
// errors.Is(err, ErrLoginDenied) checks hold; the Console renders it as a
// distinct message (identity-and-access F1).
var ErrNoBinding = fmt.Errorf("%w: no binding", ErrLoginDenied)

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
	// Refresh is the provider's refresh grant from this login: the OIDC
	// refresh token, or the LDAP user DN. Nil when the provider has none
	// (SAML, or an IdP that issued no refresh token). The login tail seals it
	// onto the user row, replacing any earlier grant; its handle redacts
	// itself in every log, error and JSON form.
	Refresh *RefreshGrant `json:"-"`
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

// GroupRefresher is implemented by providers that can re-read a user's
// groups from a stored refresh grant without the user present (OIDC, LDAP).
// The caller owns the user's refresh lease (UserStore.ClaimRefreshLease)
// before calling, and records the outcome under the same fence.
//
// A nil error is a successful redemption. Every failure is a *RefreshError
// whose Kind says what the caller may do with the grant; an error of any
// other type is treated as RefreshUncertain (ClassifyRefreshError).
type GroupRefresher interface {
	Name() string
	RefreshGroups(ctx context.Context, req RefreshRequest) (GroupRefresh, error)
}

// RefreshRequest is one redemption of a user's refresh grant. Issuer and
// Subject are the user row's identity; a provider never applies groups from
// a response that names a different one.
type RefreshRequest struct {
	UserID  uuid.UUID
	Issuer  string
	Subject string
	Grant   RefreshGrant
}

// GroupRefresh is the result of a successful redemption.
type GroupRefresh struct {
	// Groups is the user's current group set; meaningful only when
	// GroupsKnown is set.
	Groups []string
	// GroupsKnown is false when the IdP accepted the grant but returned no
	// verifiable groups (an OIDC refresh whose ID token cannot be verified,
	// or without one and without a usable userinfo answer). The caller then
	// persists Handle and keeps the stored groups and their timestamp.
	GroupsKnown bool
	// Handle is the handle to store from now on: the rotated one when the IdP
	// rotated it, otherwise the one just redeemed.
	Handle RefreshHandle
	// Rotated reports that the IdP issued a new handle.
	Rotated bool
}

// RefreshErrorKind classifies a failed redemption. There are exactly three.
type RefreshErrorKind uint8

const (
	// RefreshTransient: nothing reached the IdP, or what did consumed nothing
	// (dial/DNS failure, cancelled before send, provider not configured for
	// the grant, an LDAP read that failed). Keep the grant; retry later.
	RefreshTransient RefreshErrorKind = iota + 1
	// RefreshUncertain: the request may have reached the IdP and redeemed
	// (and rotated) the grant, but no definitive answer came back (timeout
	// after send, connection reset mid-request, 5xx, an unparsable response,
	// an answer for another identity). Mark the grant uncertain; never revoke
	// on it.
	RefreshUncertain
	// RefreshDefinitive: the IdP definitively refused (OIDC invalid_grant; the
	// LDAP user entry is gone). See DenialScope for how it interacts with an
	// uncertain grant.
	RefreshDefinitive
)

// String returns the kind as a stable, lower-case label.
func (k RefreshErrorKind) String() string {
	switch k {
	case RefreshTransient:
		return "transient"
	case RefreshUncertain:
		return "uncertain"
	case RefreshDefinitive:
		return "definitive"
	default:
		return "unknown"
	}
}

// DenialScope says what a definitive refusal is about.
type DenialScope uint8

const (
	// DenialOfGrant: the IdP rejected this grant (OIDC invalid_grant). A grant
	// that may already have been redeemed is rejected for that reason alone,
	// so on an uncertain grant the refusal only clears the grant.
	DenialOfGrant DenialScope = iota
	// DenialOfSubject: the IdP says the user is gone (LDAP entry not found).
	// No earlier redemption can cause that, so it revokes even when the
	// grant is uncertain.
	DenialOfSubject
)

// RefreshError is a classified redemption failure. Its text never contains
// the handle, a token or a response body.
type RefreshError struct {
	Kind RefreshErrorKind
	// Scope is meaningful for RefreshDefinitive only.
	Scope DenialScope
	// Reason is a short, fixed, token-free code for logs and metrics, such
	// as "invalid_grant" or "timeout_after_send".
	Reason string
	// Err is the underlying cause, if any; it never carries a credential.
	Err error
}

// NewRefreshError builds a classified failure.
func NewRefreshError(kind RefreshErrorKind, reason string, cause error) *RefreshError {
	return &RefreshError{Kind: kind, Reason: reason, Err: cause}
}

// NewRefreshDenial builds a definitive failure with its scope.
func NewRefreshDenial(scope DenialScope, reason string) *RefreshError {
	return &RefreshError{Kind: RefreshDefinitive, Scope: scope, Reason: reason}
}

func (e *RefreshError) Error() string {
	msg := "group refresh " + e.Kind.String()
	if e.Reason != "" {
		msg += ": " + e.Reason
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *RefreshError) Unwrap() error { return e.Err }

// ClassifyRefreshError returns the kind of a redemption failure: zero for
// nil, the kind of a *RefreshError, and RefreshUncertain for anything else,
// because uncertain is the outcome that never revokes.
func ClassifyRefreshError(err error) RefreshErrorKind {
	if err == nil {
		return 0
	}
	if refreshErr, ok := errors.AsType[*RefreshError](err); ok && refreshErr.Kind != 0 {
		return refreshErr.Kind
	}
	return RefreshUncertain
}

// SSOService is the shared tail: resolve grants, provision the user, mint a session.
type SSOService struct {
	users    *UserStore
	sessions *SessionStore
	policy   policy.Source
	auditor  *AuditLogger

	// beforeMint, when set (tests only), runs between the login's disabled
	// check and the session insert.
	beforeMint func()
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
		return "", nil, ErrNoBinding
	}
	// User.Role keeps only the cluster role at last login (possibly "" for a
	// confined user); requests resolve their grants afresh.
	user, created, err := s.users.upsert(ctx, ext, grants.ClusterRole())
	if errors.Is(err, ErrUserChanged) {
		outcome = OutcomeDenied
		s.auditLoginDenied(ext, method, ip, "user_changed")
		return "", nil, ErrLoginDenied
	}
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
	if s.beforeMint != nil {
		s.beforeMint()
	}
	// The insert is fenced on the disable generation this login read: a
	// disable committed since refuses it even if the user was re-enabled.
	cookie, sess, err := s.sessions.Create(ctx, CreateSessionRequest{
		UserID:            user.ID,
		AuthMethod:        method,
		SourceIP:          ip,
		UserAgent:         ua,
		DisableGeneration: user.DisableGeneration,
	})
	switch {
	case errors.Is(err, ErrUserDisabled):
		outcome = OutcomeDenied
		s.auditLoginDenied(ext, method, ip, "user_disabled")
		return "", nil, ErrLoginDenied
	case errors.Is(err, ErrUserChanged):
		outcome = OutcomeDenied
		s.auditLoginDenied(ext, method, ip, "user_changed")
		return "", nil, ErrLoginDenied
	}
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
