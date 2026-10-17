//go:build integration

// The fixture is an explicit container-built executable in the root module.
package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/caesium-cloud/caesium/internal/connector"
	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type journal struct {
	mu     sync.Mutex
	path   string
	events []map[string]any
}

func newJournal(path string) (*journal, error) {
	j := &journal{path: path}
	raw, err := os.ReadFile(path)
	if err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			var event map[string]any
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				return nil, err
			}
			j.events = append(j.events, event)

		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return j, nil
}
func (j *journal) add(event map[string]any) { j.mu.Lock(); defer j.mu.Unlock(); j.append(event) }
func (j *journal) append(event map[string]any) {
	event["at"] = time.Now().UTC().Format(time.RFC3339Nano)
	data, err := json.Marshal(event)
	if err != nil {
		log.Fatal(err)
	}
	f, err := os.OpenFile(j.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatal(err)
	}
	if _, err = f.Write(append(data, '\n')); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		log.Fatal(err)
	}
	if closeErr != nil {
		log.Fatal(closeErr)
	}
	j.events = append(j.events, event)
}

// effect records every external execution. The evidence sink never suppresses
// duplicates; its raw count must expose a second activity or a retry.
func (j *journal) effect(runID, activityID string, attempt int32, actor connector.ActorEnvelope) {
	j.add(map[string]any{"kind": "approval_effect", "run_id": runID, "activity_id": activityID, "attempt": attempt, "actor": actor})
}
func (j *journal) snapshot() []map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]map[string]any, len(j.events))
	copy(out, j.events)
	return out
}

type versionMatrix struct {
	SDK    string `json:"sdk"`
	CLI    string `json:"cli"`
	Server string `json:"server"`
}

var cliVersionPattern = regexp.MustCompile(`^temporal version ([0-9]+\.[0-9]+\.[0-9]+) \(Server ([0-9]+\.[0-9]+\.[0-9]+),`)

func validateVersions(sdk, cliOutput, server string) (versionMatrix, error) {
	matched := cliVersionPattern.FindStringSubmatch(strings.TrimSpace(cliOutput))
	if len(matched) != 3 {
		return versionMatrix{}, errors.New("temporal CLI version output is unrecognized")
	}
	observed := versionMatrix{SDK: strings.TrimPrefix(sdk, "v"), CLI: matched[1], Server: server}
	if observed.SDK != "1.49.0" || observed.CLI != "1.9.1" || observed.Server != "1.32.0" || matched[2] != observed.Server {
		return versionMatrix{}, errors.New("observed Temporal versions do not match qualified fixture matrix")
	}
	return observed, nil
}

type fixture struct {
	mu                          sync.Mutex
	c                           client.Client
	w                           worker.Worker
	a                           *activities
	j                           *journal
	faultID, faultMode          string
	faultWorkflowID, faultRunID string
	outage                      bool
	versions                    versionMatrix
}

func (f *fixture) startWorker() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.w != nil {
		return nil
	}
	w := worker.New(f.c, taskQueue, worker.Options{})
	w.RegisterWorkflowWithOptions(Publication, workflow.RegisterOptions{Name: "publication"})
	w.RegisterWorkflowWithOptions(ExternalOnly, workflow.RegisterOptions{Name: "external_only"})
	w.RegisterWorkflow(ExternalChild)
	w.RegisterActivityWithOptions(f.a.RunJob, activity.RegisterOptions{Name: "caesium.start"})
	w.RegisterActivityWithOptions(f.a.Effect, activity.RegisterOptions{Name: "fixture.effect"})
	if err := w.Start(); err != nil {
		return err
	}
	f.w = w
	f.j.add(map[string]any{"kind": "worker_started"})
	return nil
}
func (f *fixture) stopWorker() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.w != nil {
		f.w.Stop()
		f.w = nil
		f.j.add(map[string]any{"kind": "worker_stopped"})
	}
}

// rawCodec forwards the original protobuf bytes, including unknown fields.
// It does not decode or rewrite workflow payloads.
type rawCodec struct{}

func (rawCodec) Name() string { return "proto" }
func (rawCodec) Marshal(v any) ([]byte, error) {
	if raw, ok := v.(*[]byte); ok {
		return *raw, nil
	}
	return proto.Marshal(v.(proto.Message))
}
func (rawCodec) Unmarshal(raw []byte, v any) error {
	if out, ok := v.(*[]byte); ok {
		*out = append((*out)[:0], raw...)
		return nil
	}
	return proto.Unmarshal(raw, v.(proto.Message))
}
func (f *fixture) proxy(upstream *grpc.ClientConn) grpc.StreamHandler {
	return func(_ any, stream grpc.ServerStream) error {
		method, _ := grpc.MethodFromServerStream(stream)
		var request []byte
		if err := stream.RecvMsg(&request); err != nil {
			return err
		}
		// Count every connector RPC at the actual TLS frontend, before fault
		// handling. Worker/control traffic uses the private loopback client and
		// cannot satisfy the public read or zero-RPC refusal assertions.
		f.j.add(map[string]any{"kind": "provider_rpc", "method": method})
		ctx := stream.Context()
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			ctx = metadata.NewOutgoingContext(ctx, md.Copy())
		}
		f.mu.Lock()
		outage := f.outage
		faultID, faultMode := f.faultID, f.faultMode
		faultWorkflowID, faultRunID := f.faultWorkflowID, f.faultRunID
		f.mu.Unlock()
		if outage {
			f.j.add(map[string]any{"kind": "proxy_interruption", "method": method})
			return status.Error(codes.Unavailable, "fixture connector path interrupted")
		}
		var response []byte
		err := upstream.Invoke(ctx, method, &request, &response, grpc.ForceCodec(rawCodec{}))
		isUpdate := strings.HasSuffix(method, "/UpdateWorkflowExecution")
		isLookup := strings.HasSuffix(method, "/PollWorkflowExecutionUpdate")
		if isUpdate {
			req := new(workflowservice.UpdateWorkflowExecutionRequest)
			if decodeErr := proto.Unmarshal(request, req); decodeErr != nil {
				return decodeErr
			}
			res := new(workflowservice.UpdateWorkflowExecutionResponse)
			if err == nil {
				if decodeErr := proto.Unmarshal(response, res); decodeErr != nil {
					return decodeErr
				}
			}
			id := req.GetRequest().GetMeta().GetUpdateId()
			rejected := res.GetOutcome().GetFailure() != nil
			event := map[string]any{"kind": "update_rpc", "update_id": id, "rpc_code": status.Code(err).String(), "stage": res.GetStage().String(), "rejected": rejected, "fault_mode": faultMode, "request_workflow_id": req.GetWorkflowExecution().GetWorkflowId(), "request_run_id": req.GetWorkflowExecution().GetRunId(), "response_update_id": res.GetUpdateRef().GetUpdateId(), "response_workflow_id": res.GetUpdateRef().GetWorkflowExecution().GetWorkflowId(), "response_run_id": res.GetUpdateRef().GetWorkflowExecution().GetRunId()}
			matches := faultMatches(faultID, faultWorkflowID, faultRunID, id, req.GetWorkflowExecution().GetWorkflowId(), req.GetWorkflowExecution().GetRunId())
			drop := matches && ((faultMode == "accepted" && err == nil && !rejected) || (faultMode == "rejected" && err == nil && rejected))
			event["reply_dropped"] = drop
			f.j.add(event)
			if drop {
				return status.Error(codes.DeadlineExceeded, "fixture dropped real Update response")
			}
		}
		if isLookup {
			req := new(workflowservice.PollWorkflowExecutionUpdateRequest)
			if decodeErr := proto.Unmarshal(request, req); decodeErr != nil {
				return decodeErr
			}
			res := new(workflowservice.PollWorkflowExecutionUpdateResponse)
			if err == nil {
				if decodeErr := proto.Unmarshal(response, res); decodeErr != nil {
					return decodeErr
				}
			}
			f.j.add(map[string]any{"kind": "lookup_rpc", "update_id": req.GetUpdateRef().GetUpdateId(), "rpc_code": status.Code(err).String(), "stage": res.GetStage().String(), "rejected": res.GetOutcome().GetFailure() != nil, "response_update_id": res.GetUpdateRef().GetUpdateId(), "response_workflow_id": res.GetUpdateRef().GetWorkflowExecution().GetWorkflowId(), "response_run_id": res.GetUpdateRef().GetWorkflowExecution().GetRunId()})
		}
		if err != nil {
			return err
		}
		return stream.SendMsg(&response)
	}
}

// faultMatches selects one immutable execution before Caesium allocates its
// durable Update ID. Both coordinates are required, so a continuation or another
// workflow cannot consume a response-loss fault intended for this run.
func faultMatches(id, workflowID, runID, actualID, actualWorkflowID, actualRunID string) bool {
	if id != "" {
		return id == actualID
	}
	return workflowID != "" && runID != "" && workflowID == actualWorkflowID && runID == actualRunID
}

type fixtureConverter struct{ converter.DataConverter }

func (c fixtureConverter) ToPayload(value any) (*commonpb.Payload, error) {
	if _, ok := value.(opaqueHint); ok {
		return &commonpb.Payload{Metadata: map[string][]byte{"encoding": []byte("binary/fixture")}, Data: []byte("opaque-correlation-canary")}, nil
	}
	return c.DataConverter.ToPayload(value)
}
func (c fixtureConverter) ToPayloads(values ...any) (*commonpb.Payloads, error) {
	out := &commonpb.Payloads{}
	for _, v := range values {
		p, err := c.ToPayload(v)
		if err != nil {
			return nil, err
		}
		out.Payloads = append(out.Payloads, p)
	}
	return out, nil
}

func (f *fixture) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		running := f.w != nil
		f.mu.Unlock()
		if !running {
			http.Error(w, "worker missing", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		v, err := f.c.QueryWorkflow(ctx, "fixture-readiness", "", "external_status")
		if err == nil {
			var state map[string]string
			err = v.Get(&state)
			if state["stage"] != "external_wait" {
				err = errors.New("readiness query did not reach worker")
			}
		}
		if err != nil {
			http.Error(w, "real worker query failed", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, map[string]any{"ready": true, "sdk": f.versions.SDK, "cli": f.versions.CLI, "server": f.versions.Server})
	})
	mux.HandleFunc("POST /configure", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			RunnerKey string `json:"runner_key"`
		}
		if !decode(w, r, &in) {
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, f.a.server+"/auth/whoami", nil)
		if err != nil {
			http.Error(w, "runner identity request failed", http.StatusInternalServerError)
			return
		}
		req.Header.Set("Authorization", "Bearer "+in.RunnerKey)
		resp, err := f.a.http.Do(req)
		if err != nil {
			http.Error(w, "runner identity request failed", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		var principal struct {
			Kind    string `json:"kind"`
			Role    string `json:"role"`
			Subject string `json:"subject"`
		}
		if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&principal) != nil || principal.Kind != "api_key" || principal.Role != "runner" {
			http.Error(w, "fixture requires a real runner API-key principal", http.StatusBadRequest)
			return
		}
		f.a.keyMu.Lock()
		f.a.key = in.RunnerKey
		f.a.keyMu.Unlock()
		f.j.add(map[string]any{"kind": "runner_configured", "principal_kind": principal.Kind, "role": principal.Role})
		writeJSON(w, map[string]bool{"configured": true})
	})
	mux.HandleFunc("GET /evidence", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, f.j.snapshot()) })
	mux.HandleFunc("POST /worker", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Running bool `json:"running"`
		}
		if !decode(w, r, &in) {
			return
		}
		if in.Running {
			if err := f.startWorker(); err != nil {
				http.Error(w, "worker start failed", http.StatusInternalServerError)
				return
			}
		} else {
			f.stopWorker()
		}
		writeJSON(w, map[string]bool{"running": in.Running})
	})
	mux.HandleFunc("POST /fault", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			UpdateID   string `json:"update_id"`
			WorkflowID string `json:"workflow_id"`
			RunID      string `json:"run_id"`
			Mode       string `json:"mode"`
			Outage     bool   `json:"outage"`
		}
		if !decode(w, r, &in) {
			return
		}
		if in.Mode != "" && in.Mode != "accepted" && in.Mode != "rejected" {
			http.Error(w, "unknown fault", http.StatusBadRequest)
			return
		}
		if (in.WorkflowID == "") != (in.RunID == "") || (in.UpdateID != "" && in.WorkflowID != "") || (in.Mode != "" && in.UpdateID == "" && in.WorkflowID == "") {
			http.Error(w, "fault requires one pinned Update ID or workflow/run pair", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.faultID = in.UpdateID
		f.faultWorkflowID = in.WorkflowID
		f.faultRunID = in.RunID
		f.faultMode = in.Mode
		f.outage = in.Outage
		f.mu.Unlock()
		f.j.add(map[string]any{"kind": "fault_armed", "update_id": in.UpdateID, "workflow_id": in.WorkflowID, "run_id": in.RunID, "mode": in.Mode, "outage": in.Outage})
		writeJSON(w, in)
	})
	mux.HandleFunc("POST /start", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ID    string           `json:"id"`
			Type  string           `json:"type"`
			Input publicationInput `json:"input"`
		}
		if !decode(w, r, &in) {
			return
		}
		if in.ID == "" || (in.Type != "publication" && in.Type != "external_only") {
			http.Error(w, "invalid workflow", http.StatusBadRequest)
			return
		}
		if in.Type == "publication" && !in.Input.MessageOnly && (in.Input.FirstJob == "" || in.Input.SecondJob == "") {
			http.Error(w, "publication requires real job IDs", http.StatusBadRequest)
			return
		}
		var args []any
		if in.Type == "publication" {
			args = []any{in.Input}
		}
		run, err := f.c.ExecuteWorkflow(r.Context(), client.StartWorkflowOptions{ID: in.ID, TaskQueue: taskQueue}, in.Type, args...)
		if err != nil {
			http.Error(w, "workflow start failed", http.StatusInternalServerError)
			return
		}
		f.j.add(map[string]any{"kind": "workflow_started", "workflow_id": run.GetID(), "run_id": run.GetRunID(), "workflow_type": in.Type})
		writeJSON(w, map[string]string{"workflow_id": run.GetID(), "run_id": run.GetRunID()})
	})
	mux.HandleFunc("POST /signal", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ID    string `json:"id"`
			RunID string `json:"run_id"`
			Name  string `json:"name"`
			Value bool   `json:"value"`
		}
		if !decode(w, r, &in) {
			return
		}
		switch in.Name {
		case "fixture_finish", "fixture_continue", "fixture_eligibility":
		default:
			http.Error(w, "unknown signal", http.StatusBadRequest)
			return
		}
		if err := f.c.SignalWorkflow(r.Context(), in.ID, in.RunID, in.Name, in.Value); err != nil {
			http.Error(w, "signal failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]bool{"sent": true})
	})
	return mux
}
func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		http.Error(w, "invalid fixture input", http.StatusBadRequest)
		return false
	}
	return true
}
func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("fixture response: %v", err)
	}
}

func certificates(dir string) (*tls.Config, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Caesium fixture CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), caPEM, 0644); err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	var server tls.Certificate
	for i, name := range []string{"server", "tls"} {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, err
		}
		cert := &x509.Certificate{SerialNumber: big.NewInt(int64(i + 2)), Subject: pkix.Name{CommonName: name}, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
		// The distributed fixture supplies its owned Service DNS names. Plaintext
		// Temporal remains loopback-only; only this mutual-TLS proxy is exposed.
		for _, host := range strings.Split(os.Getenv("CAESIUM_CONNECTORS_TLS_DNS"), ",") {
			host = strings.TrimSpace(host)
			if host == "" {
				continue
			}
			if ip := net.ParseIP(host); ip != nil {
				cert.IPAddresses = append(cert.IPAddresses, ip)
			} else {
				cert.DNSNames = append(cert.DNSNames, host)
			}
		}
		der, err := x509.CreateCertificate(rand.Reader, cert, ca, &key.PublicKey, caKey)
		if err != nil {
			return nil, err
		}
		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
		if err := os.WriteFile(filepath.Join(dir, name+".crt"), certPEM, 0644); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, name+".key"), keyPEM, 0600); err != nil {
			return nil, err
		}
		if i == 0 {
			server, err = tls.X509KeyPair(certPEM, keyPEM)
			if err != nil {
				return nil, err
			}
		}
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{server}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}, nil
}
func main() {
	build, ok := debug.ReadBuildInfo()
	if !ok {
		log.Fatal("fixture SDK build identity is missing")
	}
	sdkVersion := ""
	for _, dependency := range build.Deps {
		if dependency.Path == "go.temporal.io/sdk" {
			sdkVersion = dependency.Version
		}
	}

	artifacts := os.Getenv("CAESIUM_CONNECTORS_ARTIFACTS")
	if artifacts == "" {
		artifacts = "/artifacts"
	}
	if err := os.MkdirAll(artifacts, 0755); err != nil {
		log.Fatal(err)
	}
	j, err := newJournal(filepath.Join(artifacts, "events.jsonl"))
	if err != nil {
		log.Fatal(err)
	}
	tlsConfig, err := certificates(filepath.Join(artifacts, "tls"))
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	var c client.Client
	for deadline := time.Now().Add(90 * time.Second); ; {
		c, err = client.DialContext(ctx, client.Options{HostPort: "127.0.0.1:7233", Namespace: "default", DataConverter: fixtureConverter{converter.GetDefaultDataConverter()}})
		if err == nil {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			log.Fatal("Temporal dev server did not become ready")
		}
		time.Sleep(time.Second)
	}
	defer c.Close()
	versionCtx, versionCancel := context.WithTimeout(ctx, 5*time.Second)
	system, err := c.WorkflowService().GetSystemInfo(versionCtx, &workflowservice.GetSystemInfoRequest{})
	versionCancel()
	if err != nil {
		log.Fatal("Temporal server version could not be observed")
	}
	cliCtx, cliCancel := context.WithTimeout(ctx, 5*time.Second)
	cliOutput, err := exec.CommandContext(cliCtx, "temporal", "--version").Output()
	cliCancel()
	if err != nil {
		log.Fatal("Temporal CLI version could not be observed")
	}
	versions, err := validateVersions(sdkVersion, string(cliOutput), system.GetServerVersion())
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifacts, "temporal-version.txt"), cliOutput, 0644); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifacts, "server-version.txt"), []byte(versions.Server+"\n"), 0644); err != nil {
		log.Fatal(err)
	}
	server := os.Getenv("CAESIUM_CONNECTORS_CAESIUM_URL")
	if server == "" {
		server = "http://127.0.0.1:8080"
	}
	f := &fixture{c: c, j: j, versions: versions, a: &activities{server: server, http: &http.Client{Timeout: 10 * time.Second}, journal: j}}
	if err := f.startWorker(); err != nil {
		log.Fatal(err)
	}
	defer f.stopWorker()
	if _, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: "fixture-readiness", TaskQueue: taskQueue}, "external_only"); err != nil {
		log.Fatal(err)
	}
	upstream, err := grpc.NewClient("127.0.0.1:7233", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}
	defer upstream.Close()
	proxyAddress := os.Getenv("CAESIUM_CONNECTORS_PROXY_LISTEN")
	if proxyAddress == "" {
		proxyAddress = "127.0.0.1:7234"
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", proxyAddress)
	if err != nil {
		log.Fatal(err)
	}
	proxy := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)), grpc.ForceServerCodec(rawCodec{}), grpc.UnknownServiceHandler(f.proxy(upstream)))
	go func() {
		if err := proxy.Serve(listener); err != nil {
			log.Printf("proxy: %v", err)
		}
	}()
	defer proxy.Stop()
	controlAddress := os.Getenv("CAESIUM_CONNECTORS_CONTROL_LISTEN")
	if controlAddress == "" {
		controlAddress = "127.0.0.1:7240"
	}
	httpServer := &http.Server{Addr: controlAddress, Handler: f.routes(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	j.add(map[string]any{"kind": "fixture_started", "sdk": versions.SDK, "cli": versions.CLI, "server": versions.Server, "candidate": os.Getenv("CAESIUM_CONNECTORS_CANDIDATE_SHA")})
	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = httpServer.Shutdown(shutdownCtx)
	fmt.Println("fixture stopped")
}
