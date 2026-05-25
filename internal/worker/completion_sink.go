package worker

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/caesium-cloud/caesium/internal/dispatch"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/log"
)

// ownerBusyBackoffs schedules the worker's retries when the owner answers a
// completion with 503 (dispatch.ErrOwnerBusy) because of transient dqlite
// contention.  It is intentionally a touch longer than the owner's own
// in-handler busy-retry budget so a burst of simultaneous completions spreads
// out across workers rather than re-colliding on the leader on every retry.
// ~1.55s total across 6 retries.
var ownerBusyBackoffs = []time.Duration{
	25 * time.Millisecond,
	50 * time.Millisecond,
	100 * time.Millisecond,
	200 * time.Millisecond,
	400 * time.Millisecond,
	800 * time.Millisecond,
}

// CompletionSink is the abstraction the runtime executor calls to finalize a
// task's terminal outcome.  It exists so a single execution path can route its
// completion either to the local DB (the ClaimNext pull path, unchanged from
// Phase 1) or back to the owning node over /internal/complete (the run-owner
// push-dispatch path).
//
// The three methods mirror the three store finalization calls the executor
// made directly before this abstraction was introduced:
//
//	Succeeded → run.Store.CompleteTaskClaimed
//	Failed    → run.Store.FailTaskClaimed
//	Cached    → run.Store.CacheHitTaskClaimed
//
// The owner re-derives the real terminal status from the result string in
// CompleteTaskClaimed, so a "failure" result routed through Succeeded still
// lands as TaskStatusFailed on the owner — byte-identical to the local path.
type CompletionSink interface {
	// Succeeded finalizes a task that ran to a normal completion (whatever the
	// underlying container result was — the result string carries it).
	Succeeded(ctx context.Context, taskRun *models.TaskRun, result string, outputs map[string]string, branchSelections []string) error
	// Failed finalizes a task whose attempts were exhausted with an error.
	Failed(ctx context.Context, taskRun *models.TaskRun, failure error) error
	// Cached finalizes a task satisfied from the result cache.
	Cached(ctx context.Context, taskRun *models.TaskRun, source run.CacheHitSource, result string, outputs map[string]string, branchSelections []string) error
}

// localSink is the default sink used by ClaimNext'd tasks.  It calls the same
// run.Store.*Claimed methods the executor called inline before the sink
// abstraction existed, so the pull path is byte-identical to Phase 1.
type localSink struct {
	store *run.Store
}

// NewLocalSink returns the default DB-backed completion sink.
func NewLocalSink(store *run.Store) CompletionSink {
	return &localSink{store: store}
}

func (s *localSink) Succeeded(_ context.Context, taskRun *models.TaskRun, result string, outputs map[string]string, branchSelections []string) error {
	return s.store.CompleteTaskClaimed(taskRun.JobRunID, taskRun.TaskID, result, taskRun.ClaimedBy, outputs, branchSelections)
}

func (s *localSink) Failed(_ context.Context, taskRun *models.TaskRun, failure error) error {
	return s.store.FailTaskClaimed(taskRun.JobRunID, taskRun.TaskID, failure, taskRun.ClaimedBy)
}

func (s *localSink) Cached(_ context.Context, taskRun *models.TaskRun, source run.CacheHitSource, result string, outputs map[string]string, branchSelections []string) error {
	return s.store.CacheHitTaskClaimed(taskRun.JobRunID, taskRun.TaskID, source, result, taskRun.ClaimedBy, outputs, branchSelections)
}

// completePoster is the seam the owner sink uses to reach the owner's
// /internal/complete endpoint.  Production wires it to dispatch.PostComplete;
// tests inject a fake that records the CompleteRequest.
type completePoster func(ctx context.Context, ownerURL, token string, req dispatch.CompleteRequest) (*dispatch.CompleteResponse, error)

// ownerSink routes a dispatched task's terminal outcome back to the run owner
// via POST /internal/complete instead of writing the hot rows locally.  This
// keeps the owner the single writer for its run's coordination rows (preserving
// the owner_generation fence) and sets up cleanly for the later in-memory-state
// layer where authoritative state lives in the owner's memory.
//
// A dispatched task carries the owner_generation, attempt, owner base URL, and
// worker_node identity it arrived with; those are threaded onto the sink so the
// CompleteRequest envelope matches what the owner expects to fence against.
type ownerSink struct {
	ownerBaseURL string
	token        string
	workerNode   string
	generation   int64
	attempt      int
	post         completePoster
}

// newOwnerSink builds an owner-routed completion sink from a dispatch envelope's
// fields.  post is the function used to POST to the owner (dispatch.PostComplete
// in production, a fake in tests).
func newOwnerSink(meta dispatchMeta, post completePoster) *ownerSink {
	if post == nil {
		post = dispatch.PostComplete
	}
	return &ownerSink{
		ownerBaseURL: meta.OwnerBaseURL,
		token:        meta.Token,
		workerNode:   meta.WorkerNode,
		generation:   meta.OwnerGeneration,
		attempt:      meta.Attempt,
		post:         post,
	}
}

func (s *ownerSink) Succeeded(ctx context.Context, taskRun *models.TaskRun, result string, outputs map[string]string, branchSelections []string) error {
	return s.send(ctx, taskRun, dispatch.CompleteRequest{
		Status:           string(run.TaskStatusSucceeded),
		Result:           result,
		Outputs:          outputs,
		BranchSelections: branchSelections,
	})
}

func (s *ownerSink) Failed(ctx context.Context, taskRun *models.TaskRun, failure error) error {
	errMsg := ""
	if failure != nil {
		errMsg = failure.Error()
	}
	return s.send(ctx, taskRun, dispatch.CompleteRequest{
		Status: string(run.TaskStatusFailed),
		Error:  errMsg,
	})
}

func (s *ownerSink) Cached(ctx context.Context, taskRun *models.TaskRun, source run.CacheHitSource, result string, outputs map[string]string, branchSelections []string) error {
	// The owner reconstructs CacheHitSource on its side; the worker only needs
	// to report result + outputs + branch selections.  (Phase A's owner handler
	// builds a CacheHitSource{RunID: req.RunID}; the richer origin metadata is a
	// Phase B concern and not load-bearing for DAG advancement.)
	return s.send(ctx, taskRun, dispatch.CompleteRequest{
		Status:           string(run.TaskStatusCached),
		Result:           result,
		Outputs:          outputs,
		BranchSelections: branchSelections,
	})
}

// send fills the fencing fields common to every completion and POSTs the
// envelope to the owner.  When the owner answers 503 (dispatch.ErrOwnerBusy)
// because of transient contention, send retries the same request with bounded
// backoff before giving up; a true fence rejection (409) or a network failure
// is terminal.  A failure to report is logged and surfaced as a dispatch-side
// metric (the task's claim lease eventually expires and recovery re-dispatches)
// — the error is never swallowed silently.
func (s *ownerSink) send(ctx context.Context, taskRun *models.TaskRun, req dispatch.CompleteRequest) error {
	req.RunID = taskRun.JobRunID
	req.TaskID = taskRun.TaskID
	req.OwnerGeneration = s.generation
	req.Attempt = s.attempt
	req.WorkerNode = s.workerNode

	url := s.ownerBaseURL + "/internal/complete"

	for attempt := 0; ; attempt++ {
		resp, err := s.post(ctx, url, s.token, req)
		if err == nil {
			if resp != nil && !resp.Accepted {
				// The owner fenced the completion (stale generation, wrong
				// worker, etc.).  This is not a transport error; the owner
				// deliberately rejected it.
				metrics.CompleteReportFailedTotal.WithLabelValues("owner_rejected").Inc()
				log.Warn("dispatched task: owner rejected completion",
					"run_id", req.RunID,
					"task_id", req.TaskID,
					"status", req.Status,
					"reason", resp.Reason,
				)
				return fmt.Errorf("owner sink: owner rejected completion: %s", resp.Reason)
			}
			return nil
		}

		// Transient owner-side contention: the owner asked us to re-send the
		// identical request once its leader frees up.  Back off and retry until
		// the schedule (or the context) is exhausted.
		if errors.Is(err, dispatch.ErrOwnerBusy) && attempt < len(ownerBusyBackoffs) {
			if sleepErr := sleepOwnerBusy(ctx, ownerBusyBackoffs[attempt]); sleepErr != nil {
				// Context cancelled/expired while backing off. Surface the
				// cancellation — not ErrOwnerBusy — so the executor's
				// context.Canceled branch fires: the container already ran to
				// completion, so it must not be marked failed or re-executed.
				return fmt.Errorf("owner sink: report completion aborted: %w", sleepErr)
			}
			continue
		}

		reason := "post_error"
		if errors.Is(err, dispatch.ErrOwnerBusy) {
			reason = "owner_busy"
		}
		metrics.CompleteReportFailedTotal.WithLabelValues(reason).Inc()
		log.Error("dispatched task: failed to report completion to owner",
			"run_id", req.RunID,
			"task_id", req.TaskID,
			"owner_url", s.ownerBaseURL,
			"status", req.Status,
			"attempts", attempt+1,
			"error", err,
		)
		return fmt.Errorf("owner sink: report completion: %w", err)
	}
}

// sleepOwnerBusy waits base (minus up to 20% jitter) or returns early if ctx is
// cancelled.  The jitter de-synchronises a thundering herd of workers all
// retrying a contended owner at the same instant.
func sleepOwnerBusy(ctx context.Context, base time.Duration) error {
	d := base
	if base > 0 {
		if maxJitter := int64(base / 5); maxJitter > 0 {
			d = base - time.Duration(rand.Int64N(maxJitter+1))
		}
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// dispatchMeta is the owner-routing metadata a dispatched task carries from the
// /internal/dispatch envelope through to its completion sink.  ClaimNext'd
// (pull-path) tasks have no dispatchMeta and use the local sink.
type dispatchMeta struct {
	OwnerBaseURL    string
	Token           string
	WorkerNode      string
	OwnerGeneration int64
	Attempt         int
}

// dispatchMetaKey is the context key under which a dispatched task's owner
// metadata is threaded from the worker to the runtime executor.  Using the
// context (rather than a field on TaskRun or a shared map) keeps the
// TaskExecutor signature unchanged and avoids any shared mutable state.
type dispatchMetaKeyType struct{}

var dispatchMetaKey = dispatchMetaKeyType{}

// withDispatchMeta returns a context carrying the dispatched-task owner metadata.
func withDispatchMeta(ctx context.Context, meta dispatchMeta) context.Context {
	return context.WithValue(ctx, dispatchMetaKey, meta)
}

// dispatchMetaFrom extracts the owner metadata from ctx, if present.  ok is
// false for ClaimNext'd tasks, which select the local sink.
func dispatchMetaFrom(ctx context.Context) (dispatchMeta, bool) {
	meta, ok := ctx.Value(dispatchMetaKey).(dispatchMeta)
	return meta, ok
}
