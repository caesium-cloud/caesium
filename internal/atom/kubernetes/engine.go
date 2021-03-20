package kubernetes

import (
	"context"
	"fmt"
	"io"
	"os/user"
	"path/filepath"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
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
		panic(err)
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
	spec := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", req.Name, uuid.New()),
			Namespace: env.Variables().KubernetesNamespace,
			Labels:    map[string]string{atom.Label: ""},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:            req.Name,
					Image:           req.Image,
					Command:         req.Command,
					ImagePullPolicy: v1.PullAlways,
				},
			},
			RestartPolicy: v1.RestartPolicyNever,
		},
	}

	pod, err := e.backend.Create(e.ctx, spec, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}

	return &Atom{metadata: pod}, nil
}

// Stop the Caesium Kubernetes pod. Stop makes a Kubernetes
// DeletePod call under the covers.
func (e *kubernetesEngine) Stop(req *atom.EngineStopRequest) error {
	var (
		cancel context.CancelFunc
		ctx    = e.ctx
		bg     = metav1.DeletePropagationBackground
		fg     = metav1.DeletePropagationForeground
		opts   = metav1.DeleteOptions{PropagationPolicy: &fg}
	)

	if req.Force {
		opts.PropagationPolicy = &bg
	}

	if req.Timeout > 0 {
		ctx, cancel = context.WithTimeout(e.ctx, req.Timeout)
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
