package kubernetes

import (
	"github.com/caesium-cloud/caesium/internal/atom"
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
)

const kubeConfig = ".kube/config"
