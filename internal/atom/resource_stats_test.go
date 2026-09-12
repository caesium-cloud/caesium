package atom

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestResourceReducerKeepsPeakAndCumulativeCPU(t *testing.T) {
	r := resourceReducer{}
	r.add(ResourceStats{MemoryBytes: new(int64(12)), CPUSeconds: new(2.0)})
	r.add(ResourceStats{MemoryBytes: new(int64(8)), CPUSeconds: new(1.0)})
	r.add(ResourceStats{MemoryBytes: new(int64(-1)), CPUSeconds: new(math.NaN())})
	require.Equal(t, int64(12), *r.peak)
	require.Equal(t, 2.0, *r.cpu)
	r.add(ResourceStats{})
	require.Equal(t, int64(12), *r.peak)
}

func TestResourceReducerIntegratesDistinctKubernetesSamples(t *testing.T) {
	r := resourceReducer{}
	now := time.Now()
	r.add(ResourceStats{CPUCores: new(0.5), SampledAt: now})
	require.Nil(t, r.cpu, "one rate does not establish elapsed CPU time")
	r.add(ResourceStats{CPUCores: new(0.5), SampledAt: now})
	require.Nil(t, r.cpu, "repeated metrics-server snapshot is not another interval")
	r.add(ResourceStats{CPUCores: new(1.5), SampledAt: now.Add(2 * time.Second)})
	require.Equal(t, 2.0, *r.cpu)
}

type samplingEngine struct {
	Engine
	entered chan struct{}
	exited  chan struct{}
}

func (e *samplingEngine) Stats(req *EngineStatsRequest) (ResourceStats, error) {
	close(e.entered)
	<-req.Context.Done()
	close(e.exited)
	return ResourceStats{}, req.Context.Err()
}

type outcomeAtom struct {
	Atom
	evidence ResourceOutcome
}

func (a outcomeAtom) ResourceOutcome() ResourceOutcome { return a.evidence }

func TestResourceSamplerCancelsAndJoinsOutstandingRequest(t *testing.T) {
	engine := &samplingEngine{entered: make(chan struct{}), exited: make(chan struct{})}
	sampler := StartResourceSampler(context.Background(), engine, "runtime", time.Hour)
	<-engine.entered
	out := sampler.Stop(nil)
	select {
	case <-engine.exited:
	default:
		t.Fatal("Stop returned before the request exited")
	}
	require.Equal(t, "none", out.StatsSource)
	require.Nil(t, out.PeakMemoryBytes)
	require.Nil(t, out.CPUSeconds)
	require.Equal(t, out, sampler.Stop(nil))
}

func TestResourceSamplerOOMLowerBoundAndUnavailableMeasurements(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limit  *int64
		peak   *int64
		source string
	}{
		{"known limit", new(int64(64)), new(int64(64)), "oom_inferred"},
		{"unknown limit", nil, nil, "oom_inferred"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := &samplingEngine{entered: make(chan struct{}), exited: make(chan struct{})}
			sampler := StartResourceSampler(context.Background(), engine, "runtime", time.Hour)
			<-engine.entered
			out := sampler.Stop(outcomeAtom{evidence: ResourceOutcome{OOMKilled: true, MemoryLimitBytes: tc.limit}})
			require.True(t, out.OOMKilled)
			require.Equal(t, tc.peak, out.PeakMemoryBytes)
			require.Nil(t, out.CPUSeconds)
			require.Equal(t, tc.source, out.StatsSource)
		})
	}
}

func TestResourceSamplerOOMRaisesSampledPeakToLimit(t *testing.T) {
	s := &ResourceSampler{cancel: func() {}, done: make(chan struct{})}
	close(s.done)
	s.reducer.add(ResourceStats{MemoryBytes: new(int64(41291776))})
	out := s.Stop(outcomeAtom{evidence: ResourceOutcome{OOMKilled: true, MemoryLimitBytes: new(int64(64 * 1024 * 1024))}})
	require.True(t, out.OOMKilled)
	require.Equal(t, int64(64*1024*1024), *out.PeakMemoryBytes)
	require.Equal(t, "oom_inferred", out.StatsSource)
}
