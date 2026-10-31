//go:build integration

package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	dockercontainer "github.com/docker/docker/api/types/container"
	dockerclient "github.com/docker/docker/client"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// The kind lane's mapped-namespace phase (identity-and-access C3,
// scripts/kind-mapped-namespace.sh) installs release `c3` in caesium-c3 with
// helm/caesium/ci/test-values-k8s-mapped.yaml: policy kubeteam →
// caesium-mapped / caesium-mapped-runner, chart-rendered RBAC, a PVC-backed
// database and one distributed node.
const (
	kubeMappedEnv         = "CAESIUM_TEST_KUBE_MAPPED"
	kubeMappedNamespace   = "caesium-mapped"
	kubeMappedAccount     = "caesium-mapped-runner"
	kubeMappedReleaseNS   = "caesium-c3"
	kubeMappedServerPod   = "c3-caesium-0"
	kubeMappedServerSA    = "c3-caesium"
	kubeMappedSecretValue = "c3-mapped-token"
	kubeMappedTenancy     = "kubeteam"
)

// holdWitness releases held steps: a step polls /hold/<token> and proceeds
// only once the test has released that token. It also records which tokens a
// step has asked for, which proves the step got that far.
type holdWitness struct {
	mu       sync.Mutex
	released map[string]bool
	asked    map[string]bool
}

func (w *holdWitness) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, "/hold/")
	w.mu.Lock()
	w.asked[token] = true
	released := w.released[token]
	w.mu.Unlock()
	if released {
		rw.WriteHeader(http.StatusOK)
		return
	}
	rw.WriteHeader(http.StatusNotFound)
}

func (w *holdWitness) release(token string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.released[token] = true
}

func (w *holdWitness) wasAsked(token string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.asked[token]
}

// TestKubernetesMappedNamespace proves on a real cluster that a job in a
// mapped tenancy namespace runs its pod, PVC and secret://k8s read in the
// mapped Kubernetes namespace with the mapped service account, through RBAC the
// chart rendered, and that after a graceful restart and after a crash recovery
// stops the previous attempt's pod through its persisted namespace-qualified
// runtime id and the re-dispatched attempt lands in the mapped namespace.
func (s *IntegrationTestSuite) TestKubernetesMappedNamespace() {
	if s.engineType != "kubernetes" || strings.TrimSpace(os.Getenv(kubeMappedEnv)) != "1" {
		s.T().Skipf("%s runs only in the kind lane's mapped-namespace phase (%s=1)", s.T().Name(), kubeMappedEnv)
	}
	kubeconfig := strings.TrimSpace(os.Getenv("KUBECONFIG"))
	s.Require().NotEmpty(kubeconfig, "KUBECONFIG must expose the kind cluster")
	witnessHost := strings.TrimSpace(os.Getenv("CAESIUM_TEST_WITNESS_HOST"))
	s.Require().NotEmpty(witnessHost, "CAESIUM_TEST_WITNESS_HOST must advertise the kind bridge gateway")
	kindNode := strings.TrimSpace(os.Getenv("CAESIUM_TEST_KIND_NODE"))
	s.Require().NotEmpty(kindNode, "CAESIUM_TEST_KIND_NODE names the kind node container")
	expectedImage := strings.TrimSpace(os.Getenv("CAESIUM_TEST_EXPECTED_IMAGE"))
	s.Require().NotEmpty(expectedImage, "CAESIUM_TEST_EXPECTED_IMAGE names the candidate image")

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	s.Require().NoError(err)
	client, err := kubernetes.NewForConfig(config)
	s.Require().NoError(err)
	ctx := s.T().Context()

	witness := &holdWitness{released: map[string]bool{}, asked: map[string]bool{}}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "0.0.0.0:0")
	s.Require().NoError(err)
	server := &http.Server{ReadHeaderTimeout: 2 * time.Second, Handler: witness}
	go func() { _ = server.Serve(listener) }()
	s.T().Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	})
	holdURL := "http://" + net.JoinHostPort(witnessHost, strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)) + "/hold/"

	// The release under test runs the CI candidate image.
	serverPod, err := client.CoreV1().Pods(kubeMappedReleaseNS).Get(ctx, kubeMappedServerPod, metav1.GetOptions{})
	s.Require().NoError(err)
	s.Require().Equal(expectedImage, kubeMappedContainer(serverPod, "caesium").Image, "the mapped release must run the CI candidate")

	s.Run("rbac", func() { s.assertKubeMappedRBAC(ctx, client) })
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	s.Run("placement", func() { s.runKubeMappedPlacement(ctx, client, witness, holdURL, suffix) })
	s.Run("graceful restart", func() { s.runKubeMappedRestart(ctx, client, witness, holdURL, suffix, false, kindNode) })
	s.Run("crash", func() { s.runKubeMappedRestart(ctx, client, witness, holdURL, suffix, true, kindNode) })
}

func kubeMappedContainer(pod *corev1.Pod, name string) corev1.Container {
	for _, c := range pod.Spec.Containers {
		if c.Name == name {
			return c
		}
	}
	return corev1.Container{}
}

// assertKubeMappedRBAC: the chart granted the server's service account pods
// and secrets in the mapped namespace and nothing in an unmapped one.
func (s *IntegrationTestSuite) assertKubeMappedRBAC(ctx context.Context, client kubernetes.Interface) {
	can := func(namespace, verb, resource string) bool {
		review, err := client.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authorizationv1.SubjectAccessReview{
			Spec: authorizationv1.SubjectAccessReviewSpec{
				User:   "system:serviceaccount:" + kubeMappedReleaseNS + ":" + kubeMappedServerSA,
				Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + kubeMappedReleaseNS, "system:authenticated"},
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Namespace: namespace, Verb: verb, Resource: resource,
				},
			},
		}, metav1.CreateOptions{})
		s.Require().NoError(err)
		return review.Status.Allowed
	}
	s.True(can(kubeMappedNamespace, "create", "pods"))
	s.True(can(kubeMappedNamespace, "delete", "pods"))
	s.True(can(kubeMappedNamespace, "get", "secrets"))
	s.True(can(kubeMappedNamespace, "list", "persistentvolumeclaims"))
	s.False(can(kubeMappedNamespace, "list", "secrets"), "secrets are get-only")
	s.False(can("caesium-unmapped", "get", "secrets"), "an unmapped namespace grants nothing")
	s.False(can("caesium-unmapped", "create", "pods"), "an unmapped namespace grants nothing")
}

func kubeMappedManifest(alias, script string, withScratch bool) string {
	volumes, mounts, env := "", "", ""
	if withScratch {
		volumes = `
volumes:
  - name: scratch
    sources:
      kubernetes:
        claimTemplate:
          size: 64Mi
`
		mounts = `
    volumeMounts:
      - volume: scratch
        path: /scratch`
		env = `
    env:
      TOKEN: secret://k8s/c3-token/value`
	}
	return fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  namespace: %s
trigger:
  type: http
  configuration:
    path: /hooks/%s
%ssteps:
  - name: step
    image: alpine:3.23
    command: ["sh", "-c", %q]%s%s
`, alias, kubeMappedTenancy, alias, volumes, script, env, mounts)
}

func holdScript(holdURL, token, marker string) string {
	return fmt.Sprintf("until wget -qO /dev/null %q; do sleep 1; done; echo %s", holdURL+token, marker)
}

// kubeMappedTaskRuntime polls the run until its single task records a runtime
// id other than previous, tolerating a server that is restarting.
func (s *IntegrationTestSuite) kubeMappedTaskRuntime(jobID, runID, previous string, timeout time.Duration) runTaskResponse {
	s.T().Helper()
	var task runTaskResponse
	s.Require().Eventually(func() bool {
		run, ok := s.tryFetchRun(jobID, runID)
		if !ok || len(run.Tasks) != 1 {
			return false
		}
		task = run.Tasks[0]
		return task.RuntimeID != "" && task.RuntimeID != previous
	}, timeout, time.Second, "run %s never recorded a new runtime id (previous %q)", runID, previous)
	return task
}

func (s *IntegrationTestSuite) tryFetchRun(jobID, runID string) (runResponse, bool) {
	var run runResponse
	resp, err := s.doJSONRequest(http.MethodGet, fmt.Sprintf("%s/v1/jobs/%s/runs/%s", s.caesiumURL, jobID, runID), nil)
	if err != nil {
		return run, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return run, false
	}
	return run, json.NewDecoder(resp.Body).Decode(&run) == nil
}

// awaitKubeMappedTaskSucceeded waits for the run's single task to succeed and
// returns it.
func (s *IntegrationTestSuite) awaitKubeMappedTaskSucceeded(jobID, runID string, timeout time.Duration) runTaskResponse {
	s.T().Helper()
	var task runTaskResponse
	s.Require().Eventually(func() bool {
		run, ok := s.tryFetchRun(jobID, runID)
		if !ok || len(run.Tasks) != 1 {
			return false
		}
		task = run.Tasks[0]
		return task.Status == "succeeded"
	}, timeout, time.Second, "the re-dispatched attempt of run %s never succeeded", runID)
	return task
}

// awaitKubeMappedRun waits for a terminal run across a restarting server.
func (s *IntegrationTestSuite) awaitKubeMappedRun(jobID, runID string, timeout time.Duration) runResponse {
	s.T().Helper()
	var run runResponse
	s.Require().Eventually(func() bool {
		got, ok := s.tryFetchRun(jobID, runID)
		run = got
		return ok && (got.Status == "succeeded" || got.Status == "failed")
	}, timeout, time.Second, "run %s never finished", runID)
	return run
}

// kubeMappedPod resolves a qualified runtime id to its pod, which must be in
// the mapped namespace.
func (s *IntegrationTestSuite) kubeMappedPod(ctx context.Context, client kubernetes.Interface, runtimeID string) *corev1.Pod {
	s.T().Helper()
	namespace, name, ok := strings.Cut(runtimeID, "/")
	s.Require().True(ok, "runtime id %q is not namespace-qualified", runtimeID)
	s.Require().Equal(kubeMappedNamespace, namespace, "runtime id %q", runtimeID)
	pod, err := client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	s.Require().NoError(err)
	return pod
}

func (s *IntegrationTestSuite) awaitPodGone(ctx context.Context, client kubernetes.Interface, namespace, name string, uid types.UID, timeout time.Duration, why string) {
	s.T().Helper()
	s.Require().Eventually(func() bool {
		pod, err := client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true
		}
		return err == nil && pod.UID != uid
	}, timeout, time.Second, "pod %s/%s (uid %s) was never stopped: %s", namespace, name, uid, why)
}

func (s *IntegrationTestSuite) runKubeMappedPlacement(ctx context.Context, client kubernetes.Interface, witness *holdWitness, holdURL, suffix string) {
	token := "placement-" + suffix
	// set -e: the step reaches the hold only after the secret read in the
	// mapped namespace matched and the PVC was writable.
	script := fmt.Sprintf("set -e; test \"$TOKEN\" = %q; echo placed > /scratch/marker; %s", kubeMappedSecretValue, holdScript(holdURL, token, "C3_PLACEMENT_DONE"))
	dir := s.writeJobManifest(kubeMappedManifest("c3-placement-"+suffix, script, true))
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias("c3-placement-" + suffix)
	runID := s.triggerRun(job.ID)

	task := s.kubeMappedTaskRuntime(job.ID, runID, "", 3*time.Minute)
	pod := s.kubeMappedPod(ctx, client, task.RuntimeID)
	s.Equal(kubeMappedAccount, pod.Spec.ServiceAccountName, "the pod runs as the mapped service account")
	s.Require().Eventually(func() bool { return witness.wasAsked(token) }, 2*time.Minute, time.Second,
		"the step never reached its hold: the secret read or the volume failed")

	claims, err := client.CoreV1().PersistentVolumeClaims(kubeMappedNamespace).List(ctx, metav1.ListOptions{})
	s.Require().NoError(err)
	owned := 0
	for _, claim := range claims.Items {
		for _, ref := range claim.OwnerReferences {
			if ref.Kind == "Pod" && ref.UID == pod.UID {
				owned++
			}
		}
	}
	s.Equal(1, owned, "the step's claim template binds a PVC owned by its pod in the mapped namespace")

	witness.release(token)
	run := s.awaitKubeMappedRun(job.ID, runID, 3*time.Minute)
	s.Require().Equal("succeeded", run.Status, "%+v", run.Tasks)
	s.Require().Len(run.Tasks, 1)
	s.Equal(task.RuntimeID, run.Tasks[0].RuntimeID, "the persisted runtime id is the mapped pod's")
}

// runKubeMappedRestart holds a step in the mapped namespace, restarts the
// server — gracefully (pod delete) or by SIGKILL of its container — and proves
// the original attempt's pod is stopped, the re-dispatched attempt runs in the
// mapped namespace and succeeds, and the run's task log is readable.
func (s *IntegrationTestSuite) runKubeMappedRestart(ctx context.Context, client kubernetes.Interface, witness *holdWitness, holdURL, suffix string, crash bool, kindNode string) {
	mode := "graceful"
	if crash {
		mode = "crash"
	}
	alias := "c3-" + mode + "-" + suffix
	token := mode + "-" + suffix
	marker := "C3_" + strings.ToUpper(mode) + "_DONE"
	dir := s.writeJobManifest(kubeMappedManifest(alias, holdScript(holdURL, token, marker), false))
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)

	first := s.kubeMappedTaskRuntime(job.ID, runID, "", 3*time.Minute)
	original := s.kubeMappedPod(ctx, client, first.RuntimeID)
	s.Require().Eventually(func() bool { return witness.wasAsked(token) }, 2*time.Minute, time.Second, "the step never started")

	before, err := client.CoreV1().Pods(kubeMappedReleaseNS).Get(ctx, kubeMappedServerPod, metav1.GetOptions{})
	s.Require().NoError(err)
	if crash {
		s.crashKubeMappedServer(ctx, before, kindNode)
	} else {
		s.Require().NoError(client.CoreV1().Pods(kubeMappedReleaseNS).Delete(ctx, kubeMappedServerPod, metav1.DeleteOptions{}))
	}
	s.awaitKubeMappedServerBack(ctx, client, before, crash)

	// Graceful: shutdown stops the attempt. Crash: nothing ran shutdown, so
	// recovery stops the same pod uid through the persisted runtime id.
	s.awaitPodGone(ctx, client, kubeMappedNamespace, original.Name, original.UID, 3*time.Minute, mode+" restart")

	next := s.kubeMappedTaskRuntime(job.ID, runID, first.RuntimeID, 3*time.Minute)
	replacement := s.kubeMappedPod(ctx, client, next.RuntimeID)
	s.NotEqual(original.UID, replacement.UID)
	s.Equal(kubeMappedAccount, replacement.Spec.ServiceAccountName)

	witness.release(token)
	task := s.awaitKubeMappedTaskSucceeded(job.ID, runID, 3*time.Minute)
	s.Equal(next.RuntimeID, task.RuntimeID, "the attempt that succeeded is the re-dispatched one")

	resp, err := s.doJSONRequest(http.MethodGet, fmt.Sprintf("%s/v1/jobs/%s/runs/%s/logs?task_id=%s", s.caesiumURL, job.ID, runID, task.ID), nil)
	s.Require().NoError(err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, resp.StatusCode, "%s", body)
	s.Contains(string(body), marker, "the run's task log is readable after the %s restart", mode)

	// Stop, re-dispatch, wait and logs are all proven above, before anything
	// fires the job again, so this trigger cannot stand in for recovery. It only
	// finalizes: with SQL advancement a run is finished by the job.Run driving
	// it, which died with the old process, and the job's next trigger adopts the
	// running run instead of starting another.
	hook, err := s.doJSONRequest(http.MethodPost, s.caesiumURL+"/v1/hooks/hooks/"+alias, nil)
	s.Require().NoError(err)
	hook.Body.Close()
	s.Require().Equal(http.StatusAccepted, hook.StatusCode)

	run := s.awaitKubeMappedRun(job.ID, runID, 3*time.Minute)
	s.Require().Equal("succeeded", run.Status, "%+v", run.Tasks)
	s.Require().Len(run.Tasks, 1)
	s.Equal(next.RuntimeID, run.Tasks[0].RuntimeID, "adopting the run re-executes nothing")
}

// crashKubeMappedServer SIGKILLs the caesium container through the kind
// node's containerd, so no shutdown path runs; the kubelet restarts it in
// the same pod.
func (s *IntegrationTestSuite) crashKubeMappedServer(ctx context.Context, pod *corev1.Pod, kindNode string) {
	containerID := ""
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == "caesium" {
			_, containerID, _ = strings.Cut(status.ContainerID, "://")
		}
	}
	s.Require().NotEmpty(containerID, "caesium container id")
	docker, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	s.Require().NoError(err)
	defer docker.Close()
	exec, err := docker.ContainerExecCreate(ctx, kindNode, dockercontainer.ExecOptions{
		Cmd:          []string{"ctr", "-n", "k8s.io", "tasks", "kill", "--signal", "SIGKILL", containerID},
		AttachStdout: true,
		AttachStderr: true,
	})
	s.Require().NoError(err)
	attached, err := docker.ContainerExecAttach(ctx, exec.ID, dockercontainer.ExecAttachOptions{})
	s.Require().NoError(err)
	var output bytes.Buffer
	_, _ = io.Copy(&output, attached.Reader)
	attached.Close()
	inspect, err := docker.ContainerExecInspect(ctx, exec.ID)
	s.Require().NoError(err)
	s.Require().Zero(inspect.ExitCode, "ctr kill: %s", output.String())
}

// awaitKubeMappedServerBack waits until the server pod is a new process (a
// recreated pod after a delete, a restarted container after a crash), Ready,
// and serving through the phase's self-healing port-forward.
func (s *IntegrationTestSuite) awaitKubeMappedServerBack(ctx context.Context, client kubernetes.Interface, before *corev1.Pod, crash bool) {
	restarts := func(pod *corev1.Pod) int32 {
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == "caesium" {
				return status.RestartCount
			}
		}
		return -1
	}
	s.Require().Eventually(func() bool {
		pod, err := client.CoreV1().Pods(kubeMappedReleaseNS).Get(ctx, kubeMappedServerPod, metav1.GetOptions{})
		if err != nil {
			return false
		}
		if crash {
			if pod.UID != before.UID || restarts(pod) <= restarts(before) {
				return false
			}
		} else if pod.UID == before.UID {
			return false
		}
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				return true
			}
		}
		return false
	}, 4*time.Minute, time.Second, "the caesium server never came back")
	s.Require().Eventually(func() bool {
		resp, err := s.doRequest(http.MethodGet, s.caesiumURL+"/health", nil)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 2*time.Minute, time.Second, "the caesium server never served after the restart")
}
