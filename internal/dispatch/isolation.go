package dispatch

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	jobdefruntime "github.com/caesium-cloud/caesium/internal/jobdef/runtime"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
)

// Isolation context in dispatch (identity-and-access design §10.1, plan C2).
//
// An owner serving a policy file resolves the run namespace's isolation
// context for every dispatch and carries it in DispatchRequest.Isolation. A
// task whose context is non-default (a Kubernetes mapping, any secret rule, or
// a namespace the file does not declare) goes only to a peer that advertises
// CapabilityIsolationV1, and travels with a tagged WorkerNode that a worker
// built before the context refuses before claiming. A peer that answers the
// capability probe without it is skipped and counted
// (caesium_dispatch_incompatible_worker_total); with no capable peer the task
// waits. An owner serving no policy file sends no context and probes nothing:
// its dispatches are byte-identical to before.

// runIsolationReader is the optional store capability that names a run's
// persisted namespace (and whether it is a quarantined replay). *run.Store
// implements it; a test's minimal stub store does not, and is then only usable
// without a policy file.
type runIsolationReader interface {
	RunIsolationInfo(ctx context.Context, runID uuid.UUID) (string, bool, error)
}

// isolationStamper is the optional store capability that marks a run's
// unfinished rows as requiring isolation (run.StampIsolationRequired).
type isolationStamper interface {
	StampIsolationRequired(ctx context.Context, runID uuid.UUID, value string) (int, error)
}

// isolationRequirementReader is the optional store capability that tells a
// node serving no policy file whether a run it owns was isolated by a
// policy-serving owner (run.RunRequiresIsolation).
type isolationRequirementReader interface {
	RunRequiresIsolation(ctx context.Context, runID uuid.UUID) (bool, error)
}

// runIsolation caches what never changes for a run (its namespace and
// quarantine flag) plus the policy revision its rows were last stamped under.
type runIsolation struct {
	namespace       string
	quarantine      bool
	stamped         bool
	stampedRevision string
	// requirementChecked/requiresIsolation cache, for a node serving no
	// policy file, whether the run's rows carry the isolation selector. A
	// positive answer is kept; a negative one only for
	// isolationRequirementRecheck, since a policy-serving node's isolation
	// fence can mark the run's rows later.
	requirementChecked   bool
	requiresIsolation    bool
	requirementCheckedAt time.Time
}

// isolationRequirementRecheck is how long a policyless owner trusts that a
// run's rows carry no isolation mark before reading them again.
var isolationRequirementRecheck = 2 * time.Second

// peerCapability is one cached capability probe.
type peerCapability struct {
	// answered: the peer answered the probe (an unreachable peer is skipped
	// without counting it as incompatible).
	answered  bool
	isolation bool
	expires   time.Time
}

const (
	// capabilityPositiveTTL bounds how long a capable answer is trusted. A
	// stale positive is harmless: the tagged envelope makes a downgraded peer
	// refuse with wrong_worker, which also evicts the entry.
	capabilityPositiveTTL = 60 * time.Second
	// capabilityNegativeTTL is short so an upgraded (or restarted) peer, or
	// one whose listener was not up yet, is picked up within seconds.
	capabilityNegativeTTL = 5 * time.Second
	// isolationWaitWarnInterval rate-limits the "no capable peer" warning per
	// task.
	isolationWaitWarnInterval = time.Minute
)

// isolationState is the DispatchLoop's isolation bookkeeping.
type isolationState struct {
	mu     sync.Mutex
	runs   map[uuid.UUID]*runIsolation
	caps   map[string]peerCapability
	warned map[uuid.UUID]map[uuid.UUID]time.Time
}

func newIsolationState() *isolationState {
	return &isolationState{
		runs:   make(map[uuid.UUID]*runIsolation),
		caps:   make(map[string]peerCapability),
		warned: make(map[uuid.UUID]map[uuid.UUID]time.Time),
	}
}

// policyFileServed reports whether this owner sends isolation contexts, and
// ready=false while a configured policy file is not installed yet.
func policyFileServed() (served, ready bool) {
	holder := policy.ProcessHolder()
	if holder == nil {
		configured := strings.TrimSpace(env.Variables().AuthPolicyFile) != ""
		return configured, !configured
	}
	return holder.FileBacked(), true
}

// runIsolationContext resolves the run's isolation context for this tick's
// dispatches, from one policy revision. ok=false means none of the run's
// tasks may be dispatched this tick (the policy is not loaded yet, or the
// run's namespace cannot be read): the run waits rather than dispatching
// without its context. A nil context with ok=true is "no policy file".
func (l *DispatchLoop) runIsolationContext(ctx context.Context, runID uuid.UUID) (iso *jobdefruntime.IsolationContext, quarantine, ok bool) {
	served, ready := policyFileServed()
	if !ready {
		return nil, false, false
	}
	if !served {
		// A run this node took over from a policy-serving owner (owner-memory
		// lease takeover) keeps its isolation requirement on its rows. This
		// node can neither resolve the context nor make a worker honour one,
		// so it holds the run instead of dispatching it unisolated.
		if l.runRequiresIsolation(ctx, runID) {
			return nil, false, false
		}
		return nil, false, true
	}
	info, err := l.runIsolationInfo(ctx, runID)
	if err != nil {
		if ctx.Err() == nil {
			log.Warn("dispatch loop: cannot read the run's namespace; holding its tasks", "run_id", runID, "error", err)
		}
		return nil, false, false
	}
	resolved, carry, ready := jobdefruntime.ResolveIsolationContext(info.namespace)
	if !ready {
		return nil, info.quarantine, false
	}
	if !carry {
		return nil, info.quarantine, true
	}
	if !resolved.Default() {
		l.stampIsolation(ctx, runID, info, resolved.PolicyRevision)
	}
	return &resolved, info.quarantine, true
}

var errNoIsolationReader = errors.New("dispatch store cannot name a run's namespace")

// runRequiresIsolation reports, for a node serving no policy file, whether the
// run's rows were isolated by a policy-serving owner. Unknown (a read error)
// counts as required: the run waits. A store without the capability (a test
// stub) is read as no requirement.
func (l *DispatchLoop) runRequiresIsolation(ctx context.Context, runID uuid.UUID) bool {
	l.iso.mu.Lock()
	if cached, ok := l.iso.runs[runID]; ok && cached.requirementChecked &&
		(cached.requiresIsolation || time.Since(cached.requirementCheckedAt) < isolationRequirementRecheck) {
		requires := cached.requiresIsolation
		l.iso.mu.Unlock()
		return requires
	}
	l.iso.mu.Unlock()
	reader, ok := l.cfg.Store.(isolationRequirementReader)
	if !ok {
		return false
	}
	requires, err := reader.RunRequiresIsolation(ctx, runID)
	if err != nil {
		if ctx.Err() == nil {
			log.Warn("dispatch loop: cannot read the run's isolation requirement; holding its tasks", "run_id", runID, "error", err)
		}
		return true
	}
	if requires {
		log.Error("dispatch loop: this node serves no access policy file but owns a run its owner isolated; holding the run (every node that can own runs must serve the policy file)", "run_id", runID)
	}
	l.iso.mu.Lock()
	entry, ok := l.iso.runs[runID]
	if !ok {
		entry = &runIsolation{}
		l.iso.runs[runID] = entry
	}
	entry.requirementChecked, entry.requiresIsolation, entry.requirementCheckedAt = true, requires, time.Now()
	l.iso.mu.Unlock()
	return requires
}

func (l *DispatchLoop) runIsolationInfo(ctx context.Context, runID uuid.UUID) (runIsolation, error) {
	l.iso.mu.Lock()
	if cached, ok := l.iso.runs[runID]; ok && cached.namespace != "" {
		info := *cached
		l.iso.mu.Unlock()
		return info, nil
	}
	l.iso.mu.Unlock()
	reader, ok := l.cfg.Store.(runIsolationReader)
	if !ok {
		return runIsolation{}, errNoIsolationReader
	}
	namespace, quarantine, err := reader.RunIsolationInfo(ctx, runID)
	if err != nil {
		return runIsolation{}, err
	}
	info := runIsolation{namespace: namespace, quarantine: quarantine}
	l.iso.mu.Lock()
	if cached, ok := l.iso.runs[runID]; ok && cached.namespace != "" {
		info = *cached
	} else if ok {
		cached.namespace, cached.quarantine = namespace, quarantine
		info = *cached
	} else {
		stored := info
		l.iso.runs[runID] = &stored
	}
	l.iso.mu.Unlock()
	return info, nil
}

// stampIsolation marks the run's unfinished rows as requiring isolation once
// per policy revision (rows registered before a reload isolated the namespace
// would otherwise stay pullable by a worker that cannot honour it).
func (l *DispatchLoop) stampIsolation(ctx context.Context, runID uuid.UUID, info runIsolation, revision string) {
	if info.stamped && info.stampedRevision == revision {
		return
	}
	stamper, ok := l.cfg.Store.(isolationStamper)
	if !ok {
		return
	}
	isolated, value := run.NamespaceIsolation(info.namespace)
	if !isolated {
		return
	}
	n, err := stamper.StampIsolationRequired(ctx, runID, value)
	if err != nil {
		if ctx.Err() == nil {
			log.Warn("dispatch loop: failed to mark the run's tasks as requiring isolation", "run_id", runID, "error", err)
		}
		return
	}
	if n > 0 {
		log.Info("dispatch loop: marked tasks as requiring an isolation-capable worker", "run_id", runID, "count", n)
	}
	l.iso.mu.Lock()
	if cached, ok := l.iso.runs[runID]; ok {
		cached.stamped = true
		cached.stampedRevision = revision
	}
	l.iso.mu.Unlock()
}

// probeCapabilities refreshes the capability cache for every peer whose entry
// is missing or expired, in parallel, each probe bounded by
// dispatchPostTimeout.
func (l *DispatchLoop) probeCapabilities(ctx context.Context, peers []peer) {
	now := time.Now()
	var stale []peer
	l.iso.mu.Lock()
	for _, p := range peers {
		if entry, ok := l.iso.caps[p.nodeID]; !ok || !now.Before(entry.expires) {
			stale = append(stale, p)
		}
	}
	l.iso.mu.Unlock()
	if len(stale) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, p := range stale {
		wg.Add(1)
		go func(p peer) {
			defer wg.Done()
			resp, err := GetCapabilities(ctx, p.baseURL+"/internal/capabilities", l.cfg.Token)
			entry := peerCapability{expires: time.Now().Add(capabilityNegativeTTL)}
			switch {
			case err != nil && errors.Is(err, ErrPeerUnreachable):
				log.Debug("dispatch loop: capability probe could not reach peer", "peer", p.nodeID, "error", err)
			case err != nil:
				// The peer answered, but not with a capability set (a build
				// without the route): it supports nothing.
				entry.answered = true
			default:
				entry.answered = true
				entry.isolation = resp.Supports(CapabilityIsolationV1)
				if entry.isolation {
					entry.expires = time.Now().Add(capabilityPositiveTTL)
				}
			}
			l.iso.mu.Lock()
			l.iso.caps[p.nodeID] = entry
			l.iso.mu.Unlock()
		}(p)
	}
	wg.Wait()
}

// peerCapabilityOf returns the cached probe for nodeID (zero when unknown).
func (l *DispatchLoop) peerCapabilityOf(nodeID string) peerCapability {
	l.iso.mu.Lock()
	defer l.iso.mu.Unlock()
	return l.iso.caps[nodeID]
}

// forgetPeerCapability drops nodeID's cached probe so the next isolated
// dispatch re-probes it.
func (l *DispatchLoop) forgetPeerCapability(nodeID string) {
	l.iso.mu.Lock()
	delete(l.iso.caps, nodeID)
	l.iso.mu.Unlock()
}

// warnNoCapablePeer logs, at most once per isolationWaitWarnInterval per task,
// that an isolated task is waiting for a capable peer.
func (l *DispatchLoop) warnNoCapablePeer(runID, execRef uuid.UUID, namespace string) {
	now := time.Now()
	l.iso.mu.Lock()
	byRun := l.iso.warned[runID]
	if byRun == nil {
		byRun = make(map[uuid.UUID]time.Time)
		l.iso.warned[runID] = byRun
	}
	last, seen := byRun[execRef]
	if seen && now.Sub(last) < isolationWaitWarnInterval {
		l.iso.mu.Unlock()
		return
	}
	byRun[execRef] = now
	l.iso.mu.Unlock()
	log.Warn("dispatch loop: no peer advertises isolation_v1; the isolated task waits for a capable worker",
		"run_id", runID, "task_ref", execRef, "namespace", namespace)
}

// forgetIsolation drops the bookkeeping of runs this node no longer owns.
func (l *DispatchLoop) forgetIsolation(owned map[uuid.UUID]int64) {
	l.iso.mu.Lock()
	defer l.iso.mu.Unlock()
	for runID := range l.iso.runs {
		if _, still := owned[runID]; !still {
			delete(l.iso.runs, runID)
		}
	}
	for runID := range l.iso.warned {
		if _, still := owned[runID]; !still {
			delete(l.iso.warned, runID)
		}
	}
}
