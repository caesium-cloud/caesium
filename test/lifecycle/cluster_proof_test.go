//go:build integration

package lifecycle

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/caesium-cloud/caesium/test/robustness/cluster"
	"github.com/caesium-cloud/caesium/test/robustness/recorder"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestResolvePodNameBySourceIP(t *testing.T) {
	const podName = "148d4542-8df5-4bf2-8ff4-d542af055498-7c39392d-d1bf-43fc-b803-fce7d606141f-attempt1-bd606dbc-2d65-4ef4-a0d2-d795606e2673"
	caller := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName},
		Status: corev1.PodStatus{PodIP: "10.244.0.42", Phase: corev1.PodRunning}}
	other := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "other"},
		Status: corev1.PodStatus{PodIP: "10.244.0.43", Phase: corev1.PodRunning}}
	name, err := resolvePodNameBySourceIP("10.244.0.42:53535", []corev1.Pod{caller, other})
	require.NoError(t, err)
	require.Equal(t, podName, name, "lookup must retain the full name beyond kubelet's 63-character hostname")
	deleting := caller.DeepCopy()
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	stopped := caller.DeepCopy()
	stopped.Status.Phase = corev1.PodSucceeded
	for _, tc := range []struct {
		name   string
		remote string
		pods   []corev1.Pod
	}{
		{"invalid remote", "not-an-ip:5000", []corev1.Pod{caller}},
		{"missing pod", "10.244.0.44:5000", []corev1.Pod{caller}},
		{"ambiguous IP", "10.244.0.42:5000", []corev1.Pod{caller, caller}},
		{"deleting pod", "10.244.0.42:5000", []corev1.Pod{*deleting}},
		{"stopped pod", "10.244.0.42:5000", []corev1.Pod{*stopped}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolvePodNameBySourceIP(tc.remote, tc.pods)
			require.Error(t, err)
		})
	}
}

func TestHeldRecorderStartRequiresQueuedRunIdentity(t *testing.T) {
	const queuedID = "689add98-8ea4-46c4-aa6b-faa12169f45c"
	makeEvent := func(runID, step, kind, nonce, pod string) recorder.Event {
		body, err := json.Marshal(rawAttemptEffect{RunID: runID, Step: step, Event: kind, Nonce: nonce, PodName: pod})
		require.NoError(t, err)
		return recorder.Event{RunID: runID, Step: step, Kind: kind, Nonce: nonce, Raw: string(body)}
	}
	start := makeEvent(queuedID, "hold", "start", "queued-start", "full-queued-pod-name")
	require.True(t, hasHeldRecorderStart(queuedID, []recorder.Event{start}))
	for _, tc := range []struct {
		name   string
		events []recorder.Event
	}{
		{"predecessor start", []recorder.Event{makeEvent("dc37ca3b-12a1-4917-ab2f-01685a1df2b4", "hold", "start", "predecessor-start", "predecessor-pod")}},
		{"completion only", []recorder.Event{makeEvent(queuedID, "hold", "complete", "queued-start", "full-queued-pod-name")}},
		{"different step", []recorder.Event{makeEvent(queuedID, "history", "start", "queued-start", "full-queued-pod-name")}},
		{"no pod identity", []recorder.Event{makeEvent(queuedID, "hold", "start", "queued-start", "")}},
		{"raw identity disagrees", []recorder.Event{{RunID: queuedID, Step: "hold", Kind: "start", Nonce: "other", Raw: start.Raw}}},
		{"no start", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.False(t, hasHeldRecorderStart(queuedID, tc.events),
				"the queued barrier must not be released from unrelated raw evidence")
		})
	}
}

func TestQueuedPreReleaseRequiresCurrentDurableAttempt(t *testing.T) {
	const (
		runID     = "689add98-8ea4-46c4-aa6b-faa12169f45c"
		jobID     = "9aa0a5d2-1095-4e47-9d78-b6307c13f763"
		publicID  = "148d4542-8df5-4bf2-8ff4-d542af055498"
		durableID = "45b15c10-f7ea-4987-9fcd-d05e0522bbd9"
		token     = "queued-test"
	)
	stalePod := publicID + "-" + runID + "-attempt1-bd606dbc-2d65-4ef4-a0d2-d795606e2673"
	currentPod := publicID + "-" + runID + "-attempt2-97675795-84e0-4aea-a36f-cc97ac70663b"
	var run apiRun
	require.NoError(t, json.Unmarshal([]byte(`{"id":"`+runID+`","job_id":"`+jobID+`","status":"running","params":{"TOKEN":"`+token+`"},"tasks":[{"id":"`+publicID+`","status":"running","attempt":2}]}`), &run))
	proof := clusterTaskProof{ID: durableID, RunID: runID, TaskID: publicID,
		Status: "running", Attempt: 2, ClaimAttempt: 2, RuntimeID: currentPod}
	rawStart := func(nonce, podName string) recorder.Event {
		body, err := json.Marshal(rawAttemptEffect{RunID: runID, Step: "hold", Event: "start", Nonce: nonce, PodName: podName})
		require.NoError(t, err)
		return recorder.Event{RunID: runID, Step: "hold", Kind: "start", Nonce: nonce, Raw: string(body)}
	}
	started := func(attempt, claim int, podName string) eventTuple {
		payload, err := json.Marshal(taskStartedAttempt{DurableID: durableID, RunID: runID, TaskID: publicID,
			RuntimeID: podName, Attempt: attempt, ClaimAttempt: claim})
		require.NoError(t, err)
		return eventTuple{Type: "task_started", TaskID: publicID, Payload: string(payload)}
	}
	staleRaw := rawStart("stale-start", stalePod)
	currentRaw := rawStart("current-start", currentPod)
	taskEvents := []eventTuple{started(1, 1, stalePod), started(2, 2, currentPod)}
	require.True(t, hasHeldRecorderStart(runID, []recorder.Event{staleRaw}),
		"the old start reaches the coarse run-level barrier")
	_, err := verifyQueuedHeldAttempt(run, runID, jobID, token, proof, []recorder.Event{staleRaw}, taskEvents)
	require.ErrorContains(t, err, "current runtime", "a stale attempt-1 start cannot release running attempt 2")
	nonces, err := verifyQueuedHeldAttempt(run, runID, jobID, token, proof,
		[]recorder.Event{staleRaw, currentRaw}, taskEvents)
	require.NoError(t, err)
	require.Equal(t, []string{"current-start"}, nonces, "snapshot only the current durable runtime's nonce")
	_, err = verifyQueuedHeldAttempt(run, runID, jobID, token, proof,
		[]recorder.Event{staleRaw, currentRaw}, taskEvents[:1])
	require.Error(t, err, "a raw start without the current persisted task_started event cannot release")
	wrongPublic := run
	wrongPublic.Tasks = append(wrongPublic.Tasks[:0:0], run.Tasks...)
	wrongPublic.Tasks[0].Attempt = 1
	_, err = verifyQueuedHeldAttempt(wrongPublic, runID, jobID, token, proof,
		[]recorder.Event{staleRaw, currentRaw}, taskEvents)
	require.Error(t, err, "the public and durable current attempts must agree")
}

func TestReadClusterTaskProofQueriesPublicIdentity(t *testing.T) {
	const (
		runID     = "7c39392d-d1bf-43fc-b803-fce7d606141f"
		publicID  = "148d4542-8df5-4bf2-8ff4-d542af055498"
		durableID = "45b15c10-f7ea-4987-9fcd-d05e0522bbd9"
	)
	type queryRequest struct {
		SQL   string `json:"sql"`
		Limit int    `json:"limit"`
	}
	requests := make(chan queryRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request queryRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		requests <- request
		_ = json.NewEncoder(w).Encode(cluster.QueryResponse{RowCount: 1, Rows: [][]any{{
			durableID, runID, publicID, "10.244.3.6:9001", 2, 1, 1, "task-pod", "running", nil,
		}}})
	}))
	defer server.Close()

	proof, err := readClusterTaskProof(context.Background(), cluster.NewHTTP(""), server.URL, runID, publicID)
	require.NoError(t, err)
	request := <-requests
	require.Equal(t, durableID, proof.ID)
	require.Equal(t, 2, request.Limit, "two rows must be visible to reject an ambiguous public task")
	require.Contains(t, request.SQL, "WHERE job_run_id = '"+runID+"' AND task_id = '"+publicID+"' LIMIT 2")
	require.NotContains(t, request.SQL, "AND id =")
}

func TestParseClusterTaskProofIdentity(t *testing.T) {
	const (
		runID     = "7c39392d-d1bf-43fc-b803-fce7d606141f"
		publicID  = "148d4542-8df5-4bf2-8ff4-d542af055498"
		durableID = "45b15c10-f7ea-4987-9fcd-d05e0522bbd9"
		otherID   = "6000f2af-5260-4ad2-947c-788e4535b329"
	)
	row := []any{durableID, runID, publicID, "10.244.3.6:9001", float64(2), float64(1), float64(1), "task-pod", "succeeded", `{"recorder_nonce":"effect-1","recorder_pod":"task-pod"}`}
	valid := cluster.QueryResponse{RowCount: 1, Rows: [][]any{row}}
	proof, err := parseClusterTaskProof(valid, runID, publicID)
	require.NoError(t, err)
	require.Equal(t, durableID, proof.ID, "public task identity must resolve to the durable row")
	require.Equal(t, runID, proof.RunID)
	require.Equal(t, publicID, proof.TaskID)
	require.Equal(t, "effect-1", proof.RecorderNonce)
	require.Equal(t, "task-pod", proof.RecorderPod)
	require.Equal(t, "task-pod", proof.RuntimeID)
	require.Equal(t, 1, proof.ClaimAttempt)
	require.Equal(t, int64(2), proof.OwnerGeneration)
	require.Equal(t, 1, proof.Attempt)

	for _, tc := range []struct {
		name string
		resp cluster.QueryResponse
	}{
		{"missing", cluster.QueryResponse{}},
		{"duplicate public task", cluster.QueryResponse{RowCount: 2, Rows: [][]any{row, {otherID, runID, publicID, "", float64(2), float64(1), float64(1), "task-pod", "running", nil}}}},
		{"inconsistent row count", cluster.QueryResponse{RowCount: 2, Rows: [][]any{row}}},
		{"wrong run", cluster.QueryResponse{RowCount: 1, Rows: [][]any{{durableID, otherID, publicID, "", float64(2), float64(1), float64(1), "task-pod", "running", nil}}}},
		{"wrong public task", cluster.QueryResponse{RowCount: 1, Rows: [][]any{{durableID, runID, otherID, "", float64(2), float64(1), float64(1), "task-pod", "running", nil}}}},
		{"invalid durable id", cluster.QueryResponse{RowCount: 1, Rows: [][]any{{"not-a-uuid", runID, publicID, "", float64(2), float64(1), float64(1), "task-pod", "running", nil}}}},
		{"missing column", cluster.QueryResponse{RowCount: 1, Rows: [][]any{row[:9]}}},
		{"invalid output", cluster.QueryResponse{RowCount: 1, Rows: [][]any{{durableID, runID, publicID, "", float64(2), float64(1), float64(1), "task-pod", "succeeded", `{"recorder_nonce":`}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseClusterTaskProof(tc.resp, runID, publicID)
			require.Error(t, err)
		})
	}
}

func TestRetainedClusterTaskIdentityRejectsReplacement(t *testing.T) {
	const (
		runID     = "7c39392d-d1bf-43fc-b803-fce7d606141f"
		publicID  = "148d4542-8df5-4bf2-8ff4-d542af055498"
		durableID = "45b15c10-f7ea-4987-9fcd-d05e0522bbd9"
		otherID   = "6000f2af-5260-4ad2-947c-788e4535b329"
	)
	seed := clusterTaskProof{ID: durableID, RunID: runID, TaskID: publicID, Attempt: 1, Status: "running"}
	terminal := seed
	terminal.Status = "succeeded"
	require.NoError(t, verifyRetainedTaskIdentity(seed, terminal, runID, publicID),
		"the same durable row may change from running to succeeded")
	terminal.Attempt = 2
	require.NoError(t, verifyRetainedTaskIdentity(seed, terminal, runID, publicID),
		"a held run may retry on the same durable row")
	terminalSeed := seed
	terminalSeed.Status = "succeeded"
	regressedSeed := seed
	regressedSeed.Attempt = 2

	for _, tc := range []struct {
		name    string
		seed    clusterTaskProof
		current clusterTaskProof
	}{
		{"replacement row with same visible attempt", seed, clusterTaskProof{ID: otherID, RunID: runID, TaskID: publicID, Attempt: 1, Status: "succeeded"}},
		{"changed terminal attempt", terminalSeed, terminal},
		{"regressed held attempt", regressedSeed, clusterTaskProof{ID: durableID, RunID: runID, TaskID: publicID, Attempt: 1, Status: "succeeded"}},
		{"missing baseline", clusterTaskProof{}, terminal},
		{"wrong baseline run", clusterTaskProof{ID: durableID, RunID: otherID, TaskID: publicID, Attempt: 1}, terminal},
		{"wrong baseline public task", clusterTaskProof{ID: durableID, RunID: runID, TaskID: otherID, Attempt: 1}, terminal},
		{"wrong current run", seed, clusterTaskProof{ID: durableID, RunID: otherID, TaskID: publicID, Attempt: 1}},
		{"wrong current public task", seed, clusterTaskProof{ID: durableID, RunID: runID, TaskID: otherID, Attempt: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Error(t, verifyRetainedTaskIdentity(tc.seed, tc.current, runID, publicID))
		})
	}
	_, err := readRetainedClusterTaskProof(context.Background(), nil, "", nil, runID, publicID)
	require.ErrorContains(t, err, "no pre-upgrade durable task proof")
}

func TestVerifySeedHeldAttemptRequiresCurrentRuntimeStart(t *testing.T) {
	const (
		runID     = "7c39392d-d1bf-43fc-b803-fce7d606141f"
		publicID  = "148d4542-8df5-4bf2-8ff4-d542af055498"
		durableID = "45b15c10-f7ea-4987-9fcd-d05e0522bbd9"
	)
	oldPod := publicID + "-" + runID + "-attempt1-bd606dbc-2d65-4ef4-a0d2-d795606e2673"
	currentPod := publicID + "-" + runID + "-attempt2-97675795-84e0-4aea-a36f-cc97ac70663b"
	proof := clusterTaskProof{ID: durableID, RunID: runID, TaskID: publicID, Status: "running",
		Attempt: 2, ClaimAttempt: 2, RuntimeID: currentPod}
	raw := func(kind, nonce, podName string) recorder.Event {
		body, err := json.Marshal(rawAttemptEffect{RunID: runID, Step: "hold", Nonce: nonce, PodName: podName, Event: kind})
		require.NoError(t, err)
		return recorder.Event{RunID: runID, Step: "hold", Kind: kind, Nonce: nonce, Raw: string(body)}
	}
	payload, err := json.Marshal(taskStartedAttempt{DurableID: durableID, RunID: runID, TaskID: publicID,
		RuntimeID: currentPod, Attempt: 2, ClaimAttempt: 2})
	require.NoError(t, err)
	events := []eventTuple{{Sequence: 9, Type: "task_started", TaskID: publicID, Payload: string(payload)}}
	currentStart := raw("start", "current-start", currentPod)
	oldStart := raw("start", "old-start", oldPod)
	require.NoError(t, verifySeedHeldAttempt(proof, []recorder.Event{oldStart, currentStart}, events))
	require.ErrorContains(t, verifySeedHeldAttempt(proof, []recorder.Event{oldStart}, events), "has no raw start",
		"an old raw start cannot qualify a newly advanced durable runtime at Seed")
	require.Error(t, verifySeedHeldAttempt(proof, []recorder.Event{oldStart, currentStart}, nil),
		"raw effects cannot replace a persisted task_started identity")
	require.ErrorContains(t, verifySeedHeldAttempt(proof,
		[]recorder.Event{oldStart, currentStart, raw("complete", "current-start", currentPod)}, events),
		"already emitted a completion")
}

func TestReconcileRetainedAttemptEffects(t *testing.T) {
	const (
		runID     = "7c39392d-d1bf-43fc-b803-fce7d606141f"
		publicID  = "148d4542-8df5-4bf2-8ff4-d542af055498"
		durableID = "45b15c10-f7ea-4987-9fcd-d05e0522bbd9"
	)
	pod := func(claim int, suffix string) string {
		return publicID + "-" + runID + "-attempt" + string(rune('0'+claim)) + "-" + suffix
	}
	seedPod := pod(1, "bd606dbc-2d65-4ef4-a0d2-d795606e2673")
	retryPod := pod(2, "97675795-84e0-4aea-a36f-cc97ac70663b")
	ghostPod := pod(3, "510e014b-11a1-48b2-8237-4827961f7090")
	seed := clusterTaskProof{ID: durableID, RunID: runID, TaskID: publicID,
		Attempt: 1, ClaimAttempt: 1, RuntimeID: seedPod, Status: "running"}
	terminal := seed
	terminal.Attempt, terminal.ClaimAttempt = 2, 2
	terminal.RuntimeID, terminal.RecorderPod = retryPod, retryPod
	terminal.Status, terminal.RecorderNonce = "succeeded", "retry-complete"
	raw := func(kind, nonce, podName string) recorder.Event {
		body, err := json.Marshal(rawAttemptEffect{RunID: runID, Step: "hold", Nonce: nonce, PodName: podName, Event: kind})
		require.NoError(t, err)
		return recorder.Event{RunID: runID, Step: "hold", Kind: kind, Nonce: nonce, Raw: string(body)}
	}
	started := func(sequence uint64, attempt, claim int, podName string) eventTuple {
		payload, err := json.Marshal(taskStartedAttempt{DurableID: durableID, RunID: runID,
			TaskID: publicID, RuntimeID: podName, Attempt: attempt, ClaimAttempt: claim})
		require.NoError(t, err)
		return eventTuple{Sequence: sequence, Type: "task_started", TaskID: publicID, Payload: string(payload)}
	}
	validRaw := []recorder.Event{raw("start", "seed-start", seedPod),
		raw("start", "retry-complete", retryPod), raw("complete", "retry-complete", retryPod)}
	validTasks := []eventTuple{started(11, 1, 1, seedPod), started(12, 2, 2, retryPod)}
	require.NoError(t, reconcileRetainedAttemptEffects(seed, terminal, []string{"seed-start"}, validRaw, validTasks),
		"each raw nonce must join a persisted durable task_started attempt by pod name")
	sameAttempt := seed
	sameAttempt.Status, sameAttempt.RecorderNonce, sameAttempt.RecorderPod = "succeeded", "seed-start", seedPod
	require.NoError(t, reconcileRetainedAttemptEffects(seed, sameAttempt, []string{"seed-start"},
		[]recorder.Event{raw("start", "seed-start", seedPod), raw("start", "duplicate-start", seedPod),
			raw("complete", "seed-start", seedPod)}, validTasks[:1]),
		"duplicate raw starts from one durable attempt remain observable")
	for _, tc := range []struct {
		name   string
		change func(*clusterTaskProof, *[]string, *[]recorder.Event, *[]eventTuple)
	}{
		{"missing task_started", func(_ *clusterTaskProof, _ *[]string, _ *[]recorder.Event, e *[]eventTuple) { *e = nil }},
		{"missing pod name", func(_ *clusterTaskProof, _ *[]string, r *[]recorder.Event, _ *[]eventTuple) {
			(*r)[1].Raw = `{"run_id":"` + runID + `","step":"hold","nonce":"retry-complete","event":"start"}`
		}},
		{"wrong durable identity", func(_ *clusterTaskProof, _ *[]string, _ *[]recorder.Event, e *[]eventTuple) {
			(*e)[1].Payload = `{"id":"6000f2af-5260-4ad2-947c-788e4535b329"}`
		}},
		{"duplicate runtime event", func(_ *clusterTaskProof, _ *[]string, _ *[]recorder.Event, e *[]eventTuple) {
			*e = append(*e, (*e)[1])
		}},
		{"raw pod without persisted runtime", func(_ *clusterTaskProof, _ *[]string, r *[]recorder.Event, _ *[]eventTuple) {
			(*r)[1] = raw("start", "retry-complete", seedPod+"-foreign")
		}},
		{"completion from different pod", func(_ *clusterTaskProof, _ *[]string, r *[]recorder.Event, _ *[]eventTuple) {
			(*r)[2] = raw("complete", "retry-complete", seedPod)
		}},
		{"terminal runtime changed", func(p *clusterTaskProof, _ *[]string, _ *[]recorder.Event, _ *[]eventTuple) {
			p.RuntimeID = seedPod
		}},
		{"terminal claim changed", func(p *clusterTaskProof, _ *[]string, _ *[]recorder.Event, _ *[]eventTuple) {
			p.ClaimAttempt = 3
		}},
		{"missing intermediate attempt", func(p *clusterTaskProof, _ *[]string, _ *[]recorder.Event, _ *[]eventTuple) {
			p.Attempt = 3
		}},
		{"ghost attempt after terminal", func(_ *clusterTaskProof, _ *[]string, r *[]recorder.Event, e *[]eventTuple) {
			*e = append(*e, started(13, 3, 3, ghostPod))
			*r = append(*r, raw("start", "ghost-start", ghostPod))
		}},
		{"claim missing from pod name", func(_ *clusterTaskProof, _ *[]string, _ *[]recorder.Event, e *[]eventTuple) {
			(*e)[1] = started(12, 2, 3, retryPod)
		}},
		{"orphan completion", func(_ *clusterTaskProof, _ *[]string, r *[]recorder.Event, _ *[]eventTuple) {
			*r = append(*r, raw("complete", "orphan", retryPod))
		}},
		{"terminal completion missing", func(_ *clusterTaskProof, _ *[]string, r *[]recorder.Event, _ *[]eventTuple) {
			*r = (*r)[:2]
		}},
		{"baseline start missing", func(_ *clusterTaskProof, _ *[]string, r *[]recorder.Event, _ *[]eventTuple) {
			*r = (*r)[1:]
		}},
		{"duplicate baseline nonce", func(_ *clusterTaskProof, b *[]string, _ *[]recorder.Event, _ *[]eventTuple) {
			*b = append(*b, "seed-start")
		}},
		{"no baseline nonce", func(_ *clusterTaskProof, b *[]string, _ *[]recorder.Event, _ *[]eventTuple) {
			*b = nil
		}},
		{"prior attempt completion", func(p *clusterTaskProof, _ *[]string, r *[]recorder.Event, _ *[]eventTuple) {
			p.RecorderNonce, p.RecorderPod = "seed-start", seedPod
			*r = append(*r, raw("complete", "seed-start", seedPod))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := terminal
			baseline := []string{"seed-start"}
			rawEvents := append([]recorder.Event(nil), validRaw...)
			taskEvents := append([]eventTuple(nil), validTasks...)
			tc.change(&candidate, &baseline, &rawEvents, &taskEvents)
			require.Error(t, reconcileRetainedAttemptEffects(seed, candidate, baseline, rawEvents, taskEvents))
		})
	}
}
