// Package dispatch implements the Phase 2 run-owner push-dispatch machinery.
//
// Two internal HTTP endpoints are defined:
//
//	POST /internal/dispatch  – owner → worker: push a ready task to a specific worker.
//	POST /internal/complete  – worker → owner: report task outcome back to owner.
//
// Both endpoints are guarded by the existing CAESIUM_INTERNAL_WAKEUP_TOKEN
// bearer-token check.  mTLS is recommended but not yet enforced in Phase A
// (Phase B will require it).  A startup log.Warn is emitted when owner mode
// is on without mTLS material configured.
//
// When CAESIUM_RUN_OWNER_ENABLED=false (default), these handlers are never
// registered and the system behaves byte-identically to Phase 1.
package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/run"
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
)

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
	Deadline        time.Time `json:"deadline"`
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
	Error           string            `json:"error,omitempty"`
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

// Handler holds the dependencies needed to serve the dispatch and complete
// endpoints.
type Handler struct {
	store      *run.Store
	leaseStore *run.LeaseStore
	nodeID     string
	token      string
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

// authorized checks the Bearer token in the request's Authorization header.
func (h *Handler) authorized(r *http.Request) bool {
	if h.token == "" {
		return false
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.EqualFold(auth[:min(len(auth), 7)], "bearer ") {
		return strings.TrimSpace(auth[7:]) == h.token
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

	// Accept the dispatch: atomically claim the task and mark it as running.
	// ClaimTaskForDispatch transitions pending→running with claimed_by=nodeID
	// in one UPDATE (equivalent to ClaimNext but targeting a specific task).
	if err := h.store.ClaimTaskForDispatch(req.RunID, req.TaskID, h.nodeID, 5*time.Minute); err != nil {
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

	w.WriteHeader(http.StatusAccepted)
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
		metrics.CompleteRejectedTotal.WithLabelValues(ReasonMalformed).Inc()
		writeJSON(w, http.StatusConflict, ErrorResponse{
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

	// Rule 1: this node must currently own the run.
	owned, err := h.leaseStore.IsOwner(ctx, h.nodeID, req.RunID)
	if err != nil {
		log.Error("complete: IsOwner check failed",
			"run_id", req.RunID,
			"node_id", h.nodeID,
			"error", err,
		)
		metrics.CompleteRejectedTotal.WithLabelValues(ReasonNotOwner).Inc()
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Code:    ReasonNotOwner,
			Message: "failed to verify run ownership",
		})
		return
	}
	if !owned {
		metrics.CompleteRejectedTotal.WithLabelValues(ReasonNotOwner).Inc()
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Code:    ReasonNotOwner,
			Message: "this node does not currently own the run",
		})
		return
	}

	// Rule 2: validate owner_generation against the current lease.
	lease, err := h.leaseStore.GetLease(ctx, req.RunID)
	if err != nil {
		metrics.CompleteRejectedTotal.WithLabelValues(ReasonMissingRun).Inc()
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Code:    ReasonMissingRun,
			Message: "run lease not found",
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

	// Rules 3 & 4 are enforced by the ClaimNext-path functions via
	// claimed_by check (ErrTaskClaimMismatch) and status == "running"
	// implicit in the update query.  We pass workerNode as the claimedBy
	// fence and let the DB do the filtering.
	switch run.TaskStatus(req.Status) {
	case run.TaskStatusSucceeded, run.TaskStatusFailed:
		var applyErr error
		if run.TaskStatus(req.Status) == run.TaskStatusSucceeded {
			applyErr = h.store.CompleteTaskClaimed(req.RunID, req.TaskID, req.Result, req.WorkerNode, req.Outputs, nil)
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
		applyErr := h.store.CacheHitTaskClaimed(req.RunID, req.TaskID, source, req.Result, req.WorkerNode, req.Outputs, nil)
		if applyErr == run.ErrTaskClaimMismatch {
			metrics.CompleteRejectedTotal.WithLabelValues(ReasonWrongWorker).Inc()
			writeJSON(w, http.StatusConflict, ErrorResponse{
				Code:    ReasonWrongWorker,
				Message: "task claimed_by mismatch or task not in running state",
			})
			return
		}
		if applyErr != nil {
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

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("dispatch: new request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")

	// Phase B will configure mTLS on this client using CAESIUM_INTERNAL_MTLS_*.
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(httpReq)
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

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(httpReq)
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
	return nil, fmt.Errorf("complete: owner returned status %d", resp.StatusCode)
}

// WarnIfNoMTLS emits a startup warning when owner mode is on but no mTLS
// material is configured.  Phase B will turn this into a hard error.
func WarnIfNoMTLS() {
	// Phase A: mTLS is recommended, not required.  Log a warning so operators
	// know they should configure it before Phase B ships.
	log.Warn(
		"run-owner mode is enabled without mTLS material configured; " +
			"this is not a supported configuration for production use. " +
			"Phase B will require CAESIUM_INTERNAL_MTLS_CA, " +
			"CAESIUM_INTERNAL_MTLS_CERT, and CAESIUM_INTERNAL_MTLS_KEY.",
	)
}
