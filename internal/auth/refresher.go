package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Group refresh (identity-and-access §11.1, plan item E2).
//
// Every session and user-bound key authenticates its user, and the user's
// stored groups decide its grants. GroupRefreshService keeps those groups
// fresh from the identity provider through the user's sealed refresh grant
// (E1), using the lease and fence primitives of UserStore:
//
//   - groups confirmed within CAESIUM_AUTH_GROUP_REFRESH_INTERVAL: nothing
//     happens (no query beyond the row the credential check already read);
//   - older, but within CAESIUM_AUTH_GROUP_MAX_STALENESS: the request proceeds
//     with the stored groups and a refresh is queued to a bounded worker pool,
//     single-flight per user on this node, claimed across nodes by the lease;
//   - past the maximum staleness, whatever the reason (no grant, repeated
//     transient failures, a node that never got to refresh): the request
//     waits for one synchronous refresh bounded by
//     CAESIUM_AUTH_GROUP_REFRESH_TIMEOUT. If it fails, the refusal is
//     persisted on the user (users.stale_refused_at), and every credential of
//     the user is refused with stale_groups on every node until a successful
//     login clears it.
//
// Outcomes are recorded under the lease fence: success stores the groups (and
// a rotated grant) by compare-and-swap, so an outcome for a grant a login has
// since replaced is discarded; a definitive IdP refusal revokes the user's
// sessions and user-bound keys in one transaction; a transient failure keeps
// everything for the next attempt. Grants, handles and tokens are never
// logged.

const (
	defaultGroupRefreshWorkers   = 4
	defaultGroupRefreshQueueSize = 256
	// asyncRefreshBackoffCap caps the wait after a failed asynchronous
	// refresh before the next one for the same user (min(interval, cap)), so
	// an IdP outage is not hammered at request rate. It never delays the
	// synchronous attempt past the maximum staleness.
	asyncRefreshBackoffCap = 30 * time.Second
	// staleLeasePoll is how often a synchronous refresh that found another
	// holder's live lease re-reads the row.
	staleLeasePoll = 250 * time.Millisecond
	// groupRefreshActor is the audit actor of refresh outcomes.
	groupRefreshActor = "system:group-refresh"
)

// Outcome labels of caesium_auth_group_refresh_total.
const (
	refreshOutcomeSuccess       = "success"
	refreshOutcomeGroupsUnknown = "groups_unknown"
	refreshOutcomeTransient     = "transient"
	refreshOutcomeUncertain     = "uncertain"
	refreshOutcomeDefinitive    = "definitive"
	refreshOutcomeDiscardedCAS  = "discarded_cas"
	refreshOutcomeUnusableGrant = "unusable_grant"
)

// GroupFreshness admits the user behind a session or a user-bound key. It
// returns the user to resolve grants from, and synced=true when it waited for
// a synchronous refresh (whatever its result): the caller must then first
// re-validate its credential on fresh rows (a refresh can revoke it, and an
// admin can revoke it meanwhile) and only then act on err. Admit itself
// refuses a user disabled meanwhile.
type GroupFreshness interface {
	Admit(ctx context.Context, user *models.User) (admitted *models.User, synced bool, err error)
}

// GroupRefreshConfig configures GroupRefreshService.
type GroupRefreshConfig struct {
	// Interval is CAESIUM_AUTH_GROUP_REFRESH_INTERVAL.
	Interval time.Duration
	// MaxStaleness is CAESIUM_AUTH_GROUP_MAX_STALENESS.
	MaxStaleness time.Duration
	// Timeout is CAESIUM_AUTH_GROUP_REFRESH_TIMEOUT: the bound of one
	// redemption (claim included). The lease is held for twice as long, so a
	// redemption always ends before its lease does.
	Timeout time.Duration
	// Node names this node in lease owners.
	Node string
	// Workers and QueueSize bound the asynchronous pool (defaults 4 and 256).
	Workers   int
	QueueSize int
}

func (c GroupRefreshConfig) validate() error {
	switch {
	case c.Interval <= 0:
		return errors.New("group refresh interval must be positive")
	case c.MaxStaleness < c.Interval:
		return errors.New("group max staleness must be at least the refresh interval")
	case c.Timeout <= 0:
		return errors.New("group refresh timeout must be positive")
	}
	return nil
}

// GroupRefreshService implements GroupFreshness. Construct it with
// NewGroupRefreshService; its workers live as long as the context given there.
type GroupRefreshService struct {
	db        *gorm.DB
	users     *UserStore
	providers map[string]GroupRefresher
	auditor   *AuditLogger
	cfg       GroupRefreshConfig
	now       func() time.Time
	ctx       context.Context

	queue chan refreshJob
	wg    sync.WaitGroup

	mu         sync.Mutex
	closed     bool
	flights    map[uuid.UUID]*refreshFlight
	retryAfter map[uuid.UUID]time.Time

	grantSecret string
	// beforeOutcome, when set (tests only), runs after the IdP answered and
	// before the outcome is recorded.
	beforeOutcome func(userID uuid.UUID)
	// beforeEnqueue, when set (tests only), runs after an asynchronous
	// flight is published and before it is queued.
	beforeEnqueue func(userID uuid.UUID)
}

var _ GroupFreshness = (*GroupRefreshService)(nil)

// GroupRefreshOption customizes GroupRefreshService.
type GroupRefreshOption func(*GroupRefreshService)

// WithGroupRefreshers registers the providers that can redeem grants, keyed
// by Name() (a grant names its provider).
func WithGroupRefreshers(refreshers ...GroupRefresher) GroupRefreshOption {
	return func(s *GroupRefreshService) {
		for _, r := range refreshers {
			if r != nil {
				s.providers[r.Name()] = r
			}
		}
	}
}

// WithGroupRefreshAuditor records auth.groups_refreshed and
// auth.sessions_revoked rows.
func WithGroupRefreshAuditor(auditor *AuditLogger) GroupRefreshOption {
	return func(s *GroupRefreshService) { s.auditor = auditor }
}

// WithGroupRefreshGrantSecret is CAESIUM_AUTH_KEY_HASH_SECRET, which seals the
// grants.
func WithGroupRefreshGrantSecret(secret string) GroupRefreshOption {
	return func(s *GroupRefreshService) { s.grantSecret = secret }
}

// WithGroupRefreshNow overrides the clock. Intended for tests.
func WithGroupRefreshNow(now func() time.Time) GroupRefreshOption {
	return func(s *GroupRefreshService) {
		if now != nil {
			s.now = now
		}
	}
}

// NewGroupRefreshService validates cfg and starts the asynchronous workers,
// which stop when ctx ends.
func NewGroupRefreshService(ctx context.Context, db *gorm.DB, cfg GroupRefreshConfig, opts ...GroupRefreshOption) (*GroupRefreshService, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.Workers <= 0 {
		cfg.Workers = defaultGroupRefreshWorkers
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultGroupRefreshQueueSize
	}
	s := &GroupRefreshService{
		db:         db,
		providers:  map[string]GroupRefresher{},
		cfg:        cfg,
		now:        time.Now,
		ctx:        ctx,
		flights:    map[uuid.UUID]*refreshFlight{},
		retryAfter: map[uuid.UUID]time.Time{},
	}
	for _, opt := range opts {
		opt(s)
	}
	s.users = NewUserStore(db, WithRefreshGrantSecret(s.grantSecret), WithUserStoreNow(s.now))
	s.queue = make(chan refreshJob, cfg.QueueSize)
	for range cfg.Workers {
		s.wg.Add(1)
		go s.work()
	}
	return s, nil
}

// Wait blocks until every worker and every running refresh has stopped
// (after the context ended). Intended for tests and shutdown.
func (s *GroupRefreshService) Wait() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.wg.Wait()
}

// goTracked runs fn on its own goroutine, joined by Wait. Once Wait has
// begun nothing new is tracked (a WaitGroup must not grow during Wait):
// fn then runs untracked when untrackedOK, and not at all otherwise.
func (s *GroupRefreshService) goTracked(fn func(), untrackedOK bool) {
	s.mu.Lock()
	tracked := !s.closed
	if tracked {
		s.wg.Add(1)
	}
	s.mu.Unlock()
	switch {
	case tracked:
		go func() {
			defer s.wg.Done()
			fn()
		}()
	case untrackedOK:
		go fn()
	}
}

// refreshFlight is one refresh attempt for one user on this node. Requests
// past the maximum staleness that arrive while it runs join it.
type refreshFlight struct {
	done chan struct{}
	// sync marks a flight a request past the maximum staleness waits for:
	// its failure persists the stale_groups refusal.
	sync atomic.Bool
	// started is taken by whoever runs the flight (a worker or a joining
	// synchronous request), exactly once.
	started atomic.Bool
	// concluded is set before done closes: the flight ran synchronously to
	// a conclusion (fresh groups, or the refusal decided).
	concluded bool
}

type refreshJob struct {
	userID uuid.UUID
	flight *refreshFlight
}

type groupAge uint8

const (
	groupsFresh groupAge = iota
	groupsDue
	groupsStale
)

// age classifies the user's groups at now. Groups are confirmed at login or
// by a refresh (GroupsRefreshedAt); a user from before E1 has none, and its
// groups were confirmed at its last login. A user with neither is stale.
func (s *GroupRefreshService) age(u *models.User, now time.Time) groupAge {
	confirmed := u.GroupsRefreshedAt
	if confirmed == nil {
		confirmed = u.LastLoginAt
	}
	if confirmed == nil {
		return groupsStale
	}
	elapsed := now.Sub(*confirmed)
	switch {
	case elapsed <= s.cfg.Interval:
		return groupsFresh
	case elapsed <= s.cfg.MaxStaleness:
		return groupsDue
	default:
		return groupsStale
	}
}

// Admit implements GroupFreshness.
func (s *GroupRefreshService) Admit(ctx context.Context, user *models.User) (*models.User, bool, error) {
	if user == nil {
		return nil, false, ErrUserNotFound
	}
	if user.StaleRefusedAt != nil {
		return nil, false, ErrStaleGroups
	}
	now := s.now().UTC()
	switch s.age(user, now) {
	case groupsFresh:
		return user, false, nil
	case groupsDue:
		s.scheduleAsync(user, now)
		return user, false, nil
	}
	admitted, completed, err := s.refreshPastBound(ctx, user.ID)
	if err != nil {
		// When the refresh completed, the caller re-validates its
		// credential first (synced), so a credential a definitive refusal
		// revoked meanwhile is answered as revoked, not as stale. A wait
		// that timed out is refused as stale without another read.
		return nil, completed, err
	}
	return admitted, true, nil
}

// refreshPastBound waits for one synchronous refresh of a user past the
// maximum staleness and re-reads the user; it reports whether the refresh
// completed within the timeout. A flight that ended without a
// conclusion (it was an asynchronous flight that had already passed the
// point where it could refuse, or a login replaced the grant meanwhile) is
// followed by one more synchronous flight within the same deadline.
func (s *GroupRefreshService) refreshPastBound(ctx context.Context, userID uuid.UUID) (*models.User, bool, error) {
	// One deadline for the whole wait, retry and re-read included: the
	// request is held at most CAESIUM_AUTH_GROUP_REFRESH_TIMEOUT. The flight
	// itself runs on the service's context and records its outcome whether
	// or not the request is still waiting.
	wait, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	for range 2 {
		flight := s.joinSync(userID)
		select {
		case <-flight.done:
		case <-wait.Done():
			return nil, false, s.waitExpired(ctx, userID)
		}
		row, err := s.loadUser(wait, userID)
		if err != nil {
			if wait.Err() != nil {
				return nil, false, s.waitExpired(ctx, userID)
			}
			return nil, false, err
		}
		if row.IsDisabled() {
			return nil, true, ErrUserDisabled
		}
		if row.StaleRefusedAt != nil {
			return nil, true, ErrStaleGroups
		}
		if s.age(row, s.now().UTC()) != groupsStale {
			return row, true, nil
		}
		// flight.done was observed: concluded is safe to read.
		if flight.concluded {
			return nil, true, ErrStaleGroups
		}
	}
	// Both flights ended without a conclusion (say the catalog kept
	// refusing the lease claim) and the groups are still stale: that was
	// this request's one synchronous attempt, and it failed. Persist the
	// refusal (fenced, only while still stale) before answering, within
	// what is left of the wait, so a later refresh cannot quietly restore
	// access; only a login lifts it. A stopping process never refuses.
	if s.ctx.Err() == nil {
		refused := make(chan struct{})
		s.goTracked(func() {
			defer close(refused)
			s.refuseIfStale(userID)
		}, false)
		select {
		case <-refused:
		case <-wait.Done():
		}
	}
	return nil, true, ErrStaleGroups
}

// waitExpired answers a request whose synchronous refresh did not complete
// within the timeout. A request that went away gets its context's error.
// Otherwise the timeout is this request's failed attempt: the refusal is
// persisted in the background (fenced as always, and only while the groups
// are still stale, so a refresh that already landed wins), and a refresh
// that succeeds later still stores its outcome, a rotated grant included,
// but does not lift the refusal; only a login does.
func (s *GroupRefreshService) waitExpired(ctx context.Context, userID uuid.UUID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.ctx.Err() == nil {
		s.goTracked(func() { s.refuseIfStale(userID) }, false)
	}
	return ErrStaleGroups
}

// joinSync joins this user's flight, marking it synchronous, or starts one.
// A queued asynchronous flight nobody runs yet is run here, now.
func (s *GroupRefreshService) joinSync(userID uuid.UUID) *refreshFlight {
	s.mu.Lock()
	flight, ok := s.flights[userID]
	if !ok {
		flight = &refreshFlight{done: make(chan struct{})}
		s.flights[userID] = flight
	}
	flight.sync.Store(true)
	s.mu.Unlock()
	if flight.started.CompareAndSwap(false, true) {
		// A flight started while the service stops still runs (its
		// waiter needs it to finish); it just is not joined.
		s.goTracked(func() { s.runFlight(userID, flight) }, true)
	}
	return flight
}

// scheduleAsync queues a refresh for a user whose groups are due, unless one
// is running, the user has no grant or a live lease, or a recent failure
// asks to wait. A full queue drops the request; the next one retries.
func (s *GroupRefreshService) scheduleAsync(user *models.User, now time.Time) {
	if len(user.RefreshGrant) == 0 {
		return
	}
	if user.RefreshLeaseUntil != nil && user.RefreshLeaseUntil.After(now) {
		return
	}
	s.mu.Lock()
	if _, running := s.flights[user.ID]; running {
		s.mu.Unlock()
		return
	}
	if until, ok := s.retryAfter[user.ID]; ok {
		if now.Before(until) {
			s.mu.Unlock()
			return
		}
		delete(s.retryAfter, user.ID)
	}
	flight := &refreshFlight{done: make(chan struct{})}
	s.flights[user.ID] = flight
	s.mu.Unlock()
	if s.beforeEnqueue != nil {
		s.beforeEnqueue(user.ID)
	}

	select {
	case s.queue <- refreshJob{userID: user.ID, flight: flight}:
	default:
		// The queue is full. A synchronous request may already have joined
		// and started this flight; only whoever takes started runs and
		// finishes it.
		if flight.started.CompareAndSwap(false, true) {
			s.finish(user.ID, flight)
		}
	}
}

func (s *GroupRefreshService) work() {
	defer s.wg.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case job := <-s.queue:
			if job.flight.started.CompareAndSwap(false, true) {
				s.runFlight(job.userID, job.flight)
			}
		}
	}
}

// finish removes the flight and wakes its waiters.
func (s *GroupRefreshService) finish(userID uuid.UUID, flight *refreshFlight) {
	s.mu.Lock()
	if s.flights[userID] == flight {
		delete(s.flights, userID)
	}
	s.mu.Unlock()
	close(flight.done)
}

// runFlight performs one attempt on the service's context, never a
// request's: a waiter that leaves does not cancel the redemption or the
// recording of its outcome.
func (s *GroupRefreshService) runFlight(userID uuid.UUID, flight *refreshFlight) {
	defer s.finish(userID, flight)
	succeeded, concluded := s.attempt(userID, flight)
	if succeeded {
		return
	}
	if flight.sync.Load() {
		// A failure caused by this process shutting down says nothing about
		// the IdP or the user: it never persists a refusal.
		if concluded && s.ctx.Err() == nil {
			s.refuseIfStale(userID)
			flight.concluded = true
		}
		return
	}
	backoff := min(s.cfg.Interval, asyncRefreshBackoffCap)
	s.mu.Lock()
	s.retryAfter[userID] = s.now().UTC().Add(backoff)
	if len(s.retryAfter) > 4096 {
		now := s.now().UTC()
		for id, until := range s.retryAfter {
			if now.After(until) {
				delete(s.retryAfter, id)
			}
		}
	}
	s.mu.Unlock()
}

// attempt claims the lease, redeems the grant and records the outcome. It
// reports whether the groups were refreshed, and whether the attempt reached
// a conclusion a refusal may rest on (false when another holder kept the
// lease, or a login replaced the grant meanwhile).
func (s *GroupRefreshService) attempt(userID uuid.UUID, flight *refreshFlight) (succeeded, concluded bool) {
	ctx, cancel := context.WithTimeout(s.ctx, s.attemptBudget())
	defer cancel()
	row, err := s.loadUser(ctx, userID)
	if err != nil {
		log.Warn("group refresh could not read the user", "user_id", userID.String(), "error", err)
		return false, false
	}
	if row.IsDisabled() || row.StaleRefusedAt != nil {
		return false, false
	}
	if s.age(row, s.now().UTC()) == groupsFresh {
		// A login (or another node's refresh) confirmed the groups after
		// this flight was queued. Fresh groups are never re-read: redeeming
		// the login's new grant here could answer a definitive refusal that
		// revokes credentials while that login is still minting its
		// session, which the revocation would then miss. A flight that read
		// the row before the login is fenced by the version the login bumps.
		return true, true
	}
	if len(row.RefreshGrant) == 0 {
		// Nothing to redeem (SAML, a cleared or unopenable grant): only a
		// login confirms these groups again.
		return false, true
	}
	owner := NewRefreshLeaseOwner(s.cfg.Node)
	version, claimed, err := s.claim(ctx, row, owner, flight)
	if err != nil {
		log.Warn("group refresh could not claim the lease", "user_id", userID.String(), "error", err)
		return false, false
	}
	if !claimed {
		// A synchronous flight that waited out its whole budget behind
		// another holder's lease failed its one attempt: that concludes
		// it (the refusal is fenced on the version it reads, so a
		// success the other holder records first still wins).
		return false, flight.sync.Load() && errors.Is(ctx.Err(), context.DeadlineExceeded) && s.ctx.Err() == nil
	}
	return s.redeem(ctx, row, version, owner, flight)
}

// claim takes the user's refresh lease. An asynchronous flight gives up on
// a held lease; a synchronous one waits (re-reading the row) for the other
// holder until its own deadline, and stops as soon as the row no longer
// needs it.
func (s *GroupRefreshService) claim(ctx context.Context, row *models.User, owner string, flight *refreshFlight) (int64, bool, error) {
	for {
		claimed, err := s.users.ClaimRefreshLease(ctx, row.ID, row.RefreshGrantVersion, owner, s.now(), 2*s.cfg.Timeout)
		if err != nil || claimed {
			return row.RefreshGrantVersion, claimed, err
		}
		if !flight.sync.Load() {
			return 0, false, nil
		}
		select {
		case <-ctx.Done():
			return 0, false, nil
		case <-time.After(staleLeasePoll):
		}
		next, err := s.loadUser(ctx, row.ID)
		if err != nil {
			return 0, false, err
		}
		if next.IsDisabled() || next.StaleRefusedAt != nil || len(next.RefreshGrant) == 0 ||
			s.age(next, s.now().UTC()) != groupsStale {
			return 0, false, nil
		}
		row = next
	}
}

// redeem runs the provider under the claimed lease and records the outcome
// on a bounded context of its own, so the outcome (a rotated grant
// included) is stored even when the redemption used up its deadline.
func (s *GroupRefreshService) redeem(ctx context.Context, row *models.User, version int64, owner string, flight *refreshFlight) (succeeded, concluded bool) {
	userID := row.ID
	persist, cancel := s.persistContext()
	defer func() { cancel() }()

	req, err := s.users.LoadLeasedRefreshGrant(ctx, userID, version, owner, s.now())
	switch {
	case errors.Is(err, ErrRefreshLeaseLost):
		return false, false
	case errors.Is(err, ErrRefreshGrantInvalid):
		applied, cerr := s.users.CompleteRefreshDiscard(persist, userID, version, owner)
		s.count(providerOther, s.casOutcome(applied, cerr, refreshOutcomeUnusableGrant))
		log.Warn("discarded a refresh grant that no longer opens; the user must sign in again once the staleness bound passes", "user_id", userID.String())
		return false, applied
	case err != nil:
		applied, _ := s.users.CompleteRefreshTransient(persist, userID, version, owner)
		s.count(providerOther, s.casOutcome(applied, nil, refreshOutcomeTransient))
		log.Warn("group refresh could not load the grant", "user_id", userID.String(), "error", err)
		return false, applied
	}

	label := refreshProviderLabel(req.Grant.Provider)
	provider := s.providers[req.Grant.Provider]
	if provider == nil {
		applied, cerr := s.users.CompleteRefreshTransient(persist, userID, version, owner)
		s.count(label, s.casOutcome(applied, cerr, refreshOutcomeTransient))
		log.Warn("no configured provider can refresh this user's grant", "user_id", userID.String(), "provider", label)
		return false, applied
	}

	result, rerr := provider.RefreshGroups(ctx, req)
	if s.beforeOutcome != nil {
		s.beforeOutcome(userID)
	}
	// The outcome gets a full persistence budget of its own from here: an
	// answer (perhaps a rotated grant) that arrived as the redemption's
	// deadline expired must still be stored.
	cancel()
	persist, cancel = s.persistContext()
	now := s.now().UTC()
	trigger := "async"
	if flight.sync.Load() {
		trigger = "sync"
	}

	switch ClassifyRefreshError(rerr) {
	case 0:
		var rotated *RefreshGrant
		if result.Rotated {
			rotated = &RefreshGrant{Provider: req.Grant.Provider, Issuer: req.Grant.Issuer, Handle: result.Handle, Login: req.Grant.Login}
		}
		if !result.GroupsKnown {
			applied, cerr := s.users.CompleteRefreshGroupsUnknown(persist, userID, version, owner, rotated)
			s.count(label, s.casOutcome(applied, cerr, refreshOutcomeGroupsUnknown))
			return false, applied
		}
		applied, cerr := s.users.CompleteRefreshSuccess(persist, userID, version, owner, result.Groups, rotated, now)
		s.count(label, s.casOutcome(applied, cerr, refreshOutcomeSuccess))
		if cerr != nil {
			log.Warn("group refresh could not record its result", "user_id", userID.String(), "provider", label, "error", cerr)
		}
		if !applied {
			return false, false
		}
		s.auditGroupsChanged(persist, row, result.Groups, label, trigger)
		return true, true

	case RefreshTransient:
		applied, cerr := s.users.CompleteRefreshTransient(persist, userID, version, owner)
		s.count(label, s.casOutcome(applied, cerr, refreshOutcomeTransient))
		log.Info("group refresh failed transiently", "user_id", userID.String(), "provider", label, "reason", refreshReason(rerr))
		return false, applied

	case RefreshUncertain:
		applied, cerr := s.users.CompleteRefreshUncertain(persist, userID, version, owner)
		s.count(label, s.casOutcome(applied, cerr, refreshOutcomeUncertain))
		log.Info("group refresh outcome is uncertain", "user_id", userID.String(), "provider", label, "reason", refreshReason(rerr))
		return false, applied

	default: // RefreshDefinitive
		scope := DenialOfGrant
		if refreshErr, ok := errors.AsType[*RefreshError](rerr); ok {
			scope = refreshErr.Scope
		}
		res, cerr := s.users.CompleteRefreshDefinitive(persist, userID, version, owner, scope, now)
		applied := cerr == nil && res.Outcome != DefinitiveDiscarded
		s.count(label, s.casOutcome(applied, cerr, refreshOutcomeDefinitive))
		if cerr != nil {
			log.Warn("group refresh could not record a definitive refusal", "user_id", userID.String(), "provider", label, "error", cerr)
			return false, false
		}
		log.Info("identity provider refused a group refresh", "user_id", userID.String(), "provider", label,
			"reason", refreshReason(rerr), "revoked", res.Outcome == DefinitiveRevoked)
		if res.Outcome == DefinitiveRevoked {
			s.auditRevoked(persist, row, label, res)
		}
		return false, applied
	}
}

// attemptBudget bounds claiming the lease and redeeming the grant: the
// timeout less a recording allowance (a fifth of it, at most a second), so
// a synchronous refresh normally completes, outcome recorded, within the
// timeout the waiting request is held for.
func (s *GroupRefreshService) attemptBudget() time.Duration {
	return s.cfg.Timeout - min(s.cfg.Timeout/5, time.Second)
}

// persistContext bounds the recording of an outcome. It derives from the
// service's context without its cancellation: an outcome already obtained
// from the IdP is recorded even while the process shuts down.
func (s *GroupRefreshService) persistContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(s.ctx), s.cfg.Timeout)
}

// refuseIfStale persists the stale_groups refusal after a failed synchronous
// attempt, fenced on the grant version it reads: a login (or a successful
// refresh on another node) since wins.
func (s *GroupRefreshService) refuseIfStale(userID uuid.UUID) {
	ctx, cancel := s.persistContext()
	defer cancel()
	row, err := s.loadUser(ctx, userID)
	if err != nil {
		log.Warn("group refresh could not re-read the user to refuse it", "user_id", userID.String(), "error", err)
		return
	}
	now := s.now().UTC()
	if row.IsDisabled() || row.StaleRefusedAt != nil || s.age(row, now) != groupsStale {
		return
	}
	marked, err := s.users.MarkStaleRefused(ctx, userID, row.RefreshGrantVersion, now)
	if err != nil {
		log.Warn("group refresh could not persist the stale_groups refusal", "user_id", userID.String(), "error", err)
		return
	}
	if marked {
		log.Info("user's groups are past the maximum staleness and could not be refreshed; refusing its credentials until it signs in again", "user_id", userID.String())
	}
}

func (s *GroupRefreshService) loadUser(ctx context.Context, userID uuid.UUID) (*models.User, error) {
	var row models.User
	err := s.db.WithContext(ctx).Where("id = ?", userID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load user: %w", err)
	}
	return &row, nil
}

// casOutcome turns a lost compare-and-swap (or a failed write) into
// discarded_cas.
func (s *GroupRefreshService) casOutcome(applied bool, err error, outcome string) string {
	if err != nil || !applied {
		return refreshOutcomeDiscardedCAS
	}
	return outcome
}

const providerOther = "other"

// refreshProviderLabel bounds the provider label: a grant's provider name
// comes from a decrypted row and is never used as a label verbatim.
func refreshProviderLabel(name string) string {
	switch name {
	case "oidc", "ldap":
		return name
	default:
		return providerOther
	}
}

func (s *GroupRefreshService) count(provider, outcome string) {
	metrics.AuthGroupRefreshTotal.WithLabelValues(provider, outcome).Inc()
}

// refreshReason is the provider's fixed, token-free reason code.
func refreshReason(err error) string {
	if refreshErr, ok := errors.AsType[*RefreshError](err); ok && refreshErr.Reason != "" {
		return refreshErr.Reason
	}
	return "unclassified"
}

// auditGroupsChanged writes auth.groups_refreshed when the stored set changed.
// Both audit writes are bounded by the outcome's persistence context, so a
// stuck audit insert never holds a flight (or shutdown) past that budget.
func (s *GroupRefreshService) auditGroupsChanged(ctx context.Context, before *models.User, groups []string, provider, trigger string) {
	if s.auditor == nil {
		return
	}
	var old []string
	if len(before.Groups) > 0 {
		if err := json.Unmarshal(before.Groups, &old); err != nil {
			old = nil
		}
	}
	added, removed := groupDelta(old, groups)
	if len(added) == 0 && len(removed) == 0 {
		return
	}
	if err := s.auditor.LogContext(ctx, AuditEntry{
		Actor:        groupRefreshActor,
		Action:       ActionAuthGroupsRefreshed,
		ResourceType: "user",
		ResourceID:   before.ID.String(),
		Outcome:      OutcomeSuccess,
		Metadata: map[string]any{
			"provider": provider,
			"added":    added,
			"removed":  removed,
			"trigger":  trigger,
		},
		Namespace: models.ClusterNamespace,
	}); err != nil {
		log.Warn("failed to write audit log", "error", err)
	}
}

// auditRevoked writes auth.sessions_revoked (reason idp_refresh_denied) after
// a definitive refusal revoked the user's credentials.
func (s *GroupRefreshService) auditRevoked(ctx context.Context, user *models.User, provider string, res DefinitiveResult) {
	if s.auditor == nil || (res.SessionsRevoked <= 0 && res.KeysRevoked <= 0) {
		return
	}
	metadata := map[string]any{
		"reason":   SessionsRevokedReasonIdPRefreshDenied,
		"count":    res.SessionsRevoked,
		"provider": provider,
	}
	if res.KeysRevoked > 0 {
		metadata["keys_revoked"] = res.KeysRevoked
	}
	if err := s.auditor.LogContext(ctx, AuditEntry{
		Actor:        groupRefreshActor,
		Action:       ActionSessionsRevoked,
		ResourceType: "user",
		ResourceID:   user.ID.String(),
		Outcome:      OutcomeSuccess,
		Metadata:     metadata,
		Namespace:    models.ClusterNamespace,
	}); err != nil {
		log.Warn("failed to write audit log", "error", err)
	}
}

// groupDelta returns the groups only in next (added) and only in prev
// (removed), each sorted and de-duplicated; never nil.
func groupDelta(prev, next []string) (added, removed []string) {
	added, removed = []string{}, []string{}
	for _, g := range next {
		if !slices.Contains(prev, g) && !slices.Contains(added, g) {
			added = append(added, g)
		}
	}
	for _, g := range prev {
		if !slices.Contains(next, g) && !slices.Contains(removed, g) {
			removed = append(removed, g)
		}
	}
	slices.Sort(added)
	slices.Sort(removed)
	return added, removed
}
