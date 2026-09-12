//go:build integration

package cluster

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	CaesiumAppLabel      = "app.kubernetes.io/name"
	CaesiumAppValue      = "caesium"
	CaesiumInstanceLabel = "app.kubernetes.io/instance"
	CaesiumInstanceValue = "caesium"
	CaesiumContainer     = "caesium"
	HTTPPort             = 8080
	DqlitePort           = 9001
	NodeAddressPort      = "9001"
)

type Env struct {
	Namespace       string
	TaskImage       string
	ServerImage     string
	CandidateSHA    string
	RobustnessID    string
	ManualKey       string
	PodName         string
	PodIP           string
	CandidateDigest string
}

func LoadEnv() (Env, error) {
	ns := firstNonEmpty(
		os.Getenv("POD_NAMESPACE"),
		os.Getenv("CAESIUM_ROBUSTNESS_ID"),
		os.Getenv("ROBUSTNESS_ID"),
	)
	if ns == "" {
		return Env{}, fmt.Errorf("POD_NAMESPACE or CAESIUM_ROBUSTNESS_ID is required")
	}
	task := strings.TrimSpace(os.Getenv("CAESIUM_ROBUSTNESS_TASK_IMAGE"))
	if task == "" {
		return Env{}, fmt.Errorf("CAESIUM_ROBUSTNESS_TASK_IMAGE is required")
	}
	server := strings.TrimSpace(os.Getenv("CAESIUM_ROBUSTNESS_SERVER_IMAGE"))
	if server == "" {
		return Env{}, fmt.Errorf("CAESIUM_ROBUSTNESS_SERVER_IMAGE is required")
	}
	sha := firstNonEmpty(os.Getenv("CANDIDATE_SHA"), os.Getenv("CAESIUM_ROBUSTNESS_CANDIDATE_SHA"))
	if sha == "" {
		return Env{}, fmt.Errorf("CANDIDATE_SHA is required")
	}
	return Env{
		Namespace:       ns,
		TaskImage:       task,
		ServerImage:     server,
		CandidateSHA:    sha,
		RobustnessID:    firstNonEmpty(os.Getenv("CAESIUM_ROBUSTNESS_ID"), os.Getenv("ROBUSTNESS_ID"), ns),
		ManualKey:       firstNonEmpty(os.Getenv("CAESIUM_MANUAL_TRIGGER_API_KEY"), "caesium-robustness-manual-key"),
		PodName:         os.Getenv("POD_NAME"),
		PodIP:           os.Getenv("POD_IP"),
		CandidateDigest: firstNonEmpty(os.Getenv("CAESIUM_ROBUSTNESS_CANDIDATE_DIGEST"), os.Getenv("CANDIDATE_DIGEST")),
	}, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func InClusterClient() (*kubernetes.Clientset, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := strings.TrimSpace(os.Getenv("KUBECONFIG"))
		if kubeconfig == "" {
			return nil, fmt.Errorf("in-cluster config: %w (runner must run inside the owned namespace)", err)
		}
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("kubeconfig %s: %w", kubeconfig, err)
		}
	}
	return kubernetes.NewForConfig(cfg)
}

func CaesiumSelector() string {
	return fmt.Sprintf("%s=%s,%s=%s", CaesiumAppLabel, CaesiumAppValue, CaesiumInstanceLabel, CaesiumInstanceValue)
}

func Poll(ctx context.Context, interval time.Duration, fn func() (bool, error)) error {
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	if ok, err := fn(); err != nil {
		return err
	} else if ok {
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			ok, err := fn()
			if err != nil {
				return err
			}
			if ok {
				return nil
			}
		}
	}
}

func IsControlPlaneNode(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "control-plane") || strings.Contains(n, "controlplane") || strings.Contains(n, "master")
}

func IsWorkerNode(name string) bool {
	return name != "" && !IsControlPlaneNode(name)
}

func PodReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func CaesiumContainerStatus(pod *corev1.Pod) (*corev1.ContainerStatus, error) {
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		if cs.Name == CaesiumContainer {
			return cs, nil
		}
	}
	return nil, fmt.Errorf("pod %s has no container %s", pod.Name, CaesiumContainer)
}

func StripContainerdPrefix(id string) string {
	id = strings.TrimSpace(id)
	for _, prefix := range []string{"containerd://", "docker://", "cri-o://"} {
		id = strings.TrimPrefix(id, prefix)
	}
	return id
}

func ListOptions() metav1.ListOptions {
	return metav1.ListOptions{LabelSelector: CaesiumSelector()}
}
