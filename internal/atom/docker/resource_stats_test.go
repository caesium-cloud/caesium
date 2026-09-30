package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/docker/docker/api/types/container"
	"github.com/stretchr/testify/require"
)

func TestResourceOOMUsesInspectAndHonorsGate(t *testing.T) {
	t.Cleanup(func() { require.NoError(t, env.Process()) })
	limit := int64(64 * 1024 * 1024)
	for _, tc := range []struct {
		name    string
		enabled string
		exit    int
		inspect bool
		memory  int64
		wantOOM bool
		want    atom.Result
	}{
		{name: "inspect OOM", enabled: "true", exit: 137, inspect: true, memory: limit, wantOOM: true, want: atom.ResourceFailure},
		{name: "inspect OOM gate off", enabled: "false", exit: 137, inspect: true, memory: limit, wantOOM: true, want: atom.Killed},
		{name: "SIGKILL with limit", enabled: "true", exit: 137, memory: limit, want: atom.Killed},
		{name: "SIGKILL with limit gate off", enabled: "false", exit: 137, memory: limit, want: atom.Killed},
		{name: "SIGKILL without limit", enabled: "true", exit: 137, want: atom.Killed},
		{name: "success with limit", enabled: "true", memory: limit, want: atom.Success},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CAESIUM_RESOURCE_STATS_ENABLED", tc.enabled)
			require.NoError(t, env.Process())
			metadata := newContainer("runtime", &container.State{ExitCode: tc.exit, OOMKilled: tc.inspect})
			if tc.memory > 0 {
				metadata.HostConfig = &container.HostConfig{Resources: container.Resources{Memory: tc.memory}}
			}
			a := &Atom{metadata: metadata}
			require.Equal(t, tc.want, a.Result())
			require.Equal(t, tc.exit, *a.ExitCode())
			require.True(t, a.ResourceOutcome().OOMKnown)
			require.Equal(t, tc.wantOOM, a.ResourceOutcome().OOMKilled)
			if tc.memory > 0 {
				require.Equal(t, tc.memory, *a.ResourceOutcome().MemoryLimitBytes)
			} else {
				require.Nil(t, a.ResourceOutcome().MemoryLimitBytes)
			}
		})
	}
}

func TestResourceStatsDockerDecodesSnapshotAndRejectsAbsence(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		available bool
		memory    int64
		cpu       float64
	}{
		{name: "measured", body: `{"read":"2026-09-09T00:00:00Z","memory_stats":{"usage":4096},"cpu_stats":{"cpu_usage":{"total_usage":2500000000}}}`, available: true, memory: 4096, cpu: 2.5},
		{name: "empty", body: `{}`},
		{name: "timestamp without usage", body: `{"read":"2026-09-09T00:00:00Z"}`},
		{name: "measured zeros", body: `{"read":"2026-09-09T00:00:00Z","memory_stats":{"usage":0},"cpu_stats":{"cpu_usage":{"total_usage":0}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &mockDockerBackend{}
			backend.On("ContainerStatsOneShot", "runtime").Return(container.StatsResponseReader{Body: io.NopCloser(strings.NewReader(tc.body))}, nil).Once()
			engine := &dockerEngine{ctx: context.Background(), backend: backend}
			stats, err := engine.Stats(&atom.EngineStatsRequest{ID: "runtime"})
			if tc.available {
				require.NoError(t, err)
				require.Equal(t, tc.memory, *stats.MemoryBytes)
				require.Equal(t, tc.cpu, *stats.CPUSeconds)
			} else {
				require.ErrorIs(t, err, atom.ErrStatsUnavailable)
				require.Nil(t, stats.MemoryBytes)
				require.Nil(t, stats.CPUSeconds)
			}
			backend.AssertExpectations(t)
		})
	}
}

type waitOutcomeBackend struct {
	dockerBackend
	inspect func(context.Context) (container.InspectResponse, error)
}

func (b *waitOutcomeBackend) ContainerInspect(ctx context.Context, _ string) (container.InspectResponse, error) {
	return b.inspect(ctx)
}
func (b *waitOutcomeBackend) ContainerStatsOneShot(context.Context, string) (container.StatsResponseReader, error) {
	return container.StatsResponseReader{}, atom.ErrStatsUnavailable
}
func (b *waitOutcomeBackend) ContainerWait(context.Context, string, container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	result := make(chan container.WaitResponse, 1)
	result <- container.WaitResponse{StatusCode: 137}
	return result, make(chan error)
}

func TestWaitCapturesLateOOMEvidenceWithoutInferringSIGKILL(t *testing.T) {
	t.Cleanup(func() { require.NoError(t, env.Process()) })
	for _, tc := range []struct {
		name                        string
		enabled, lateOOM, restarted bool
		memory                      int64
		lateOOMDelay                time.Duration
	}{
		// Docker can deliver OOM metadata after its wait notification. This
		// deliberately publishes evidence after the previous one-second window.
		{name: "late OOM after prior window", enabled: true, lateOOM: true, lateOOMDelay: 1100 * time.Millisecond},
		{name: "ordinary SIGKILL", enabled: true},
		{name: "gate disabled", lateOOM: true},
		{name: "replacement runtime", enabled: true, lateOOM: true, restarted: true},
		{name: "SIGKILL with limit", enabled: true, memory: 64 * 1024 * 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CAESIUM_RESOURCE_STATS_ENABLED", fmt.Sprint(tc.enabled))
			require.NoError(t, env.Process())
			calls := 0
			backend := &waitOutcomeBackend{inspect: func(ctx context.Context) (container.InspectResponse, error) {
				calls++
				state := &container.State{Status: "exited", ExitCode: 137, StartedAt: "original"}
				if tc.lateOOMDelay > 0 && calls > 1 {
					// Start this delay after the terminal wait's first inspect. A
					// one-second production window must expire before it can observe
					// this metadata, independent of test setup scheduling.
					timer := time.NewTimer(tc.lateOOMDelay)
					defer timer.Stop()
					select {
					case <-timer.C:
					case <-ctx.Done():
						return container.InspectResponse{}, ctx.Err()
					}
				}
				if tc.lateOOM && calls > 1 {
					state.OOMKilled = tc.lateOOM
					if tc.restarted {
						state.StartedAt = "replacement"
					}
				}
				resp := newContainer("runtime", state)
				if tc.memory > 0 {
					resp.HostConfig = &container.HostConfig{Resources: container.Resources{Memory: tc.memory}}
				}
				return resp, nil
			}}
			engine := &dockerEngine{ctx: context.Background(), backend: backend}
			final, err := engine.Wait(&atom.EngineWaitRequest{ID: "runtime", Context: context.Background()})
			require.NoError(t, err)
			want := atom.Killed
			if tc.enabled && tc.lateOOM && !tc.restarted {
				want = atom.ResourceFailure
			}
			require.Equal(t, want, final.Result())
			require.Equal(t, want == atom.ResourceFailure, final.(*Atom).ResourceOutcome().OOMKilled)
			// A converged observation is known evidence, OOM or not; a replaced
			// runtime cannot settle this attempt's verdict.
			require.Equal(t, !tc.enabled || !tc.restarted, final.(*Atom).ResourceOutcome().OOMKnown)
			if !tc.enabled {
				require.Equal(t, 1, calls)
			}
		})
	}
}

func TestWaitOutcomeReinspectionHonorsCancellation(t *testing.T) {
	t.Cleanup(func() { require.NoError(t, env.Process()) })
	t.Setenv("CAESIUM_RESOURCE_STATS_ENABLED", "true")
	require.NoError(t, env.Process())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	backend := &waitOutcomeBackend{inspect: func(ctx context.Context) (container.InspectResponse, error) {
		calls++
		if calls > 1 {
			cancel()
			<-ctx.Done()
			return container.InspectResponse{}, ctx.Err()
		}
		return newContainer("runtime", &container.State{Status: "exited", ExitCode: 137}), nil
	}}
	engine := &dockerEngine{ctx: context.Background(), backend: backend}
	start := time.Now()
	_, err := engine.Wait(&atom.EngineWaitRequest{ID: "runtime", Context: ctx})
	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, time.Since(start), 500*time.Millisecond)
}

func shortenOOMWindow(t *testing.T, settle, poll, finalInspect time.Duration) {
	t.Helper()
	prevSettle, prevPoll, prevFinal := dockerOOMMetadataSettleTimeout, dockerOOMMetadataPollInterval, dockerOOMMetadataFinalInspectTimeout
	dockerOOMMetadataSettleTimeout, dockerOOMMetadataPollInterval, dockerOOMMetadataFinalInspectTimeout = settle, poll, finalInspect
	t.Cleanup(func() {
		dockerOOMMetadataSettleTimeout, dockerOOMMetadataPollInterval, dockerOOMMetadataFinalInspectTimeout = prevSettle, prevPoll, prevFinal
	})
}

// TestWaitKeepsOOMUnknownWhenConvergenceFails drives the real engine.Wait path
// and the resource sampler: an initial exited/137/false snapshot is not a
// verdict until a follow-up observation of the same attempt converges.
func TestWaitKeepsOOMUnknownWhenConvergenceFails(t *testing.T) {
	t.Cleanup(func() { require.NoError(t, env.Process()) })
	t.Setenv("CAESIUM_RESOURCE_STATS_ENABLED", "true")
	require.NoError(t, env.Process())
	const settle = 200 * time.Millisecond
	shortenOOMWindow(t, settle, 10*time.Millisecond, 100*time.Millisecond)
	errDaemon := errors.New("docker daemon unavailable")
	for _, tc := range []struct {
		name string
		// inWindow and postWindow answer follow-up inspects; the first inspect
		// always returns the exited/137/false snapshot.
		inWindow, postWindow  func(context.Context) (bool, error)
		wantKnown, wantOOM    bool
		wantResult            atom.Result
		wantMultipleFollowUps bool
	}{
		{
			name:                  "every follow-up inspect fails",
			inWindow:              func(context.Context) (bool, error) { return false, errDaemon },
			postWindow:            func(context.Context) (bool, error) { return false, errDaemon },
			wantResult:            atom.Killed,
			wantMultipleFollowUps: true,
		},
		{
			name:     "post-window inspect hits its deadline",
			inWindow: func(context.Context) (bool, error) { return false, nil },
			postWindow: func(ctx context.Context) (bool, error) {
				<-ctx.Done()
				return false, ctx.Err()
			},
			wantResult:            atom.Killed,
			wantMultipleFollowUps: true,
		},
		{
			// The ordinary SIGKILL: Docker's settled record after the window is a
			// known non-OOM verdict even when earlier polls failed.
			name:                  "post-window inspect converges on SIGKILL",
			inWindow:              func(context.Context) (bool, error) { return false, errDaemon },
			postWindow:            func(context.Context) (bool, error) { return false, nil },
			wantKnown:             true,
			wantResult:            atom.Killed,
			wantMultipleFollowUps: true,
		},
		{
			name:       "late OOM transition after failed polls",
			inWindow:   lateOOMAfterFailures(3, errDaemon),
			postWindow: func(context.Context) (bool, error) { return false, errDaemon },
			wantKnown:  true,
			wantOOM:    true,
			wantResult: atom.ResourceFailure,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var started time.Time
			calls := 0
			backend := &waitOutcomeBackend{inspect: func(ctx context.Context) (container.InspectResponse, error) {
				calls++
				state := &container.State{Status: "exited", ExitCode: 137, StartedAt: "original"}
				if calls > 1 {
					answer := tc.inWindow
					if time.Since(started) >= settle {
						answer = tc.postWindow
					}
					oom, err := answer(ctx)
					if err != nil {
						return container.InspectResponse{}, err
					}
					state.OOMKilled = oom
				} else {
					started = time.Now()
				}
				resp := newContainer("runtime", state)
				resp.HostConfig = &container.HostConfig{Resources: container.Resources{Memory: 64 * 1024 * 1024}}
				return resp, nil
			}}
			engine := &dockerEngine{ctx: context.Background(), backend: backend}
			sampler := atom.StartResourceSampler(context.Background(), engine, "runtime", time.Hour)
			final, err := engine.Wait(&atom.EngineWaitRequest{ID: "runtime", Context: context.Background()})
			require.NoError(t, err)
			require.Equal(t, tc.wantResult, final.Result())
			require.Equal(t, 137, *final.ExitCode())
			outcome := final.(*Atom).ResourceOutcome()
			require.Equal(t, tc.wantKnown, outcome.OOMKnown)
			require.Equal(t, tc.wantOOM, outcome.OOMKilled)
			// Neither exit 137 nor the applied limit is inferred into an OOM.
			summary := sampler.Stop(final)
			require.Equal(t, tc.wantKnown, summary.OOMKnown)
			require.Equal(t, tc.wantOOM, summary.OOMKilled)
			if !tc.wantOOM {
				require.Nil(t, summary.PeakMemoryBytes)
				require.Equal(t, "none", summary.StatsSource)
			}
			if tc.wantMultipleFollowUps {
				require.Greater(t, calls, 2)
			}
		})
	}
}

func lateOOMAfterFailures(failures int, err error) func(context.Context) (bool, error) {
	seen := 0
	return func(context.Context) (bool, error) {
		seen++
		if seen <= failures {
			return false, err
		}
		return true, nil
	}
}
