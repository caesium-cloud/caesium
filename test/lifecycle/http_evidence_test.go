//go:build integration

package lifecycle

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/test/robustness/cluster"
)

type lifecycleClosingBody struct {
	io.ReadCloser
	closes int
}

func (b *lifecycleClosingBody) Close() error {
	b.closes++
	return b.ReadCloser.Close()
}

func TestMixedProtocolProbeRejectsValidJSONWithReadErrorAndKeepsRetryReason(t *testing.T) {
	cause := errors.New("incomplete capability")
	body := `{"node_id":"member","protocol_version":2}`
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		calls := 0
		incomplete := &lifecycleClosingBody{ReadCloser: &lifecycleIncompleteBody{data: []byte(body), err: cause}}
		ic := &cluster.InternalClient{Token: "token", HTTP: &http.Client{Transport: lifecycleEvidenceTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.Method != http.MethodGet || r.URL.Path != "/internal/capabilities" || r.Header.Get("Authorization") != "Bearer token" {
				t.Error("lost read-only probe route or authorization")
			}
			if calls == 1 {
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: incomplete, Request: r}, nil
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		})}}
		window := map[string]mixedHeldMember{"member": {member: cluster.Member{IP: "192.0.2.10"}}}
		got, reason := probeMixedProtocolsWithClient(t.Context(), t, ic, window)
		wantReason := fmt.Sprintf("member capabilities incomplete after status %d (%s): incomplete capability", status, body)
		if got != nil || reason != wantReason || calls != 1 || incomplete.closes != 1 || window["member"].Protocol != 0 {
			t.Fatalf("got=%v reason=%s calls=%d", got, reason, calls)
		}
		// An unavailable probe is retryable; only the later complete response
		// may publish a member protocol. The failed call must not mutate input.
		got, reason = probeMixedProtocolsWithClient(t.Context(), t, ic, window)
		if reason != "" || len(got) != 1 || got["member"].Protocol != 2 || got["member"].NodeID != "member" || calls != 2 {
			t.Fatalf("complete retry got=%v reason=%s calls=%d", got, reason, calls)
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
