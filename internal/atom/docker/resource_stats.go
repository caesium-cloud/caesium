package docker

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/docker/docker/api/types/container"
)

// Package variables so tests can shorten the window; production never mutates them.
var (
	dockerOOMMetadataSettleTimeout = 5 * time.Second
	dockerOOMMetadataPollInterval  = 50 * time.Millisecond
	// dockerOOMMetadataFinalInspectTimeout bounds the post-window inspect that
	// settles a non-OOM verdict. It runs on the caller's context, so an
	// in-window poll cut short by the settle deadline cannot decide the verdict.
	dockerOOMMetadataFinalInspectTimeout = 2 * time.Second
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

// Docker publishes exit and OOM notifications separately (containerd's TaskOOM
// and TaskExit are independent events), so a not-running wait can finish
// before inspect carries OOMKilled. For an exit 137 without that flag the
// initial snapshot is not a verdict: poll for the OOM transition during a
// bounded window, then settle the verdict from one post-window inspect of the
// same runtime attempt. Only such a converged observation is known OOM
// evidence. When it cannot be obtained (the follow-up inspect fails, or the
// runtime was replaced) the outcome keeps OOM unknown instead of freezing the
// pre-notification false flag. Exit 137 and a memory limit never imply OOM.
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
	unresolved := func(reason string) atom.Atom {
		log.Debug("docker OOM evidence unresolved; keeping OOM unknown",
			"container_id", metadata.ID,
			"started_at", metadata.State.StartedAt,
			"exit_code", metadata.State.ExitCode,
			"reason", reason,
			"elapsed", time.Since(started).Round(time.Millisecond),
			"inspect_attempts", attempts,
			"last_inspect_error", lastInspectError,
		)
		return &Atom{metadata: metadata, oomUnresolved: true}
	}
	for {
		select {
		case <-settleCtx.Done():
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			finalCtx, finalCancel := context.WithTimeout(ctx, dockerOOMMetadataFinalInspectTimeout)
			attempts++
			inspected, inspectErr := e.backend.ContainerInspect(finalCtx, id)
			finalCancel()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if inspectErr != nil {
				lastInspectError = inspectErr.Error()
				return unresolved("post-window inspect failed"), nil
			}
			if !sameWaitAttempt(metadata, inspected) {
				return unresolved("runtime replaced"), nil
			}
			// Converged: the settled runtime record is the verdict, OOM or not.
			return &Atom{metadata: inspected}, nil
		case <-ticker.C:
			attempts++
			inspected, inspectErr := e.backend.ContainerInspect(settleCtx, id)
			if inspectErr != nil {
				lastInspectError = inspectErr.Error()
				continue
			}
			// A restarted or mismatched runtime is not evidence for this attempt.
			if !sameWaitAttempt(metadata, inspected) {
				return unresolved("runtime replaced"), nil
			}
			if inspected.State.OOMKilled {
				return &Atom{metadata: inspected}, nil
			}
		}
	}
}

// sameWaitAttempt reports whether inspected still describes the exited runtime
// attempt captured by initial.
func sameWaitAttempt(initial, inspected container.InspectResponse) bool {
	return inspected.ContainerJSONBase != nil &&
		inspected.ID == initial.ID &&
		inspected.State != nil &&
		!inspected.State.Running &&
		inspected.State.ExitCode == initial.State.ExitCode &&
		inspected.State.StartedAt == initial.State.StartedAt
}
