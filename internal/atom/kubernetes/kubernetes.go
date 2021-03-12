package kubernetes

import (
	"github.com/caesium-dev/caesium/internal/atom"
	v1 "k8s.io/api/core/v1"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

type kubernetesBackend interface {
	corev1.PodInterface
}

var (
	stateMap = map[v1.PodPhase]atom.State{
		v1.PodPending:   atom.Created,
		v1.PodRunning:   atom.Running,
		v1.PodSucceeded: atom.Stopped,
		v1.PodFailed:    atom.Stopped,
		v1.PodUnknown:   atom.Invalid,
	}
	resultMap = map[int32]atom.Result{
		0:   atom.Success,
		1:   atom.Failure,
		125: atom.StartupFailure,
		126: atom.StartupFailure,
		127: atom.StartupFailure,
		137: atom.Killed,
		143: atom.Terminated,
	}
)

const kubeConfig = ".kube/config"
