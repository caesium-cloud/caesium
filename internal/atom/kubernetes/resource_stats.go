package kubernetes

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/pkg/env"
	"k8s.io/apimachinery/pkg/api/resource"
)

func (e *kubernetesEngine) Stats(req *atom.EngineStatsRequest) (atom.ResourceStats, error) {
	if e.statsClient == nil {
		return atom.ResourceStats{}, atom.ErrStatsUnavailable
	}
	ctx := req.Context
	if ctx == nil {
		ctx = e.ctx
	}
	payload, err := e.statsClient.Get().AbsPath("/apis/metrics.k8s.io/v1beta1/namespaces", env.Variables().KubernetesNamespace, "pods", req.ID).DoRaw(ctx)
	if err != nil {
		return atom.ResourceStats{}, fmt.Errorf("%w: %v", atom.ErrStatsUnavailable, err)
	}
	return decodePodResourceStats(payload)
}

func decodePodResourceStats(payload []byte) (atom.ResourceStats, error) {
	var sample struct {
		Timestamp  time.Time `json:"timestamp"`
		Containers []struct {
			Usage map[string]string `json:"usage"`
		} `json:"containers"`
	}
	if err := json.Unmarshal(payload, &sample); err != nil {
		return atom.ResourceStats{}, err
	}
	if sample.Timestamp.IsZero() || len(sample.Containers) == 0 {
		return atom.ResourceStats{}, atom.ErrStatsUnavailable
	}
	memory, cpu := resource.Quantity{}, resource.Quantity{}
	for _, container := range sample.Containers {
		mem, err := resource.ParseQuantity(container.Usage["memory"])
		if err != nil || mem.Sign() < 0 {
			return atom.ResourceStats{}, atom.ErrStatsUnavailable
		}
		cores, err := resource.ParseQuantity(container.Usage["cpu"])
		if err != nil || cores.Sign() < 0 {
			return atom.ResourceStats{}, atom.ErrStatsUnavailable
		}
		memory.Add(mem)
		cpu.Add(cores)
	}
	bytes, cores := memory.Value(), cpu.AsApproximateFloat64()
	return atom.ResourceStats{MemoryBytes: &bytes, CPUCores: &cores, SampledAt: sample.Timestamp}, nil
}
