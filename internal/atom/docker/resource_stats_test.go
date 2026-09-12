package docker

import (
	"context"
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
		{name: "limit without inspect OOM", enabled: "true", exit: 137, memory: limit, wantOOM: true, want: atom.ResourceFailure},
		{name: "limit without inspect OOM gate off", enabled: "false", exit: 137, memory: limit, want: atom.Killed},
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
		body      string
		available bool
	}{
		{`{"read":"2026-09-09T00:00:00Z","memory_stats":{"usage":4096},"cpu_stats":{"cpu_usage":{"total_usage":2500000000}}}`, true},
		{`{}`, false},
		{`{"read":"2026-09-09T00:00:00Z"}`, false},
	} {
		backend := &mockDockerBackend{}
		backend.On("ContainerStatsOneShot", "runtime").Return(container.StatsResponseReader{Body: io.NopCloser(strings.NewReader(tc.body))}, nil).Once()
		engine := &dockerEngine{ctx: context.Background(), backend: backend}
		stats, err := engine.Stats(&atom.EngineStatsRequest{ID: "runtime"})
		if tc.available {
			require.NoError(t, err)
			require.Equal(t, int64(4096), *stats.MemoryBytes)
			require.Equal(t, 2.5, *stats.CPUSeconds)
		} else {
			require.ErrorIs(t, err, atom.ErrStatsUnavailable)
			require.Nil(t, stats.MemoryBytes)
		}
		backend.AssertExpectations(t)
	}
}

type waitOutcomeBackend struct {
	dockerBackend
	inspect func(context.Context) (container.InspectResponse, error)
}

func (b *waitOutcomeBackend) ContainerInspect(ctx context.Context, _ string) (container.InspectResponse, error) {
	return b.inspect(ctx)
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
	}{
		{name: "late OOM", enabled: true, lateOOM: true},
		{name: "ordinary SIGKILL", enabled: true},
		{name: "gate disabled", lateOOM: true},
		{name: "replacement runtime", enabled: true, lateOOM: true, restarted: true},
		{name: "limit without inspect OOM", enabled: true, memory: 64 * 1024 * 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CAESIUM_RESOURCE_STATS_ENABLED", fmt.Sprint(tc.enabled))
			require.NoError(t, env.Process())
			calls := 0
			backend := &waitOutcomeBackend{inspect: func(context.Context) (container.InspectResponse, error) {
				calls++
				state := &container.State{Status: "exited", ExitCode: 137, StartedAt: "original"}
				if calls > 1 {
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
			if tc.enabled && !tc.restarted && (tc.lateOOM || tc.memory > 0) {
				want = atom.ResourceFailure
			}
			require.Equal(t, want, final.Result())
			require.Equal(t, want == atom.ResourceFailure, final.(*Atom).ResourceOutcome().OOMKilled)
			if !tc.enabled || tc.memory > 0 {
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
