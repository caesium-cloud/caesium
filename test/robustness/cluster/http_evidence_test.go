//go:build integration

package cluster

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/bodylimit"
)

type incompleteEvidenceBody struct {
	io.Reader
	err    error
	closed bool
}

func (b *incompleteEvidenceBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF && b.err != nil {
		err = b.err
	}
	return n, err
}
func (b *incompleteEvidenceBody) Close() error { b.closed = true; return nil }

func TestHTTPDoRejectsOverflowAndRetainsStatusPartialBody(t *testing.T) {
	for _, n := range []int{4 << 20, 4<<20 + 1} {
		body := &incompleteEvidenceBody{Reader: strings.NewReader(strings.Repeat("x", n))}
		h := &HTTP{Client: &http.Client{Transport: queryRoundTripper(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 202, Body: body}, nil
		})}}
		status, raw, err := h.Do(context.Background(), http.MethodPost, "http://query.test/mutation", nil)
		if status != 202 || len(raw) != 4<<20 || !body.closed || errors.Is(err, bodylimit.ErrTooLarge) != (n > 4<<20) {
			t.Fatalf("%d bytes: status=%d retained=%d closed=%t err=%v", n, status, len(raw), body.closed, err)
		}
	}
}

func TestTriggerReadFailureIsUncertainEvenWithValidAcceptedJSON(t *testing.T) {
	const id = "e2a55b78-4f0e-4903-a9eb-36a3ff647959"
	readErr := errors.New("interrupted response")
	body := &incompleteEvidenceBody{Reader: strings.NewReader(`{"id":"` + id + `"}`), err: readErr}
	h := &HTTP{Client: &http.Client{Transport: queryRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 202, Body: body}, nil
	})}}
	status, raw, err := h.TriggerRunRaw(context.Background(), "http://query.test", id)
	if status != 202 || string(raw) != `{"id":"`+id+`"}` || !errors.Is(err, readErr) || !strings.Contains(err.Error(), "may have committed") || !body.closed {
		t.Fatalf("accepted incomplete response: status=%d raw=%q err=%v closed=%t", status, raw, err, body.closed)
	}
}

func TestMutationTransportFailureIsUncertainButPreflightIsDefinitive(t *testing.T) {
	const id = "e2a55b78-4f0e-4903-a9eb-36a3ff647959"
	lost := errors.New("response lost after submission")
	calls := 0
	h := &HTTP{Client: &http.Client{Transport: queryRoundTripper(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, lost
	})}}
	status, raw, err := h.TriggerRunRaw(context.Background(), "http://query.test", id)
	if status != 0 || raw != nil || !errors.Is(err, lost) || !strings.Contains(err.Error(), "may have committed") || calls != 1 {
		t.Fatalf("ambiguous submission: status=%d raw=%q calls=%d err=%v", status, raw, calls, err)
	}
	_, _, err = h.TriggerRunRaw(context.Background(), "%", id)
	if err == nil || strings.Contains(err.Error(), "may have committed") || calls != 1 {
		t.Fatalf("request preflight: calls=%d, %v", calls, err)
	}
	_, _, err = h.Do(context.Background(), http.MethodPost, "http://query.test", make(chan int))
	if err == nil || calls != 1 {
		t.Fatalf("marshal preflight: calls=%d, %v", calls, err)
	}
	if _, wrapped := errors.AsType[*transportFailure](err); wrapped {
		t.Fatal("local failure classified as transport")
	}
	client := &InternalClient{HTTP: h.Client}
	ex := client.Post(context.Background(), "http://query.test", "/internal/dispatch", nil)
	if ex.Status != 0 || !strings.Contains(ex.Err, "may have committed") {
		t.Fatalf("internal uncertain outcome: %+v", ex)
	}
	ex = client.Post(context.Background(), "http://query.test", "/internal/dispatch", make(chan int))
	if ex.Err == "" || strings.Contains(ex.Err, "may have committed") {
		t.Fatalf("internal local failure: %+v", ex)
	}
}

func TestInternalPostRequiresCompleteBoundedEvidence(t *testing.T) {
	readErr := errors.New("interrupted response")
	for _, tc := range []struct {
		name, body string
		err        error
		wantErr    bool
	}{
		{"complete", `{"accepted":true}`, nil, false},
		{"valid-json-read-error", `{"accepted":true}`, readErr, true},
		{"exact-cap", strings.Repeat("x", 1<<20), nil, false},
		{"overflow", strings.Repeat("x", 1<<20+1), nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &incompleteEvidenceBody{Reader: strings.NewReader(tc.body), err: tc.err}
			client := &InternalClient{HTTP: &http.Client{Transport: queryRoundTripper(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 202, Body: body}, nil
			})}}
			ex := client.Post(context.Background(), "http://query.test", "/internal/dispatch", map[string]string{"run_id": "run"})
			if ex.Status != 202 || (ex.Err != "") != tc.wantErr || !body.closed || len(ex.Body) > 1<<20 {
				t.Fatalf("exchange = %+v, closed=%t", ex, body.closed)
			}
			if tc.wantErr && !strings.Contains(ex.Err, "may have committed") {
				t.Fatal(ex.Err)
			}
			if tc.err != nil && ex.Body != tc.body {
				t.Fatal("lost possible committed identity/body evidence")
			}
		})
	}
}
