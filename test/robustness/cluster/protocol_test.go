//go:build integration

package cluster

import (
	"encoding/base64"
	"reflect"
	"testing"
)

func TestEncodeDataHostRequest(t *testing.T) {
	req := HostRequest{
		RequestID:        "req-1",
		Action:           ActionCordon,
		OwnerPod:         "pod-1",
		OwnerKindNode:    "node-1",
		OwnerContainerID: "containerd://abc",
		RequestedAt:      "2026-10-03T12:00:00Z",
		Params:           map[string]string{"key": "value"},
	}
	const payload = `{"request_id":"req-1","action":"cordon","owner_pod":"pod-1","owner_kind_node":"node-1","owner_container_id":"containerd://abc","requested_at":"2026-10-03T12:00:00Z","params":{"key":"value"}}`

	got, err := encodeData(req)
	if err != nil {
		t.Fatalf("encodeData() error = %v", err)
	}
	want := map[string]string{
		"payload":     payload,
		"payload_b64": base64.StdEncoding.EncodeToString([]byte(payload)),
		"request_id":  req.RequestID,
		"action":      req.Action,
		"status":      "",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("encodeData() = %#v, want %#v", got, want)
	}
}
