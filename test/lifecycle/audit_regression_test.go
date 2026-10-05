//go:build integration

package lifecycle

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/bodylimit"
	"github.com/caesium-cloud/caesium/test/robustness/cluster"
)

type lifecycleEvidenceTransport func(*http.Request) (*http.Response, error)

func (f lifecycleEvidenceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type lifecycleIncompleteBody struct {
	data []byte
	err  error
}

func (b *lifecycleIncompleteBody) Read(p []byte) (int, error) {
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
func (*lifecycleIncompleteBody) Close() error { return nil }

func TestTaskProofUnavailableMatchesBothCausesWithSameDiagnostic(t *testing.T) {
	cause := errors.New("incomplete query")
	const raw = `{"rows":[]}`
	for _, tc := range []struct {
		name  string
		body  io.ReadCloser
		cause error
	}{
		{"valid JSON read error", &lifecycleIncompleteBody{data: []byte(raw), err: cause}, cause},
		{"exact cap plus one", io.NopCloser(strings.NewReader(raw + strings.Repeat(" ", (4<<20)+1-len(raw)))), bodylimit.ErrTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &lifecycleClosingBody{ReadCloser: tc.body}
			calls := 0
			h := &cluster.HTTP{Client: &http.Client{Transport: lifecycleEvidenceTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, Request: r}, nil
			})}}
			proof, err := readClusterTaskProof(t.Context(), h, "http://evidence.invalid", "7c39392d-d1bf-43fc-b803-fce7d606141f", "148d4542-8df5-4bf2-8ff4-d542af055498")
			if !errors.Is(err, errTaskProofUnavailable) || !errors.Is(err, tc.cause) || proof != (clusterTaskProof{}) || calls != 1 || body.closes != 1 {
				t.Fatalf("proof=%+v error=%v calls=%d closes=%d", proof, err, calls, body.closes)
			}
			want := fmt.Sprintf("%v: %v", errTaskProofUnavailable, tc.cause)
			if err.Error() != want {
				t.Fatalf("diagnostic %q want %q", err, want)
			}
		})
	}
}
