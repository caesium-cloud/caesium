//go:build integration

package performance

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type incompleteProbeBody struct {
	data []byte
	err  error
}

func (b *incompleteProbeBody) Read(p []byte) (int, error) {
	if len(b.data) == 0 {
		return 0, b.err
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	if len(b.data) == 0 {
		return n, b.err
	}
	return n, nil
}
func (*incompleteProbeBody) Close() error { return nil }
func TestServerEnvProbeRejectsIncompleteValidEvidence(t *testing.T) {
	const capBytes = 1 << 20
	for _, tc := range []struct {
		name   string
		body   io.ReadCloser
		reason string
	}{
		{"readError", &incompleteProbeBody{data: []byte(`{"Config":{"Env":["ENABLED=true"]}}`), err: errors.New("incomplete probe")}, "incomplete probe"},
		{"overflow", io.NopCloser(strings.NewReader(`{"Config":{"Env":["ENABLED=true"]}}` + strings.Repeat(" ", capBytes))), "request body too large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := readServerEnvResponse(&http.Response{StatusCode: http.StatusOK, Body: tc.body}, "server")
			if p.available || p.env != nil || !strings.Contains(p.reason, tc.reason) || !strings.Contains(p.reason, "HTTP 200") {
				t.Fatalf("%+v", p)
			}
		})
	}
}
func TestServerEnvProbeCompleteEvidence(t *testing.T) {
	body := `{"Config":{"Env":["ENABLED=true"]}}`
	p := readServerEnvResponse(&http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, "server")
	if !p.available || p.env["ENABLED"] != "true" {
		t.Fatalf("%+v", p)
	}
	p = readServerEnvResponse(&http.Response{StatusCode: http.StatusForbidden, Body: io.NopCloser(strings.NewReader(body))}, "server")
	if p.available || !strings.Contains(p.reason, "HTTP 403") {
		t.Fatalf("%+v", p)
	}
}
