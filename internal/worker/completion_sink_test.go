package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/dispatch"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// recordingPoster captures the CompleteRequest the owner sink POSTs so tests
// can assert the envelope fields without standing up an HTTP server.
type recordingPoster struct {
	calls []dispatch.CompleteRequest
	url   string
	token string
	resp  *dispatch.CompleteResponse
	err   error
}

func (p *recordingPoster) post(_ context.Context, url, token string, req dispatch.CompleteRequest) (*dispatch.CompleteResponse, error) {
	p.url = url
	p.token = token
	p.calls = append(p.calls, req)
	if p.err != nil {
		return nil, p.err
	}
	if p.resp != nil {
		return p.resp, nil
	}
	return &dispatch.CompleteResponse{Accepted: true}, nil
}

func sampleTaskRun() *models.TaskRun {
	return &models.TaskRun{
		ID:        uuid.New(),
		JobRunID:  uuid.New(),
		TaskID:    uuid.New(),
		ClaimedBy: "10.0.0.5:9001",
	}
}

func ownerMeta() dispatchMeta {
	return dispatchMeta{
		OwnerBaseURL:    "http://10.0.0.1:8080",
		Token:           "tok",
		WorkerNode:      "10.0.0.5:9001",
		OwnerGeneration: 7,
		Attempt:         3,
	}
}

// TestOwnerSink_Succeeded asserts the owner sink POSTs a CompleteRequest with
// all fencing fields and the succeeded status/result/outputs/branches.
func TestOwnerSink_Succeeded(t *testing.T) {
	p := &recordingPoster{}
	meta := ownerMeta()
	sink := newOwnerSink(meta, p.post)
	task := sampleTaskRun()

	outputs := map[string]string{"rows": "42"}
	branches := []string{"left"}
	err := sink.Succeeded(context.Background(), task, "success", outputs, branches)
	require.NoError(t, err)

	require.Len(t, p.calls, 1)
	got := p.calls[0]
	require.Equal(t, task.JobRunID, got.RunID)
	require.Equal(t, task.TaskID, got.TaskID)
	require.Equal(t, int64(7), got.OwnerGeneration)
	require.Equal(t, 3, got.Attempt)
	require.Equal(t, "10.0.0.5:9001", got.WorkerNode)
	require.Equal(t, string(run.TaskStatusSucceeded), got.Status)
	require.Equal(t, "success", got.Result)
	require.Equal(t, outputs, got.Outputs)
	require.Equal(t, branches, got.BranchSelections)
	require.Empty(t, got.Error)

	// URL and token plumbed correctly.
	require.Equal(t, "http://10.0.0.1:8080/internal/complete", p.url)
	require.Equal(t, "tok", p.token)
}

// TestOwnerSink_Failed asserts the failed status and error string are sent.
func TestOwnerSink_Failed(t *testing.T) {
	p := &recordingPoster{}
	sink := newOwnerSink(ownerMeta(), p.post)
	task := sampleTaskRun()

	err := sink.Failed(context.Background(), task, errors.New("boom"))
	require.NoError(t, err)

	require.Len(t, p.calls, 1)
	got := p.calls[0]
	require.Equal(t, string(run.TaskStatusFailed), got.Status)
	require.Equal(t, "boom", got.Error)
	require.Empty(t, got.Result)
}

// TestOwnerSink_Cached asserts the cached status, result, outputs and branch
// selections are sent.
func TestOwnerSink_Cached(t *testing.T) {
	p := &recordingPoster{}
	sink := newOwnerSink(ownerMeta(), p.post)
	task := sampleTaskRun()

	outputs := map[string]string{"k": "v"}
	branches := []string{"b1", "b2"}
	err := sink.Cached(context.Background(), task, run.CacheHitSource{RunID: uuid.New()}, "success", outputs, branches)
	require.NoError(t, err)

	require.Len(t, p.calls, 1)
	got := p.calls[0]
	require.Equal(t, string(run.TaskStatusCached), got.Status)
	require.Equal(t, "success", got.Result)
	require.Equal(t, outputs, got.Outputs)
	require.Equal(t, branches, got.BranchSelections)
}

// TestOwnerSink_PostErrorSurfaces asserts a transport error is returned (not
// swallowed) so the caller logs it and the lease-expiry recovery kicks in.
func TestOwnerSink_PostErrorSurfaces(t *testing.T) {
	p := &recordingPoster{err: errors.New("connection refused")}
	sink := newOwnerSink(ownerMeta(), p.post)

	err := sink.Succeeded(context.Background(), sampleTaskRun(), "success", nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "connection refused")
}

// TestOwnerSink_OwnerRejectionSurfaces asserts an owner fence rejection
// (accepted=false) is returned as an error so it isn't silently lost.
func TestOwnerSink_OwnerRejectionSurfaces(t *testing.T) {
	p := &recordingPoster{resp: &dispatch.CompleteResponse{Accepted: false, Reason: "stale_generation"}}
	sink := newOwnerSink(ownerMeta(), p.post)

	err := sink.Succeeded(context.Background(), sampleTaskRun(), "success", nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "stale_generation")
}

// busyPoster returns dispatch.ErrOwnerBusy (the owner's retryable 503) for the
// first failFor calls — or every call when alwaysBusy — then accepts.  It lets
// the retry tests assert exactly how many times the sink re-posted.
type busyPoster struct {
	calls      int
	failFor    int
	alwaysBusy bool
}

func (p *busyPoster) post(_ context.Context, _, _ string, _ dispatch.CompleteRequest) (*dispatch.CompleteResponse, error) {
	p.calls++
	if p.alwaysBusy || p.calls <= p.failFor {
		return nil, dispatch.ErrOwnerBusy
	}
	return &dispatch.CompleteResponse{Accepted: true}, nil
}

// withFastOwnerBusyBackoffs shrinks the retry schedule to n×1ms for the
// duration of the test so the retry tests don't sleep the real ~1.55s budget.
// Tests using it must not run in parallel while the package var is swapped.
func withFastOwnerBusyBackoffs(t *testing.T, n int) {
	t.Helper()
	orig := ownerBusyBackoffs
	fast := make([]time.Duration, n)
	for i := range fast {
		fast[i] = time.Millisecond
	}
	ownerBusyBackoffs = fast
	t.Cleanup(func() { ownerBusyBackoffs = orig })
}

// TestOwnerSink_RetriesOnOwnerBusyThenSucceeds asserts the sink re-posts the
// identical completion when the owner answers 503 (ErrOwnerBusy) and reports
// success once the owner's contention clears.
func TestOwnerSink_RetriesOnOwnerBusyThenSucceeds(t *testing.T) {
	withFastOwnerBusyBackoffs(t, 5)
	p := &busyPoster{failFor: 2}
	sink := newOwnerSink(ownerMeta(), p.post)

	err := sink.Succeeded(context.Background(), sampleTaskRun(), "success", nil, nil)
	require.NoError(t, err)
	require.Equal(t, 3, p.calls, "two 503s then a success = 3 posts")
}

// TestOwnerSink_OwnerBusyExhaustedSurfaces asserts a sustained 503 is retried
// across the whole schedule and then surfaced as ErrOwnerBusy (not swallowed),
// so lease-expiry recovery can re-dispatch the task.
func TestOwnerSink_OwnerBusyExhaustedSurfaces(t *testing.T) {
	withFastOwnerBusyBackoffs(t, 3)
	p := &busyPoster{alwaysBusy: true}
	sink := newOwnerSink(ownerMeta(), p.post)

	err := sink.Succeeded(context.Background(), sampleTaskRun(), "success", nil, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, dispatch.ErrOwnerBusy)
	require.Equal(t, len(ownerBusyBackoffs)+1, p.calls, "initial attempt + one retry per backoff entry")
}

// TestOwnerSink_TerminalErrorNotRetried asserts a non-busy error (a true fence
// rejection or a network failure) is returned immediately without retrying, so
// the worker never spins on a permanent rejection.
func TestOwnerSink_TerminalErrorNotRetried(t *testing.T) {
	withFastOwnerBusyBackoffs(t, 5)
	p := &recordingPoster{err: errors.New("connection refused")}
	sink := newOwnerSink(ownerMeta(), p.post)

	err := sink.Succeeded(context.Background(), sampleTaskRun(), "success", nil, nil)
	require.Error(t, err)
	require.Len(t, p.calls, 1, "a terminal (non-busy) error must not be retried")
}

// TestOwnerSink_OwnerBusyContextCancelStops asserts a cancelled context aborts
// the retry loop promptly and surfaces the busy error rather than spinning.
func TestOwnerSink_OwnerBusyContextCancelStops(t *testing.T) {
	withFastOwnerBusyBackoffs(t, 5)
	p := &busyPoster{alwaysBusy: true}
	sink := newOwnerSink(ownerMeta(), p.post)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := sink.Succeeded(ctx, sampleTaskRun(), "success", nil, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, dispatch.ErrOwnerBusy)
	require.Equal(t, 1, p.calls, "a pre-cancelled context must stop after the first attempt")
}

// fakeSink is an injectable CompletionSink for executor tests; it records which
// terminal method the executor invoked.
type fakeSink struct {
	succeeded int
	failed    int
	cached    int
	lastErr   error
}

func (f *fakeSink) Succeeded(context.Context, *models.TaskRun, string, map[string]string, []string) error {
	f.succeeded++
	return nil
}

func (f *fakeSink) Failed(_ context.Context, _ *models.TaskRun, err error) error {
	f.failed++
	f.lastErr = err
	return nil
}

func (f *fakeSink) Cached(context.Context, *models.TaskRun, run.CacheHitSource, string, map[string]string, []string) error {
	f.cached++
	return nil
}

// TestSinkFor_SelectsLocalWithoutMeta asserts the executor picks its configured
// local sink for a context with no dispatch metadata (the ClaimNext pull path).
// Uses a fake sink so the test pins the selection wiring, not store behavior.
func TestSinkFor_SelectsLocalWithoutMeta(t *testing.T) {
	local := &fakeSink{}
	e := &runtimeExecutor{localSink: local}

	got := e.sinkFor(context.Background())
	require.Same(t, local, got, "ClaimNext'd tasks must use the configured local sink")
}

// TestSinkFor_SelectsOwnerWithMeta asserts the executor builds an owner sink
// when the context carries dispatch metadata (the push path).
func TestSinkFor_SelectsOwnerWithMeta(t *testing.T) {
	local := &fakeSink{}
	e := &runtimeExecutor{localSink: local}

	ctx := withDispatchMeta(context.Background(), ownerMeta())
	got := e.sinkFor(ctx)
	owner, isOwner := got.(*ownerSink)
	require.True(t, isOwner, "dispatched tasks must use the owner sink")
	require.NotSame(t, CompletionSink(local), got)
	// The owner sink carries the envelope fields from the metadata.
	require.Equal(t, "http://10.0.0.1:8080", owner.ownerBaseURL)
	require.Equal(t, int64(7), owner.generation)
	require.Equal(t, 3, owner.attempt)
}

// TestDispatchMetaRoundTrip asserts metadata threaded through the context is
// recovered intact.
func TestDispatchMetaRoundTrip(t *testing.T) {
	meta := ownerMeta()
	ctx := withDispatchMeta(context.Background(), meta)
	got, ok := dispatchMetaFrom(ctx)
	require.True(t, ok)
	require.Equal(t, meta, got)

	_, ok = dispatchMetaFrom(context.Background())
	require.False(t, ok, "a bare context must not report dispatch metadata")
}
