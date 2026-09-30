package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

func newHTTPLogEngine(t *testing.T, ctx context.Context, handler http.HandlerFunc) *kubernetesEngine {
	t.Helper()
	return newHTTPLogEngineWithHandler(t, ctx, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/log") {
			writeLogPod(w, &v1.Pod{Spec: v1.PodSpec{NodeName: "worker"}, Status: v1.PodStatus{Phase: v1.PodRunning}})
			return
		}
		handler(w, r)
	})
}

func newHTTPLogEngineWithHandler(t *testing.T, ctx context.Context, handler http.HandlerFunc) *kubernetesEngine {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := corev1.NewForConfig(&rest.Config{Host: server.URL})
	require.NoError(t, err)
	return &kubernetesEngine{ctx: ctx, backend: client.Pods("logs-test")}
}

func writeLogPod(w http.ResponseWriter, pod *v1.Pod) {
	pod.TypeMeta = metav1.TypeMeta{Kind: "Pod", APIVersion: "v1"}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(pod)
}

func writeLogAPIError(w http.ResponseWriter, code int, reason metav1.StatusReason, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(metav1.Status{
		TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status:   metav1.StatusFailure, Reason: reason, Message: message, Code: int32(code),
	})
}

func pendingLogMessage(pod, reason string) string {
	return fmt.Sprintf("container %q in pod %q is waiting to start: %s", "atom", pod, reason)
}

func TestLogsRetriesContainerReadinessThroughHTTPAPI(t *testing.T) {
	var attempts atomic.Int32
	since := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	engine := newHTTPLogEngine(t, context.Background(), func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/namespaces/logs-test/pods/task/log" ||
			r.URL.Query().Get("follow") != "true" || r.URL.Query().Get("timestamps") != "true" ||
			r.URL.Query().Get("sinceTime") != since.Format(time.RFC3339) {
			t.Errorf("unexpected log request: %s", r.URL.String())
		}
		switch attempts.Add(1) {
		case 1:
			writeLogAPIError(w, http.StatusBadRequest, metav1.StatusReasonBadRequest, pendingLogMessage("task", "ContainerCreating"))
		case 2:
			writeLogAPIError(w, http.StatusBadRequest, metav1.StatusReasonBadRequest, pendingLogMessage("task", "PodInitializing"))
		default:
			_, _ = io.WriteString(w, "live safe output\n")
		}
	})
	reader, err := engine.Logs(&atom.EngineLogsRequest{ID: "task", Since: since})
	require.NoError(t, err)
	t.Cleanup(func() { _ = reader.Close() })
	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, "live safe output\n", string(data))
	require.Equal(t, int32(3), attempts.Load())
}

func TestLogsDoesNotRetryPermanentHTTPAPIErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		code    int
		reason  metav1.StatusReason
		message string
	}{
		{"unauthorized", http.StatusUnauthorized, metav1.StatusReasonUnauthorized, "credentials required"},
		{"forbidden", http.StatusForbidden, metav1.StatusReasonForbidden, pendingLogMessage("task", "ContainerCreating")},
		{"missing pod", http.StatusNotFound, metav1.StatusReasonNotFound, "pod task not found"},
		{"bad request", http.StatusBadRequest, metav1.StatusReasonBadRequest, "invalid log options"},
		{"image pull failure", http.StatusBadRequest, metav1.StatusReasonBadRequest, pendingLogMessage("task", "ErrImagePull")},
		{"crash loop", http.StatusBadRequest, metav1.StatusReasonBadRequest, pendingLogMessage("task", "CrashLoopBackOff")},
		{"different pod", http.StatusBadRequest, metav1.StatusReasonBadRequest, pendingLogMessage("other", "ContainerCreating")},
		{"different container", http.StatusBadRequest, metav1.StatusReasonBadRequest, `container "other" in pod "task" is waiting to start: ContainerCreating`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			engine := newHTTPLogEngine(t, context.Background(), func(w http.ResponseWriter, _ *http.Request) {
				attempts.Add(1)
				writeLogAPIError(w, tc.code, tc.reason, tc.message)
			})
			reader, err := engine.Logs(&atom.EngineLogsRequest{ID: "task"})
			require.ErrorContains(t, err, tc.message)
			require.Nil(t, reader)
			require.Equal(t, int32(1), attempts.Load())
		})
	}
}

func TestLogsReadinessWaitHonorsParentDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var attempts atomic.Int32
	engine := newHTTPLogEngine(t, ctx, func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		writeLogAPIError(w, http.StatusBadRequest, metav1.StatusReasonBadRequest, pendingLogMessage("task", "ContainerCreating"))
	})
	reader, err := engine.Logs(&atom.EngineLogsRequest{ID: "task"})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, reader)
	require.LessOrEqual(t, attempts.Load(), int32(1))
}

func TestLogsReadinessWaitHonorsParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requested := make(chan struct{}, 1)
	engine := newHTTPLogEngine(t, ctx, func(w http.ResponseWriter, _ *http.Request) {
		writeLogAPIError(w, http.StatusBadRequest, metav1.StatusReasonBadRequest, pendingLogMessage("task", "ContainerCreating"))
		select {
		case requested <- struct{}{}:
		default:
		}
	})
	done := make(chan error, 1)
	go func() {
		reader, err := engine.Logs(&atom.EngineLogsRequest{ID: "task"})
		if reader != nil {
			_ = reader.Close()
		}
		done <- err
	}()
	<-requested
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("readiness wait ignored cancellation")
	}
}

func TestLogsSetupTimeoutCancelsHungHTTPHeaders(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requestEnded := make(chan struct{})
	engine := newHTTPLogEngine(t, ctx, func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		close(requestEnded)
	})
	reader, err := openLogsWithinTestBudget(t, engine, cancel)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, reader)
	select {
	case <-requestEnded:
	case <-time.After(time.Second):
		t.Fatal("setup timeout leaked an HTTP request")
	}
}

func TestLogsSetupTimeoutBoundsRepeatedContainerReadinessErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var attempts atomic.Int32
	engine := newHTTPLogEngine(t, ctx, func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		writeLogAPIError(w, http.StatusBadRequest, metav1.StatusReasonBadRequest, pendingLogMessage("task", "ContainerCreating"))
	})
	reader, err := openLogsWithinTestBudget(t, engine, cancel)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, reader)
	require.Greater(t, attempts.Load(), int32(1), "the readiness error must be retried within the setup budget")
}

func openLogsWithinTestBudget(t *testing.T, engine *kubernetesEngine, cancel context.CancelFunc) (io.ReadCloser, error) {
	t.Helper()
	type result struct {
		reader io.ReadCloser
		err    error
	}
	done := make(chan result, 1)
	go func() {
		reader, err := engine.logsWhenReady(&atom.EngineLogsRequest{ID: "task"}, 500*time.Millisecond)
		done <- result{reader, err}
	}()
	watchdog := time.NewTimer(2 * time.Second)
	defer watchdog.Stop()
	select {
	case got := <-done:
		if got.reader != nil {
			t.Cleanup(func() { _ = got.reader.Close() })
		}
		return got.reader, got.err
	case <-watchdog.C:
		// Cancel and join before failing so a wrong opening budget cannot leave
		// the HTTP handler or readiness loop alive during server cleanup.
		cancel()
		select {
		case got := <-done:
			if got.reader != nil {
				_ = got.reader.Close()
			}
		case <-time.After(time.Second):
			t.Error("log setup did not stop after parent cancellation")
		}
		t.Fatal("500ms log setup budget exceeded the 2s outer watchdog")
		return nil, nil
	}
}

func TestLogsSetupTimeoutStopsAfterReturningLiveReader(t *testing.T) {
	continueStream := make(chan struct{})
	engine := newHTTPLogEngine(t, context.Background(), func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		select {
		case <-continueStream:
			_, _ = io.WriteString(w, "last\n")
		case <-r.Context().Done():
		}
	})
	const timeout = 500 * time.Millisecond
	reader, err := engine.logsWhenReady(&atom.EngineLogsRequest{ID: "task"}, timeout)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reader.Close() })
	// The stream must remain readable beyond its opening budget.
	<-time.After(2 * timeout)
	close(continueStream)
	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, "first\nlast\n", string(data))
}

func TestLogsReaderCloseAndParentCancelReleaseHTTPRequest(t *testing.T) {
	for _, action := range []string{"close", "cancel"} {
		t.Run(action, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			requestEnded := make(chan struct{})
			engine := newHTTPLogEngine(t, ctx, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, "live\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(requestEnded)
			})
			reader, err := engine.Logs(&atom.EngineLogsRequest{ID: "task"})
			require.NoError(t, err)
			t.Cleanup(func() { _ = reader.Close() })
			if action == "close" {
				require.NoError(t, reader.Close())
			} else {
				cancel()
			}
			select {
			case <-requestEnded:
			case <-time.After(time.Second):
				t.Fatal("returned reader leaked its HTTP request")
			}
		})
	}
}

func TestLogsRetriesUnscheduledAndKubeletReadinessThroughHTTPAPI(t *testing.T) {
	for _, tc := range []struct {
		name    string
		code    int
		message string
		pod     *v1.Pod
	}{
		{"unscheduled", http.StatusNoContent, "", &v1.Pod{Status: v1.PodStatus{Phase: v1.PodPending}}},
		{"Kueue gated", http.StatusNoContent, "", &v1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{kueueQueueLabel: "queue"}}, Spec: v1.PodSpec{SchedulingGates: []v1.PodSchedulingGate{{Name: "kueue.x-k8s.io/admission"}}}, Status: v1.PodStatus{Phase: v1.PodPending}}},
		{"scheduled empty nonterminal", http.StatusNoContent, "", &v1.Pod{Spec: v1.PodSpec{NodeName: "worker"}, Status: v1.PodStatus{Phase: v1.PodRunning}}},
		{"kubelet pod not synced", http.StatusNotFound, `pod "task" does not exist`, &v1.Pod{Spec: v1.PodSpec{NodeName: "worker"}, Status: v1.PodStatus{Phase: v1.PodPending}}},
		{"container not available", http.StatusBadRequest, `container "atom" in pod "task" is not available`, &v1.Pod{Spec: v1.PodSpec{NodeName: "worker"}, Status: v1.PodStatus{Phase: v1.PodPending}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logRequests, podRequests atomic.Int32
			engine := newHTTPLogEngineWithHandler(t, context.Background(), func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/log") {
					podRequests.Add(1)
					writeLogPod(w, tc.pod)
					return
				}
				if logRequests.Add(1) < 3 {
					if tc.code == http.StatusNoContent {
						w.WriteHeader(tc.code)
					} else {
						reason := metav1.StatusReasonBadRequest
						if tc.code == http.StatusNotFound {
							reason = metav1.StatusReasonNotFound
						}
						writeLogAPIError(w, tc.code, reason, tc.message)
					}
					return
				}
				_, _ = io.WriteString(w, "##caesium::output {\"ok\":\"yes\"}\nlast safe line\n")
			})
			reader, err := engine.Logs(&atom.EngineLogsRequest{ID: "task"})
			require.NoError(t, err)
			t.Cleanup(func() { _ = reader.Close() })
			data, err := io.ReadAll(reader)
			require.NoError(t, err)
			require.Equal(t, "##caesium::output {\"ok\":\"yes\"}\nlast safe line\n", string(data), "the peek must preserve every marker byte")
			require.Equal(t, int32(3), logRequests.Load())
			require.Equal(t, int32(2), podRequests.Load())
		})
	}
}

func TestLogsReadinessDoesNotMaskMissingOrPermanentlyUnavailablePods(t *testing.T) {
	for _, tc := range []struct {
		name    string
		logCode int
		message string
		pod     *v1.Pod
		getCode int
	}{
		{"true missing apiserver pod", http.StatusNotFound, `pod "task" does not exist`, nil, 0},
		{"kubelet missing then apiserver missing", http.StatusNotFound, `pod "task" does not exist`, nil, http.StatusNotFound},
		{"unavailable container with image failure", http.StatusBadRequest, `container "atom" in pod "task" is not available`, &v1.Pod{Status: v1.PodStatus{Phase: v1.PodPending, ContainerStatuses: []v1.ContainerStatus{{Name: "atom", State: v1.ContainerState{Waiting: &v1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}}}, 0},
		{"empty nonterminal with image failure", http.StatusNoContent, "", &v1.Pod{Status: v1.PodStatus{Phase: v1.PodPending, ContainerStatuses: []v1.ContainerStatus{{Name: "atom", State: v1.ContainerState{Waiting: &v1.ContainerStateWaiting{Reason: "ErrImagePull"}}}}}}, 0},
		{"empty deleting pod", http.StatusNoContent, "", &v1.Pod{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &metav1.Time{Time: time.Now()}}, Status: v1.PodStatus{Phase: v1.PodRunning}}, 0},
		{"empty but get forbidden", http.StatusNoContent, "", nil, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logRequests, podRequests atomic.Int32
			engine := newHTTPLogEngineWithHandler(t, context.Background(), func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/log") {
					podRequests.Add(1)
					if tc.getCode != 0 {
						if tc.getCode == http.StatusForbidden {
							writeLogAPIError(w, tc.getCode, metav1.StatusReasonForbidden, "pod lookup forbidden")
						} else {
							writeLogAPIError(w, tc.getCode, metav1.StatusReasonNotFound, "pod object is missing")
						}
					} else {
						writeLogPod(w, tc.pod)
					}
					return
				}
				logRequests.Add(1)
				if tc.logCode == http.StatusNoContent {
					w.WriteHeader(tc.logCode)
					return
				}
				if tc.name == "true missing apiserver pod" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusNotFound)
					_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}, Status: metav1.StatusFailure, Reason: metav1.StatusReasonNotFound, Message: tc.message, Code: http.StatusNotFound, Details: &metav1.StatusDetails{Kind: "pods", Name: "task"}})
					return
				}
				reason := metav1.StatusReasonBadRequest
				if tc.logCode == http.StatusNotFound {
					reason = metav1.StatusReasonNotFound
				}
				writeLogAPIError(w, tc.logCode, reason, tc.message)
			})
			reader, err := engine.Logs(&atom.EngineLogsRequest{ID: "task"})
			require.Error(t, err)
			require.Nil(t, reader)
			require.Equal(t, int32(1), logRequests.Load())
			wantGets := int32(1)
			if tc.name == "true missing apiserver pod" {
				wantGets = 0
			}
			require.Equal(t, wantGets, podRequests.Load())
		})
	}
}

func TestLogsEmptyTerminalPodRemainsValid(t *testing.T) {
	engine := newHTTPLogEngineWithHandler(t, context.Background(), func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/log") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeLogPod(w, &v1.Pod{Status: v1.PodStatus{Phase: v1.PodSucceeded}})
	})
	reader, err := engine.Logs(&atom.EngineLogsRequest{ID: "task"})
	require.NoError(t, err)
	defer reader.Close()
	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Empty(t, data)
}

func TestLogsSetupBudgetBoundsPodLookupAndFirstByte(t *testing.T) {
	for _, phase := range []string{"pod lookup", "first byte"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			requestEnded := make(chan struct{})
			engine := newHTTPLogEngineWithHandler(t, ctx, func(w http.ResponseWriter, r *http.Request) {
				if phase == "pod lookup" && strings.HasSuffix(r.URL.Path, "/log") {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				if phase == "first byte" {
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
				close(requestEnded)
			})
			reader, err := openLogsWithinTestBudget(t, engine, cancel)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.Nil(t, reader)
			select {
			case <-requestEnded:
			case <-time.After(time.Second):
				t.Fatal("opening budget leaked an HTTP request")
			}
		})
	}
}

func TestLogsSetupBudgetLeavesRESTWriteTime(t *testing.T) {
	require.Less(t, kubernetesLogReadinessTimeout, 30*time.Second, "log setup must leave time to respond before the REST write timeout")
}

func TestLogsParentCancellationReleasesPodLookup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lookupStarted := make(chan struct{})
	requestEnded := make(chan struct{})
	engine := newHTTPLogEngineWithHandler(t, ctx, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/log") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		close(lookupStarted)
		<-r.Context().Done()
		close(requestEnded)
	})
	done := make(chan error, 1)
	go func() {
		reader, err := engine.Logs(&atom.EngineLogsRequest{ID: "task"})
		if reader != nil {
			_ = reader.Close()
		}
		done <- err
	}()
	select {
	case <-lookupStarted:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("authoritative pod lookup did not start")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("pod lookup ignored parent cancellation")
	}
	select {
	case <-requestEnded:
	case <-time.After(time.Second):
		t.Fatal("parent cancellation leaked the pod lookup")
	}
}
