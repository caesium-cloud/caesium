//go:build integration

package robustness

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type raceRoundTripFunc func(*http.Request) (*http.Response, error)

func (f raceRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRequestRaceGateRequiresBothRequestsBeforeForwarding(t *testing.T) {
	gate := newRequestRaceGate()
	defer gate.releaseBoth()
	var forwarded atomic.Int32
	next := raceRoundTripFunc(func(*http.Request) (*http.Response, error) {
		forwarded.Add(1)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	results := make(chan error, 2)
	for _, name := range []string{"replace", "complete"} {
		go func(name string) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://race.test/"+name, nil)
			if err == nil {
				var resp *http.Response
				resp, err = gate.transport(name, next).RoundTrip(req)
				if resp != nil {
					_ = resp.Body.Close()
				}
			}
			results <- err
		}(name)
	}
	arrivals, err := gate.awaitBoth(ctx)
	if err != nil || len(arrivals) != 2 || forwarded.Load() != 0 {
		t.Fatalf("requests forwarded before both were ready: arrivals=%+v err=%v forwards=%d", arrivals, err, forwarded.Load())
	}
	released := gate.releaseBoth()
	for _, arrival := range arrivals {
		if !arrival.At.Before(released) {
			t.Fatalf("contender %s did not arrive before release", arrival.Name)
		}
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("forwarded request: %v", err)
		}
	}
	if forwarded.Load() != 2 {
		t.Fatalf("forwarded %d, want 2", forwarded.Load())
	}
}

func TestRequestRaceGateCannotCertifyOneMissingContender(t *testing.T) {
	gate := newRequestRaceGate()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if arrivals, err := gate.awaitBoth(ctx); err == nil || len(arrivals) != 0 {
		t.Fatalf("missing contenders were certified: arrivals=%+v err=%v", arrivals, err)
	}
}
