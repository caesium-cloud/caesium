package atom

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"
)

var ErrStatsUnavailable = errors.New("runtime resource statistics unavailable")

type EngineStatsRequest struct {
	ID      string
	Context context.Context
}

// ResourceStats is one runtime observation. CPUSeconds is cumulative CPU time
// since container start; CPUCores is a sampled rate when the runtime exposes no
// counter (metrics.k8s.io). Nil means absent, not zero.
type ResourceStats struct {
	MemoryBytes *int64
	CPUSeconds  *float64
	CPUCores    *float64
	SampledAt   time.Time
}

// ResourceOutcome is inspect evidence from the completed runtime, separate from
// the coarse Result. ResourceFailure alone does not establish an OOM.
type ResourceOutcome struct {
	OOMKilled        bool
	MemoryLimitBytes *int64
}

// ResourceOutcomeProvider is optional for custom engines. An engine without
// inspect evidence must not turn a generic resource failure into an OOM.
type ResourceOutcomeProvider interface{ ResourceOutcome() ResourceOutcome }

type ResourceSummary struct {
	PeakMemoryBytes *int64
	CPUSeconds      *float64
	OOMKilled       bool
	StatsSource     string
}

// ResourceSampler samples one attempt. Stop cancels and joins the request before
// callers remove the runtime or persist the final outcome; no late sample can
// overwrite a retry's observations.
type ResourceSampler struct {
	cancel  context.CancelFunc
	done    chan struct{}
	once    sync.Once
	reducer resourceReducer
}

type resourceReducer struct {
	peak     *int64
	cpu      *float64
	lastRate *float64
	lastTime time.Time
}

func (r *resourceReducer) add(stats ResourceStats) {
	if stats.MemoryBytes != nil && *stats.MemoryBytes >= 0 && (r.peak == nil || *stats.MemoryBytes > *r.peak) {
		value := *stats.MemoryBytes
		r.peak = &value
	}
	if stats.CPUSeconds != nil && validCPU(*stats.CPUSeconds) && (r.cpu == nil || *stats.CPUSeconds > *r.cpu) {
		value := *stats.CPUSeconds
		r.cpu = &value
	}
	if stats.CPUCores != nil && validCPU(*stats.CPUCores) && !stats.SampledAt.IsZero() {
		if r.lastRate != nil && stats.SampledAt.After(r.lastTime) {
			value := (*r.lastRate + *stats.CPUCores) / 2 * stats.SampledAt.Sub(r.lastTime).Seconds()
			if r.cpu != nil {
				value += *r.cpu
			}
			r.cpu = &value
		}
		if stats.SampledAt.After(r.lastTime) {
			value := *stats.CPUCores
			r.lastRate, r.lastTime = &value, stats.SampledAt
		}
	}
}

func validCPU(value float64) bool { return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0) }

func StartResourceSampler(ctx context.Context, engine Engine, runtimeID string, interval time.Duration) *ResourceSampler {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &ResourceSampler{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		sample := func() {
			if ctx.Err() != nil {
				return
			}
			requestCtx, requestCancel := context.WithTimeout(ctx, 5*time.Second)
			defer requestCancel()
			stats, err := engine.Stats(&EngineStatsRequest{ID: runtimeID, Context: requestCtx})
			if err == nil {
				s.reducer.add(stats)
			}
		}
		sample()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sample()
			}
		}
	}()
	return s
}

func (s *ResourceSampler) Stop(final Atom) ResourceSummary {
	if s == nil {
		return ResourceSummary{}
	}
	s.once.Do(func() { s.cancel(); <-s.done })
	out := ResourceSummary{PeakMemoryBytes: s.reducer.peak, CPUSeconds: s.reducer.cpu, StatsSource: "none"}
	if out.PeakMemoryBytes != nil || out.CPUSeconds != nil {
		out.StatsSource = "sampled"
	}
	if provider, ok := final.(ResourceOutcomeProvider); ok {
		evidence := provider.ResourceOutcome()
		out.OOMKilled = evidence.OOMKilled
		if out.OOMKilled {
			// An observed cgroup limit is a censored lower bound, not a sampled
			// peak. Never infer a number from an absent/unbounded limit.
			if evidence.MemoryLimitBytes != nil && *evidence.MemoryLimitBytes > 0 && (out.PeakMemoryBytes == nil || *evidence.MemoryLimitBytes > *out.PeakMemoryBytes) {
				value := *evidence.MemoryLimitBytes
				out.PeakMemoryBytes = &value
				out.StatsSource = "oom_inferred"
			} else if out.PeakMemoryBytes == nil {
				out.StatsSource = "oom_inferred"
			}
		}
	}
	return out
}
