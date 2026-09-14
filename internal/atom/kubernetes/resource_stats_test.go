package kubernetes

import (
	"context"
	"testing"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes/fake"
)

func TestResourceOOMUsesTerminationReasonAndHonorsGate(t *testing.T) {
	t.Cleanup(func() { require.NoError(t, env.Process()) })
	pod := &v1.Pod{Spec: v1.PodSpec{Containers: []v1.Container{{Name: "task", Resources: v1.ResourceRequirements{Limits: v1.ResourceList{v1.ResourceMemory: resource.MustParse("64Mi")}}}}}, Status: v1.PodStatus{ContainerStatuses: []v1.ContainerStatus{{Name: "task", State: v1.ContainerState{Terminated: &v1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}}}}}}
	for _, enabled := range []string{"false", "true"} {
		t.Setenv("CAESIUM_RESOURCE_STATS_ENABLED", enabled)
		require.NoError(t, env.Process())
		a := &Atom{metadata: pod}
		want := atom.Killed
		if enabled == "true" {
			want = atom.ResourceFailure
		}
		require.Equal(t, want, a.Result())
		require.Equal(t, 137, *a.ExitCode())
		require.Equal(t, int64(64*1024*1024), *a.ResourceOutcome().MemoryLimitBytes)
	}
	pod.Status.ContainerStatuses[0].State.Terminated.Reason = "Error"
	require.Equal(t, atom.Killed, (&Atom{metadata: pod}).Result())
}

func TestResourceStatsKubernetesUnavailableDoesNotFabricateZero(t *testing.T) {
	engine := NewEngine(context.Background(), fake.NewClientset().CoreV1())
	stats, err := engine.Stats(&atom.EngineStatsRequest{ID: "runtime"})
	require.ErrorIs(t, err, atom.ErrStatsUnavailable)
	require.Nil(t, stats.MemoryBytes)
	require.Nil(t, stats.CPUSeconds)
	for _, input := range []string{`{}`, `{"timestamp":"2026-09-09T00:00:00Z","containers":[{"usage":{"cpu":"2m"}}]}`} {
		_, err := decodePodResourceStats([]byte(input))
		require.ErrorIs(t, err, atom.ErrStatsUnavailable)
	}
}

func TestResourceStatsKubernetesUsesMeasuredQuantities(t *testing.T) {
	stats, err := decodePodResourceStats([]byte(`{"timestamp":"2026-09-09T00:00:00Z","containers":[{"usage":{"cpu":"250m","memory":"4Mi"}},{"usage":{"cpu":"750m","memory":"2Mi"}}]}`))
	require.NoError(t, err)
	require.Equal(t, int64(6*1024*1024), *stats.MemoryBytes)
	require.Equal(t, 1.0, *stats.CPUCores)
	require.Nil(t, stats.CPUSeconds, "metrics API exposes a rate, not accumulated CPU")
}
