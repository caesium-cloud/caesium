package docker

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
)

const (
	dockerOOMMetadataSettleTimeout = 5 * time.Second
	dockerOOMMetadataPollInterval  = 50 * time.Millisecond
)

func (e *dockerEngine) Stats(req *atom.EngineStatsRequest) (atom.ResourceStats, error) {
	ctx := req.Context
	if ctx == nil {
		ctx = e.ctx
	}
	response, err := e.backend.ContainerStatsOneShot(ctx, req.ID)
	if err != nil {
		return atom.ResourceStats{}, err
	}
	defer func() { _ = response.Body.Close() }()
	var sample struct {
		Read        time.Time `json:"read"`
		MemoryStats struct {
			Usage *uint64 `json:"usage"`
		} `json:"memory_stats"`
		CPUStats struct {
			CPUUsage struct {
				TotalUsage *uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
		} `json:"cpu_stats"`
	}
	if err := json.NewDecoder(response.Body).Decode(&sample); err != nil {
		return atom.ResourceStats{}, err
	}
	// Empty or zero stats from stopped/unavailable containers are not measurements.
	if sample.Read.IsZero() {
		return atom.ResourceStats{}, atom.ErrStatsUnavailable
	}
	out := atom.ResourceStats{SampledAt: sample.Read}
	if sample.MemoryStats.Usage != nil && *sample.MemoryStats.Usage > 0 && *sample.MemoryStats.Usage <= math.MaxInt64 {
		memory := int64(*sample.MemoryStats.Usage)
		out.MemoryBytes = &memory
	}
	if sample.CPUStats.CPUUsage.TotalUsage != nil && *sample.CPUStats.CPUUsage.TotalUsage > 0 {
		cpu := float64(*sample.CPUStats.CPUUsage.TotalUsage) / 1e9
		out.CPUSeconds = &cpu
	}
	if out.MemoryBytes == nil && out.CPUSeconds == nil {
		return atom.ResourceStats{}, atom.ErrStatsUnavailable
	}
	return out, nil
}

// Docker publishes exit and OOM notifications separately. A not-running wait
// can finish before inspect carries OOMKilled; allow that metadata to converge
// before executors freeze the outcome and remove the container. A plain SIGKILL
// remains killed when the bounded window expires without runtime OOM evidence.
func (e *dockerEngine) inspectWaitOutcome(ctx context.Context, id string) (atom.Atom, error) {
	metadata, err := e.backend.ContainerInspect(ctx, id)
	if err != nil {
		return nil, err
	}
	final := &Atom{metadata: metadata}
	if !env.Variables().ResourceStatsEnabled || metadata.State == nil || metadata.State.ExitCode != 137 || metadata.State.OOMKilled {
		return final, nil
	}
	settleCtx, cancel := context.WithTimeout(ctx, dockerOOMMetadataSettleTimeout)
	defer cancel()
	ticker := time.NewTicker(dockerOOMMetadataPollInterval)
	defer ticker.Stop()
	started := time.Now()
	attempts := 0
	lastInspectError := ""
	for {
		select {
		case <-settleCtx.Done():
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			log.Debug("docker OOM observation window expired without OOM evidence",
				"container_id", metadata.ID,
				"started_at", metadata.State.StartedAt,
				"exit_code", metadata.State.ExitCode,
				"elapsed", time.Since(started).Round(time.Millisecond),
				"inspect_attempts", attempts,
				"last_inspect_error", lastInspectError,
			)
			return final, nil
		case <-ticker.C:
			attempts++
			inspected, inspectErr := e.backend.ContainerInspect(settleCtx, id)
			if inspectErr != nil {
				lastInspectError = inspectErr.Error()
				continue
			}
			// A restarted or mismatched runtime is not evidence for this attempt.
			if inspected.ID != metadata.ID || inspected.State == nil || inspected.State.Running || inspected.State.ExitCode != metadata.State.ExitCode || inspected.State.StartedAt != metadata.State.StartedAt {
				return final, nil
			}
			if inspected.State.OOMKilled {
				return &Atom{metadata: inspected}, nil
			}
		}
	}
}
