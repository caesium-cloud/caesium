//go:build integration

package robustness

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/test/robustness/cluster"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type startResponseRoundTripper struct {
	mu        sync.Mutex
	responses []string
	keys      []string
	bodies    []string
}

func (rt *startResponseRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	index := len(rt.keys)
	rt.keys = append(rt.keys, req.Header.Get("Idempotency-Key"))
	rt.bodies = append(rt.bodies, string(raw))
	response := rt.responses[min(index, len(rt.responses)-1)]
	return &http.Response{
		StatusCode: http.StatusAccepted,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(response)),
		Request:    req,
	}, nil
}

func newStartResponseRunner(responses ...string) (*soakRunner, *startResponseRoundTripper) {
	rt := &startResponseRoundTripper{responses: responses}
	sr := &soakRunner{
		fe:      &faultEnv{topo: cluster.Topology{Members: []cluster.Member{{Name: "member", IP: "192.0.2.10"}}}},
		client:  &http.Client{Transport: rt},
		faulted: map[string]string{},
	}
	return sr, rt
}

func TestSoakStartClassifiesAcceptedOutcomes(t *testing.T) {
	const (
		runID   = "e2a55b78-4f0e-4903-a9eb-36a3ff647959"
		queueID = "bcefa3b8-50a4-491e-8b4b-6d8cae604f17"
	)
	for _, tc := range []struct {
		name, body, outcome, runID, queueID string
		wantErr                             bool
	}{
		{name: "created valid identity", body: `{"outcome":"created","id":"` + runID + `"}`, outcome: "created", runID: runID},
		{name: "created missing identity", body: `{"outcome":"created"}`, outcome: "created", wantErr: true},
		{name: "created malformed identity", body: `{"outcome":"created","id":"bad"}`, outcome: "created", wantErr: true},
		{name: "queued valid queue", body: `{"outcome":"queued","queue_id":"` + queueID + `"}`, outcome: "queued", queueID: queueID},
		{name: "queued optional queue missing", body: `{"outcome":"queued"}`, outcome: "queued"},
		{name: "queued malformed queue", body: `{"outcome":"queued","queue_id":"bad"}`, outcome: "queued", wantErr: true},
		{name: "dropped valid queue", body: `{"outcome":"dropped","queue_id":"` + queueID + `"}`, outcome: "dropped", queueID: queueID},
		{name: "dropped optional queue missing", body: `{"outcome":"dropped"}`, outcome: "dropped"},
		{name: "dropped malformed queue", body: `{"outcome":"dropped","queue_id":"bad"}`, outcome: "dropped", wantErr: true},
		{name: "skipped concurrency", body: `{"outcome":"skipped"}`, outcome: "skipped"},
		{name: "skipped dataset hold", body: `{"outcome":"skipped","run_id":"` + runID + `"}`, outcome: "skipped", runID: runID},
		{name: "skipped malformed run", body: `{"outcome":"skipped","run_id":"bad"}`, outcome: "skipped", wantErr: true},
		{name: "missing outcome", body: `{}`, wantErr: true},
		{name: "unknown outcome", body: `{"outcome":"accepted"}`, outcome: "accepted", wantErr: true},
		{name: "non-JSON 202", body: `not JSON`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sr, _ := newStartResponseRunner(tc.body)
			got := sr.start(context.Background(), cluster.Member{IP: "192.0.2.10"}, "job-id", "same-key", nil, "")
			if got.HTTPStatus != http.StatusAccepted || got.Outcome != tc.outcome || got.RunID != tc.runID || got.QueueID != tc.queueID || (got.Err != "") != tc.wantErr || got.uncertain() != tc.wantErr {
				t.Fatalf("start outcome = %+v", got)
			}
		})
	}
}

func TestSoakStartUncertainOutcomeReplaysSameKeyAndStaysInconclusive(t *testing.T) {
	for _, body := range []string{`{"outcome":"future"}`, `{"outcome":"created","id":"bad"}`, `not JSON`} {
		t.Run(body, func(t *testing.T) {
			const key = "same-idempotency-key"
			sr, rt := newStartResponseRunner(body, body)
			member := cluster.Member{IP: "192.0.2.10"}
			entry := sr.startTracked(context.Background(), member, "job-id", key, map[string]string{"region": "west"}, "high", "test", nil, true)
			if err := sr.reconcileWithRetry(context.Background(), entry, 2, 0); err == nil {
				t.Fatal("repeated unknown outcome was treated as reconciled")
			}
			if entry.Reconciled || entry.RunID != "" || entry.Err == "" {
				t.Fatalf("unknown identity was not retained as inconclusive: %+v", entry)
			}
			rt.mu.Lock()
			defer rt.mu.Unlock()
			if len(rt.keys) != 3 {
				t.Fatalf("request keys = %v, want initial attempt plus two replays", rt.keys)
			}
			for _, got := range rt.keys {
				if got != key {
					t.Fatalf("replay key = %q, want %q", got, key)
				}
			}
			if rt.bodies[0] != rt.bodies[1] || rt.bodies[1] != rt.bodies[2] {
				t.Fatalf("replay request bodies differ: %q", rt.bodies)
			}
		})
	}
}

func TestFinalDrainEvidenceFiltersPodsAndFindsOwnedLeaks(t *testing.T) {
	const token = "soak-owner"
	ownedPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "owned-pod"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Env: []corev1.EnvVar{{Name: "CAESIUM_SOAK_OWNER", Value: token}}}}},
	}
	unownedPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "unowned-pod"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Env: []corev1.EnvVar{{Name: "CAESIUM_SOAK_OWNER", Value: "another-soak"}}}}},
	}
	baseInventory := containerInventoryEvidence{
		Counts:   map[string]int{"owned_task_containers": 0, "task_running": 0, "task_pods": 1},
		TaskPods: []taskPodInventoryEntry{{Pod: "unowned-pod", Owned: false}},
	}
	base := finalDrainObservation{
		podInventoryObserved: true, podInventoryReadable: true,
		containerInventoryObserved: true, containerInventoryReadable: true,
		containerInventory: baseInventory,
	}

	if failures, inconclusive := finalDrainEvidence(base, token); len(failures) != 0 || len(inconclusive) != 0 {
		t.Fatalf("unowned task pod blocked drain: failures=%v inconclusive=%v", failures, inconclusive)
	}
	base.taskPods = []corev1.Pod{unownedPod}
	if failures, inconclusive := finalDrainEvidence(base, token); len(failures) != 0 || len(inconclusive) != 0 {
		t.Fatalf("unowned Kubernetes pod blocked drain: failures=%v inconclusive=%v", failures, inconclusive)
	}

	base.taskPods = []corev1.Pod{ownedPod}
	base.containerInventory.Counts["owned_task_containers"] = 2
	base.containerInventory.Counts["task_running"] = 1
	base.containerInventory.TaskPods = append(base.containerInventory.TaskPods, taskPodInventoryEntry{Pod: "owned-pod", Owned: true})
	failures, inconclusive := finalDrainEvidence(base, token)
	if len(failures) != 3 || len(inconclusive) != 0 {
		t.Fatalf("owned leaks = failures %v, inconclusive %v; want pod, container and running failures", failures, inconclusive)
	}
}

func TestFinalDrainEvidenceBlocksMissingMalformedAndEmbeddedErrorInventory(t *testing.T) {
	valid := `{"counts":{"owned_task_containers":0,"task_running":0,"task_pods":0},"task_pods":[]}`
	parsed, err := parseContainerInventoryEvidence(valid)
	if err != nil || !containerInventorySettled(parsed) {
		t.Fatalf("valid empty inventory = %+v, %v", parsed, err)
	}
	for _, raw := range []string{
		"not JSON",
		`{"counts":{},"task_pods":[]}`,
		`{"counts":{"owned_task_containers":0,"task_running":0,"task_pods":0},"task_pods":[],"error":"node list failed"}`,
	} {
		if _, err := parseContainerInventoryEvidence(raw); err == nil {
			t.Errorf("inventory %s was accepted", raw)
		}
	}

	observation := finalDrainObservation{}
	_, gaps := finalDrainEvidence(observation, "token")
	if len(gaps) != 2 {
		t.Fatalf("missing inventories gaps = %v, want both pod and host inventory gaps", gaps)
	}
	lastGood, err := parseContainerInventoryEvidence(`{"counts":{"owned_task_containers":1,"task_running":0,"task_pods":0},"task_pods":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	observation.containerInventoryObserved = true
	observation.containerInventoryReadable = false // A later malformed read must not erase the last successful leak evidence.
	observation.containerInventory = lastGood
	failures, gaps := finalDrainEvidence(observation, "token")
	if len(failures) != 1 || len(gaps) != 2 {
		t.Fatalf("last successful observation = failures %v, gaps %v; want retained leak plus missing pod/current host evidence", failures, gaps)
	}
}

func TestCheckpointPollerCancelsInFlightQueryAndJoinsOnFailure(t *testing.T) {
	queryStarted := make(chan struct{})
	queryCanceled := make(chan struct{})
	var poller *checkpointPoller
	injectedFailure := errors.New("injected retention failure")
	err := func() error {
		poller = startCheckpointPoller(context.Background(), time.Hour, time.Minute,
			func(ctx context.Context) ([]int64, error) {
				close(queryStarted)
				<-ctx.Done()
				close(queryCanceled)
				return nil, ctx.Err()
			},
			func([]int64, error) {},
		)
		defer poller.stop()
		select {
		case <-queryStarted:
			return injectedFailure
		case <-time.After(2 * time.Second):
			return errors.New("checkpoint poll query did not start")
		}
	}()
	if !errors.Is(err, injectedFailure) {
		t.Fatalf("failure path returned %v, want injected failure", err)
	}
	select {
	case <-queryCanceled:
	default:
		t.Fatal("deferred poller stop returned before canceling the in-flight query")
	}
	select {
	case <-poller.done:
	default:
		t.Fatal("deferred poller stop returned before joining the query goroutine")
	}
}
