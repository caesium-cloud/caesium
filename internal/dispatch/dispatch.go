// Package dispatch implements the Phase 2 run-owner push-dispatch machinery.
//
// Two internal HTTP endpoints are defined:
//
//	POST /internal/dispatch  – owner → worker: push a ready task to a specific worker.
//	POST /internal/complete  – worker → owner: report task outcome back to owner.
//
// Both endpoints are guarded by the existing CAESIUM_INTERNAL_WAKEUP_TOKEN
// bearer-token check and run on the dedicated internal mTLS listener when owner
// mode is enabled.
//
// When CAESIUM_RUN_OWNER_ENABLED=false (default), these handlers are never
// registered and the system behaves byte-identically to Phase 1.
package dispatch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	jobdefruntime "github.com/caesium-cloud/caesium/internal/jobdef/runtime"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/dqlite"
	"github.com/caesium-cloud/caesium/pkg/log"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Rejection reason labels for caesium_complete_rejected_total.
const (
	ReasonStaleGeneration               = "stale_generation"
	ReasonWrongWorker                   = "wrong_worker"
	ReasonInvalidStatus                 = "invalid_status"
	ReasonCompletionApplicationRejected = "completion_application_rejected"
	ReasonCompletionApplyFailed         = "completion_apply_failed"
	ReasonTaskNotRunning                = "task_not_running"
	// ReasonTerminalRun is a permanent run-state fence. Keep it distinct from
	// legacy task_not_running, which PostComplete classifies as an application
	// rejection so a worker can report a deterministic result failure.
	ReasonTerminalRun = "terminal_run"
	ReasonNotOwner    = "not_owner"
	ReasonMissingRun  = "missing_run"
	ReasonMalformed   = "malformed"
	// ReasonContention labels caesium_complete_retryable_total when the owner
	// could not apply a completion because of transient dqlite contention and
	// answered 503 so the worker retries.  It is NOT a fence violation.
	ReasonContention = "contention"
	// ReasonOwnerNotReady asks a push worker to retry while the memory owner is
	// rebuilding its state. Falling back to SQL here would create two DAG writers.
	ReasonOwnerNotReady = "owner_not_ready"
	// ReasonAmbiguousTask rejects a dispatch that names only a catalog task id
	// for a fan-out group that is already expanded into N instance rows.  There
	// is no answer to "run this task" in that case, and every downstream write
	// resolves the identity through loadTaskRunByIDOrUnique, so accepting it
	// either strands a claim inside a transaction or drives a legacy group-wide
	// write.  Only an owner from BEFORE instance-addressed dispatch sends one.
	ReasonAmbiguousTask = "ambiguous_task"
	// ReasonNoCapacity rejects a dispatch the addressed worker has no free
	// execution slot for.  It is deliberately distinct from the generic
	// task_not_running 409: a capacity rejection says "ask me again later",
	// every other rejection says "this dispatch was wrong".  The owner's
	// dispatch loop backs the task off only for this one, so a task rejected
	// for any other reason is still retried on the very next tick.
	ReasonNoCapacity = "no_capacity"
	// ReasonTooLarge rejects an envelope over MaxDispatchBodyBytes. It is
	// answered before anything is claimed, and never by truncating the body.
	ReasonTooLarge = "too_large"
	// ReasonNoIsolationPolicy refuses an isolation re-check on a node that
	// serves no access policy file: it cannot vouch for any secret rule.
	ReasonNoIsolationPolicy = "no_isolation_policy"
)

// CapabilityIsolationV1 is the capability a worker advertises in GET
// /internal/capabilities when it executes a dispatched task under the
// isolation context the owner carries (identity-and-access design §10.1). An
// owner dispatches a task whose context is non-default only to a peer that
// advertises it; CAESIUM_WORKER_ISOLATION_V1_DISABLED=true withdraws it, which can only
// narrow where isolated tasks run.
const CapabilityIsolationV1 = "isolation_v1"

// isolationWorkerNodePrefix tags DispatchRequest.WorkerNode for a task whose
// isolation context is non-default. A worker built before the context checks
// WorkerNode against its own node id before claiming anything, so it answers a
// tagged envelope with 409 wrong_worker instead of running the task without
// the owner's rules — whatever the owner's capability cache believed.
const isolationWorkerNodePrefix = CapabilityIsolationV1 + "/"

// IsolationWorkerNode is the WorkerNode an owner sends nodeID for a dispatch
// whose isolation context is non-default.
func IsolationWorkerNode(nodeID string) string {
	return isolationWorkerNodePrefix + nodeID
}

// MaxDispatchBodyBytes bounds a /internal/dispatch envelope and an
// /internal/isolation answer. A context carries one namespace's settings,
// which a valid policy bounds as encoded JSON (policy.MaxEncodedNamespaceBytes,
// checked after YAML alias expansion); the fixed part keeps the envelope's
// historical 64 KiB for everything else. A sender refuses a larger body and a
// receiver rejects one (413 too_large) — neither ever truncates.
const MaxDispatchBodyBytes = 1<<16 + policy.MaxEncodedNamespaceBytes

// ErrDispatchTooLarge is returned by PostDispatch for an envelope over
// MaxDispatchBodyBytes; nothing is sent.
var ErrDispatchTooLarge = errors.New("dispatch: envelope exceeds the internal size limit")

// ErrNoCapacity is the sentinel a WorkerSubmitter returns when it has no free
// execution slot for a dispatched task.  It lives here rather than in the
// worker package because the handler must recognise it without importing the
// worker (the worker imports this package); worker.ErrInboundFull is an alias
// for it, so errors.Is answers true for either name.
var ErrNoCapacity = errors.New("worker: no execution capacity for dispatched task")

// Internal dispatch protocol versioning.
//
// The owner and the worker are separate processes upgraded separately.  Two
// mixed-version directions matter, and only one is still guarded:
//
//   - An OLD owner dispatching to this build omits TaskRunID; for a fan-out
//     group that is already expanded its catalog id names N rows.
//     HandleDispatch fails that closed with 409 ReasonAmbiguousTask rather
//     than resolving it to an arbitrary sibling — see ClaimTaskForDispatch /
//     ErrAmbiguousTaskRun.
//   - A NEW owner dispatching to a PRE-v2 peer is NOT guarded: that peer
//     ignores the TaskRunID it does not understand and processes the catalog
//     id.  The pre-flight capability probe that prevented this was removed in
//     #358 on the premise that every deployed node speaks v2; a fleet that
//     re-introduces a v1 node re-opens it.
const (
	// InternalProtocolVersion is the internal coordination protocol this build
	// speaks.  Bumped when the wire contract gains a field a peer must
	// understand rather than merely tolerate.  v1 was catalog-addressed
	// dispatch; v2 added instance-addressed dispatch.
	InternalProtocolVersion = 2
)

// CapabilitiesResponse is the body of GET /internal/capabilities: what this node
// can be asked to do.  Additive by construction — an unknown capability string
// is ignored, and a peer that does not serve the endpoint at all is read as
// supporting nothing beyond v1.
type CapabilitiesResponse struct {
	NodeID          string   `json:"node_id"`
	ProtocolVersion int      `json:"protocol_version"`
	Capabilities    []string `json:"capabilities"`
}

// Supports reports whether the response advertises the named capability.
func (c *CapabilitiesResponse) Supports(name string) bool {
	if c == nil {
		return false
	}
	return slices.Contains(c.Capabilities, name)
}

// CapabilityAdvertiser is implemented by a worker that can name the optional
// internal-protocol features it supports, surfaced via GET
// /internal/capabilities.  The WORKER is meant to be the honest source: the
// handler only routes, while the worker is what would actually execute
// whatever the capability describes.  *worker.Worker implements it and
// advertises CapabilityIsolationV1.
type CapabilityAdvertiser interface {
	Capabilities() []string
}

// ErrOwnerBusy is returned by PostComplete when the owner answered 503 Service
// Unavailable: it could not apply the completion because of transient dqlite
// contention and is asking the worker to retry the identical request.  This is
// distinct from a fence rejection (409), which is terminal — callers should
// retry on ErrOwnerBusy and give up on any other error.
var ErrOwnerBusy = errors.New("owner busy: retryable")

// ErrOwnerNotReady means recovery has not published the run yet. Workers retain
// the completion while their execution context and claim remain valid.
var ErrOwnerNotReady = errors.New("owner recovery pending: retryable")

// ErrOwnerRejected is an authoritative ownership/attempt fence, not a runtime failure.
var ErrOwnerRejected = errors.New("owner completion fenced")

// ErrCompletionApplicationRejected means the finished result failed validation,
// while its worker claim remains eligible to report an ordinary task failure.
var ErrCompletionApplicationRejected = errors.New("completion application rejected")

// ErrPeerUnreachable wraps a TRANSPORT failure talking to a peer probed via
// GetCapabilities, as distinct from a peer that answered with an unwelcome
// status (e.g. 404 from a build with no such route).  A caller that needs to
// tell "peer is down" apart from "peer answered and supports nothing" can
// check for this with errors.Is.
var ErrPeerUnreachable = errors.New("peer unreachable")

// internalClient is the shared HTTP client used for both PostDispatch and
// PostComplete. Sharing keeps the underlying TCP/keep-alive pool warm
// across requests so we don't pay connection setup on every dispatch.
// The per-call timeout is enforced via context.WithTimeout, not on the
// client itself, so callers can extend it if their workload needs it.
// ConfigureInternalMTLS swaps in a TLS-enabled transport at startup when
// run-owner mode is on; the call sites stay the same.
var internalClient = &http.Client{
	Timeout: 30 * time.Second,
}

// dispatchPostTimeout bounds a single /internal/dispatch POST so an unreachable
// peer fails fast instead of stalling the dispatch loop (the task is simply
// retried next tick against another peer).
const dispatchPostTimeout = 4 * time.Second

// configureMTLSOnce ensures the shared internal client is swapped for its
// TLS-enabled form exactly once, even if ConfigureInternalMTLS is called from
// multiple goroutines (e.g. concurrent tests) — avoiding a data race on the
// package-level internalClient.
var configureMTLSOnce sync.Once

// ConfigureInternalMTLS replaces the shared internal client with one that
// presents this node's client certificate and verifies peers against the
// configured CA.  Called once at startup when run-owner mode is enabled, before
// any dispatch or completion POST is issued.  Subsequent calls are no-ops.  Peer
// internal endpoints are reached over https on the internal port (see
// DispatchLoopConfig.InternalPort).
func ConfigureInternalMTLS(clientTLS *tls.Config) {
	configureMTLSOnce.Do(func() {
		internalClient = &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: clientTLS,
			},
		}
	})
}

// ValidCompleteStatuses are the only task statuses workers may report.
// "skipped" is deliberately excluded — skipping is an owner-side DAG decision.
var ValidCompleteStatuses = map[string]bool{
	string(run.TaskStatusSucceeded): true,
	string(run.TaskStatusFailed):    true,
	string(run.TaskStatusCached):    true,
}

// DispatchRequest is the envelope pushed by the owner to a worker to ask it
// to execute a specific task.
type DispatchRequest struct {
	RunID  uuid.UUID `json:"run_id"`
	TaskID uuid.UUID `json:"task_id"`
	// TaskRunID identifies the specific instance. Empty on older workers;
	// the owner falls back to the unique (run, task) row and rejects
	// ambiguity when more than one instance exists.
	TaskRunID       uuid.UUID `json:"task_run_id,omitempty"`
	OwnerGeneration int64     `json:"owner_generation"`
	Attempt         int       `json:"attempt"`
	WorkerNode      string    `json:"worker_node"`
	// OwnerBaseURL is the owner's own HTTP API base URL
	// (http://<owner-host>:<apiPort>).  The receiving worker POSTs its task
	// completion back to OwnerBaseURL + "/internal/complete" so the owner
	// remains the single writer for its run's hot rows.  Set by the dispatch
	// loop from the owner's node address + API port.
	OwnerBaseURL string    `json:"owner_base_url"`
	Deadline     time.Time `json:"deadline"`
	// Isolation is the run namespace's isolation context, resolved by the
	// owner from its policy file for this dispatch (identity-and-access design
	// §10.1). Absent when the owner serves no policy file — the envelope is
	// then byte-identical to before — and from owners built before it.
	Isolation *jobdefruntime.IsolationContext `json:"isolation,omitempty"`
}

// CompleteRequest is the envelope sent by a worker back to the owner when a
// task execution finishes.
type CompleteRequest struct {
	RunID           uuid.UUID         `json:"run_id"`
	TaskID          uuid.UUID         `json:"task_id"`
	TaskRunID       uuid.UUID         `json:"task_run_id,omitempty"`
	OwnerGeneration int64             `json:"owner_generation"`
	Attempt         int               `json:"attempt"`
	WorkerNode      string            `json:"worker_node"`
	Status          string            `json:"status"`
	Result          string            `json:"result,omitempty"`
	Outputs         map[string]string `json:"outputs,omitempty"`
	// BranchSelections carries the downstream branch names a `type: branch`
	// task chose at runtime. The owner uses this to propagate `skipped` to the
	// non-selected branches. Empty for non-branch tasks.
	BranchSelections []string `json:"branch_selections,omitempty"`
	// Partitions is the producer's parsed partition list. Empty for non-producers
	// and for older workers (the owner then treats the group as unfanned).
	Partitions []pkgtask.Partition `json:"partitions,omitempty"`
	Error      string              `json:"error,omitempty"`
}

// CompleteResponse is the JSON body returned by /internal/complete.
type CompleteResponse struct {
	// Accepted is true when the completion was applied.
	Accepted bool   `json:"accepted"`
	Reason   string `json:"reason,omitempty"`
}

// ErrorResponse is a structured 409 body with a rejection reason label.
type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// InboundDispatch is a task accepted by this node for execution plus the owner
// metadata it needs to report completion back to the owner.  HandleDispatch
// builds one of these and hands it to the worker via the WorkerSubmitter.
type InboundDispatch struct {
	// Task is the full task_runs row to execute (image/command/engine/etc.).
	Task *models.TaskRun
	// OwnerBaseURL is the owner's API base URL; the worker POSTs its completion
	// to OwnerBaseURL + "/internal/complete".
	OwnerBaseURL string
	// OwnerGeneration / Attempt / WorkerNode are the fencing fields the owner
	// validates on the completion envelope.
	OwnerGeneration int64
	Attempt         int
	WorkerNode      string
	// Isolation is the owner's isolation context for the task, or nil (no
	// owner policy file, or an owner built before it): the worker then
	// resolves the namespace's settings in-process, as before.
	Isolation *jobdefruntime.IsolationContext
}

// WorkerSubmitter is the seam the dispatch handler uses to hand an accepted
// task to the local worker's execution pool.  The worker implementation
// (worker.Worker.SubmitDispatched) enqueues the task onto its inbound channel
// for the Run loop to drain onto the shared pool.  It returns an error when the
// worker cannot accept the task (inbound buffer full or worker not running) so
// HandleDispatch can roll back the claim and let the owner re-dispatch.
//
// It is an interface so dispatch tests can inject a fake without standing up a
// real worker + pool.
type WorkerSubmitter interface {
	SubmitDispatched(d InboundDispatch) error
}

// Handler holds the dependencies needed to serve the dispatch and complete
// endpoints.
type Handler struct {
	store      *run.Store
	leaseStore *run.LeaseStore
	nodeID     string
	token      string
	// submitter hands accepted dispatches to the local worker pool.  When nil
	// (worker disabled on this node), HandleDispatch cannot execute the task and
	// rolls back the claim so the owner re-dispatches elsewhere.
	submitter WorkerSubmitter
	// ownerManager, when set (CAESIUM_RUN_OWNER_IN_MEMORY=true), routes
	// completions through the in-memory DAG state instead of the SQL-advancement
	// path.  Nil keeps the proven B2 path.
	ownerManager *run.OwnerManager
}

// NewHandler constructs a Handler.  store is the run.Store; leaseStore is the
// run-lease store used to verify ownership; nodeID is this node's address;
// token is the CAESIUM_INTERNAL_WAKEUP_TOKEN value used for bearer-token auth.
func NewHandler(store *run.Store, leaseStore *run.LeaseStore, nodeID, token string) *Handler {
	return &Handler{
		store:      store,
		leaseStore: leaseStore,
		nodeID:     nodeID,
		token:      token,
	}
}

// WithWorkerSubmitter wires the local worker's submit seam into the handler so
// accepted dispatches flow onto the worker's shared execution pool.  Returns
// the handler for chaining at construction time.
func (h *Handler) WithWorkerSubmitter(s WorkerSubmitter) *Handler {
	h.submitter = s
	return h
}

// WithOwnerManager enables the in-memory advancement path: completions are
// applied to the owner's RunState and persisted as terminal-only rows, instead
// of the SQL-advancement path.  Returns the handler for chaining.
func (h *Handler) WithOwnerManager(m *run.OwnerManager) *Handler {
	h.ownerManager = m
	return h
}

// authorized checks the Bearer token in the request's Authorization header.
func (h *Handler) authorized(r *http.Request) bool {
	if h.token == "" {
		return false
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.EqualFold(auth[:min(len(auth), 7)], "bearer ") {
		// Hash both sides to a fixed 32-byte digest before the constant-time
		// compare. A direct ConstantTimeCompare/hmac.Equal short-circuits when the
		// lengths differ, which would leak the token's byte-length via a timing
		// oracle (an attacker submits 1-, 2-, 3-byte tokens until the comparison
		// stops short-circuiting). Comparing equal-length digests removes that.
		got := sha256.Sum256([]byte(strings.TrimSpace(auth[7:])))
		want := sha256.Sum256([]byte(h.token))
		return subtle.ConstantTimeCompare(got[:], want[:]) == 1
	}
	return false
}

// capabilities is the capability set this node advertises.
func (h *Handler) capabilities() []string {
	if h.submitter == nil {
		return nil
	}
	if adv, ok := h.submitter.(CapabilityAdvertiser); ok {
		return adv.Capabilities()
	}
	return nil
}

// HandleCapabilities handles GET /internal/capabilities: reports this node's
// internal-protocol version and the optional feature set it advertises via
// CapabilityAdvertiser (CapabilityIsolationV1 from a current worker).
// It is deliberately a plain read with no side effects, guarded by the same
// bearer token as the other internal endpoints.
func (h *Handler) HandleCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	caps := h.capabilities()
	if caps == nil {
		caps = []string{}
	}
	writeJSON(w, http.StatusOK, CapabilitiesResponse{
		NodeID:          h.nodeID,
		ProtocolVersion: InternalProtocolVersion,
		Capabilities:    caps,
	})
}

// HandleDispatch handles POST /internal/dispatch.
//
// The worker accepts the dispatch by:
//  1. Parsing and validating the envelope.
//  2. Calling StartTaskClaimed to transition the task to "running".
//  3. Returning 202 ACK.
//
// If the worker cannot accept (task already claimed, owner mismatch, etc.)
// it returns 409 and the owner falls back to writing the task to the DB with
// claimed_by="" for ClaimNext recovery.
func (h *Handler) HandleDispatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req DispatchRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxDispatchBodyBytes+1))
	if err == nil && len(body) > MaxDispatchBodyBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, ErrorResponse{
			Code:    ReasonTooLarge,
			Message: fmt.Sprintf("dispatch request exceeds %d bytes", MaxDispatchBodyBytes),
		})
		return
	}
	if err != nil || json.Unmarshal(body, &req) != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{
			Code:    ReasonMalformed,
			Message: "failed to decode dispatch request",
		})
		return
	}

	// Validate that this node is the intended recipient. A task whose
	// isolation context is non-default arrives tagged (IsolationWorkerNode) and
	// only with that context; an untagged envelope carries no context or a
	// default one. Anything else is refused before the claim.
	expectedNode := h.nodeID
	if !req.Isolation.Default() {
		// A node that does not advertise the capability (no worker, or
		// CAESIUM_WORKER_ISOLATION_V1_DISABLED=true) never takes an isolated
		// task, however the envelope is addressed.
		if !slices.Contains(h.capabilities(), CapabilityIsolationV1) {
			writeJSON(w, http.StatusConflict, ErrorResponse{
				Code:    ReasonWrongWorker,
				Message: fmt.Sprintf("this node %q does not advertise %s", h.nodeID, CapabilityIsolationV1),
			})
			return
		}
		expectedNode = IsolationWorkerNode(h.nodeID)
	}
	if req.WorkerNode != expectedNode {
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Code:    ReasonWrongWorker,
			Message: fmt.Sprintf("this node is %q, dispatch addressed to %q", expectedNode, req.WorkerNode),
		})
		return
	}

	// Derive the claim TTL from the envelope's deadline so a tight per-task
	// deadline doesn't leave a stale 5-min claim if execution finishes early.
	// Under 30s left is not clamped to 30s: the claim falls back to 5 minutes, so
	// the renewal ticker has room to extend long-running tasks. The deadline is
	// already running when it gets here, so a 30s deadline lands on this branch.
	ttl := time.Until(req.Deadline)
	if ttl < 30*time.Second {
		ttl = 5 * time.Minute
	}

	// A worker must be wired up to execute the task.  Without one, accepting the
	// dispatch would claim the task with nobody to run it (the exact orphaning
	// B1 measured).  Reject before claiming so the owner re-dispatches elsewhere.
	if h.submitter == nil {
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Code:    ReasonTaskNotRunning,
			Message: "no worker available on this node to execute dispatched tasks",
		})
		return
	}

	// Accept the dispatch: atomically claim the task and mark it as running.
	// ClaimTaskForDispatch transitions pending→running with claimed_by=nodeID
	// in one UPDATE (equivalent to ClaimNext but targeting a specific task),
	// stamping owner_generation so subsequent writes fence against takeover.
	// In in-memory mode the owner advanced the DAG in memory (the DB's
	// outstanding_predecessors counter is intentionally stale), so trust the
	// owner's readiness decision rather than re-checking it here.
	trustOwnerReadiness := h.ownerManager != nil
	if err := h.store.ClaimTaskForDispatch(req.RunID, dispatchTaskRef(req), h.nodeID, req.OwnerGeneration, ttl, trustOwnerReadiness); err != nil {
		// An instance-blind dispatch for a group that is already expanded.  Only
		// an owner that predates instance-addressed dispatch omits TaskRunID, and
		// for an expanded group its catalog id names N rows, so the claim's
		// loadTaskRunByIDOrUnique resolves nothing and the transaction rolls back
		// having written nothing.  Reported with its own code rather than folded
		// into the generic "not running" 409, which reads like an ordinary lost
		// claim race and hides the rolling-upgrade mismatch behind it.  Detected
		// from the claim itself rather than by a pre-flight count so the check and
		// the claim are one transaction: a group that expands in between cannot
		// slip through with a stale "unambiguous" answer.
		if errors.Is(err, run.ErrAmbiguousTaskRun) {
			log.Warn("dispatch: rejected instance-blind dispatch for an expanded fan-out group",
				"run_id", req.RunID, "task_id", req.TaskID, "error", err)
			writeJSON(w, http.StatusConflict, ErrorResponse{
				Code: ReasonAmbiguousTask,
				Message: fmt.Sprintf(
					"task %s is expanded into fan-out instances; dispatch must name task_run_id",
					req.TaskID),
			})
			return
		}
		if err == run.ErrTaskClaimMismatch {
			writeJSON(w, http.StatusConflict, ErrorResponse{
				Code:    ReasonTaskNotRunning,
				Message: "task not in pending state; may have been claimed or completed by another path",
			})
			return
		}
		log.Error("dispatch: ClaimTaskForDispatch failed",
			"run_id", req.RunID,
			"task_id", req.TaskID,
			"error", err,
		)
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Code:    ReasonTaskNotRunning,
			Message: "failed to accept dispatch",
		})
		return
	}

	// Load the full task row to execute (image/command/engine/etc.).  If the
	// row vanished between claim and load (reclaimed by another node in the
	// race window), roll back and reject.
	taskRun, err := h.store.LoadDispatchedTaskRun(req.RunID, dispatchTaskRef(req), h.nodeID)
	if err != nil {
		h.rollbackClaim(req)
		// Surface the underlying error rather than dropping it: a missing row is
		// the expected reclaim race, but a transient DB/connection failure here
		// looks identical from the fixed 409 body otherwise.
		log.Warn("dispatch: could not load claimed task row; rolled back claim",
			"run_id", req.RunID,
			"task_id", req.TaskID,
			"error", err,
		)
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Code:    ReasonTaskNotRunning,
			Message: "claimed task row not found",
		})
		return
	}

	// Hand the claimed task to the local worker pool.  SubmitDispatched is
	// non-blocking: it returns an error if the inbound buffer is full or the
	// worker is not running.  On failure we MUST NOT leave the task
	// claimed-but-orphaned — roll the claim back to pending so the owner's next
	// dispatch tick re-dispatches it (here or to a peer), and reject with 409.
	if submitErr := h.submitter.SubmitDispatched(InboundDispatch{
		Task:            taskRun,
		OwnerBaseURL:    req.OwnerBaseURL,
		OwnerGeneration: req.OwnerGeneration,
		Attempt:         req.Attempt,
		WorkerNode:      h.nodeID,
		Isolation:       req.Isolation,
	}); submitErr != nil {
		h.rollbackClaim(req)
		log.Warn("dispatch: worker could not accept task; rolled back claim",
			"run_id", req.RunID,
			"task_id", req.TaskID,
			"task_run_id", req.TaskRunID,
			"error", submitErr,
		)
		// Saturation is reported under its own code so the owner can back the
		// task off instead of re-posting it every tick; anything else keeps the
		// generic code and the owner's immediate retry.
		code := ReasonTaskNotRunning
		if errors.Is(submitErr, ErrNoCapacity) {
			code = ReasonNoCapacity
		}
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Code:    code,
			Message: "worker busy; task returned to dispatch pool",
		})
		return
	}

	w.WriteHeader(http.StatusAccepted)
}

// dispatchTaskRef is the row this dispatch addresses.  The claim, the load, and
// the rollback must all resolve the *same* row, and for a fanned step the
// catalog task id names N of them: every one of ClaimTaskForDispatch /
// LoadDispatchedTaskRun / ReleaseTaskClaim resolves through
// loadTaskRunByIDOrUnique, which rejects that ambiguity rather than picking a
// sibling.  An older owner omits TaskRunID, and the catalog id then still names
// exactly one row (the rolling-upgrade fallback).
func dispatchTaskRef(req DispatchRequest) uuid.UUID {
	if req.TaskRunID != uuid.Nil {
		return req.TaskRunID
	}
	return req.TaskID
}

// rollbackClaim reverts a just-claimed task back to the dispatchable pending
// state so the owner re-dispatches it.  Logged but not surfaced to the caller
// beyond the 409 the caller already returns; a failed rollback is rare (the
// claim lease still expires and ClaimNext recovery covers it).
func (h *Handler) rollbackClaim(req DispatchRequest) {
	if err := h.store.ReleaseTaskClaim(req.RunID, dispatchTaskRef(req), h.nodeID, req.OwnerGeneration); err != nil {
		log.Error("dispatch: failed to roll back claim after worker rejected task",
			"run_id", req.RunID,
			"task_id", req.TaskID,
			"error", err,
		)
	}
}

// HandleComplete handles POST /internal/complete.
//
// Validation rules (any mismatch → 409):
//  1. This node currently owns the run (run_leases.owner_node == self &&
//     !expired).
//  2. The envelope's owner_generation matches the current lease generation.
//  3. worker_node matches claimed_by on the task_runs row.
//  4. The task is currently in "running" status.
//  5. status ∈ {succeeded, failed, cached} — "skipped" is rejected.
//
// On success, the owner applies the completion via the existing
// CompleteTaskClaimed / CacheHitTaskClaimed / FailTaskClaimed path and
// returns 200 with {"accepted": true}.
func (h *Handler) HandleComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req CompleteRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || json.Unmarshal(body, &req) != nil {
		// Malformed JSON is a 400, not a fence violation — don't bump the
		// fence-rejection counter or operators can't trust it for alerting.
		writeJSON(w, http.StatusBadRequest, ErrorResponse{
			Code:    ReasonMalformed,
			Message: "failed to decode complete request",
		})
		return
	}

	ctx := r.Context()
	metricQuarantined := sync.OnceValue(func() bool {
		return h.completeMetricQuarantined(ctx, req.RunID, req.TaskID)
	})
	recordRejected := func(reason string) {
		if !metricQuarantined() {
			metrics.CompleteRejectedTotal.WithLabelValues(reason).Inc()
		}
	}

	// Rule 5: validate status vocabulary.
	if !ValidCompleteStatuses[req.Status] {
		recordRejected(ReasonInvalidStatus)
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Code:    ReasonInvalidStatus,
			Message: fmt.Sprintf("invalid status %q; must be one of {succeeded, failed, cached}", req.Status),
		})
		return
	}

	// Rules 1 & 2 in a single DB call: GetLease returns the row, we check
	// ownership (owner_node, expiry) and generation in memory. A failed read
	// does not prove that the lease is absent: retain the worker's completed
	// result for retry rather than discarding it as an ownership fence.
	lease, err := h.leaseStore.GetLease(ctx, req.RunID)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		log.Warn("complete: cannot read run lease; asking worker to retry",
			"run_id", req.RunID, "error", err)
		if !metricQuarantined() {
			metrics.CompleteRetryableTotal.WithLabelValues(ReasonOwnerNotReady).Inc()
		}
		writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{
			Code:    ReasonOwnerNotReady,
			Message: "run lease could not be read; retry completion",
		})
		return
	}
	if err == nil && lease == nil {
		log.Warn("complete: run lease read returned no row or error; asking worker to retry",
			"run_id", req.RunID)
		if !metricQuarantined() {
			metrics.CompleteRetryableTotal.WithLabelValues(ReasonOwnerNotReady).Inc()
		}
		writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{
			Code:    ReasonOwnerNotReady,
			Message: "run lease could not be read; retry completion",
		})
		return
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// A concurrency replacement cancels the run and deletes its lease in
		// one transaction. When that wins before this read, the missing lease
		// is a permanent run fence only if the durable run status proves it.
		var persisted struct{ Status string }
		readErr := h.store.DB().WithContext(ctx).Model(&models.JobRun{}).
			Select("status").Where("id = ?", req.RunID).Take(&persisted).Error
		if readErr != nil && !errors.Is(readErr, gorm.ErrRecordNotFound) {
			log.Warn("complete: cannot read run status after missing lease; asking worker to retry",
				"run_id", req.RunID, "error", readErr)
			if !metricQuarantined() {
				metrics.CompleteRetryableTotal.WithLabelValues(ReasonOwnerNotReady).Inc()
			}
			writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{
				Code:    ReasonOwnerNotReady,
				Message: "run status could not be read; retry completion",
			})
			return
		}
		if readErr == nil {
			switch run.Status(persisted.Status) {
			case run.StatusSucceeded, run.StatusFailed, run.StatusCancelled, run.StatusSkipped:
				recordRejected(ReasonTerminalRun)
				writeJSON(w, http.StatusConflict, ErrorResponse{
					Code:    ReasonTerminalRun,
					Message: "run has already reached a terminal state",
				})
				return
			}
		}
		recordRejected(ReasonMissingRun)
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Code:    ReasonMissingRun,
			Message: "run lease not found",
		})
		return
	}
	if lease.OwnerNode != h.nodeID || lease.LeaseExpiresAt.Before(time.Now().UTC()) {
		recordRejected(ReasonNotOwner)
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Code:    ReasonNotOwner,
			Message: "this node does not currently own the run",
		})
		return
	}
	if lease.Generation != req.OwnerGeneration {
		recordRejected(ReasonStaleGeneration)
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Code:    ReasonStaleGeneration,
			Message: fmt.Sprintf("owner generation mismatch: expected %d, got %d", lease.Generation, req.OwnerGeneration),
		})
		return
	}

	// Run-owner in-memory path: when enabled and this node holds the run's
	// in-memory state, advance the DAG in memory and persist terminal-only rows
	// (no per-transition SQL advancement). A run not yet tracked must retry:
	// SQL fallback could otherwise commit across recovery's terminal-tail read.
	if h.ownerManager != nil {
		res, omErr := h.ownerManager.CompleteInstance(
			req.RunID, req.TaskID, req.TaskRunID, run.TaskStatus(req.Status),
			req.Result, req.Error, req.WorkerNode, req.Outputs, req.BranchSelections, req.Partitions,
		)
		if omErr != nil {
			if dqlite.IsContentionError(omErr) {
				h.rejectRetryable(w, req, omErr, metricQuarantined())
				return
			}
			if errors.Is(omErr, run.ErrRunTerminal) {
				recordRejected(ReasonTerminalRun)
				writeJSON(w, http.StatusConflict, ErrorResponse{
					Code:    ReasonTerminalRun,
					Message: "run has already reached a terminal state",
				})
				return
			}
			if errors.Is(omErr, run.ErrTaskClaimMismatch) {
				recordRejected(ReasonWrongWorker)
				writeJSON(w, http.StatusConflict, ErrorResponse{
					Code:    ReasonWrongWorker,
					Message: "task claimed_by mismatch or task not in running state",
				})
				return
			}
			log.Error("complete: owner-manager apply failed",
				"run_id", req.RunID, "task_id", req.TaskID, "status", req.Status, "error", omErr)
			writeCompletionApplyError(w, omErr)
			return
		}
		if res.Owned {
			writeJSON(w, http.StatusOK, CompleteResponse{Accepted: true})
			return
		}
		// A completed memory owner drops its in-memory state but retains the
		// lease. Distinguish that durable terminal state from a running owner
		// that has not recovered yet. Read only the run status: a retry can
		// reopen the same run ID, so a running or unreadable row must remain
		// retryable rather than being mistaken for a terminal duplicate.
		var persisted struct{ Status string }
		readErr := h.store.DB().WithContext(ctx).Model(&models.JobRun{}).
			Select("status").Where("id = ?", req.RunID).Take(&persisted).Error
		if readErr == nil {
			switch run.Status(persisted.Status) {
			case run.StatusSucceeded, run.StatusFailed, run.StatusCancelled, run.StatusSkipped:
				recordRejected(ReasonTerminalRun)
				writeJSON(w, http.StatusConflict, ErrorResponse{
					Code:    ReasonTerminalRun,
					Message: "run has already reached a terminal state",
				})
				return
			}
		} else {
			log.Warn("complete: cannot read run status for untracked memory owner; asking worker to retry",
				"run_id", req.RunID, "error", readErr)
		}
		if !metricQuarantined() {
			metrics.CompleteRetryableTotal.WithLabelValues(ReasonOwnerNotReady).Inc()
		}
		writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{
			Code:    ReasonOwnerNotReady,
			Message: "owner is rebuilding run state; retry completion",
		})
		return
	}

	// Rules 3 & 4 are enforced by the ClaimNext-path functions via
	// claimed_by check (ErrTaskClaimMismatch) and status == "running"
	// implicit in the update query.  We pass workerNode as the claimedBy
	// fence and let the DB do the filtering.
	switch run.TaskStatus(req.Status) {
	case run.TaskStatusSucceeded, run.TaskStatusFailed:
		var applyErr error
		if run.TaskStatus(req.Status) == run.TaskStatusSucceeded {
			completeID := req.TaskID
			if req.TaskRunID != uuid.Nil {
				completeID = req.TaskRunID
			}
			applyErr = h.store.CompleteTaskClaimedWithPartitions(req.RunID, completeID, req.Result, req.WorkerNode, req.Outputs, req.BranchSelections, req.Partitions)
		} else {
			failID := req.TaskID
			if req.TaskRunID != uuid.Nil {
				failID = req.TaskRunID
			}
			applyErr = h.store.FailTaskClaimed(req.RunID, failID, fmt.Errorf("%s", req.Error), req.WorkerNode)
		}
		if applyErr == run.ErrTaskClaimMismatch {
			recordRejected(ReasonWrongWorker)
			writeJSON(w, http.StatusConflict, ErrorResponse{
				Code:    ReasonWrongWorker,
				Message: "task claimed_by mismatch or task not in running state",
			})
			return
		}
		if applyErr != nil {
			if dqlite.IsContentionError(applyErr) {
				h.rejectRetryable(w, req, applyErr, metricQuarantined())
				return
			}
			log.Error("complete: apply failed",
				"run_id", req.RunID,
				"task_id", req.TaskID,
				"status", req.Status,
				"error", applyErr,
			)
			writeCompletionApplyError(w, applyErr)
			return
		}

	case run.TaskStatusCached:
		source := run.CacheHitSource{RunID: req.RunID}
		cacheID := req.TaskID
		if req.TaskRunID != uuid.Nil {
			cacheID = req.TaskRunID
		}
		// A cached fan-out producer still carries its partition list, and the SQL
		// lane expands the group inside the cache-hit transaction. Called
		// directly so removing the store method is a build error, never a
		// silently un-expanded group.
		var applyErr error
		if len(req.Partitions) > 0 {
			applyErr = h.store.CacheHitTaskClaimedWithPartitions(req.RunID, cacheID, source, req.Result, req.WorkerNode, req.Outputs, req.BranchSelections, req.Partitions)
		} else {
			applyErr = h.store.CacheHitTaskClaimed(req.RunID, cacheID, source, req.Result, req.WorkerNode, req.Outputs, req.BranchSelections)
		}
		if applyErr == run.ErrTaskClaimMismatch {
			recordRejected(ReasonWrongWorker)
			writeJSON(w, http.StatusConflict, ErrorResponse{
				Code:    ReasonWrongWorker,
				Message: "task claimed_by mismatch or task not in running state",
			})
			return
		}
		if applyErr != nil {
			if dqlite.IsContentionError(applyErr) {
				h.rejectRetryable(w, req, applyErr, metricQuarantined())
				return
			}
			log.Error("complete: CacheHitTaskClaimed failed",
				"run_id", req.RunID,
				"task_id", req.TaskID,
				"error", applyErr,
			)
			writeCompletionApplyError(w, applyErr)
			return
		}
	}

	writeJSON(w, http.StatusOK, CompleteResponse{Accepted: true})
}

func (h *Handler) completeMetricQuarantined(ctx context.Context, runID, taskID uuid.UUID) bool {
	if h.store == nil {
		return false
	}
	quarantined, err := h.store.TaskQuarantine(ctx, runID, taskID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			log.Debug("complete: task quarantine marker not found; emitting complete metrics",
				"run_id", runID,
				"task_id", taskID,
			)
		} else {
			log.Warn("complete: failed to read task quarantine marker; emitting complete metrics",
				"run_id", runID,
				"task_id", taskID,
				"error", err,
			)
		}
		return false
	}
	return quarantined
}

// rejectRetryable answers a completion the owner could not apply because of
// transient dqlite contention.  It returns 503 (not 409) so the worker knows
// the request is safe to re-send once the leader's contention clears, and logs
// at warn rather than error because this is expected under burst load and is
// not a lost completion unless the worker exhausts its own retries.
func (h *Handler) rejectRetryable(w http.ResponseWriter, req CompleteRequest, applyErr error, quarantined bool) {
	if !quarantined {
		metrics.CompleteRetryableTotal.WithLabelValues(ReasonContention).Inc()
	}
	log.Warn("complete: transient contention, asking worker to retry",
		"run_id", req.RunID,
		"task_id", req.TaskID,
		"status", req.Status,
		"error", applyErr,
	)
	writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{
		Code:    ReasonContention,
		Message: "owner busy applying completion; retry",
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

// PostDispatch sends a DispatchRequest to the target worker node and returns
// whether the worker accepted (202) or rejected (409), plus the rejection's
// reason code when the worker supplied one (ErrorResponse.Code — e.g.
// ReasonNoCapacity).  The reason is "" for an acceptance, for a worker that
// answered without a structured body, and for a build predating the code (a
// rolling upgrade simply gets the old undifferentiated retry behaviour).  On
// rejection or network error, the caller should fall back to writing the task
// to the DB with claimed_by="" for ClaimNext recovery.
func PostDispatch(ctx context.Context, targetURL, token string, req DispatchRequest) (bool, string, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return false, "", fmt.Errorf("dispatch: marshal: %w", err)
	}
	if len(body) > MaxDispatchBodyBytes {
		return false, "", ErrDispatchTooLarge
	}

	// Fail fast on an unreachable peer: a dispatch is cheap to retry on the next
	// tick (to a different peer), so a dead node in the round-robin must not hang
	// the loop for the client's full 30s timeout — critical during failover, when
	// the just-crashed owner can still be in the peer list.
	dialCtx, cancel := context.WithTimeout(ctx, dispatchPostTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(dialCtx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return false, "", fmt.Errorf("dispatch: new request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := internalClient.Do(httpReq)
	if err != nil {
		return false, "", fmt.Errorf("dispatch: http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusAccepted {
		_, _ = io.Copy(io.Discard, resp.Body)
		return true, "", nil
	}
	// 409 or any non-202: worker rejected.  Read the reason code so the caller
	// can tell backpressure ("no capacity, ask later") from a rejection that
	// says this dispatch itself was wrong.  A body that is missing, oversized,
	// or not an ErrorResponse yields "" and the undifferentiated behaviour.
	var rejection ErrorResponse
	rejectBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	// Drain whatever the limit left behind so the connection stays reusable.
	_, _ = io.Copy(io.Discard, resp.Body)
	if readErr == nil {
		_ = json.Unmarshal(rejectBody, &rejection)
	}
	return false, rejection.Code, nil
}

// GetCapabilities probes a peer's GET /internal/capabilities.
//
// Every failure mode — an old build with no such route (404), an unreachable
// node, a malformed body — is reported as an error rather than a zero-value
// success, so a caller can tell "peer answered: supports nothing" (see
// CapabilitiesResponse.Supports) apart from "could not reach the peer at all"
// (see ErrPeerUnreachable).
func GetCapabilities(ctx context.Context, targetURL, token string) (*CapabilitiesResponse, error) {
	probeCtx, cancel := context.WithTimeout(ctx, dispatchPostTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(probeCtx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("capabilities: new request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)

	resp, err := internalClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("capabilities: http: %w: %w", ErrPeerUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("capabilities: peer returned status %d", resp.StatusCode)
	}
	var out CapabilitiesResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("capabilities: decode: %w", err)
	}
	return &out, nil
}

// writeCompletionApplyError distinguishes deterministic partition validation from
// unexpected storage/application faults; neither is an ownership fence.
func writeCompletionApplyError(w http.ResponseWriter, err error) {
	if _, ok := errors.AsType[*pkgtask.PartitionError](err); ok {
		writeJSON(w, http.StatusUnprocessableEntity, ErrorResponse{Code: ReasonCompletionApplicationRejected, Message: err.Error()})
		return
	}
	writeJSON(w, http.StatusInternalServerError, ErrorResponse{Code: ReasonCompletionApplyFailed, Message: err.Error()})
}

// PostComplete sends a CompleteRequest from a worker to the owner node.
func PostComplete(ctx context.Context, ownerURL, token string, req CompleteRequest) (*CompleteResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("complete: marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, ownerURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("complete: new request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := internalClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("complete: http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var result CompleteResponse
	if resp.StatusCode == http.StatusOK {
		_ = json.Unmarshal(respBody, &result)
		return &result, nil
	}
	// Recovery is distinct from short write contention so the worker can retain
	// the result within its claim-aware reporting window instead of exhausting that budget.
	// Unknown 503 bodies retain the existing contention classification.
	if resp.StatusCode == http.StatusServiceUnavailable {
		var failure ErrorResponse
		if json.Unmarshal(respBody, &failure) == nil && failure.Code == ReasonOwnerNotReady {
			return nil, fmt.Errorf("complete: owner recovery pending: %w", ErrOwnerNotReady)
		}
		return nil, fmt.Errorf("complete: owner returned status %d: %w", resp.StatusCode, ErrOwnerBusy)
	}
	var failure ErrorResponse
	decodedFailure := json.Unmarshal(respBody, &failure) == nil
	// Prior owners used task_not_running only for completion application faults;
	// actual claim/status fences use wrong_worker. Preserve that mixed-version path.
	if decodedFailure && ((resp.StatusCode == http.StatusUnprocessableEntity && failure.Code == ReasonCompletionApplicationRejected) ||
		(resp.StatusCode == http.StatusConflict && failure.Code == ReasonTaskNotRunning)) {
		return nil, fmt.Errorf("complete: %w: %s", ErrCompletionApplicationRejected, failure.Message)
	}
	if resp.StatusCode == http.StatusConflict {
		return nil, fmt.Errorf("complete: owner returned status %d: %w: %s", resp.StatusCode, ErrOwnerRejected, failure.Message)
	}
	return nil, fmt.Errorf("complete: owner returned status %d: %s", resp.StatusCode, failure.Message)
}

// WarnIfNoToken emits a startup warning when owner mode is on but the
// internal wakeup token is not set.  Without the token, the dispatch and
// complete endpoints reject every request (bearer-token check fails closed),
// so run-owner dispatch is silently inert — adding lease overhead with zero
// benefit.  Warn-only: unlike the mTLS material (a hard startup error), a
// missing token is recoverable by setting it without regenerating certs.
func WarnIfNoToken(token string) {
	if strings.TrimSpace(token) != "" {
		return
	}
	log.Warn(
		"run-owner mode is enabled but CAESIUM_INTERNAL_WAKEUP_TOKEN is not set; " +
			"the /internal/dispatch and /internal/complete endpoints require a " +
			"Bearer token and will reject every request without one — " +
			"run-owner dispatch will be silently inert. " +
			"Set CAESIUM_INTERNAL_WAKEUP_TOKEN on every node to enable dispatch.",
	)
}

// IsolationRequest is the body of POST /internal/isolation: a worker executing
// a dispatched task asks the task's owner for the run's isolation context as
// the owner's policy states it now.
type IsolationRequest struct {
	RunID uuid.UUID `json:"run_id"`
}

// HandleIsolation handles POST /internal/isolation. The owner resolves the
// run's isolation context from the run's persisted namespace and its current
// policy, exactly as it does for a dispatch, so a worker re-checking a task's
// secret references (before the cache lookup, before resolution, and for replay
// verification) applies a rule the owner revoked since the dispatch. It is
// guarded by the same bearer token (and, on the internal listener, mTLS) as
// /internal/dispatch. 503 while the owner's configured policy is not loaded,
// and 409 when the owner serves no policy file at all: the worker denies.
func (h *Handler) HandleIsolation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req IsolationRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<12))
	if err != nil || json.Unmarshal(body, &req) != nil || req.RunID == uuid.Nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Code: ReasonMalformed, Message: "failed to decode isolation request"})
		return
	}
	namespace, _, err := h.store.RunIsolationInfo(r.Context(), req.RunID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeJSON(w, http.StatusNotFound, ErrorResponse{Code: ReasonMissingRun, Message: "run not found"})
			return
		}
		log.Warn("isolation: run namespace lookup failed", "run_id", req.RunID, "error", err)
		writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{Code: ReasonContention, Message: "run namespace unavailable"})
		return
	}
	isolation, carry, ready := jobdefruntime.ResolveIsolationContext(namespace)
	if !ready {
		writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{Code: ReasonOwnerNotReady, Message: "access policy not loaded"})
		return
	}
	if !carry {
		// This node serves no policy file (an owner restarted without
		// CAESIUM_AUTH_POLICY_FILE after it dispatched the task). Its empty
		// context would read as "no rules" and allow every secret the task
		// names; it cannot vouch for any rule, so the worker denies. Only a
		// worker that received a policy-backed context ever asks.
		writeJSON(w, http.StatusConflict, ErrorResponse{Code: ReasonNoIsolationPolicy, Message: "this node serves no access policy file"})
		return
	}
	isolation.Namespace = namespace
	writeJSON(w, http.StatusOK, isolation)
}

// FetchIsolation asks the owner at ownerBaseURL for runID's current isolation
// context (POST /internal/isolation). Every failure - an unreachable owner, a
// non-200 answer, an oversized or malformed body - is an error; callers deny.
func FetchIsolation(ctx context.Context, ownerBaseURL, token string, runID uuid.UUID) (*jobdefruntime.IsolationContext, error) {
	body, err := json.Marshal(IsolationRequest{RunID: runID})
	if err != nil {
		return nil, fmt.Errorf("isolation: marshal: %w", err)
	}
	fetchCtx, cancel := context.WithTimeout(ctx, dispatchPostTimeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(fetchCtx, http.MethodPost, strings.TrimRight(ownerBaseURL, "/")+"/internal/isolation", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("isolation: new request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := internalClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("isolation: http: %w: %w", ErrPeerUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, MaxDispatchBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("isolation: read: %w", err)
	}
	if len(respBody) > MaxDispatchBodyBytes {
		return nil, fmt.Errorf("isolation: answer exceeds %d bytes", MaxDispatchBodyBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("isolation: owner returned status %d", resp.StatusCode)
	}
	var out jobdefruntime.IsolationContext
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("isolation: decode: %w", err)
	}
	return &out, nil
}
