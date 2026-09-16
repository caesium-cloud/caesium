//go:build integration

package cluster

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"

	"github.com/caesium-cloud/caesium/test/robustness/recorder"
)

func EnsureRecorderService(ctx context.Context, kube *kubernetes.Clientset, ns string) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      recorder.ServiceName,
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/name": recorder.ServiceName,
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app.kubernetes.io/name": "robustness-runner",
			},
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       recorder.ServicePort,
				TargetPort: intstr.FromInt(recorder.ServicePort),
			}},
		},
	}
	_, err := kube.CoreV1().Services(ns).Create(ctx, svc, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func CreateProbePod(ctx context.Context, kube *kubernetes.Clientset, ns, name, probeID, taskImage string) error {
	script := fmt.Sprintf(`set -eu
RECORDER=%q
PROBE_ID=%q
wget -qO- --header='Content-Type: application/json' --post-data="{\"id\":\"${PROBE_ID}\"}" "${RECORDER}/probe"
wget -qO- "${RECORDER}/probe/${PROBE_ID}"
`, RecorderURL(), probeID)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/name": "robustness-probe",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:            "probe",
				Image:           taskImage,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"sh", "-c", script},
			}},
		},
	}
	_, err := kube.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	return err
}

func WaitProbe(ctx context.Context, sink *recorder.Sink, probeID string) error {
	return Poll(ctx, 500*time.Millisecond, func() (bool, error) {
		return sink.ProbeSeen(probeID), nil
	})
}

func WaitPodSucceeded(ctx context.Context, kube *kubernetes.Clientset, ns, name string) error {
	return Poll(ctx, time.Second, func() (bool, error) {
		pod, err := kube.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		switch pod.Status.Phase {
		case corev1.PodSucceeded:
			return true, nil
		case corev1.PodFailed:
			return false, fmt.Errorf("pod %s failed: %s", name, pod.Status.Message)
		default:
			return false, nil
		}
	})
}
