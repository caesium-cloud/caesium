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
	"crypto/hmac"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/dqlite"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
)

// Rejection reason labels for caesium_complete_rejected_total.
const (
	ReasonStaleGeneration = "stale_generation"
	ReasonWrongWorker     = "wrong_worker"
	ReasonInvalidStatus   = "invalid_status"
	ReasonTaskNotRunning  = "task_not_running"
	ReasonNotOwner        = "not_owner"
	ReasonMissingRun      = "missing_run"
	ReasonMalformed       = "malformed"
	// ReasonContention labels caesium_complete_retryable_total when the owner
	// could not apply a completion because of transient dqlite contention and
	// answered 503 so the worker retries.  It is NOT a fence violation.
	ReasonContention = "contention"
)

// ErrOwnerBusy is returned by PostComplete when the owner answered 503 Service
// Unavailable: it could not apply the completion because of transient dqlite
// contention and is asking the worker to retry the identical request.  This is
// distinct from a fence rejection (409), which is terminal — callers should
// retry on ErrOwnerBusy and give up on any other error.
var ErrOwnerBusy = errors.New("owner busy: retryable")

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
	RunID           uuid.UUID `json:"run_id"`
	TaskID          uuid.UUID `json:"task_id"`
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
}

// CompleteRequest is the envelope sent by a worker back to the owner when a
// task execution finishes.
type CompleteRequest struct {
	RunID           uuid.UUID         `json:"run_id"`
	TaskID          uuid.UUID         `json:"task_id"`
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
	Error            string   `json:"error,omitempty"`
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
		return hmac.Equal([]byte(strings.TrimSpace(auth[7:])), []byte(h.token))
	}
	return false
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
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil || json.Unmarshal(body, &req) != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{
			Code:    ReasonMalformed,
			Message: "failed to decode dispatch request",
		})
		return
	}

	// Validate that this node is the intended recipient.
	if req.WorkerNode != h.nodeID {
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Code:    ReasonWrongWorker,
			Message: fmt.Sprintf("this node is %q, dispatch addressed to %q", h.nodeID, req.WorkerNode),
		})
		return
	}

	// Derive the claim TTL from the envelope's deadline so a tight per-task
	// deadline doesn't leave a stale 5-min claim if execution finishes early.
	// Floor at 30s so the renewal ticker has room to extend long-running tasks.
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
	if err := h.store.ClaimTaskForDispatch(req.RunID, req.TaskID, h.nodeID, req.OwnerGeneration, ttl, trustOwnerReadiness); err != nil {
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
	taskRun, err := h.store.LoadDispatchedTaskRun(req.RunID, req.TaskID, h.nodeID)
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
	}); submitErr != nil {
		h.rollbackClaim(req)
		log.Warn("dispatch: worker could not accept task; rolled back claim",
			"run_id", req.RunID,
			"task_id", req.TaskID,
			"error", submitErr,
		)
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Code:    ReasonTaskNotRunning,
			Message: "worker busy; task returned to dispatch pool",
		})
		return
	}

	w.WriteHeader(http.StatusAccepted)
}

// rollbackClaim reverts a just-claimed task back to the dispatchable pending
// state so the owner re-dispatches it.  Logged but not surfaced to the caller
// beyond the 409 the caller already returns; a failed rollback is rare (the
// claim lease still expires and ClaimNext recovery covers it).
func (h *Handler) rollbackClaim(req DispatchRequest) {
	if err := h.store.ReleaseTaskClaim(req.RunID, req.TaskID, h.nodeID, req.OwnerGeneration); err != nil {
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

	// Rule 5: validate status vocabulary.
	if !ValidCompleteStatuses[req.Status] {
		metrics.CompleteRejectedTotal.WithLabelValues(ReasonInvalidStatus).Inc()
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Code:    ReasonInvalidStatus,
			Message: fmt.Sprintf("invalid status %q; must be one of {succeeded, failed, cached}", req.Status),
		})
		return
	}

	// Rules 1 & 2 in a single DB call: GetLease returns the row, we check
	// ownership (owner_node, expiry) and generation in memory.
	lease, err := h.leaseStore.GetLease(ctx, req.RunID)
	if err != nil {
		metrics.CompleteRejectedTotal.WithLabelValues(ReasonMissingRun).Inc()
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Code:    ReasonMissingRun,
			Message: "run lease not found",
		})
		return
	}
	if lease.OwnerNode != h.nodeID || lease.LeaseExpiresAt.Before(time.Now().UTC()) {
		metrics.CompleteRejectedTotal.WithLabelValues(ReasonNotOwner).Inc()
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Code:    ReasonNotOwner,
			Message: "this node does not currently own the run",
		})
		return
	}
	if lease.Generation != req.OwnerGeneration {
		metrics.CompleteRejectedTotal.WithLabelValues(ReasonStaleGeneration).Inc()
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Code:    ReasonStaleGeneration,
			Message: fmt.Sprintf("owner generation mismatch: expected %d, got %d", lease.Generation, req.OwnerGeneration),
		})
		return
	}

	// Run-owner in-memory path: when enabled and this node holds the run's
	// in-memory state, advance the DAG in memory and persist terminal-only rows
	// (no per-transition SQL advancement).  A run not tracked here (Owned=false)
	// falls through to the SQL path below as a safety net.
	if h.ownerManager != nil {
		res, omErr := h.ownerManager.Complete(
			req.RunID, req.TaskID, run.TaskStatus(req.Status),
			req.Result, req.Error, req.WorkerNode, req.Outputs, req.BranchSelections,
		)
		if omErr != nil {
			if dqlite.IsContentionError(omErr) {
				h.rejectRetryable(w, req, omErr)
				return
			}
			if errors.Is(omErr, run.ErrTaskClaimMismatch) {
				metrics.CompleteRejectedTotal.WithLabelValues(ReasonWrongWorker).Inc()
				writeJSON(w, http.StatusConflict, ErrorResponse{
					Code:    ReasonWrongWorker,
					Message: "task claimed_by mismatch or task not in running state",
				})
				return
			}
			log.Error("complete: owner-manager apply failed",
				"run_id", req.RunID, "task_id", req.TaskID, "status", req.Status, "error", omErr)
			writeJSON(w, http.StatusConflict, ErrorResponse{
				Code:    ReasonTaskNotRunning,
				Message: "failed to apply task completion",
			})
			return
		}
		if res.Owned {
			writeJSON(w, http.StatusOK, CompleteResponse{Accepted: true})
			return
		}
		// Not tracked in memory here — fall through to the SQL path.
	}

	// Rules 3 & 4 are enforced by the ClaimNext-path functions via
	// claimed_by check (ErrTaskClaimMismatch) and status == "running"
	// implicit in the update query.  We pass workerNode as the claimedBy
	// fence and let the DB do the filtering.
	switch run.TaskStatus(req.Status) {
	case run.TaskStatusSucceeded, run.TaskStatusFailed:
		var applyErr error
		if run.TaskStatus(req.Status) == run.TaskStatusSucceeded {
			applyErr = h.store.CompleteTaskClaimed(req.RunID, req.TaskID, req.Result, req.WorkerNode, req.Outputs, req.BranchSelections)
		} else {
			applyErr = h.store.FailTaskClaimed(req.RunID, req.TaskID, fmt.Errorf("%s", req.Error), req.WorkerNode)
		}
		if applyErr == run.ErrTaskClaimMismatch {
			metrics.CompleteRejectedTotal.WithLabelValues(ReasonWrongWorker).Inc()
			writeJSON(w, http.StatusConflict, ErrorResponse{
				Code:    ReasonWrongWorker,
				Message: "task claimed_by mismatch or task not in running state",
			})
			return
		}
		if applyErr != nil {
			if dqlite.IsContentionError(applyErr) {
				h.rejectRetryable(w, req, applyErr)
				return
			}
			log.Error("complete: apply failed",
				"run_id", req.RunID,
				"task_id", req.TaskID,
				"status", req.Status,
				"error", applyErr,
			)
			writeJSON(w, http.StatusConflict, ErrorResponse{
				Code:    ReasonTaskNotRunning,
				Message: "failed to apply task completion",
			})
			return
		}

	case run.TaskStatusCached:
		source := run.CacheHitSource{RunID: req.RunID}
		applyErr := h.store.CacheHitTaskClaimed(req.RunID, req.TaskID, source, req.Result, req.WorkerNode, req.Outputs, req.BranchSelections)
		if applyErr == run.ErrTaskClaimMismatch {
			metrics.CompleteRejectedTotal.WithLabelValues(ReasonWrongWorker).Inc()
			writeJSON(w, http.StatusConflict, ErrorResponse{
				Code:    ReasonWrongWorker,
				Message: "task claimed_by mismatch or task not in running state",
			})
			return
		}
		if applyErr != nil {
			if dqlite.IsContentionError(applyErr) {
				h.rejectRetryable(w, req, applyErr)
				return
			}
			log.Error("complete: CacheHitTaskClaimed failed",
				"run_id", req.RunID,
				"task_id", req.TaskID,
				"error", applyErr,
			)
			writeJSON(w, http.StatusConflict, ErrorResponse{
				Code:    ReasonTaskNotRunning,
				Message: "failed to apply cache-hit completion",
			})
			return
		}
	}

	writeJSON(w, http.StatusOK, CompleteResponse{Accepted: true})
}

// rejectRetryable answers a completion the owner could not apply because of
// transient dqlite contention.  It returns 503 (not 409) so the worker knows
// the request is safe to re-send once the leader's contention clears, and logs
// at warn rather than error because this is expected under burst load and is
// not a lost completion unless the worker exhausts its own retries.
func (h *Handler) rejectRetryable(w http.ResponseWriter, req CompleteRequest, applyErr error) {
	metrics.CompleteRetryableTotal.WithLabelValues(ReasonContention).Inc()
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

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

// PostDispatch sends a DispatchRequest to the target worker node and returns
// whether the worker accepted (202) or rejected (409).  On rejection or
// network error, the caller should fall back to writing the task to the DB
// with claimed_by="" for ClaimNext recovery.
func PostDispatch(ctx context.Context, targetURL, token string, req DispatchRequest) (bool, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return false, fmt.Errorf("dispatch: marshal: %w", err)
	}

	// Fail fast on an unreachable peer: a dispatch is cheap to retry on the next
	// tick (to a different peer), so a dead node in the round-robin must not hang
	// the loop for the client's full 30s timeout — critical during failover, when
	// the just-crashed owner can still be in the peer list.
	dialCtx, cancel := context.WithTimeout(ctx, dispatchPostTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(dialCtx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("dispatch: new request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := internalClient.Do(httpReq)
	if err != nil {
		return false, fmt.Errorf("dispatch: http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode == http.StatusAccepted {
		return true, nil
	}
	// 409 or any non-202: worker rejected.
	return false, nil
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
	// 503: the owner hit transient contention applying the completion and wants
	// the worker to retry the same request.  Wrap ErrOwnerBusy so the caller can
	// distinguish it from a terminal fence rejection (409) via errors.Is.
	if resp.StatusCode == http.StatusServiceUnavailable {
		return nil, fmt.Errorf("complete: owner returned status %d: %w", resp.StatusCode, ErrOwnerBusy)
	}
	return nil, fmt.Errorf("complete: owner returned status %d", resp.StatusCode)
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
