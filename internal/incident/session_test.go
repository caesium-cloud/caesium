package incident

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// --- fakes ---------------------------------------------------------------

type fakeAtom struct {
	id     string
	result atom.Result
}

func (f *fakeAtom) ID() string           { return f.id }
func (f *fakeAtom) State() atom.State    { return atom.Stopped }
func (f *fakeAtom) Result() atom.Result  { return f.result }
func (f *fakeAtom) ExitCode() *int       { return nil }
func (f *fakeAtom) CreatedAt() time.Time { return time.Now() }
func (f *fakeAtom) StartedAt() time.Time { return time.Now() }
func (f *fakeAtom) StoppedAt() time.Time { return time.Now() }
func (f *fakeAtom) Engine() atom.Engine  { return nil }

type fakeEngine struct {
	result     atom.Result
	logs       string
	createdEnv map[string]string
	stopped    bool
}

func (e *fakeEngine) Get(*atom.EngineGetRequest) (atom.Atom, error)     { return nil, nil }
func (e *fakeEngine) List(*atom.EngineListRequest) ([]atom.Atom, error) { return nil, nil }
func (e *fakeEngine) Create(req *atom.EngineCreateRequest) (atom.Atom, error) {
	e.createdEnv = req.Spec.Env
	return &fakeAtom{id: "atom-" + req.Name, result: e.result}, nil
}
func (e *fakeEngine) Wait(req *atom.EngineWaitRequest) (atom.Atom, error) {
	return &fakeAtom{id: req.ID, result: e.result}, nil
}
func (e *fakeEngine) Stop(*atom.EngineStopRequest) error { e.stopped = true; return nil }
func (e *fakeEngine) Logs(*atom.EngineLogsRequest) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(e.logs)), nil
}

type fakeCreds struct {
	minted  []mintCall
	revoked []uuid.UUID
}

type mintCall struct {
	incidentID uuid.UUID
	allowlist  []string
}

func (c *fakeCreds) MintAgentSessionKey(incidentID uuid.UUID, allowlist []string, ttl time.Duration) (*auth.CreateKeyResponse, error) {
	c.minted = append(c.minted, mintCall{incidentID: incidentID, allowlist: allowlist})
	return &auth.CreateKeyResponse{
		Plaintext: "csk_live_faketoken",
		Key:       &models.APIKey{ID: uuid.New()},
	}, nil
}

func (c *fakeCreds) RevokeKey(id uuid.UUID) error {
	c.revoked = append(c.revoked, id)
	return nil
}

// --- tests ---------------------------------------------------------------

func TestSupervisorRunRecordsSessionMintsAndRevokes(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ctx := context.Background()

	tr := mkTrigger(t, db)
	jobID := mkJob(t, db, tr, "vendor-x")
	store := NewStore(db)
	inc, _, err := store.OpenOrAppend(ctx, OpenParams{JobID: jobID, TaskName: "extract", Class: ClassUnknown})
	require.NoError(t, err)

	profile := &models.AgentProfile{ID: uuid.New(), Name: "triage", Image: "caesium/triage:latest", Engine: models.AtomEngineDocker}
	require.NoError(t, db.Create(profile).Error)

	creds := &fakeCreds{}
	engine := &fakeEngine{result: atom.Success, logs: "agent booted\nplan: retry\n"}
	sup := NewSupervisor(db, creds, func(context.Context, models.AtomEngine) (atom.Engine, error) {
		return engine, nil
	}, SupervisorConfig{SessionTimeout: time.Minute})

	session, err := sup.Run(ctx, inc, profile)
	require.NoError(t, err)
	require.NotNil(t, session)

	// Reload the persisted session.
	var got models.AgentSession
	require.NoError(t, db.First(&got, "id = ?", session.ID).Error)
	require.Equal(t, models.AgentSessionStateSucceeded, got.State)
	require.Equal(t, inc.ID, got.IncidentID)
	require.NotNil(t, got.TokenID)
	require.Contains(t, got.SessionLog, "agent booted")
	require.NotNil(t, got.CompletedAt)

	// A scoped token was minted with the incident's FROZEN allowlist.
	require.Len(t, creds.minted, 1)
	require.Equal(t, inc.ID, creds.minted[0].incidentID)
	require.Equal(t, []string{"vendor-x"}, creds.minted[0].allowlist)

	// The container received the scoped token + incident id, and the token was
	// revoked so it never outlives the session.
	require.Equal(t, "csk_live_faketoken", engine.createdEnv["CAESIUM_AGENT_TOKEN"])
	require.Equal(t, inc.ID.String(), engine.createdEnv["CAESIUM_INCIDENT_ID"])
	require.True(t, engine.stopped)
	require.Len(t, creds.revoked, 1)
	require.Equal(t, *got.TokenID, creds.revoked[0])
}

func TestSupervisorDispatchEnforcesGlobalCap(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ctx := context.Background()

	tr := mkTrigger(t, db)
	jobID := mkJob(t, db, tr, "vendor-x")
	store := NewStore(db)
	inc, _, err := store.OpenOrAppend(ctx, OpenParams{JobID: jobID, TaskName: "extract", Class: ClassUnknown})
	require.NoError(t, err)

	// Seed an already-running session to occupy the single global slot.
	now := time.Now().UTC()
	require.NoError(t, db.Create(&models.AgentSession{
		ID:         uuid.New(),
		IncidentID: inc.ID,
		State:      models.AgentSessionStateRunning,
		CreatedAt:  now,
		UpdatedAt:  now,
	}).Error)

	profile := &models.AgentProfile{ID: uuid.New(), Name: "triage", Image: "x", Engine: models.AtomEngineDocker}
	require.NoError(t, db.Create(profile).Error)

	sup := NewSupervisor(db, &fakeCreds{}, func(context.Context, models.AtomEngine) (atom.Engine, error) {
		return &fakeEngine{result: atom.Success}, nil
	}, SupervisorConfig{MaxConcurrentSessions: 1, PerJobConcurrentSessions: 1})

	_, err = sup.Dispatch(ctx, inc, profile)
	require.ErrorIs(t, err, ErrGlobalSessionCap)
}

// TestSupervisorDispatchEnforcesPerJobCap proves the per-job cap is enforced by
// the DB reservation (and diagnosed to the ErrJobSessionCap sentinel) even when
// the global cap has headroom.
func TestSupervisorDispatchEnforcesPerJobCap(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ctx := context.Background()

	tr := mkTrigger(t, db)
	jobID := mkJob(t, db, tr, "vendor-x")
	store := NewStore(db)
	inc, _, err := store.OpenOrAppend(ctx, OpenParams{JobID: jobID, TaskName: "extract", Class: ClassUnknown})
	require.NoError(t, err)

	// One running session for this job's incident occupies the per-job slot.
	now := time.Now().UTC()
	require.NoError(t, db.Create(&models.AgentSession{
		ID:         uuid.New(),
		IncidentID: inc.ID,
		State:      models.AgentSessionStateRunning,
		CreatedAt:  now,
		UpdatedAt:  now,
	}).Error)

	profile := &models.AgentProfile{ID: uuid.New(), Name: "triage", Image: "x", Engine: models.AtomEngineDocker}
	require.NoError(t, db.Create(profile).Error)

	// Global cap has headroom (5); only the per-job cap (1) is binding.
	sup := NewSupervisor(db, &fakeCreds{}, func(context.Context, models.AtomEngine) (atom.Engine, error) {
		return &fakeEngine{result: atom.Success}, nil
	}, SupervisorConfig{MaxConcurrentSessions: 5, PerJobConcurrentSessions: 1})

	_, err = sup.Dispatch(ctx, inc, profile)
	require.ErrorIs(t, err, ErrJobSessionCap)
}

func TestSupervisorRunTimeoutMarksTimedOut(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ctx := context.Background()

	tr := mkTrigger(t, db)
	jobID := mkJob(t, db, tr, "vendor-x")
	store := NewStore(db)
	inc, _, err := store.OpenOrAppend(ctx, OpenParams{JobID: jobID, TaskName: "extract", Class: ClassUnknown})
	require.NoError(t, err)

	profile := &models.AgentProfile{ID: uuid.New(), Name: "triage", Image: "x", Engine: models.AtomEngineDocker}
	require.NoError(t, db.Create(profile).Error)

	// A wait that respects the deadline: block until ctx is done, then report the
	// deadline error via the returned atom being nil + ctx error.
	engine := &slowEngine{}
	sup := NewSupervisor(db, &fakeCreds{}, func(context.Context, models.AtomEngine) (atom.Engine, error) {
		return engine, nil
	}, SupervisorConfig{SessionTimeout: 50 * time.Millisecond})

	session, err := sup.Run(ctx, inc, profile)
	require.NoError(t, err)
	var got models.AgentSession
	require.NoError(t, db.First(&got, "id = ?", session.ID).Error)
	require.Equal(t, models.AgentSessionStateTimedOut, got.State)
}

type slowEngine struct{ fakeEngine }

func (e *slowEngine) Wait(req *atom.EngineWaitRequest) (atom.Atom, error) {
	<-req.Context.Done()
	return nil, req.Context.Err()
}

// blockingEngine keeps every session "running" until release is closed, so a
// batch of concurrent dispatches genuinely overlap and exercise the cap.
type blockingEngine struct {
	release chan struct{}
}

func (e *blockingEngine) Get(*atom.EngineGetRequest) (atom.Atom, error)     { return nil, nil }
func (e *blockingEngine) List(*atom.EngineListRequest) ([]atom.Atom, error) { return nil, nil }
func (e *blockingEngine) Create(req *atom.EngineCreateRequest) (atom.Atom, error) {
	return &fakeAtom{id: "atom-" + req.Name, result: atom.Success}, nil
}
func (e *blockingEngine) Wait(req *atom.EngineWaitRequest) (atom.Atom, error) {
	select {
	case <-e.release:
		return &fakeAtom{id: req.ID, result: atom.Success}, nil
	case <-req.Context.Done():
		return nil, req.Context.Err()
	}
}
func (e *blockingEngine) Stop(*atom.EngineStopRequest) error { return nil }
func (e *blockingEngine) Logs(*atom.EngineLogsRequest) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

// TestSupervisorDispatchCapRaceNeverOvershoots proves the cap holds ACROSS
// SUPERVISOR INSTANCES — the cross-process property an in-process mutex cannot
// provide. It constructs N separate Supervisor structs (each with its own
// mutex and its own credential minter) that SHARE ONE DATABASE, then dispatches
// them concurrently against a global cap of 1. Correctness must come from the
// DB-atomic conditional reservation, not the mutex: exactly one is admitted,
// N-1 get ErrGlobalSessionCap, exactly one session row exists, and exactly one
// token is minted (no leaked live credentials).
func TestSupervisorDispatchCapRaceNeverOvershoots(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ctx := context.Background()

	tr := mkTrigger(t, db)
	jobID := mkJob(t, db, tr, "vendor-x")
	store := NewStore(db)
	inc, _, err := store.OpenOrAppend(ctx, OpenParams{JobID: jobID, TaskName: "extract", Class: ClassUnknown})
	require.NoError(t, err)

	profile := &models.AgentProfile{ID: uuid.New(), Name: "triage", Image: "x", Engine: models.AtomEngineDocker}
	require.NoError(t, db.Create(profile).Error)

	const n = 8
	release := make(chan struct{})

	// N distinct supervisors (distinct mutexes, distinct minters), one shared DB.
	supers := make([]*Supervisor, n)
	credsList := make([]*fakeCreds, n)
	for i := 0; i < n; i++ {
		credsList[i] = &fakeCreds{}
		supers[i] = NewSupervisor(db, credsList[i], func(context.Context, models.AtomEngine) (atom.Engine, error) {
			return &blockingEngine{release: release}, nil
		}, SupervisorConfig{MaxConcurrentSessions: 1, PerJobConcurrentSessions: 1, SessionTimeout: time.Minute})
	}

	type outcome struct {
		session *models.AgentSession
		err     error
	}
	results := make(chan outcome, n)
	for i := 0; i < n; i++ {
		sup := supers[i]
		go func() {
			sess, err := sup.Dispatch(ctx, inc, profile)
			results <- outcome{session: sess, err: err}
		}()
	}

	// The lone winner blocks in Wait until released; the losers reject at the DB
	// reservation. Collect the n-1 cap rejections first, then release the winner.
	capRejections := 0
	for capRejections < n-1 {
		r := <-results
		require.ErrorIs(t, r.err, ErrGlobalSessionCap)
		capRejections++
	}
	close(release)

	winner := <-results
	require.NoError(t, winner.err)
	require.NotNil(t, winner.session)

	// Exactly one session row ever existed — the cap was never overshot.
	var count int64
	require.NoError(t, db.Model(&models.AgentSession{}).Count(&count).Error)
	require.Equal(t, int64(1), count)

	// Exactly one token was minted across all supervisors — no extra live creds.
	totalMinted := 0
	for _, c := range credsList {
		totalMinted += len(c.minted)
	}
	require.Equal(t, 1, totalMinted)
}

// cancelEngine signals once it has ENTERED Wait — i.e. after the supervisor's
// "mark running" persist has already completed on the healthy parent context —
// then blocks on the context so the test can cancel the parent mid-session. By
// signalling from Wait rather than Create, no DB write ever executes on the
// cancelled context (the single-connection in-memory SQLite would otherwise
// evict the poisoned connection and drop the schema); only the ctx-wait here and
// finalize's DETACHED persist are exercised by the cancellation — which is
// exactly the production property under test.
type cancelEngine struct {
	waiting chan struct{}
	stopped bool
}

func (e *cancelEngine) Get(*atom.EngineGetRequest) (atom.Atom, error)     { return nil, nil }
func (e *cancelEngine) List(*atom.EngineListRequest) ([]atom.Atom, error) { return nil, nil }
func (e *cancelEngine) Create(req *atom.EngineCreateRequest) (atom.Atom, error) {
	return &fakeAtom{id: "atom-" + req.Name, result: atom.Success}, nil
}
func (e *cancelEngine) Wait(req *atom.EngineWaitRequest) (atom.Atom, error) {
	e.waiting <- struct{}{}
	<-req.Context.Done()
	return nil, req.Context.Err()
}
func (e *cancelEngine) Stop(*atom.EngineStopRequest) error { e.stopped = true; return nil }
func (e *cancelEngine) Logs(*atom.EngineLogsRequest) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

// TestSupervisorFinalizeOnCancelledContext proves that a cancelled parent
// context still drives the session to a terminal state AND revokes the scoped
// token — a cancelled ctx must never leak a live credential.
func TestSupervisorFinalizeOnCancelledContext(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	tr := mkTrigger(t, db)
	jobID := mkJob(t, db, tr, "vendor-x")
	store := NewStore(db)
	inc, _, err := store.OpenOrAppend(context.Background(), OpenParams{JobID: jobID, TaskName: "extract", Class: ClassUnknown})
	require.NoError(t, err)

	profile := &models.AgentProfile{ID: uuid.New(), Name: "triage", Image: "x", Engine: models.AtomEngineDocker}
	require.NoError(t, db.Create(profile).Error)

	creds := &fakeCreds{}
	engine := &cancelEngine{waiting: make(chan struct{}, 1)}
	sup := NewSupervisor(db, creds, func(context.Context, models.AtomEngine) (atom.Engine, error) {
		return engine, nil
	}, SupervisorConfig{SessionTimeout: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *models.AgentSession, 1)
	go func() {
		sess, _ := sup.Run(ctx, inc, profile)
		done <- sess
	}()

	// Cancel only once the supervisor has recorded the session running and is
	// blocked in Wait — so the cancellation lands on the ctx-wait and finalize's
	// detached persist, never on a live DB write.
	<-engine.waiting
	cancel()

	session := <-done
	require.NotNil(t, session)

	// The session is terminal (cancelled) and persisted despite the cancelled ctx.
	var got models.AgentSession
	require.NoError(t, db.First(&got, "id = ?", session.ID).Error)
	require.Equal(t, models.AgentSessionStateCancelled, got.State)
	require.NotNil(t, got.CompletedAt)

	// The scoped token was revoked — no leaked live credential.
	require.Len(t, creds.revoked, 1)
	require.NotNil(t, got.TokenID)
	require.Equal(t, *got.TokenID, creds.revoked[0])
}
