//go:build integration

package lifecycle

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

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
	h := &cluster.HTTP{Client: &http.Client{Transport: lifecycleEvidenceTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: &lifecycleIncompleteBody{data: []byte(`{"rows":[]}`), err: cause}, Request: r}, nil
	})}}
	_, err := readClusterTaskProof(t.Context(), h, "http://evidence.invalid", "7c39392d-d1bf-43fc-b803-fce7d606141f", "148d4542-8df5-4bf2-8ff4-d542af055498")
	if !errors.Is(err, errTaskProofUnavailable) || !errors.Is(err, cause) {
		t.Fatalf("error tree lost a cause: %v", err)
	}
	want := fmt.Sprintf("%v: %v", errTaskProofUnavailable, cause)
	if err.Error() != want {
		t.Fatalf("diagnostic %q want %q", err, want)
	}
}
