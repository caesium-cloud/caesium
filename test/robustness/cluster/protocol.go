//go:build integration

package cluster

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	RequestConfigMap = "robustness-host-request"
	AckConfigMap     = "robustness-host-ack"
	RecordsConfigMap = "robustness-records"

	ActionCordon  = "cordon"
	ActionKill    = "kill"
	ActionRestart = "restart"
	ActionDone    = "done"
)

type HostRequest struct {
	RequestID        string `json:"request_id"`
	Action           string `json:"action"`
	OwnerPod         string `json:"owner_pod"`
	OwnerKindNode    string `json:"owner_kind_node"`
	OwnerContainerID string `json:"owner_container_id"`
	RunID            string `json:"run_id,omitempty"`
	RequestedAt      string `json:"requested_at"`
}

type HostAck struct {
	RequestID string `json:"request_id"`
	Action    string `json:"action"`
	Status    string `json:"status"`
	Evidence  string `json:"evidence"`
	Error     string `json:"error,omitempty"`
}

func encodeData(v any) (map[string]string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"payload":     string(raw),
		"payload_b64": base64.StdEncoding.EncodeToString(raw),
		"request_id":  fieldString(v, "RequestID"),
		"action":      fieldString(v, "Action"),
		"status":      fieldString(v, "Status"),
	}, nil
}

func fieldString(v any, name string) string {
	switch t := v.(type) {
	case HostRequest:
		switch name {
		case "RequestID":
			return t.RequestID
		case "Action":
			return t.Action
		}
	case HostAck:
		switch name {
		case "RequestID":
			return t.RequestID
		case "Action":
			return t.Action
		case "Status":
			return t.Status
		}
	}
	return ""
}

func putConfigMap(ctx context.Context, kube *kubernetes.Clientset, ns, name string, data map[string]string) error {
	existing, err := kube.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Data:       data,
		}
		_, err = kube.CoreV1().ConfigMaps(ns).Create(ctx, cm, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	existing.Data = data
	_, err = kube.CoreV1().ConfigMaps(ns).Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

func getConfigMap(ctx context.Context, kube *kubernetes.Clientset, ns, name string) (*corev1.ConfigMap, error) {
	return kube.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
}

func RequestHost(ctx context.Context, kube *kubernetes.Clientset, ns string, req HostRequest) (HostAck, error) {
	if req.RequestID == "" {
		return HostAck{}, fmt.Errorf("host request_id is required")
	}
	if req.Action == "" {
		return HostAck{}, fmt.Errorf("host action is required")
	}
	if req.RequestedAt == "" {
		req.RequestedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	data, err := encodeData(req)
	if err != nil {
		return HostAck{}, err
	}
	data["owner_pod"] = req.OwnerPod
	data["owner_kind_node"] = req.OwnerKindNode
	data["owner_container_id"] = req.OwnerContainerID
	data["run_id"] = req.RunID
	if err := putConfigMap(ctx, kube, ns, RequestConfigMap, data); err != nil {
		return HostAck{}, fmt.Errorf("write host request: %w", err)
	}

	var ack HostAck
	err = Poll(ctx, 500*time.Millisecond, func() (bool, error) {
		cm, err := getConfigMap(ctx, kube, ns, AckConfigMap)
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		parsed, err := parseAck(cm)
		if err != nil {
			return false, nil
		}
		if parsed.RequestID != req.RequestID {
			return false, nil
		}
		ack = parsed
		return true, nil
	})
	if err != nil {
		return HostAck{}, fmt.Errorf("waiting for host ack %s/%s: %w", req.Action, req.RequestID, err)
	}
	if ack.Status != "ok" {
		return ack, fmt.Errorf("host action %s failed: %s %s", req.Action, ack.Status, ack.Error+ack.Evidence)
	}
	if ack.Action != req.Action {
		return ack, fmt.Errorf("host ack action %q != request %q", ack.Action, req.Action)
	}
	return ack, nil
}

func parseAck(cm *corev1.ConfigMap) (HostAck, error) {
	if cm == nil || cm.Data == nil {
		return HostAck{}, fmt.Errorf("empty ack")
	}
	if raw := cm.Data["payload"]; raw != "" {
		var ack HostAck
		if err := json.Unmarshal([]byte(raw), &ack); err == nil && ack.RequestID != "" {
			return ack, nil
		}
	}
	if raw := cm.Data["payload_b64"]; raw != "" {
		b, err := base64.StdEncoding.DecodeString(raw)
		if err == nil {
			var ack HostAck
			if err := json.Unmarshal(b, &ack); err == nil && ack.RequestID != "" {
				return ack, nil
			}
		}
	}
	ack := HostAck{
		RequestID: cm.Data["request_id"],
		Action:    cm.Data["action"],
		Status:    cm.Data["status"],
		Evidence:  cm.Data["evidence"],
		Error:     cm.Data["error"],
	}
	if b, err := base64.StdEncoding.DecodeString(ack.Evidence); err == nil && len(b) > 0 {
		ack.Evidence = string(b)
	}
	if ack.RequestID == "" {
		return HostAck{}, fmt.Errorf("ack missing request_id")
	}
	return ack, nil
}

func WriteRecords(ctx context.Context, kube *kubernetes.Clientset, ns, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	cm, err := getConfigMap(ctx, kube, ns, RecordsConfigMap)
	if apierrors.IsNotFound(err) {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: RecordsConfigMap, Namespace: ns},
			Data:       map[string]string{},
		}
		cm, err = kube.CoreV1().ConfigMaps(ns).Create(ctx, cm, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		if apierrors.IsAlreadyExists(err) {
			cm, err = getConfigMap(ctx, kube, ns, RecordsConfigMap)
			if err != nil {
				return err
			}
		}
	} else if err != nil {
		return err
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[key] = string(raw)
	cm.Data[key+"_b64"] = base64.StdEncoding.EncodeToString(raw)
	_, err = kube.CoreV1().ConfigMaps(ns).Update(ctx, cm, metav1.UpdateOptions{})
	return err
}
