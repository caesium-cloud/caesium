//go:build integration

package robustness

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/bodylimit"
	"github.com/caesium-cloud/caesium/test/robustness/cluster"
	"github.com/caesium-cloud/caesium/test/robustness/faults"
)

type qualificationBody struct {
	data []byte
	err  error
}

func (b *qualificationBody) Read(p []byte) (int, error) {
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
func (*qualificationBody) Close() error { return nil }

type qualificationTransport func(*http.Request) (*http.Response, error)

func (f qualificationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestSoakStartIncompleteBodyReconcilesOnlyWithSameKey(t *testing.T) {
	const id = "e2a55b78-4f0e-4903-a9eb-36a3ff647959"
	body := `{"outcome":"created","id":"` + id + `"}`
	for _, tc := range []struct {
		name    string
		raw     string
		failure error
	}{
		{"validJSONReadError", body, errors.New("incomplete start")},
		{"overflow", body + strings.Repeat(" ", 1<<20), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			var keys []string
			sr := &soakRunner{fe: &faultEnv{topo: cluster.Topology{Members: []cluster.Member{{Name: "member", IP: "192.0.2.10"}}}}, faulted: map[string]string{}}
			sr.client = &http.Client{Transport: qualificationTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				keys = append(keys, req.Header.Get("Idempotency-Key"))
				b := io.NopCloser(strings.NewReader(body))
				if calls == 1 {
					b = io.NopCloser(strings.NewReader(tc.raw))
					if tc.failure != nil {
						b = &qualificationBody{data: []byte(tc.raw), err: tc.failure}
					}
				}
				return &http.Response{StatusCode: http.StatusAccepted, Header: make(http.Header), Body: b, Request: req}, nil
			})}
			entry := sr.startTracked(t.Context(), sr.fe.topo.Members[0], "job", "same-key", nil, "", "test", nil, true)
			if entry.Err == "" || !strings.Contains(entry.Err, id) || entry.RunID != "" || entry.HTTPStatus != http.StatusAccepted || calls != 1 || !entry.uncertainOrEmpty() {
				t.Fatalf("%+v calls=%d", entry, calls)
			}
			if err := sr.reconcileWithRetry(t.Context(), entry, 1, 0); err != nil {
				t.Fatal(err)
			}
			if calls != 2 || keys[0] != "same-key" || keys[1] != "same-key" || !entry.Reconciled || entry.RunID != id || entry.uncertainOrEmpty() {
				t.Fatalf("%+v keys=%v", entry, keys)
			}
		})
	}
}
func TestNoKeyHealReadErrorPreservesPossibleIdentityAndStatus(t *testing.T) {
	const id = "e2a55b78-4f0e-4903-a9eb-36a3ff647959"
	body := `{"id":"` + id + `"}`
	failure := errors.New("heal body incomplete")
	for _, tc := range []struct {
		body  io.ReadCloser
		cause error
	}{
		{&qualificationBody{data: []byte(body), err: failure}, failure},
		{io.NopCloser(strings.NewReader(body + strings.Repeat(" ", 1<<20))), bodylimit.ErrTooLarge},
	} {
		run, err := readHealedRunResponse(&http.Response{StatusCode: http.StatusAccepted, Body: tc.body})
		var uncertain *uncertainHealError
		if !errors.As(err, &uncertain) || !errors.Is(err, tc.cause) || run.ID != id || uncertain.status != http.StatusAccepted {
			t.Fatalf("%+v %v", run, err)
		}
	}
}
func TestHealThroughInterposerIncompleteResponseNeverBlindRetries(t *testing.T) {
	const id = "e2a55b78-4f0e-4903-a9eb-36a3ff647959"
	var calls atomic.Int32
	body := `{"id":"` + id + `"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Idempotency-Key") != "" {
			t.Error("unexpected key")
		}
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	interposer, err := faults.NewInterposer(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := interposer.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = interposer.Close(t.Context()) }()
	op, run, err := probeInterposerHealed(t.Context(), &faultEnv{}, interposer, "job", 2*time.Second)
	var uncertain *uncertainHealError
	if !errors.As(err, &uncertain) || calls.Load() != 1 || !op.PossiblyCommitted || op.UpstreamStatus != http.StatusAccepted || !strings.Contains(op.UpstreamBody, id) {
		t.Fatalf("op=%+v run=%+v err=%v calls=%d", op, run, err, calls.Load())
	}
}
