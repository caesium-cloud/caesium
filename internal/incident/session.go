package incident

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/atom/docker"
	"github.com/caesium-cloud/caesium/internal/atom/kubernetes"
	"github.com/caesium-cloud/caesium/internal/atom/podman"
	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Errors the dispatcher returns when a session cannot be launched under the
// configured caps. They are sentinel values so callers (and tests) can branch.
var (
	// ErrGlobalSessionCap is returned when the global concurrent-session cap is
	// already reached. The incident stays open and queues for later triage.
	ErrGlobalSessionCap = errors.New("incident: global agent-session cap reached")
	// ErrJobSessionCap is returned when the per-job concurrent-session cap is
	// already reached. A different-key failure on a job with an active session
	// still opens its own incident, which queues rather than being dropped.
	ErrJobSessionCap = errors.New("incident: per-job agent-session cap reached")
	// ErrNoProfile is returned when a session is requested without a profile.
	ErrNoProfile = errors.New("incident: agent session requires a profile")
)

// AgentCredentialManager mints and revokes the scoped, short-lived credential a
// session runs with. auth.Service satisfies it. It is an interface so the
// supervisor can be unit-tested without the full auth service.
type AgentCredentialManager interface {
	MintAgentSessionKey(incidentID uuid.UUID, allowlist []string, ttl time.Duration) (*auth.CreateKeyResponse, error)
	RevokeKey(id uuid.UUID) error
}

// EngineFactory resolves an atom.Engine for an engine type. Injectable so tests
// can supply a fake engine without a container runtime.
type EngineFactory func(ctx context.Context, engineType models.AtomEngine) (atom.Engine, error)

// DefaultEngineFactory selects the concrete engine for a profile, mirroring the
// worker's runtime executor.
func DefaultEngineFactory(ctx context.Context, engineType models.AtomEngine) (atom.Engine, error) {
	switch engineType {
	case models.AtomEngineDocker, "":
		return docker.NewEngine(ctx), nil
	case models.AtomEngineKubernetes:
		return kubernetes.NewEngine(ctx), nil
	case models.AtomEnginePodman:
		return podman.NewEngine(ctx), nil
	default:
		return nil, fmt.Errorf("incident: unsupported agent engine type: %v", engineType)
	}
}

// SupervisorConfig carries the supervisor's operational limits (from env).
type SupervisorConfig struct {
	// APIBaseURL is the Caesium API base URL injected into the agent container so
	// it can reach its scoped tool surface. Falls back to a sane localhost value.
	APIBaseURL string
	// SessionTimeout is the wall-clock budget a session may run before being
	// forcibly stopped and marked timed_out.
	SessionTimeout time.Duration
	// MaxConcurrentSessions caps globally-active sessions (<=0 means 1).
	MaxConcurrentSessions int
	// PerJobConcurrentSessions caps active sessions per job (<=0 means 1).
	PerJobConcurrentSessions int
}

func (c SupervisorConfig) normalized() SupervisorConfig {
	if c.SessionTimeout <= 0 {
		c.SessionTimeout = 10 * time.Minute
	}
	if c.MaxConcurrentSessions <= 0 {
		c.MaxConcurrentSessions = 1
	}
	if c.PerJobConcurrentSessions <= 0 {
		c.PerJobConcurrentSessions = 1
	}
	if c.APIBaseURL == "" {
		c.APIBaseURL = "http://127.0.0.1:8080"
	}
	return c
}

// Supervisor drives a single agent container through the existing atom.Engine
// (create → wait → logs → stop) with wall-clock enforcement and persisted
// session logs, materializing an AgentSession record — deliberately NOT a
// JobRun/TaskRun (a session as a run would pollute the quarantine-filtered run
// stats and feed its own exhaust into the incident bus). It runs on the leader
// node in v1 and enforces the concurrent-session caps against the shared store,
// not per-process, so an N-node cluster does not multiply them.
type Supervisor struct {
	db        *gorm.DB
	store     *Store
	creds     AgentCredentialManager
	newEngine EngineFactory
	cfg       SupervisorConfig
}

// sessionSupervisor is the process-wide session supervisor, set at startup
// behind the master gate. The incident manager / executor (Streams B, E) obtain
// it here to dispatch a triage session; it is nil when the feature is disabled.
var sessionSupervisor *Supervisor

// SetSessionSupervisor registers the process-wide session supervisor.
func SetSessionSupervisor(s *Supervisor) { sessionSupervisor = s }

// SessionSupervisor returns the registered session supervisor, or nil.
func SessionSupervisor() *Supervisor { return sessionSupervisor }

// NewSupervisor constructs a session supervisor.
func NewSupervisor(db *gorm.DB, creds AgentCredentialManager, factory EngineFactory, cfg SupervisorConfig) *Supervisor {
	if factory == nil {
		factory = DefaultEngineFactory
	}
	return &Supervisor{
		db:        db,
		store:     NewStore(db),
		creds:     creds,
		newEngine: factory,
		cfg:       cfg.normalized(),
	}
}

// Dispatch enforces the concurrent-session caps against the shared store, then
// launches a session for the incident. It returns ErrGlobalSessionCap /
// ErrJobSessionCap without launching when a cap is already reached — the caller
// leaves the incident open to queue for later triage rather than dropping it.
func (s *Supervisor) Dispatch(ctx context.Context, inc *models.Incident, profile *models.AgentProfile) (*models.AgentSession, error) {
	if profile == nil {
		return nil, ErrNoProfile
	}

	globalActive, err := s.store.CountActiveAgentSessions(ctx)
	if err != nil {
		return nil, err
	}
	if globalActive >= int64(s.cfg.MaxConcurrentSessions) {
		return nil, ErrGlobalSessionCap
	}

	jobActive, err := s.store.CountActiveAgentSessionsForJob(ctx, inc.JobID)
	if err != nil {
		return nil, err
	}
	if jobActive >= int64(s.cfg.PerJobConcurrentSessions) {
		return nil, ErrJobSessionCap
	}

	return s.Run(ctx, inc, profile)
}

// Run mints a scoped token, records the AgentSession, launches the profile image
// through the engine, waits under a wall-clock budget, persists the container
// log, stops the container, sets the terminal state, and revokes the token so it
// never outlives the session. It is synchronous (the caller owns goroutine
// lifecycle) and cap enforcement is the caller's job via Dispatch.
func (s *Supervisor) Run(ctx context.Context, inc *models.Incident, profile *models.AgentProfile) (*models.AgentSession, error) {
	if profile == nil {
		return nil, ErrNoProfile
	}

	allowlist := unmarshalAllowlist(inc.AllowedJobs)

	// Mint a scoped credential valid slightly past the wall-clock budget so a
	// clean shutdown never races the token expiry. It is revoked on completion
	// regardless.
	minted, err := s.creds.MintAgentSessionKey(inc.ID, allowlist, s.cfg.SessionTimeout+time.Minute)
	if err != nil {
		return nil, fmt.Errorf("incident: mint agent session key: %w", err)
	}
	var tokenID *uuid.UUID
	if minted.Key != nil {
		id := minted.Key.ID
		tokenID = &id
	}

	engineType := profile.Engine
	if engineType == "" {
		engineType = models.AtomEngineDocker
	}

	now := time.Now().UTC()
	session := &models.AgentSession{
		ID:         uuid.New(),
		Namespace:  inc.Namespace,
		IncidentID: inc.ID,
		ProfileID:  &profile.ID,
		Engine:     engineType,
		TokenID:    tokenID,
		State:      models.AgentSessionStatePending,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := s.db.WithContext(ctx).Create(session).Error; err != nil {
		s.revoke(tokenID)
		return nil, fmt.Errorf("incident: create agent session: %w", err)
	}

	// From here on, always finalize the session and revoke the token.
	engine, err := s.newEngine(ctx, engineType)
	if err != nil {
		s.finalize(ctx, session, models.AgentSessionStateFailed, "", tokenID)
		return session, fmt.Errorf("incident: resolve engine: %w", err)
	}

	spec := container.Spec{Env: s.sessionEnv(inc, minted.Plaintext, profile)}
	a, err := engine.Create(&atom.EngineCreateRequest{
		Name:  "caesium-agent-" + session.ID.String(),
		Image: profile.Image,
		Spec:  spec,
	})
	if err != nil {
		s.finalize(ctx, session, models.AgentSessionStateFailed, "", tokenID)
		return session, fmt.Errorf("incident: create agent container: %w", err)
	}

	// Mark running with the container/atom identity for the UI.
	started := time.Now().UTC()
	session.State = models.AgentSessionStateRunning
	session.ContainerID = a.ID()
	session.StartedAt = &started
	session.UpdatedAt = started
	s.persist(ctx, session, map[string]any{
		"state":        session.State,
		"container_id": session.ContainerID,
		"started_at":   session.StartedAt,
		"updated_at":   session.UpdatedAt,
	})

	// Wall-clock budget: the wait context is bounded so a runaway agent is
	// forcibly stopped and recorded timed_out rather than burning tokens forever.
	waitCtx, cancel := context.WithTimeout(ctx, s.cfg.SessionTimeout)
	defer cancel()

	final, waitErr := engine.Wait(&atom.EngineWaitRequest{ID: a.ID(), Context: waitCtx})
	if final == nil {
		final = a
	}

	logText := s.captureLogs(engine, a.ID(), started)

	// Stop the container before reading terminal disposition; best-effort.
	if stopErr := engine.Stop(&atom.EngineStopRequest{ID: a.ID(), Force: true}); stopErr != nil {
		log.Warn("incident: failed to stop agent container", "session_id", session.ID, "atom_id", a.ID(), "error", stopErr)
	}

	state := terminalState(final.Result(), waitErr, waitCtx.Err())
	s.finalize(ctx, session, state, logText, tokenID)
	return session, nil
}

// sessionEnv builds the container environment. The scoped token, the incident
// id, and the API base URL are injected so the agent can fetch its bundle and
// call its tool surface. The bundle itself is fetched over HTTP (env injection
// cannot carry a 1 MiB log tail), per the design.
func (s *Supervisor) sessionEnv(inc *models.Incident, token string, profile *models.AgentProfile) map[string]string {
	env := map[string]string{
		"CAESIUM_API_URL":     s.cfg.APIBaseURL,
		"CAESIUM_AGENT_TOKEN": token,
		"CAESIUM_INCIDENT_ID": inc.ID.String(),
	}
	// NOTE: model-credential secret:// refs declared on the profile are resolved
	// and injected by the deployment's secret machinery (BYO model key), not by
	// Caesium's API — deliberately not surfaced here. See profile.SecretRefs.
	_ = profile
	return env
}

func (s *Supervisor) captureLogs(engine atom.Engine, atomID string, since time.Time) string {
	logs, err := engine.Logs(&atom.EngineLogsRequest{ID: atomID, Since: since})
	if err != nil || logs == nil {
		return ""
	}
	defer func() { _ = logs.Close() }()
	// Bound the read so a chatty agent cannot exhaust memory; the UI streams the
	// live view separately.
	const maxSessionLog = 1 << 20 // 1 MiB
	buf, err := io.ReadAll(io.LimitReader(logs, maxSessionLog))
	if err != nil {
		return string(buf)
	}
	return string(buf)
}

// finalize writes the terminal state + session log and revokes the scoped token
// so the credential dies with the session.
func (s *Supervisor) finalize(ctx context.Context, session *models.AgentSession, state models.AgentSessionState, logText string, tokenID *uuid.UUID) {
	completed := time.Now().UTC()
	session.State = state
	session.SessionLog = logText
	session.CompletedAt = &completed
	session.UpdatedAt = completed
	s.persist(ctx, session, map[string]any{
		"state":        state,
		"session_log":  logText,
		"completed_at": session.CompletedAt,
		"updated_at":   session.UpdatedAt,
	})
	s.revoke(tokenID)
}

func (s *Supervisor) persist(ctx context.Context, session *models.AgentSession, updates map[string]any) {
	if err := s.db.WithContext(ctx).
		Model(&models.AgentSession{}).
		Where("id = ?", session.ID).
		Updates(updates).Error; err != nil {
		log.Warn("incident: failed to persist agent session", "session_id", session.ID, "error", err)
	}
}

func (s *Supervisor) revoke(tokenID *uuid.UUID) {
	if tokenID == nil || s.creds == nil {
		return
	}
	if err := s.creds.RevokeKey(*tokenID); err != nil {
		log.Warn("incident: failed to revoke agent session token", "token_id", *tokenID, "error", err)
	}
}

// terminalState maps the engine's terminal Result plus any wait/timeout error to
// the AgentSession terminal state.
func terminalState(result atom.Result, waitErr, ctxErr error) models.AgentSessionState {
	if errors.Is(ctxErr, context.DeadlineExceeded) {
		return models.AgentSessionStateTimedOut
	}
	if errors.Is(ctxErr, context.Canceled) {
		return models.AgentSessionStateCancelled
	}
	if waitErr != nil {
		return models.AgentSessionStateFailed
	}
	switch result {
	case atom.Success:
		return models.AgentSessionStateSucceeded
	case atom.ResourceFailure, atom.Killed, atom.Terminated:
		return models.AgentSessionStateTimedOut
	default:
		return models.AgentSessionStateFailed
	}
}
