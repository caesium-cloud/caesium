//go:build integration

package robustness

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// requestRaceGate holds both client RoundTrips before either reaches a server.
// The two invocation intervals therefore overlap. It does not claim the two
// server transactions execute concurrently; durable event order decides that.
type requestRaceGate struct {
	ready   chan raceArrival
	release chan struct{}
	once    sync.Once
}

type raceArrival struct {
	Name string    `json:"name"`
	At   time.Time `json:"at"`
}

func newRequestRaceGate() *requestRaceGate {
	return &requestRaceGate{ready: make(chan raceArrival, 2), release: make(chan struct{})}
}

type gatedRoundTripper struct {
	gate *requestRaceGate
	name string
	next http.RoundTripper
}

func (g *requestRaceGate) transport(name string, next http.RoundTripper) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	return gatedRoundTripper{gate: g, name: name, next: next}
}

func (g gatedRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	select {
	case g.gate.ready <- raceArrival{Name: g.name, At: time.Now().UTC()}:
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	select {
	case <-g.gate.release:
		return g.next.RoundTrip(req)
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
}

func (g *requestRaceGate) awaitBoth(ctx context.Context) ([]raceArrival, error) {
	arrivals := make([]raceArrival, 0, 2)
	seen := map[string]bool{}
	for len(arrivals) < 2 {
		select {
		case arrival := <-g.ready:
			if arrival.Name != "replace" && arrival.Name != "complete" {
				return nil, fmt.Errorf("unexpected race contender %q", arrival.Name)
			}
			if seen[arrival.Name] {
				return nil, fmt.Errorf("duplicate race contender %q", arrival.Name)
			}
			seen[arrival.Name] = true
			arrivals = append(arrivals, arrival)
		case <-ctx.Done():
			return nil, fmt.Errorf("both race contenders did not reach transport gate: %w", ctx.Err())
		}
	}
	return arrivals, nil
}

func (g *requestRaceGate) releaseBoth() time.Time {
	at := time.Now().UTC()
	g.once.Do(func() { close(g.release) })
	return at
}
