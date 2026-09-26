package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

func newHTTPLogEngine(t *testing.T, ctx context.Context, handler http.HandlerFunc) *kubernetesEngine {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := corev1.NewForConfig(&rest.Config{Host: server.URL})
	require.NoError(t, err)
	return &kubernetesEngine{ctx: ctx, backend: client.Pods("logs-test")}
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
