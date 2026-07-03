package incident

import (
	"context"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"gorm.io/gorm"
)

// TimerFunc handles a fired durable timer. Stream B registers the concrete
// snooze_retry handler; in Phase 0 with no handler registered the sweeper simply
// records the timer as fired.
type TimerFunc func(ctx context.Context, timer models.RemediationTimer) error

// TimerSupervisor is the leader-gated durable-timer sweeper. It periodically
// fires due RemediationTimer rows so a pending snooze/retry survives
// restart/failover (no in-process time.NewTimer is the sole record). Timers
// whose owning incident has reached a terminal state are never fired — they are
// cancelled at the transition, and the sweeper double-checks before firing.
type TimerSupervisor struct {
	db          *gorm.DB
	store       *Store
	leaderCheck LeaderCheck
	interval    time.Duration
	batch       int
	handlers    map[string]TimerFunc
}

// NewTimerSupervisor constructs the sweeper. A zero interval defaults to 5s.
func NewTimerSupervisor(db *gorm.DB, leaderCheck LeaderCheck, interval time.Duration) *TimerSupervisor {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &TimerSupervisor{
		db:          db,
		store:       NewStore(db),
		leaderCheck: leaderCheck,
		interval:    interval,
		batch:       100,
		handlers:    make(map[string]TimerFunc),
	}
}

// RegisterHandler registers the handler invoked when a timer of the given kind
// fires.
func (t *TimerSupervisor) RegisterHandler(kind string, fn TimerFunc) {
	t.handlers[kind] = fn
}

// Run drives the sweep loop until ctx is cancelled.
func (t *TimerSupervisor) Run(ctx context.Context) {
	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := t.SweepOnce(ctx); err != nil && ctx.Err() == nil {
				log.Error("incident timer sweep failed", "error", err)
			}
		}
	}
}

// SweepOnce fires all due timers once. It is leader-gated so an N-node cluster
// fires each timer exactly once.
func (t *TimerSupervisor) SweepOnce(ctx context.Context) error {
	if t.leaderCheck != nil {
		leader, err := t.leaderCheck(ctx)
		if err != nil {
			return err
		}
		if !leader {
			return nil
		}
	}

	due, err := t.store.DueTimers(ctx, time.Now().UTC(), t.batch)
	if err != nil {
		return err
	}
	for i := range due {
		t.fire(ctx, due[i])
	}
	return nil
}

// fire claims and dispatches a single due timer. A timer whose owning incident
// is terminal is cancelled rather than fired.
func (t *TimerSupervisor) fire(ctx context.Context, timer models.RemediationTimer) {
	// Double-check the owning incident is not terminal before firing (guards the
	// close-races-with-fire window).
	inc, err := t.store.Get(ctx, timer.IncidentID)
	if err != nil {
		log.Warn("incident timer: owning incident lookup failed; skipping", "timer_id", timer.ID, "error", err)
		return
	}
	if inc.Status.IsTerminal() {
		if _, cerr := t.store.CancelTimersForIncident(ctx, timer.IncidentID); cerr != nil {
			log.Warn("incident timer: failed to cancel timer for terminal incident", "timer_id", timer.ID, "error", cerr)
		}
		return
	}

	claimed, err := t.store.ClaimTimer(ctx, timer.ID)
	if err != nil {
		log.Warn("incident timer: claim failed", "timer_id", timer.ID, "error", err)
		return
	}
	if !claimed {
		// Another sweep or a cancellation won the race.
		return
	}

	handler, ok := t.handlers[timer.Kind]
	if !ok {
		// Phase 0: no handler registered (Stream B provides them). The timer is
		// already recorded as fired; nothing else to do.
		log.Info("incident timer fired (no handler registered)", "timer_id", timer.ID, "kind", timer.Kind, "incident_id", timer.IncidentID)
		return
	}
	if err := handler(ctx, timer); err != nil {
		log.Error("incident timer handler failed", "timer_id", timer.ID, "kind", timer.Kind, "error", err)
	}
}
