package podman

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/containers/podman/v5/pkg/bindings"
)

func (e *podmanEngine) Stats(req *atom.EngineStatsRequest) (atom.ResourceStats, error) {
	ctx := req.Context
	if ctx == nil {
		ctx = e.ctx
	}
	return e.backend.ContainerStats(ctx, req.ID)
}

func (cli *podmanClient) ContainerStats(ctx context.Context, id string) (atom.ResourceStats, error) {
	conn, err := bindings.GetClient(cli.ctx)
	if err != nil {
		return atom.ResourceStats{}, err
	}
	// A bounded non-streaming request avoids the SDK Stats helper's unbuffered
	// delivery goroutine, which can remain blocked after caller cancellation.
	params := url.Values{"containers": {id}, "stream": {"false"}}
	response, err := conn.DoRequest(ctx, nil, http.MethodGet, "/containers/stats", params, nil)
	if err != nil {
		return atom.ResourceStats{}, err
	}
	defer func() { _ = response.Body.Close() }()
	var report struct {
		Error json.RawMessage
		Stats []struct {
			ContainerID string
			SystemNano  uint64
			MemUsage    *uint64
			CPUNano     *uint64
		}
	}
	if err := response.Process(&report); err != nil {
		return atom.ResourceStats{}, err
	}
	if len(report.Error) > 0 && string(report.Error) != "null" {
		return atom.ResourceStats{}, fmt.Errorf("podman resource stats failed: %s", report.Error)
	}
	if len(report.Stats) != 1 {
		return atom.ResourceStats{}, atom.ErrStatsUnavailable
	}
	sample := report.Stats[0]
	if sample.ContainerID != id {
		return atom.ResourceStats{}, fmt.Errorf("resource stats runtime identity mismatch")
	}
	// libpod's GetContainerStats returns a nil error and a record left at its
	// zero values when the container is neither running nor paused, and every
	// numeric field is serialised unconditionally, so the pointers alone cannot
	// separate "unavailable" from "measured zero". SystemNano is stamped only on
	// the measured path, so require it before accepting any measurement.
	if sample.SystemNano == 0 || sample.SystemNano > math.MaxInt64 {
		return atom.ResourceStats{}, atom.ErrStatsUnavailable
	}
	out := atom.ResourceStats{SampledAt: time.Unix(0, int64(sample.SystemNano))}
	if sample.MemUsage != nil && *sample.MemUsage <= math.MaxInt64 {
		value := int64(*sample.MemUsage)
		out.MemoryBytes = &value
	}
	if sample.CPUNano != nil {
		cpu := float64(*sample.CPUNano) / 1e9
		out.CPUSeconds = &cpu
	}
	if out.MemoryBytes == nil && out.CPUSeconds == nil {
		return atom.ResourceStats{}, atom.ErrStatsUnavailable
	}
	return out, nil
}
