//go:build integration

package lifecycle

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/test/robustness/cluster"
)

func TestMixedProtocolProbeRejectsValidJSONWithReadErrorAndKeepsRetryReason(t *testing.T) {
	cause := errors.New("incomplete capability")
	body := `{"node_id":"member","protocol_version":2}`
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		calls := 0
		ic := &cluster.InternalClient{Token: "token", HTTP: &http.Client{Transport: lifecycleEvidenceTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.Header.Get("Authorization") != "Bearer token" {
				t.Error("lost authorization")
			}
			return &http.Response{StatusCode: status, Header: make(http.Header), Body: &lifecycleIncompleteBody{data: []byte(body), err: cause}, Request: r}, nil
		})}}
		got, reason := probeMixedProtocolsWithClient(t.Context(), t, ic, map[string]mixedHeldMember{"member": {member: cluster.Member{IP: "192.0.2.10"}}})
		if got != nil || !strings.Contains(reason, cause.Error()) || !strings.Contains(reason, "member") || !strings.Contains(reason, body) || calls != 1 {
			t.Fatalf("got=%v reason=%s calls=%d", got, reason, calls)
		}
	}
}
func TestMixedProtocolProbeRetainsUnboundedCompleteReader(t *testing.T) {
	// This caller had no cap. Preserve a complete body larger than the 1MiB
	// caps used by other readers rather than introducing a new limit here.
	body := `{"node_id":"member","protocol_version":2}` + strings.Repeat(" ", (1<<20)+1)
	ic := &cluster.InternalClient{HTTP: &http.Client{Transport: lifecycleEvidenceTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}}
	got, reason := probeMixedProtocolsWithClient(t.Context(), t, ic, map[string]mixedHeldMember{"member": {member: cluster.Member{IP: "192.0.2.10"}}})
	if reason != "" || got["member"].Protocol != 2 || got["member"].NodeID != "member" {
		t.Fatalf("got=%v reason=%s", got, reason)
	}
}
