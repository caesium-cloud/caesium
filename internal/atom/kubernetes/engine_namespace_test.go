package kubernetes

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

const (
	testDefaultNamespace = "caesium"
	testMappedNamespace  = "caesium-marketing"
)

var podsResource = schema.GroupVersionResource{Version: "v1", Resource: "pods"}

func fakeNamespacedEngine(ctx context.Context, client *fake.Clientset) *kubernetesEngine {
	core := client.CoreV1()
	return &kubernetesEngine{
		ctx:              ctx,
		pods:             func(namespace string) kubernetesBackend { return core.Pods(namespace) },
		defaultNamespace: testDefaultNamespace,
	}
}

func terminalPod(namespace, name string, phase v1.PodPhase) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: map[string]string{atom.Label: ""}},
		Status:     v1.PodStatus{Phase: phase},
	}
}

// actionNamespaces lists the namespaces of every recorded action of verb on pods.
func actionNamespaces(client *fake.Clientset, verb string) []string {
	var out []string
	for _, action := range client.Actions() {
		if action.GetVerb() == verb && action.GetResource().Resource == "pods" {
			out = append(out, action.GetNamespace())
		}
	}
	return out
}

func TestCreateRunsInTheMappedNamespaceWithServiceAccountPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stepSA  string
		target  atom.KubeTarget
		wantNS  string
		wantSA  string
		claimed bool
	}{
		{name: "zero target is the engine default", wantNS: testDefaultNamespace},
		{name: "mapped namespace, namespace default account", target: atom.KubeTarget{Namespace: testMappedNamespace, ServiceAccountName: "marketing-jobs"}, wantNS: testMappedNamespace, wantSA: "marketing-jobs", claimed: true},
		{name: "step account wins over the namespace default", stepSA: "step-sa", target: atom.KubeTarget{Namespace: testMappedNamespace, ServiceAccountName: "marketing-jobs"}, wantNS: testMappedNamespace, wantSA: "step-sa"},
		{name: "account default without a namespace mapping", target: atom.KubeTarget{ServiceAccountName: "shared-jobs"}, wantNS: testDefaultNamespace, wantSA: "shared-jobs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewClientset()
			engine := fakeNamespacedEngine(context.Background(), client)
			spec := container.Spec{}
			if tc.stepSA != "" {
				spec.Kubernetes = &container.KubernetesSpec{ServiceAccountName: tc.stepSA}
			}
			if tc.claimed {
				spec.ResolvedVolumeMounts = []container.VolumeMount{{Name: "data", Type: container.VolumeMountTypePVC, Source: "marketing-data", Target: "/data"}}
			}
			created, err := engine.Create(&atom.EngineCreateRequest{Name: "step", Image: "busybox:1.36.1", Spec: spec, Target: tc.target})
			require.NoError(t, err)
			ns, name, err := resolveRuntimeID(created.ID(), "", "unused")
			require.NoError(t, err)
			require.Equal(t, tc.wantNS, ns, "runtime id %q", created.ID())
			require.Equal(t, qualifyRuntimeID(tc.wantNS, name), created.ID(), "new pods always get a qualified runtime id")

			pod, err := client.CoreV1().Pods(tc.wantNS).Get(context.Background(), name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, tc.wantNS, pod.Namespace)
			require.Equal(t, tc.wantSA, pod.Spec.ServiceAccountName)
			if tc.claimed {
				require.Len(t, pod.Spec.Volumes, 1)
				require.NotNil(t, pod.Spec.Volumes[0].PersistentVolumeClaim)
				require.Equal(t, "marketing-data", pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName,
					"a claim is namespace-local: the pod in the mapped namespace mounts that namespace's PVC")
			}
			require.Equal(t, []string{tc.wantNS}, actionNamespaces(client, "create"))
		})
	}
}

func TestQualifiedRuntimeIDAddressesItsNamespace(t *testing.T) {
	client := fake.NewClientset(
		terminalPod(testMappedNamespace, "job-a", v1.PodSucceeded),
		terminalPod(testDefaultNamespace, "job-a", v1.PodFailed),
	)
	engine := fakeNamespacedEngine(context.Background(), client)
	qualified := qualifyRuntimeID(testMappedNamespace, "job-a")

	got, err := engine.Get(&atom.EngineGetRequest{ID: qualified})
	require.NoError(t, err)
	require.Equal(t, qualified, got.ID())
	require.Equal(t, atom.Success, got.Result(), "the mapped namespace's pod, not the same-named default one")

	waited, err := engine.Wait(&atom.EngineWaitRequest{ID: qualified, Namespace: testMappedNamespace})
	require.NoError(t, err)
	require.Equal(t, atom.Success, waited.Result())

	// A legacy unqualified id resolves in the engine default namespace.
	legacy, err := engine.Get(&atom.EngineGetRequest{ID: "job-a"})
	require.NoError(t, err)
	require.Equal(t, atom.Failure, legacy.Result())
	require.Equal(t, qualifyRuntimeID(testDefaultNamespace, "job-a"), legacy.ID())

	// A qualified id never silently addresses another namespace.
	_, err = engine.Get(&atom.EngineGetRequest{ID: qualified, Namespace: testDefaultNamespace})
	require.ErrorContains(t, err, "not the requested")
	require.ErrorContains(t, engine.Stop(&atom.EngineStopRequest{ID: qualified, Namespace: "finance"}), "not the requested")
	_, err = engine.Get(&atom.EngineGetRequest{ID: "a/b/c"})
	require.ErrorContains(t, err, "malformed runtime id")

	require.NoError(t, engine.Stop(&atom.EngineStopRequest{ID: qualified, Force: true}))
	_, err = client.CoreV1().Pods(testMappedNamespace).Get(context.Background(), "job-a", metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err), "Stop deletes the mapped namespace's pod")
	_, err = client.CoreV1().Pods(testDefaultNamespace).Get(context.Background(), "job-a", metav1.GetOptions{})
	require.NoError(t, err, "the same-named default pod is untouched")
}

func TestWaitWatchesInTheMappedNamespace(t *testing.T) {
	running := terminalPod(testMappedNamespace, "job-w", v1.PodRunning)
	client := fake.NewClientset(running)
	watcher := watch.NewFake()
	client.PrependWatchReactor("pods", func(action k8stesting.Action) (bool, watch.Interface, error) {
		return true, watcher, nil
	})
	engine := fakeNamespacedEngine(context.Background(), client)

	type waitResult struct {
		atom atom.Atom
		err  error
	}
	done := make(chan waitResult, 1)
	go func() {
		a, err := engine.Wait(&atom.EngineWaitRequest{ID: qualifyRuntimeID(testMappedNamespace, "job-w")})
		done <- waitResult{a, err}
	}()
	// The watch closes without a terminal event: Wait falls back to a Get,
	// which must address the same namespace.
	succeeded := running.DeepCopy()
	succeeded.Status.Phase = v1.PodSucceeded
	require.Eventually(t, func() bool { return len(actionNamespaces(client, "watch")) == 1 }, 5*time.Second, 10*time.Millisecond)
	_, err := client.CoreV1().Pods(testMappedNamespace).UpdateStatus(context.Background(), succeeded, metav1.UpdateOptions{})
	require.NoError(t, err)
	watcher.Stop()
	select {
	case got := <-done:
		require.NoError(t, got.err)
		require.Equal(t, atom.Success, got.atom.Result())
		require.Equal(t, qualifyRuntimeID(testMappedNamespace, "job-w"), got.atom.ID())
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after the watch closed")
	}
	require.Equal(t, []string{testMappedNamespace}, actionNamespaces(client, "watch"))
	for _, ns := range actionNamespaces(client, "get") {
		require.Equal(t, testMappedNamespace, ns)
	}
}

func TestCreateCancelledInAMappedNamespaceCleansUpThere(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod := action.(k8stesting.CreateAction).GetObject().(*v1.Pod)
		require.NoError(t, client.Tracker().Create(podsResource, pod, action.GetNamespace()))
		cancel()
		return true, pod, nil
	})
	engine := fakeNamespacedEngine(ctx, client)

	created, err := engine.Create(&atom.EngineCreateRequest{Name: "step", Image: "busybox:1.36.1", Target: atom.KubeTarget{Namespace: testMappedNamespace}})
	require.Nil(t, created)
	require.ErrorIs(t, err, context.Canceled)
	pods, err := client.CoreV1().Pods(testMappedNamespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Empty(t, pods.Items, "the pod allocated while the caller gave up is deleted in its own namespace")
	require.NotEmpty(t, actionNamespaces(client, "delete"))
	for _, ns := range actionNamespaces(client, "delete") {
		require.Equal(t, testMappedNamespace, ns)
	}
}

func TestCreateAmbiguousAllocationInAMappedNamespaceCleansUpThere(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod := action.(k8stesting.CreateAction).GetObject().(*v1.Pod)
		require.NoError(t, client.Tracker().Create(podsResource, pod, action.GetNamespace()))
		return true, nil, context.DeadlineExceeded
	})
	engine := fakeNamespacedEngine(context.Background(), client)

	_, err := engine.Create(&atom.EngineCreateRequest{Name: "step", Image: "busybox:1.36.1", Target: atom.KubeTarget{Namespace: testMappedNamespace}})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	pods, err := client.CoreV1().Pods(testMappedNamespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Empty(t, pods.Items)
	for _, ns := range actionNamespaces(client, "delete") {
		require.Equal(t, testMappedNamespace, ns)
	}
}

func TestListWalksEveryRequestedNamespace(t *testing.T) {
	client := fake.NewClientset(
		terminalPod(testDefaultNamespace, "a", v1.PodRunning),
		terminalPod(testMappedNamespace, "b", v1.PodRunning),
		terminalPod("finance-k8s", "c", v1.PodRunning),
		terminalPod("unmapped", "d", v1.PodRunning),
	)
	engine := fakeNamespacedEngine(context.Background(), client)

	atoms, err := engine.List(&atom.EngineListRequest{Namespace: testMappedNamespace, Namespaces: []string{"finance-k8s", testMappedNamespace, testDefaultNamespace, ""}})
	require.NoError(t, err)
	var ids []string
	for _, a := range atoms {
		ids = append(ids, a.ID())
	}
	require.ElementsMatch(t, []string{"caesium/a", "caesium-marketing/b", "finance-k8s/c"}, ids)
	require.ElementsMatch(t, []string{testDefaultNamespace, testMappedNamespace, "finance-k8s"}, actionNamespaces(client, "list"), "each namespace once")

	only, err := engine.List(&atom.EngineListRequest{})
	require.NoError(t, err)
	require.Len(t, only, 1, "no extra namespaces: the default only")
}

// recordingServer serves pod reads, logs and metrics for any namespace and
// records each request path.
type recordingServer struct {
	mu    sync.Mutex
	paths []string
}

func (r *recordingServer) handler(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.paths = append(r.paths, req.URL.Path)
	r.mu.Unlock()
	switch {
	case strings.HasSuffix(req.URL.Path, "/log"):
		_, _ = io.WriteString(w, "2026-10-21T00:00:00Z hello\n")
	case strings.HasPrefix(req.URL.Path, "/apis/metrics.k8s.io/"):
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"timestamp":  time.Now().UTC().Format(time.RFC3339),
			"containers": []map[string]any{{"name": "atom", "usage": map[string]string{"cpu": "1m", "memory": "1Mi"}}},
		})
	default:
		writeLogPod(w, &v1.Pod{Spec: v1.PodSpec{NodeName: "worker"}, Status: v1.PodStatus{Phase: v1.PodRunning}})
	}
}

func TestLogsAndStatsAddressTheMappedNamespace(t *testing.T) {
	rec := &recordingServer{}
	server := httptest.NewServer(http.HandlerFunc(rec.handler))
	t.Cleanup(server.Close)
	client, err := corev1.NewForConfig(&rest.Config{Host: server.URL})
	require.NoError(t, err)
	engine := &kubernetesEngine{
		ctx:              context.Background(),
		pods:             func(namespace string) kubernetesBackend { return client.Pods(namespace) },
		defaultNamespace: testDefaultNamespace,
		statsClient:      client.RESTClient(),
	}
	qualified := qualifyRuntimeID(testMappedNamespace, "job-l")

	logs, err := engine.Logs(&atom.EngineLogsRequest{ID: qualified, Namespace: testMappedNamespace})
	require.NoError(t, err)
	body, err := io.ReadAll(logs)
	require.NoError(t, err)
	require.Contains(t, string(body), "hello")
	require.NoError(t, logs.Close())

	_, _ = engine.Stats(&atom.EngineStatsRequest{ID: qualified})
	_, err = engine.Stats(&atom.EngineStatsRequest{ID: qualified, Namespace: "finance"})
	require.ErrorIs(t, err, atom.ErrStatsUnavailable)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	require.Contains(t, rec.paths, "/api/v1/namespaces/caesium-marketing/pods/job-l/log")
	require.Contains(t, rec.paths, "/apis/metrics.k8s.io/v1beta1/namespaces/caesium-marketing/pods/job-l")
	for _, path := range rec.paths {
		require.NotContains(t, path, "/namespaces/"+testDefaultNamespace+"/", "nothing reads the default namespace")
	}
}
