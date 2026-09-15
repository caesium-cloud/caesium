//go:build integration

package test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

type deadlineWitness struct {
	mu     sync.Mutex
	events map[string]time.Time
}

func (w *deadlineWitness) record(key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events[key] = time.Now()
}

func (w *deadlineWitness) at(key string) (time.Time, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	at, ok := w.events[key]
	return at, ok
}

// TestDistributedKubernetesDeadlines is intentionally selected by the Helm CI
// distributed-Kubernetes phase. It observes the actual worker claim, exact pod
// deletion, retained terminal state and an independent callback that a leaked
// TERM-ignoring container would send after its deadline.
func (s *IntegrationTestSuite) TestDistributedKubernetesDeadlines() {
	if s.engineType != "kubernetes" || os.Getenv("CAESIUM_EXECUTION_MODE") != "distributed" {
		s.T().Skip("requires the explicit distributed Kubernetes integration phase")
	}

	witnessHost := strings.TrimSpace(os.Getenv("CAESIUM_TEST_WITNESS_HOST"))
	s.Require().NotEmpty(witnessHost, "CAESIUM_TEST_WITNESS_HOST must advertise the kind bridge gateway")
	kubeconfig := strings.TrimSpace(os.Getenv("KUBECONFIG"))
	s.Require().NotEmpty(kubeconfig, "KUBECONFIG must expose the test kind cluster")

	witness := &deadlineWitness{events: make(map[string]time.Time)}
	listener, err := (&net.ListenConfig{}).Listen(s.T().Context(), "tcp", "0.0.0.0:0")
	s.Require().NoError(err)
	server := &http.Server{
		ReadHeaderTimeout: 2 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			witness.record(strings.TrimPrefix(r.URL.Path, "/"))
			w.WriteHeader(http.StatusNoContent)
		}),
	}
	go func() { _ = server.Serve(listener) }()
	s.T().Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	})
	witnessAddress := net.JoinHostPort(witnessHost, strconv.Itoa(listener.Addr().(*net.TCPAddr).Port))

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	s.Require().NoError(err)
	client, err := kubernetes.NewForConfig(config)
	s.Require().NoError(err)

	// Zero metadata values retain the configured defaults rather than becoming
	// immediate deadlines. This also proves the pod can reach the witness before
	// an absence-of-late-callback assertion is meaningful.
	controlToken := "control-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	controlAlias := "distributed-k8s-deadline-" + controlToken
	controlScript := fmt.Sprintf("wget -qO /dev/null %q; echo CONTROL_REACHED", "http://"+witnessAddress+"/"+controlToken+"/control")
	controlManifest := distributedKubernetesDeadlineManifest(controlAlias, "taskTimeout: 0s\n  runTimeout: 0s", controlScript)
	controlDir := s.writeJobManifest(controlManifest)
	defer os.RemoveAll(controlDir)
	s.runCLI("job", "apply", "--path", controlDir, "--server", s.caesiumURL)
	controlJob := s.requireJobByAlias(controlAlias)
	controlRunID := s.triggerRun(controlJob.ID)
	controlRun := s.awaitRun(controlJob.ID, controlRunID, runTimeout)
	s.Equal("succeeded", controlRun.Status)
	s.Require().Eventually(func() bool {
		_, ok := witness.at(controlToken + "/control")
		return ok
	}, 10*time.Second, 100*time.Millisecond, "the Kubernetes pod could not reach the independent witness")

	// A run deadline starts at row creation, before dispatch and pod startup.
	// Leave those stages room even on a busy CI node; the witness below still
	// waits beyond the selected deadline to prove that the container stopped.
	s.runDistributedKubernetesDeadlineScenario(client, witness, witnessAddress, "task", 30*time.Second, 2*time.Minute)
	s.runDistributedKubernetesDeadlineScenario(client, witness, witnessAddress, "run", 2*time.Minute, time.Minute)
}

func (s *IntegrationTestSuite) runDistributedKubernetesDeadlineScenario(
	client kubernetes.Interface,
	witness *deadlineWitness,
	witnessAddress, deadlineKind string,
	taskTimeout, runTimeoutLimit time.Duration,
) {
	s.T().Helper()
	token := deadlineKind + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	alias := "distributed-k8s-deadline-" + token
	marker := "SHOULD_NOT_REACH_" + strings.ToUpper(deadlineKind)
	startURL := "http://" + witnessAddress + "/" + token + "/started"
	lateURL := "http://" + witnessAddress + "/" + token + "/late"
	selectedTimeout := taskTimeout
	if deadlineKind == "run" {
		selectedTimeout = runTimeoutLimit
	}
	lateDelay := selectedTimeout + 5*time.Second
	script := fmt.Sprintf("set -eu\nwget -qO /dev/null %q\necho STARTED_%s\ntrap '' TERM\nsleep %d\nwget -qO /dev/null %q\necho %s",
		startURL, strings.ToUpper(deadlineKind), int(lateDelay/time.Second), lateURL, marker)
	metadata := fmt.Sprintf("taskTimeout: %s\n  runTimeout: %s", taskTimeout, runTimeoutLimit)
	dir := s.writeJobManifest(distributedKubernetesDeadlineManifest(alias, metadata, script))
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	taskID := s.jobTaskIDByName(job.ID, "deadline")
	runID := s.triggerRun(job.ID)

	var runningTask runTaskResponse
	s.Require().Eventually(func() bool {
		var observed runResponse
		if err := s.tryGetJSON(fmt.Sprintf("/v1/jobs/%s/runs/%s", job.ID, runID), &observed); err != nil {
			return false
		}
		for _, task := range observed.Tasks {
			if task.ID == taskID && task.Status == "running" && task.ClaimedBy != "" && task.ClaimAttempt > 0 && task.RuntimeID != "" {
				runningTask = task
				return true
			}
		}
		return false
	}, selectedTimeout, 100*time.Millisecond,
		"task never exposed a distributed worker claim and exact runtime id")

	var podNamespace string
	var podUID string
	s.Require().Eventually(func() bool {
		pods, err := client.CoreV1().Pods("").List(s.T().Context(), metav1.ListOptions{})
		if err != nil {
			return false
		}
		for _, pod := range pods.Items {
			if pod.Name == runningTask.RuntimeID {
				podNamespace = pod.Namespace
				podUID = string(pod.UID)
				return podUID != ""
			}
		}
		return false
	}, 10*time.Second, 100*time.Millisecond, "the claimed runtime pod was never observed")

	var startedAt time.Time
	s.Require().Eventually(func() bool {
		var ok bool
		startedAt, ok = witness.at(token + "/started")
		return ok
	}, selectedTimeout, 100*time.Millisecond, "pod startup did not reach the witness before the configured deadline; inspect scheduling/image-pull events")

	finished := s.awaitRun(job.ID, runID, selectedTimeout+30*time.Second)
	s.Equal("failed", finished.Status)
	s.Require().Len(finished.Tasks, 1)
	s.Equal("failed", finished.Tasks[0].Status)
	s.Empty(finished.Tasks[0].Output, "a deadline must not publish post-deadline structured output")
	s.NotEqual("success", finished.Tasks[0].Result, "late runtime completion must not overwrite the timeout")
	if deadlineKind == "run" {
		s.Contains(finished.Error, "run timed out after "+runTimeoutLimit.String())
		s.Contains(finished.Tasks[0].Error, "run timed out after "+runTimeoutLimit.String())
	} else {
		s.NotContains(finished.Error, "run timed out")
		s.Contains(finished.Tasks[0].Error, "timed out after "+taskTimeout.String())
	}

	s.Require().Eventually(func() bool {
		pods, err := client.CoreV1().Pods(podNamespace).List(s.T().Context(), metav1.ListOptions{})
		if err != nil {
			return false
		}
		for _, pod := range pods.Items {
			if string(pod.UID) == podUID {
				return false
			}
		}
		return true
	}, 15*time.Second, 100*time.Millisecond, "the exact deadline pod remained present after terminal persistence")

	deadline := startedAt.Add(lateDelay + time.Second)
	for time.Now().Before(deadline) {
		if _, ok := witness.at(token + "/late"); ok {
			s.T().Fatalf("%s deadline pod executed its post-deadline callback", deadlineKind)
		}
		time.Sleep(100 * time.Millisecond)
	}
	_, late := witness.at(token + "/late")
	s.False(late, "%s deadline pod executed after termination", deadlineKind)

	retained := s.fetchRun(job.ID, runID)
	s.Equal("failed", retained.Status, "late runtime activity must not overwrite the terminal run")
	s.Require().Len(retained.Tasks, 1)
	s.Equal("failed", retained.Tasks[0].Status, "late runtime activity must not overwrite the terminal task")
	s.Empty(retained.Tasks[0].Output)
	s.NotEqual("success", retained.Tasks[0].Result)
}

func distributedKubernetesDeadlineManifest(alias, metadataTimeouts, script string) string {
	return fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  %s
trigger:
  type: cron
  configuration:
    cron: "0 0 1 1 *"
steps:
  - name: deadline
    image: alpine:3.23
    command: ["sh", "-c", %q]
`, alias, metadataTimeouts, script)
}
