package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/user"
	"path/filepath"
	"slices"
	"strings"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Engine defines the interface for treating the
// Kubernetes API as a atom.Engine.
type Engine interface {
	atom.Engine
}

type kubernetesEngine struct {
	ctx     context.Context
	backend kubernetesBackend
}

var getKubernetesCore = func(k8sCfg string) corev1.CoreV1Interface {
	if k8sCfg == "" {
		u, _ := user.Current()
		k8sCfg = filepath.Join(u.HomeDir, kubeConfig)
	} else {
		k8sCfg = filepath.Join(k8sCfg, kubeConfig)
	}

	config, err := clientcmd.BuildConfigFromFlags("", k8sCfg)
	if err != nil {
		// Fall back to in-cluster config when running inside a Kubernetes pod.
		config, err = rest.InClusterConfig()
		if err != nil {
			panic(err)
		}
	}

	cli, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err)
	}

	return cli.CoreV1()
}

// NewEngine creates a new instance of kubernetes.Engine
// for interacting with kubernetes.Atoms.
func NewEngine(ctx context.Context, core ...corev1.CoreV1Interface) Engine {
	var backend corev1.CoreV1Interface

	if len(core) > 0 {
		backend = core[0]
	} else {
		backend = getKubernetesCore(env.Variables().KubernetesConfig)
	}

	return &kubernetesEngine{
		ctx:     ctx,
		backend: backend.Pods(env.Variables().KubernetesNamespace),
	}
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
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", req.Name, uuid.New()),
			Namespace: env.Variables().KubernetesNamespace,
			Labels:    map[string]string{atom.Label: ""},
		},
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
	}

	pod, err := e.backend.Create(e.ctx, spec, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}

	return &Atom{metadata: pod}, nil
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
		cancel context.CancelFunc
		ctx    = context.Background()
		bg     = metav1.DeletePropagationBackground
		fg     = metav1.DeletePropagationForeground
		opts   = metav1.DeleteOptions{PropagationPolicy: &fg}
	)

	if req.Force {
		opts.PropagationPolicy = &bg
	}

	if req.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	return e.backend.Delete(ctx, req.ID, opts)
}

// Logs streams the log output from a Caesium Kubernetes pod's
// only container based on the request input.
func (e *kubernetesEngine) Logs(req *atom.EngineLogsRequest) (io.ReadCloser, error) {
	opts := &v1.PodLogOptions{
		Follow:     true,
		Timestamps: true,
	}

	if !req.Since.IsZero() {
		opts.SinceTime = &metav1.Time{Time: req.Since}
	}

	logs := e.backend.GetLogs(req.ID, opts)
	if logs == nil {
		return nil, fmt.Errorf("failed to retrieve logs")
	}

	return logs.Stream(e.ctx)
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
			Name: name,
			VolumeSource: v1.VolumeSource{
				HostPath: &v1.HostPathVolumeSource{Path: m.Source},
			},
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
		volume.VolumeSource.PersistentVolumeClaim = &v1.PersistentVolumeClaimVolumeSource{
			ClaimName: mount.Source,
			ReadOnly:  mount.ReadOnly,
		}
	case container.VolumeMountTypeClaimTemplate:
		template, err := buildPersistentVolumeClaimTemplate(mount.ClaimTemplate)
		if err != nil {
			return v1.Volume{}, fmt.Errorf("kubernetes claimTemplate volume %q: %w", mount.Name, err)
		}
		volume.VolumeSource.Ephemeral = &v1.EphemeralVolumeSource{VolumeClaimTemplate: template}
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
		ObjectMeta: metav1.ObjectMeta{
			Labels:      template.Labels,
			Annotations: template.Annotations,
		},
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
