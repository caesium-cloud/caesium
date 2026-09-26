package kubernetes

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const kubernetesStopAPITimeout = 30 * time.Second

const (
	// Leave time for the REST log handler to respond before its 30s WriteTimeout.
	kubernetesLogReadinessTimeout = 10 * time.Second
	kubernetesLogReadinessPoll    = 100 * time.Millisecond
)

// kueueQueueLabel is the label Kueue reads to assign a workload to a LocalQueue.
// Stamping it on the pod delegates admission to Kueue: its webhook gates the pod
// (injecting the kueue.x-k8s.io/admission scheduling gate) until the named
// queue's quota is available, then un-gates it. Caesium never bin-packs or
// schedules itself — it hands scheduling to Kueue. See
// https://kueue.sigs.k8s.io/docs/tasks/run/plain_pods/.
const kueueQueueLabel = "kueue.x-k8s.io/queue-name"

// Engine defines the interface for treating the
// Kubernetes API as a atom.Engine.
type Engine interface {
	atom.Engine
}

type kubernetesEngine struct {
	ctx     context.Context
	backend kubernetesBackend
}

// getKubernetesCore resolves the CoreV1 client from the local kubeconfig,
// falling back to in-cluster config. It returns an error instead of panicking
// so every caller of NewEngine gets a clean, actionable failure (an
// unreachable/missing kubeconfig is an expected runtime condition — e.g.
// `caesium dev` running outside a cluster with no local kubeconfig — not a
// programmer error).
//
// The path resolved here is env.Variables().KubernetesConfig
// (CAESIUM_KUBERNETES_CONFIG) joined with kubeConfig, or $HOME/.kube/config
// when that env var is unset — NOT the standard KUBECONFIG env var, which
// clientcmd.BuildConfigFromFlags never consults once given an explicit path.
var getKubernetesCore = func(k8sCfg string) (corev1.CoreV1Interface, error) {
	configPath := k8sCfg
	if configPath == "" {
		u, _ := user.Current()
		configPath = filepath.Join(u.HomeDir, kubeConfig)
	} else {
		configPath = filepath.Join(configPath, kubeConfig)
	}

	config, err := clientcmd.BuildConfigFromFlags("", configPath)
	if err != nil {
		// Fall back to in-cluster config when running inside a Kubernetes pod.
		config, err = rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf(
				"load kubeconfig %s and in-cluster config both failed (last error: %w)",
				configPath, err)
		}
	}

	cli, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("build kubernetes client: %w", err)
	}

	return cli.CoreV1(), nil
}

// NewEngine creates a new instance of kubernetes.Engine for interacting with
// kubernetes.Atoms. It returns an error (rather than panicking) when the
// kubeconfig cannot be loaded and no in-cluster config is available, so every
// caller can surface a clean, actionable message instead of an unrecovered
// panic.
func NewEngine(ctx context.Context, core ...corev1.CoreV1Interface) (Engine, error) {
	var backend corev1.CoreV1Interface

	if len(core) > 0 {
		backend = core[0]
	} else {
		var err error
		backend, err = getKubernetesCore(env.Variables().KubernetesConfig)
		if err != nil {
			return nil, fmt.Errorf("kubernetes engine unavailable: %w; set CAESIUM_KUBERNETES_CONFIG to a directory containing .kube/config (defaults to $HOME/.kube/config), or run inside a cluster with a valid service account", err)
		}
	}

	return &kubernetesEngine{
		ctx:     ctx,
		backend: backend.Pods(env.Variables().KubernetesNamespace),
	}, nil
}

// Get a Caesium Kubernetes pod and its corresponding metadata.
func (e *kubernetesEngine) Get(req *atom.EngineGetRequest) (atom.Atom, error) {
	pod, err := e.backend.Get(e.ctx, req.ID, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	return &Atom{metadata: pod}, nil
}

// List all of Caesium's Kubernetes pods.
func (e *kubernetesEngine) List(req *atom.EngineListRequest) ([]atom.Atom, error) {
	pods, err := e.backend.List(
		e.ctx,
		metav1.ListOptions{LabelSelector: atom.Label},
	)
	if err != nil {
		return nil, err
	}

	atoms := make([]atom.Atom, len(pods.Items))

	for i, pod := range pods.Items {
		atoms[i] = &Atom{metadata: &pod}
	}

	return atoms, nil
}

// Create a Caesium Kubernetes pod. Currently every pod that
// Caesium creates has exactly one pod.
func (e *kubernetesEngine) Create(req *atom.EngineCreateRequest) (atom.Atom, error) {
	volumeMounts, volumes, err := convertKubernetesMounts(req.Name, req.Spec.Mounts, req.Spec.ResolvedVolumeMounts)
	if err != nil {
		return nil, err
	}
	envVars := convertEnvVars(req.Spec.Env)

	spec := &v1.Pod{
		Name:      fmt.Sprintf("%s-%s", req.Name, uuid.New()),
		Namespace: env.Variables().KubernetesNamespace,
		Labels:    map[string]string{atom.Label: ""},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					// Each Caesium pod runs exactly one container, so a fixed
					// name is sufficient. The pod name already carries the
					// full task/run identity for kubectl and log correlation.
					Name:            "atom",
					Image:           req.Image,
					Command:         req.Command,
					Env:             envVars,
					WorkingDir:      req.Spec.WorkDir,
					VolumeMounts:    volumeMounts,
					ImagePullPolicy: v1.PullIfNotPresent,
				},
			},
			Volumes:       volumes,
			RestartPolicy: v1.RestartPolicyNever,
		},
	}
	if req.Spec.Kubernetes != nil {
		if len(req.Spec.Kubernetes.PodAnnotations) > 0 {
			spec.Annotations = req.Spec.Kubernetes.PodAnnotations
		}
		if req.Spec.Kubernetes.ServiceAccountName != "" {
			spec.Spec.ServiceAccountName = req.Spec.Kubernetes.ServiceAccountName
		}
		if req.Spec.Kubernetes.AutomountServiceAccountToken != nil {
			spec.Spec.AutomountServiceAccountToken = req.Spec.Kubernetes.AutomountServiceAccountToken
		}
		// Delegate admission to Kueue. The label is all Caesium sets — Kueue's
		// webhook gates the pod (injecting the kueue.x-k8s.io/admission
		// scheduling gate, the pod-level equivalent of a suspended Job) until the
		// LocalQueue has quota, then un-gates it. Caesium does not schedule.
		if q := req.Spec.Kubernetes.QueueName; q != "" {
			spec.Labels[kueueQueueLabel] = q
		}
	}

	// Issue the allocation call itself against a bounded, DETACHED context
	// rather than e.ctx: e.ctx is cancellable (e.g. SIGINT from `caesium
	// dev`), and if it is cancelled WHILE this request is in flight, the
	// API server may already have persisted the pod by the time the client
	// sees a context-cancelled error — leaving nothing to clean up, since
	// we would never learn the pod exists. spec.Name is generated
	// client-side above, before this call, so it stays findable regardless
	// of which context the call itself used. The bounded request may still
	// end with an ambiguous allocation; cleanup uses that unique name both
	// while the response is pending and after it returns.
	createCtx, cancelCreate := context.WithTimeout(context.Background(), createRequestTimeout)
	defer cancelCreate()
	if err := e.ctx.Err(); err != nil {
		return nil, err
	}
	// The API may have persisted and scheduled this pod while its Create
	// response is still in flight. Start deletion at the caller's deadline,
	// using the name generated above, rather than waiting for that response.
	// A delete may race ahead of creation and return NotFound; retry while
	// Create remains pending, then reconcile once more after its response.
	createReturned := make(chan struct{})
	earlyCleanupDone := make(chan struct{})
	stopEarlyCleanup := context.AfterFunc(e.ctx, func() {
		defer close(earlyCleanupDone)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-createReturned:
				return
			case <-createCtx.Done():
				return
			default:
			}
			// Create may return while a delete is in flight. Keep this
			// individual API call short so joining the watcher cannot add
			// the full ordinary 30-second Stop budget to completion.
			err := e.Stop(&atom.EngineStopRequest{ID: spec.Name, Force: true, Timeout: time.Second})
			if err == nil {
				return
			}
			if !apierrors.IsNotFound(err) {
				log.Warn("failed to delete pod during pending Create; retrying", "name", spec.Name, "error", err)
			}
			select {
			case <-createReturned:
				return
			case <-createCtx.Done():
				return
			case <-ticker.C:
			}
		}
	})

	pod, err := e.backend.Create(createCtx, spec, metav1.CreateOptions{})
	close(createReturned)
	if !stopEarlyCleanup() {
		<-earlyCleanupDone
	}
	if err != nil {
		if e.ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			// A caller cancellation or the detached request's own deadline
			// leaves allocation ambiguous: the API server may have persisted
			// this uniquely named Caesium pod before its response reached us.
			e.cleanupFailedCreate(spec.Name, err)
			if e.ctx.Err() != nil {
				return nil, e.ctx.Err()
			}
		}
		return nil, err
	}
	if e.ctx.Err() != nil {
		// Create succeeded — the pod exists — but the caller is no longer
		// waiting for it. Remove it and report the cancellation, not a
		// spurious success. See #480.
		e.cleanupFailedCreate(spec.Name, e.ctx.Err())
		return nil, e.ctx.Err()
	}

	return &Atom{metadata: pod}, nil
}

// createRequestTimeout bounds the Create API call above, independent of the
// caller's (possibly SIGINT-cancelled) context.
const createRequestTimeout = 30 * time.Second

// cleanupFailedCreate best-effort deletes a pod that was successfully
// created but whose Create call is failing for an unrelated reason (most
// commonly: the caller's context was cancelled around the create request).
// Stop already deletes against a detached context.Background() (see its own
// comment), so this is safe to call regardless of why Create is failing.
func (e *kubernetesEngine) cleanupFailedCreate(name string, cause error) {
	if err := e.Stop(&atom.EngineStopRequest{ID: name, Force: true}); err != nil && !apierrors.IsNotFound(err) {
		log.Warn("failed to clean up pod after Create failed", "name", name, "cause", cause, "error", err)
	}
}

func (e *kubernetesEngine) Wait(req *atom.EngineWaitRequest) (atom.Atom, error) {
	waitCtx := e.ctx
	if req != nil && req.Context != nil {
		waitCtx = req.Context
	}
	pod, err := e.backend.Get(e.ctx, req.ID, metav1.GetOptions{})
	if err == nil && (pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed) {
		return &Atom{metadata: pod}, nil
	}

	watcher, err := e.backend.Watch(waitCtx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("metadata.name", req.ID).String(),
	})
	if err != nil {
		return nil, err
	}
	defer watcher.Stop()

	for {
		select {
		case <-waitCtx.Done():
			return nil, waitCtx.Err()
		case evt, ok := <-watcher.ResultChan():
			if !ok {
				return e.Get(&atom.EngineGetRequest{ID: req.ID})
			}
			pod, ok := evt.Object.(*v1.Pod)
			if !ok || pod == nil {
				continue
			}
			if pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
				return &Atom{metadata: pod}, nil
			}
		}
	}
}

// Stop the Caesium Kubernetes pod. Stop makes a Kubernetes
// DeletePod call under the covers.
//
// We use context.Background() as the base so that pod cleanup
// succeeds even when the parent context has been cancelled
// (e.g. by a run-level timeout).
func (e *kubernetesEngine) Stop(req *atom.EngineStopRequest) error {
	var (
		bg   = metav1.DeletePropagationBackground
		fg   = metav1.DeletePropagationForeground
		opts = metav1.DeleteOptions{PropagationPolicy: &fg}
	)

	if req.Force {
		opts.PropagationPolicy = &bg
		// Use a short graceful deletion for deadline stops and ordinary teardown.
		// Kubelet kills a TERM-ignoring container after this grace period before
		// removing the pod. Zero would instead remove the API object without
		// waiting for kubelet, losing the record of a possibly still-running pod.
		grace := int64(1)
		opts.GracePeriodSeconds = &grace
	}

	apiTimeout := req.Timeout
	if apiTimeout <= 0 {
		apiTimeout = kubernetesStopAPITimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
	defer cancel()

	return e.backend.Delete(ctx, req.ID, opts)
}

// Logs streams the log output from a Caesium Kubernetes pod's
// only container based on the request input.
func (e *kubernetesEngine) Logs(req *atom.EngineLogsRequest) (io.ReadCloser, error) {
	return e.logsWhenReady(req, kubernetesLogReadinessTimeout)
}

func (e *kubernetesEngine) logsWhenReady(req *atom.EngineLogsRequest, timeout time.Duration) (io.ReadCloser, error) {
	opts := &v1.PodLogOptions{
		Follow:     true,
		Timestamps: true,
	}

	if !req.Since.IsZero() {
		opts.SinceTime = &metav1.Time{Time: req.Since}
	}

	// Bound opening the stream, including an API request that never returns its
	// headers. Stop this timer on success: a setup deadline must not terminate a
	// healthy live reader. The reader retains parent cancellation until Close.
	ctx, cancel := context.WithCancelCause(e.ctx)
	timer := time.AfterFunc(timeout, func() { cancel(context.DeadlineExceeded) })
	defer timer.Stop()
	failed := true
	defer func() {
		if failed {
			cancel(nil)
		}
	}()

	for {
		if ctx.Err() != nil {
			return nil, context.Cause(ctx)
		}
		logs := e.backend.GetLogs(req.ID, opts)
		if logs == nil {
			return nil, fmt.Errorf("failed to retrieve logs")
		}
		reader, err := logs.Stream(ctx)
		if err == nil && reader != nil {
			// The apiserver returns 204 for an unbound (including Kueue-gated)
			// pod. client-go exposes that as a successful, empty reader. Peek
			// under the opening budget so it cannot finish marker capture early.
			buffered := bufio.NewReader(reader)
			_, peekErr := buffered.Peek(1)
			if peekErr == nil || errors.Is(peekErr, io.EOF) {
				final := false
				if errors.Is(peekErr, io.EOF) {
					pod, getErr := e.backend.Get(ctx, req.ID, metav1.GetOptions{})
					if getErr != nil {
						_ = reader.Close()
						if ctx.Err() != nil {
							return nil, context.Cause(ctx)
						}
						return nil, getErr
					}
					if ctx.Err() != nil {
						_ = reader.Close()
						return nil, context.Cause(ctx)
					}
					final = pod != nil && (pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed)
					if !final && !podCanStartLogs(pod) {
						_ = reader.Close()
						return nil, fmt.Errorf("empty log stream for unavailable pod %q", req.ID)
					}
				}
				if peekErr == nil || final {
					if !timer.Stop() || ctx.Err() != nil {
						_ = reader.Close()
						if ctx.Err() != nil {
							return nil, context.Cause(ctx)
						}
						return nil, context.DeadlineExceeded
					}
					failed = false
					return &kubernetesLogReader{Reader: buffered, closer: reader, cancel: cancel}, nil
				}
			} else {
				_ = reader.Close()
				if ctx.Err() != nil {
					return nil, context.Cause(ctx)
				}
				return nil, peekErr
			}
			_ = reader.Close()
		} else {
			if reader != nil {
				_ = reader.Close()
			}
			if ctx.Err() != nil {
				return nil, context.Cause(ctx)
			}
			if err == nil {
				return nil, fmt.Errorf("failed to retrieve log stream")
			}
			if !containerLogsPending(err, req.ID) {
				return nil, err
			}
			// A kubelet may not have synced a newly bound pod yet. Confirm the
			// apiserver object still exists and has no permanent startup failure.
			pod, getErr := e.backend.Get(ctx, req.ID, metav1.GetOptions{})
			if getErr != nil {
				if ctx.Err() != nil {
					return nil, context.Cause(ctx)
				}
				return nil, getErr
			}
			if ctx.Err() != nil {
				return nil, context.Cause(ctx)
			}
			if !podCanStartLogs(pod) {
				return nil, err
			}
		}

		// A newly created pod can reject /log before its only container starts.
		// Retry only observed readiness; authorization, missing pods, image
		// failures and other API errors retain their original failure.
		select {
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		case <-time.After(kubernetesLogReadinessPoll):
		}
	}
}

func containerLogsPending(err error, pod string) bool {
	var status *apierrors.StatusError
	if !errors.As(err, &status) {
		return false
	}
	if apierrors.IsNotFound(err) {
		// An apiserver object lookup names Details.Kind=pods. The kubelet's
		// unsynced-pod response has this narrower message and is confirmed by Get.
		return (status.ErrStatus.Details == nil || status.ErrStatus.Details.Kind != "pods") &&
			status.ErrStatus.Message == fmt.Sprintf("pod %q does not exist", pod)
	}
	if !apierrors.IsBadRequest(err) {
		return false
	}
	if status.ErrStatus.Message == fmt.Sprintf("container %q in pod %q is not available", "atom", pod) {
		return true
	}
	prefix := fmt.Sprintf("container %q in pod %q is waiting to start: ", "atom", pod)
	if !strings.HasPrefix(status.ErrStatus.Message, prefix) {
		return false
	}
	reason := strings.TrimPrefix(status.ErrStatus.Message, prefix)
	return reason == "ContainerCreating" || reason == "PodInitializing"
}

func podCanStartLogs(pod *v1.Pod) bool {
	if pod == nil || pod.DeletionTimestamp != nil ||
		(pod.Status.Phase != v1.PodPending && pod.Status.Phase != v1.PodRunning) {
		return false
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == "atom" && status.State.Waiting != nil {
			reason := status.State.Waiting.Reason
			return reason == "" || reason == "ContainerCreating" || reason == "PodInitializing"
		}
	}
	return true
}

type kubernetesLogReader struct {
	io.Reader
	closer io.Closer
	cancel context.CancelCauseFunc
}

func (r *kubernetesLogReader) Close() error {
	r.cancel(nil)
	return r.closer.Close()
}

func convertEnvVars(env map[string]string) []v1.EnvVar {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	vars := make([]v1.EnvVar, 0, len(keys))
	for _, k := range keys {
		vars = append(vars, v1.EnvVar{Name: k, Value: env[k]})
	}
	return vars
}

func convertKubernetesMounts(baseName string, mounts []container.Mount, resolvedMounts []container.VolumeMount) ([]v1.VolumeMount, []v1.Volume, error) {
	if len(mounts) == 0 && len(resolvedMounts) == 0 {
		return nil, nil, nil
	}
	volumeMounts := make([]v1.VolumeMount, 0, len(mounts)+len(resolvedMounts))
	volumes := make([]v1.Volume, 0, len(mounts)+len(resolvedMounts))
	for idx, m := range mounts {
		if m.Source == "" || m.Target == "" {
			continue
		}
		name := sanitizeVolumeName(fmt.Sprintf("%s-mnt-%d", baseName, idx))
		volumeMounts = append(volumeMounts, v1.VolumeMount{
			Name:      name,
			MountPath: m.Target,
			ReadOnly:  m.ReadOnly,
		})
		volumes = append(volumes, v1.Volume{
			Name:     name,
			HostPath: &v1.HostPathVolumeSource{Path: m.Source},
		})
	}
	for idx, m := range resolvedMounts {
		if m.Target == "" {
			continue
		}
		name := sanitizeVolumeName(fmt.Sprintf("%s-vol-%s-%d", baseName, m.Name, idx))
		volumeMounts = append(volumeMounts, v1.VolumeMount{
			Name:      name,
			MountPath: m.Target,
			ReadOnly:  m.ReadOnly,
			SubPath:   m.SubPath,
		})
		volume, err := convertResolvedKubernetesVolume(name, m)
		if err != nil {
			return nil, nil, err
		}
		volumes = append(volumes, volume)
	}
	return volumeMounts, volumes, nil
}

func convertResolvedKubernetesVolume(name string, mount container.VolumeMount) (v1.Volume, error) {
	volume := v1.Volume{Name: name}
	switch mount.Type {
	case container.VolumeMountTypePVC:
		if mount.Source == "" {
			return v1.Volume{}, fmt.Errorf("kubernetes pvc volume %q missing source", mount.Name)
		}
		volume.PersistentVolumeClaim = &v1.PersistentVolumeClaimVolumeSource{
			ClaimName: mount.Source,
			ReadOnly:  mount.ReadOnly,
		}
	case container.VolumeMountTypeClaimTemplate:
		template, err := buildPersistentVolumeClaimTemplate(mount.ClaimTemplate)
		if err != nil {
			return v1.Volume{}, fmt.Errorf("kubernetes claimTemplate volume %q: %w", mount.Name, err)
		}
		volume.Ephemeral = &v1.EphemeralVolumeSource{VolumeClaimTemplate: template}
	case container.VolumeMountTypeVolumeSource:
		if len(mount.VolumeSource) == 0 {
			return v1.Volume{}, fmt.Errorf("kubernetes volumeSource volume %q is empty", mount.Name)
		}
		data, err := json.Marshal(mount.VolumeSource)
		if err != nil {
			return v1.Volume{}, fmt.Errorf("marshal kubernetes volumeSource %q: %w", mount.Name, err)
		}
		if err := json.Unmarshal(data, &volume.VolumeSource); err != nil {
			return v1.Volume{}, fmt.Errorf("decode kubernetes volumeSource %q: %w", mount.Name, err)
		}
	default:
		return v1.Volume{}, fmt.Errorf("volume %q type %q is not supported by kubernetes", mount.Name, mount.Type)
	}
	return volume, nil
}

func buildPersistentVolumeClaimTemplate(template *container.KubernetesClaimTemplate) (*v1.PersistentVolumeClaimTemplate, error) {
	if template == nil {
		return nil, fmt.Errorf("missing claim template")
	}
	quantity, err := resource.ParseQuantity(template.Size)
	if err != nil {
		return nil, fmt.Errorf("invalid size %q: %w", template.Size, err)
	}
	accessMode := template.AccessMode
	if accessMode == "" {
		accessMode = string(v1.ReadWriteOnce)
	}
	claim := &v1.PersistentVolumeClaimTemplate{
		Labels:      template.Labels,
		Annotations: template.Annotations,
		Spec: v1.PersistentVolumeClaimSpec{
			AccessModes: []v1.PersistentVolumeAccessMode{v1.PersistentVolumeAccessMode(accessMode)},
			Resources: v1.VolumeResourceRequirements{
				Requests: v1.ResourceList{v1.ResourceStorage: quantity},
			},
		},
	}
	if template.StorageClass != "" {
		storageClass := template.StorageClass
		claim.Spec.StorageClassName = &storageClass
	}
	return claim, nil
}

func sanitizeVolumeName(value string) string {
	value = strings.ToLower(value)
	value = strings.ReplaceAll(value, "_", "-")
	if len(value) > 63 {
		return value[:63]
	}
	return value
}
