//go:build integration

package lifecycle

import (
	"encoding/json"
	"testing"

	"github.com/caesium-cloud/caesium/test/robustness/recorder"
	"github.com/stretchr/testify/require"
)

func TestRuntimePodName(t *testing.T) {
	for id, want := range map[string]string{
		"caesium/task-pod": "task-pod",
		"task-pod":         "task-pod", // recorded by an earlier release
	} {
		got, err := runtimePodName(id)
		require.NoError(t, err, id)
		require.Equal(t, want, got, id)
	}
	for _, bad := range []string{"/task-pod", "caesium/", "a/b/c"} {
		_, err := runtimePodName(bad)
		require.Error(t, err, bad)
	}
}

// TestVerifySeedHeldAttemptAcceptsQualifiedRuntimeIDs: this release records
// Kubernetes runtime ids as "<namespace>/<pod>" (identity-and-access C1); the
// recorder only knows the pod name, so the proofs match on the pod name.
func TestVerifySeedHeldAttemptAcceptsQualifiedRuntimeIDs(t *testing.T) {
	const (
		runID     = "7c39392d-d1bf-43fc-b803-fce7d606141f"
		publicID  = "148d4542-8df5-4bf2-8ff4-d542af055498"
		durableID = "45b15c10-f7ea-4987-9fcd-d05e0522bbd9"
	)
	pod := publicID + "-" + runID + "-attempt2-97675795-84e0-4aea-a36f-cc97ac70663b"
	qualified := "caesium/" + pod
	proof := clusterTaskProof{ID: durableID, RunID: runID, TaskID: publicID, Status: "running",
		Attempt: 2, ClaimAttempt: 2, RuntimeID: qualified}
	body, err := json.Marshal(rawAttemptEffect{RunID: runID, Step: "hold", Nonce: "current-start", PodName: pod, Event: "start"})
	require.NoError(t, err)
	start := recorder.Event{RunID: runID, Step: "hold", Kind: "start", Nonce: "current-start", Raw: string(body)}
	payload, err := json.Marshal(taskStartedAttempt{DurableID: durableID, RunID: runID, TaskID: publicID,
		RuntimeID: qualified, Attempt: 2, ClaimAttempt: 2})
	require.NoError(t, err)
	events := []eventTuple{{Sequence: 9, Type: "task_started", TaskID: publicID, Payload: string(payload)}}
	require.NoError(t, verifySeedHeldAttempt(proof, []recorder.Event{start}, events))
}

// TestRawCompletionMatchesTaskAcceptsQualifiedRuntimeIDs: the completion
// proof matches a qualified durable runtime id to the recorder's bare pod
// name, for both the start and the completion events, and still rejects a
// completion recorded by another pod.
func TestRawCompletionMatchesTaskAcceptsQualifiedRuntimeIDs(t *testing.T) {
	const runID = "7c39392d-d1bf-43fc-b803-fce7d606141f"
	pod := "148d4542-8df5-4bf2-8ff4-d542af055498-" + runID + "-attempt1-bd606dbc-2d65-4ef4-a0d2-d795606e2673"
	raw := func(kind, podName string) recorder.Event {
		body, err := json.Marshal(rawAttemptEffect{RunID: runID, Step: "hold", Nonce: "n1", PodName: podName, Event: kind})
		require.NoError(t, err)
		return recorder.Event{RunID: runID, Step: "hold", Kind: kind, Nonce: "n1", Raw: string(body)}
	}
	for _, runtimeID := range []string{"caesium/" + pod, pod} {
		proof := clusterTaskProof{RuntimeID: runtimeID, RecorderNonce: "n1", RecorderPod: pod}
		require.True(t, rawCompletionMatchesTask(runID, proof, []recorder.Event{raw("start", pod), raw("complete", pod)}), runtimeID)
		require.False(t, rawCompletionMatchesTask(runID, proof, []recorder.Event{raw("start", pod), raw("complete", "other-pod")}), runtimeID)
	}
	require.False(t, rawCompletionMatchesTask(runID, clusterTaskProof{RuntimeID: "a/b/c", RecorderNonce: "n1", RecorderPod: "b"}, nil))
}
