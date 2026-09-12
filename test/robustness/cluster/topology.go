//go:build integration

package cluster

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type Member struct {
	Pod         corev1.Pod
	Name        string
	UID         string
	IP          string
	Node        string
	ContainerID string
	Image       string
	ImageID     string
	NodeAddress string
	PVCName     string
	VolumeName  string
}

func (m Member) HTTPBase() string {
	return fmt.Sprintf("http://%s:%d", m.IP, HTTPPort)
}

func (m Member) DqliteAddr() string {
	return fmt.Sprintf("%s:%d", m.IP, DqlitePort)
}

type Topology struct {
	Members []Member
}

func (t Topology) ByName(name string) (Member, bool) {
	for _, m := range t.Members {
		if m.Name == name {
			return m, true
		}
	}
	return Member{}, false
}

func (t Topology) ByIP(ip string) (Member, bool) {
	for _, m := range t.Members {
		if m.IP == ip {
			return m, true
		}
	}
	return Member{}, false
}

func (t Topology) ByNodeAddress(addr string) (Member, bool) {
	for _, m := range t.Members {
		if m.NodeAddress == addr {
			return m, true
		}
	}
	return Member{}, false
}

func (t Topology) Survivors(owner Member) []Member {
	var out []Member
	for _, m := range t.Members {
		if m.Name != owner.Name {
			out = append(out, m)
		}
	}
	return out
}

func DiscoverTopology(ctx context.Context, kube *kubernetes.Clientset, ns string) (Topology, error) {
	pods, err := kube.CoreV1().Pods(ns).List(ctx, ListOptions())
	if err != nil {
		return Topology{}, err
	}
	pvcs, err := kube.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return Topology{}, err
	}
	pvcByPod := map[string]corev1.PersistentVolumeClaim{}
	for _, pvc := range pvcs.Items {
		pvcByPod[pvc.Name] = pvc
	}

	var members []Member
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp != nil {
			continue
		}
		cs, err := CaesiumContainerStatus(&pod)
		if err != nil {
			return Topology{}, err
		}
		m := Member{
			Pod:         pod,
			Name:        pod.Name,
			UID:         string(pod.UID),
			IP:          pod.Status.PodIP,
			Node:        pod.Spec.NodeName,
			ContainerID: StripContainerdPrefix(cs.ContainerID),
			Image:       cs.Image,
			ImageID:     cs.ImageID,
			NodeAddress: fmt.Sprintf("%s:%s", pod.Status.PodIP, NodeAddressPort),
		}
		claim := "data-" + pod.Name
		if pvc, ok := pvcByPod[claim]; ok {
			m.PVCName = pvc.Name
			m.VolumeName = pvc.Spec.VolumeName
		}
		members = append(members, m)
	}
	return Topology{Members: members}, nil
}

func RequireReadyTopology(ctx context.Context, kube *kubernetes.Clientset, ns, candidateDigest string) (Topology, error) {
	topo, err := DiscoverTopology(ctx, kube, ns)
	if err != nil {
		return Topology{}, err
	}
	if len(topo.Members) != 3 {
		return Topology{}, fmt.Errorf("expected 3 caesium pods, found %d (single-node startup is rejected)", len(topo.Members))
	}

	uids := map[string]struct{}{}
	ips := map[string]struct{}{}
	nodes := map[string]struct{}{}
	for _, m := range topo.Members {
		if m.UID == "" || m.IP == "" || m.Node == "" {
			return Topology{}, fmt.Errorf("pod %s missing uid/ip/node (uid=%q ip=%q node=%q)", m.Name, m.UID, m.IP, m.Node)
		}
		if !PodReady(&m.Pod) {
			return Topology{}, fmt.Errorf("pod %s is not Ready", m.Name)
		}
		if !IsWorkerNode(m.Node) {
			return Topology{}, fmt.Errorf("pod %s scheduled on non-worker node %s", m.Name, m.Node)
		}
		if m.ContainerID == "" {
			return Topology{}, fmt.Errorf("pod %s missing container ID", m.Name)
		}
		if candidateDigest != "" && !ImageIDMatchesCandidate(m.ImageID, candidateDigest) {
			return Topology{}, fmt.Errorf("pod %s image digest %q does not match candidate %q", m.Name, m.ImageID, candidateDigest)
		}
		if m.PVCName == "" || m.VolumeName == "" {
			return Topology{}, fmt.Errorf("pod %s missing bound PVC/volume (pvc=%q volume=%q)", m.Name, m.PVCName, m.VolumeName)
		}
		uids[m.UID] = struct{}{}
		ips[m.IP] = struct{}{}
		nodes[m.Node] = struct{}{}
	}
	if len(uids) != 3 || len(ips) != 3 || len(nodes) != 3 {
		return Topology{}, fmt.Errorf("caesium members are not distinct: uids=%d ips=%d nodes=%d", len(uids), len(ips), len(nodes))
	}

	bound := 0
	pvcs, err := kube.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return Topology{}, err
	}
	for _, pvc := range pvcs.Items {
		if strings.HasPrefix(pvc.Name, "data-caesium-") && pvc.Status.Phase == corev1.ClaimBound && pvc.Spec.VolumeName != "" {
			bound++
		}
	}
	if bound < 3 {
		return Topology{}, fmt.Errorf("expected 3 bound data PVCs, found %d", bound)
	}
	return topo, nil
}

func RefreshMember(ctx context.Context, kube *kubernetes.Clientset, ns, name string) (Member, error) {
	pod, err := kube.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Member{}, err
	}
	cs, err := CaesiumContainerStatus(pod)
	if err != nil {
		return Member{}, err
	}
	m := Member{
		Pod:         *pod,
		Name:        pod.Name,
		UID:         string(pod.UID),
		IP:          pod.Status.PodIP,
		Node:        pod.Spec.NodeName,
		ContainerID: StripContainerdPrefix(cs.ContainerID),
		Image:       cs.Image,
		ImageID:     cs.ImageID,
		NodeAddress: fmt.Sprintf("%s:%s", pod.Status.PodIP, NodeAddressPort),
		PVCName:     "data-" + pod.Name,
	}
	pvc, err := kube.CoreV1().PersistentVolumeClaims(ns).Get(ctx, m.PVCName, metav1.GetOptions{})
	if err == nil {
		m.VolumeName = pvc.Spec.VolumeName
	}
	return m, nil
}

func ListTaskPods(ctx context.Context, kube *kubernetes.Clientset, ns string) ([]corev1.Pod, error) {
	pods, err := kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: "cloud.caesium",
	})
	if err != nil {
		return nil, err
	}
	return pods.Items, nil
}

func RequireTasksOnSurvivors(pods []corev1.Pod, owner Member) error {
	if len(pods) == 0 {
		return fmt.Errorf("no fixture task pods observed")
	}
	for _, p := range pods {
		if p.Spec.NodeName == "" {
			return fmt.Errorf("task pod %s has no node", p.Name)
		}
		if p.Spec.NodeName == owner.Node {
			return fmt.Errorf("task pod %s scheduled on cordoned owner node %s", p.Name, owner.Node)
		}
		if IsControlPlaneNode(p.Spec.NodeName) {
			return fmt.Errorf("task pod %s scheduled on control-plane node %s", p.Name, p.Spec.NodeName)
		}
	}
	return nil
}
