//go:build integration

package recorder

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	ListenAddr  = "0.0.0.0:8090"
	ServiceName = "robustness-recorder"
	ServicePort = 8090
)

// Event is one append-only recorder observation.
type Event struct {
	Kind      string            `json:"kind"`
	At        time.Time         `json:"at"`
	RunID     string            `json:"run_id,omitempty"`
	Step      string            `json:"step,omitempty"`
	Nonce     string            `json:"nonce,omitempty"`
	ProbeID   string            `json:"probe_id,omitempty"`
	Operation string            `json:"operation_id,omitempty"`
	Detail    map[string]string `json:"detail,omitempty"`
	Raw       string            `json:"raw,omitempty"`
}

type startBody struct {
	RunID string `json:"run_id"`
	Step  string `json:"step"`
	Nonce string `json:"nonce"`
	Event string `json:"event,omitempty"`
}

type probeBody struct {
	ID string `json:"id"`
}

// Sink is the in-cluster HTTP effect ledger and barrier.
type Sink struct {
	mu          sync.Mutex
	events      []Event
	probes      map[string]time.Time
	starts      []Event
	released    map[string]struct{}
	releasedAll bool
	srv         *http.Server
	ln          net.Listener
}

func New() *Sink {
	return &Sink{
		probes:   make(map[string]time.Time),
		released: make(map[string]struct{}),
	}
}

func (s *Sink) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/probe", s.handleProbe)
	mux.HandleFunc("/probe/", s.handleProbeGet)
	mux.HandleFunc("/start", s.handleStart)
	mux.HandleFunc("/effect", s.handleStart)
	mux.HandleFunc("/wait", s.handleWait)
	mux.HandleFunc("/release", s.handleRelease)
	mux.HandleFunc("/records", s.handleRecords)

	ln, err := net.Listen("tcp", ListenAddr)
	if err != nil {
		return err
	}
	s.ln = ln
	s.srv = &http.Server{
		Addr:              ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = s.srv.Serve(ln) }()
	return nil
}

func (s *Sink) Close(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

func (s *Sink) Addr() string {
	if s.ln == nil {
		return ListenAddr
	}
	return s.ln.Addr().String()
}

func (s *Sink) Record(ev Event) {
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	s.mu.Lock()
	s.events = append(s.events, ev)
	s.mu.Unlock()
}

func (s *Sink) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.events))
	copy(out, s.events)
	return out
}

func (s *Sink) ProbeSeen(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.probes[id]
	return ok
}

func (s *Sink) Starts() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.starts))
	copy(out, s.starts)
	return out
}

func (s *Sink) StartsFor(runID, step string) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Event
	for _, ev := range s.starts {
		if ev.RunID == runID && (step == "" || ev.Step == step) {
			out = append(out, ev)
		}
	}
	return out
}

func (s *Sink) CompletionsFor(runID, step string) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Event
	for _, ev := range s.events {
		if ev.Kind != "complete" {
			continue
		}
		if ev.RunID == runID && (step == "" || ev.Step == step) {
			out = append(out, ev)
		}
	}
	return out
}

func (s *Sink) Release(runID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(runID) == "" {
		s.releasedAll = true
		return
	}
	s.released[runID] = struct{}{}
	s.events = append(s.events, Event{
		Kind:  "release",
		At:    time.Now().UTC(),
		RunID: runID,
	})
}

func (s *Sink) Released(runID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.releasedAll {
		return true
	}
	_, ok := s.released[runID]
	return ok
}

func (s *Sink) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Sink) handleProbe(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	id := strings.TrimSpace(string(body))
	var parsed probeBody
	if err := json.Unmarshal(body, &parsed); err == nil && strings.TrimSpace(parsed.ID) != "" {
		id = strings.TrimSpace(parsed.ID)
	}
	if q := strings.TrimSpace(r.URL.Query().Get("id")); q != "" {
		id = q
	}
	if id == "" {
		http.Error(w, "probe id required", http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	s.mu.Lock()
	s.probes[id] = now
	s.events = append(s.events, Event{Kind: "probe", At: now, ProbeID: id, Raw: string(body)})
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Sink) handleProbeGet(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/probe/")
	id = strings.TrimSpace(id)
	if id == "" {
		http.Error(w, "probe id required", http.StatusBadRequest)
		return
	}
	if !s.ProbeSeen(id) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Sink) handleStart(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var parsed startBody
	if err := json.Unmarshal(body, &parsed); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	parsed.RunID = strings.TrimSpace(parsed.RunID)
	parsed.Step = strings.TrimSpace(parsed.Step)
	parsed.Nonce = strings.TrimSpace(parsed.Nonce)
	if parsed.RunID == "" || parsed.Step == "" || parsed.Nonce == "" {
		http.Error(w, "run_id, step, and nonce are required", http.StatusBadRequest)
		return
	}
	kind := "start"
	if strings.EqualFold(strings.TrimSpace(parsed.Event), "complete") {
		kind = "complete"
	}
	ev := Event{
		Kind:  kind,
		At:    time.Now().UTC(),
		RunID: parsed.RunID,
		Step:  parsed.Step,
		Nonce: parsed.Nonce,
		Raw:   string(body),
	}
	s.mu.Lock()
	s.events = append(s.events, ev)
	if kind == "start" {
		s.starts = append(s.starts, ev)
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Sink) handleWait(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.URL.Query().Get("run_id"))
	if s.Released(runID) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("released"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("waiting"))
}

func (s *Sink) handleRelease(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.URL.Query().Get("run_id"))
	if runID == "" {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var parsed startBody
		if err := json.Unmarshal(body, &parsed); err == nil {
			runID = strings.TrimSpace(parsed.RunID)
		}
	}
	s.Release(runID)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("released"))
}

func (s *Sink) handleRecords(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.Events())
}
