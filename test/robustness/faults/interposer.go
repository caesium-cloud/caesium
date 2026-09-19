package faults

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Interposer is a CLIENT-side response fault, owned by the runner and sitting
// on the recorder/runner's own route to a member.
//
// A1 records that no mutation endpoint has a verified bounded quorum-loss
// rejection contract, so a missing or late response is legal uncertainty:
// "a transport timeout/disconnect is possibly committed". To test how such an
// operation must be handled, the loss has to happen AFTER an observable commit
// — which is precisely what a proxy on the client's own route can arrange and
// a network partition cannot, because a partition hides whether the server
// committed.
//
// The interposer therefore forwards the request, reads the upstream response in
// full (the controller-observed truth, retained), and only then drops or delays
// it. The client sees a transport error or a deadline; the operation is
// recorded as PossiblyCommitted and must be reconciled by identity afterwards,
// never reported as rejected.
type Interposer struct {
	upstream *url.URL
	client   *http.Client

	mu     sync.Mutex
	policy Policy
	ops    []Operation

	srv *http.Server
	ln  net.Listener
}

// Mode selects what happens to a matched response.
type Mode string

const (
	// ModePassThrough forwards the response unchanged.
	ModePassThrough Mode = "pass"
	// ModeDropResponse discards the response after the upstream produced it,
	// closing the connection so the client observes a transport error.
	ModeDropResponse Mode = "drop"
	// ModeDelayResponse holds the response, so a client deadline can expire
	// while the operation has already committed.
	ModeDelayResponse Mode = "delay"
)

// Policy decides which operations are faulted.
type Policy struct {
	Mode Mode
	// Method and PathContains select the operation. Empty matches everything,
	// which is why a test must set them: faulting every request would make the
	// resulting history unattributable.
	Method       string
	PathContains string
	Delay        time.Duration
	// Once applies the fault to the first match only.
	Once bool

	applied bool
}

func (p Policy) matches(r *http.Request) bool {
	if p.Mode == "" || p.Mode == ModePassThrough {
		return false
	}
	if p.Method != "" && !strings.EqualFold(p.Method, r.Method) {
		return false
	}
	if p.PathContains != "" && !strings.Contains(r.URL.Path, p.PathContains) {
		return false
	}
	return true
}

// Operation is one recorded invocation through the interposer.
//
// The recorder assigns the operation ID before the invocation; it is a
// correlation key, never a server idempotency token, so a lost response must
// not be retried as if it were idempotent.
type Operation struct {
	ID             string    `json:"operation_id"`
	Method         string    `json:"method"`
	Path           string    `json:"path"`
	InvokedAt      time.Time `json:"invoked_at"`
	UpstreamAt     time.Time `json:"upstream_at"`
	SettledAt      time.Time `json:"settled_at"`
	UpstreamStatus int       `json:"upstream_status"`
	UpstreamBody   string    `json:"upstream_body,omitempty"`
	UpstreamErr    string    `json:"upstream_error,omitempty"`
	Applied        Mode      `json:"applied"`
	// PossiblyCommitted marks every operation whose client outcome no longer
	// determines the server outcome. Such an operation must be reconciled from
	// public state by identity, and a client-side timeout must never be
	// recorded as a rejection.
	PossiblyCommitted bool `json:"possibly_committed"`
}

// NewInterposer proxies to upstreamBase (e.g. "http://10.244.1.5:8080").
func NewInterposer(upstreamBase string) (*Interposer, error) {
	u, err := url.Parse(strings.TrimRight(upstreamBase, "/"))
	if err != nil {
		return nil, err
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("upstream %q needs a scheme and host", upstreamBase)
	}
	return &Interposer{
		upstream: u,
		// No client timeout: the fault, not the transport, decides the delay.
		client: &http.Client{},
	}, nil
}

// Start listens on an ephemeral loopback port.
func (i *Interposer) Start() error {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	i.ln = ln
	i.srv = &http.Server{Handler: http.HandlerFunc(i.handle), ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = i.srv.Serve(ln) }()
	return nil
}

// BaseURL is the address a client should use instead of the member's own.
func (i *Interposer) BaseURL() string {
	if i.ln == nil {
		return ""
	}
	return "http://" + i.ln.Addr().String()
}

// Upstream is the member the interposer forwards to.
func (i *Interposer) Upstream() string { return i.upstream.String() }

// Close stops the listener.
func (i *Interposer) Close(ctx context.Context) error {
	if i.srv == nil {
		return nil
	}
	return i.srv.Shutdown(ctx)
}

// SetPolicy arms or disarms the response fault.
func (i *Interposer) SetPolicy(p Policy) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.policy = p
}

// Operations returns every recorded invocation, in order.
func (i *Interposer) Operations() []Operation {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := make([]Operation, len(i.ops))
	copy(out, i.ops)
	return out
}

// PossiblyCommitted returns the operations whose outcome the client cannot
// decide.
func (i *Interposer) PossiblyCommitted() []Operation {
	var out []Operation
	for _, op := range i.Operations() {
		if op.PossiblyCommitted {
			out = append(out, op)
		}
	}
	return out
}

func (i *Interposer) record(op Operation) {
	i.mu.Lock()
	i.ops = append(i.ops, op)
	i.mu.Unlock()
}

func (i *Interposer) takePolicy(r *http.Request) Policy {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.policy.matches(r) {
		return Policy{Mode: ModePassThrough}
	}
	if i.policy.Once {
		if i.policy.applied {
			return Policy{Mode: ModePassThrough}
		}
		i.policy.applied = true
	}
	return i.policy
}

func (i *Interposer) handle(w http.ResponseWriter, r *http.Request) {
	opID := fmt.Sprintf("op-%d-%s", time.Now().UnixNano(), strings.TrimPrefix(r.URL.Path, "/"))
	op := Operation{
		ID:        opID,
		Method:    r.Method,
		Path:      r.URL.Path,
		InvokedAt: time.Now().UTC(),
		Applied:   ModePassThrough,
	}
	policy := i.takePolicy(r)

	target := *i.upstream
	target.Path = r.URL.Path
	target.RawQuery = r.URL.RawQuery

	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		op.UpstreamErr = "read request body: " + err.Error()
		op.SettledAt = time.Now().UTC()
		i.record(op)
		http.Error(w, "interposer could not read the request body", http.StatusBadGateway)
		return
	}

	// The upstream call deliberately uses a context detached from the client's:
	// the whole point is that the server keeps going after the client gives up.
	req, err := http.NewRequestWithContext(context.WithoutCancel(r.Context()), r.Method, target.String(), strings.NewReader(string(body)))
	if err != nil {
		op.UpstreamErr = err.Error()
		op.SettledAt = time.Now().UTC()
		i.record(op)
		http.Error(w, "interposer could not build the upstream request", http.StatusBadGateway)
		return
	}
	for k, vs := range r.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	resp, err := i.client.Do(req)
	if err != nil {
		op.UpstreamErr = err.Error()
		op.SettledAt = time.Now().UTC()
		// The upstream itself failed in a way the interposer cannot classify,
		// so the operation is still possibly committed.
		op.PossiblyCommitted = true
		i.record(op)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	op.UpstreamAt = time.Now().UTC()
	op.UpstreamStatus = resp.StatusCode
	op.UpstreamBody = string(respBody)

	switch policy.Mode {
	case ModeDropResponse:
		op.Applied = ModeDropResponse
		op.PossiblyCommitted = true
		op.SettledAt = time.Now().UTC()
		i.record(op)
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			// Without hijacking we cannot lose the response honestly, so fail
			// loudly instead of pretending the fault happened.
			http.Error(w, "interposer cannot drop the response on this transport", http.StatusInternalServerError)
			return
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			http.Error(w, "interposer could not hijack the connection", http.StatusInternalServerError)
			return
		}
		_ = conn.Close()
		return
	case ModeDelayResponse:
		op.Applied = ModeDelayResponse
		op.PossiblyCommitted = true
		time.Sleep(policy.Delay)
	}

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, writeErr := w.Write(respBody)
	op.SettledAt = time.Now().UTC()
	if writeErr != nil {
		op.UpstreamErr = "client write: " + writeErr.Error()
		op.PossiblyCommitted = true
	}
	i.record(op)
}

// ReconcileNote renders the required handling of a possibly committed
// operation, so a record can never read as "the mutation was rejected".
func ReconcileNote(op Operation) string {
	if !op.PossiblyCommitted {
		return ""
	}
	return fmt.Sprintf(
		"operation %s (%s %s) is POSSIBLY COMMITTED: the controller observed upstream status %d, "+
			"but the client received no usable response. Reconcile by recorded identity from public state; "+
			"do not treat the client outcome as a rejection and do not retry it as idempotent.",
		op.ID, op.Method, op.Path, op.UpstreamStatus)
}
