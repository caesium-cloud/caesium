package podman

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/containers/podman/v5/libpod/define"
	"github.com/containers/podman/v5/pkg/bindings"
	"github.com/containers/podman/v5/pkg/domain/entities/types"
	"github.com/stretchr/testify/require"
)

func TestResourceOOMUsesInspectAndHonorsGate(t *testing.T) {
	t.Cleanup(func() { require.NoError(t, env.Process()) })
	limit := int64(64 * 1024 * 1024)
	for _, tc := range []struct {
		name    string
		enabled string
		inspect bool
		wantOOM bool
		want    atom.Result
	}{
		{name: "inspect OOM", enabled: "true", inspect: true, wantOOM: true, want: atom.ResourceFailure},
		{name: "inspect OOM gate off", enabled: "false", inspect: true, wantOOM: true, want: atom.Killed},
		{name: "SIGKILL with limit", enabled: "true", want: atom.Killed},
		{name: "SIGKILL with limit gate off", enabled: "false", want: atom.Killed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CAESIUM_RESOURCE_STATS_ENABLED", tc.enabled)
			require.NoError(t, env.Process())
			metadata := newContainer("runtime", &define.InspectContainerState{ExitCode: 137, OOMKilled: tc.inspect})
			metadata.HostConfig = &define.InspectContainerHostConfig{Memory: limit}
			a := &Atom{metadata: metadata}
			require.Equal(t, tc.want, a.Result())
			require.Equal(t, 137, *a.ExitCode())
			require.Equal(t, tc.wantOOM, a.ResourceOutcome().OOMKilled)
			require.Equal(t, limit, *a.ResourceOutcome().MemoryLimitBytes)
		})
	}
}

// podmanStatsServer serves the one stats report the client under test will
// fetch, and the ping that bindings.NewConnection performs first.
func podmanStatsServer(t *testing.T, report types.ContainerStatsReport) context.Context {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Libpod-API-Version", "5.8.6")
		if strings.HasSuffix(r.URL.Path, "/_ping") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("OK"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(report)
	}))
	t.Cleanup(server.Close)
	ctx, err := bindings.NewConnection(context.Background(), "tcp://"+strings.TrimPrefix(server.URL, "http://"))
	require.NoError(t, err)
	return ctx
}

// A container that is neither running nor paused makes libpod's
// GetContainerStats return a nil error and a record left at its zero values, so
// the reply cannot be distinguished from a measurement by the presence of the
// numeric fields alone. Accepting it would persist peak_memory_bytes=0 and
// cpu_seconds=0 as sampled observations for any task that exits before the
// first sampler request.
func TestContainerStatsRejectsUnmeasuredReport(t *testing.T) {
	const id = "d1f2a3b4c5d6"
	for _, tc := range []struct {
		name     string
		stats    define.ContainerStats
		measured bool
		memory   int64
		cpu      float64
	}{
		{
			name:  "stopped container returns a default-valued record",
			stats: define.ContainerStats{ContainerID: id, Name: "measure"},
		},
		{
			name:  "unmeasured record carrying a limit is still not an observation",
			stats: define.ContainerStats{ContainerID: id, Name: "measure", MemLimit: 64 * 1024 * 1024},
		},
		{
			name:     "running container stamps the sampling clock",
			stats:    define.ContainerStats{ContainerID: id, Name: "measure", SystemNano: 1700000000000000000, MemUsage: 96 * 1024 * 1024, CPUNano: 2500000000},
			measured: true,
			memory:   96 * 1024 * 1024,
			cpu:      2.5,
		},
		{
			name:  "a measured record of zeros is still unavailable",
			stats: define.ContainerStats{ContainerID: id, Name: "measure", SystemNano: 1700000000000000000},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := podmanStatsServer(t, types.ContainerStatsReport{Stats: []define.ContainerStats{tc.stats}})
			cli := &podmanClient{ctx: ctx}
			stats, err := cli.ContainerStats(ctx, id)
			if !tc.measured {
				require.ErrorIs(t, err, atom.ErrStatsUnavailable)
				require.Nil(t, stats.MemoryBytes)
				require.Nil(t, stats.CPUSeconds)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, stats.MemoryBytes)
			require.Equal(t, tc.memory, *stats.MemoryBytes)
			require.NotNil(t, stats.CPUSeconds)
			require.InDelta(t, tc.cpu, *stats.CPUSeconds, 1e-9)
			require.Equal(t, time.Unix(0, int64(tc.stats.SystemNano)), stats.SampledAt)
		})
	}
}

// The identity guard must not be bypassed by a measured record for some other
// container.
func TestContainerStatsRejectsForeignRuntimeIdentity(t *testing.T) {
	stats := define.ContainerStats{ContainerID: "someone-else", Name: "other", SystemNano: 1700000000000000000, MemUsage: 4096}
	ctx := podmanStatsServer(t, types.ContainerStatsReport{Stats: []define.ContainerStats{stats}})
	cli := &podmanClient{ctx: ctx}
	_, err := cli.ContainerStats(ctx, "d1f2a3b4c5d6")
	require.ErrorContains(t, err, "resource stats runtime identity mismatch")
}
