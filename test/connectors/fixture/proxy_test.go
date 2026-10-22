//go:build integration

package main

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	commonpb "go.temporal.io/api/common/v1"
	querypb "go.temporal.io/api/query/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

func proxyTestClient(t *testing.T, f *fixture, upstream grpc.StreamHandler) *grpc.ClientConn {
	t.Helper()
	providerListener := bufconn.Listen(1024 * 1024)
	provider := grpc.NewServer(grpc.ForceServerCodec(rawCodec{}), grpc.UnknownServiceHandler(upstream))
	go func() { _ = provider.Serve(providerListener) }()
	dial := func(listener *bufconn.Listener) *grpc.ClientConn {
		conn, err := grpc.NewClient("passthrough:///fixture", grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
		require.NoError(t, err)
		return conn
	}
	providerConn := dial(providerListener)
	proxyListener := bufconn.Listen(1024 * 1024)
	proxy := grpc.NewServer(grpc.ForceServerCodec(rawCodec{}), grpc.UnknownServiceHandler(f.proxy(providerConn)))
	go func() { _ = proxy.Serve(proxyListener) }()
	conn := dial(proxyListener)
	t.Cleanup(func() {
		_ = conn.Close()
		proxy.Stop()
		_ = proxyListener.Close()
		_ = providerConn.Close()
		provider.Stop()
		_ = providerListener.Close()
	})
	return conn
}

func TestFixtureProxyRecordsSanitizedTerminalReadOutcome(t *testing.T) {
	const credential = "credential-must-not-be-journaled"
	const remoteError = "remote-error-must-not-be-journaled"
	const payload = "payload-must-not-be-journaled"
	methods := []struct {
		name     string
		request  proto.Message
		response proto.Message
	}{
		{"QueryWorkflow", &workflowservice.QueryWorkflowRequest{
			Namespace: "default", Execution: &commonpb.WorkflowExecution{WorkflowId: "workflow", RunId: "pinned-run"},
			Query: &querypb.WorkflowQuery{QueryType: payload, QueryArgs: &commonpb.Payloads{Payloads: []*commonpb.Payload{{Data: []byte(payload)}}}},
		}, &workflowservice.QueryWorkflowResponse{QueryResult: &commonpb.Payloads{Payloads: []*commonpb.Payload{{Data: []byte(payload)}}}}},
		{"DescribeWorkflowExecution", &workflowservice.DescribeWorkflowExecutionRequest{
			Namespace: "default", Execution: &commonpb.WorkflowExecution{WorkflowId: "workflow", RunId: "pinned-run"},
		}, &workflowservice.DescribeWorkflowExecutionResponse{WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Type: &commonpb.WorkflowType{Name: payload}}}},
	}
	for _, method := range methods {
		for _, code := range []codes.Code{codes.OK, codes.PermissionDenied, codes.DeadlineExceeded, codes.Canceled, codes.Unavailable} {
			t.Run(method.name+"/"+code.String(), func(t *testing.T) {
				journal, err := newJournal(filepath.Join(t.TempDir(), "events.jsonl"))
				require.NoError(t, err)
				f := &fixture{j: journal}
				request, err := proto.Marshal(method.request)
				require.NoError(t, err)
				response, err := proto.Marshal(method.response)
				require.NoError(t, err)
				forwarded := make(chan []byte, 1)
				conn := proxyTestClient(t, f, func(_ any, stream grpc.ServerStream) error {
					var actual []byte
					if err := stream.RecvMsg(&actual); err != nil {
						return err
					}
					forwarded <- actual
					if code != codes.OK {
						return status.Error(code, remoteError)
					}
					return stream.SendMsg(&response)
				})
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				ctx = metadata.AppendToOutgoingContext(ctx, "authorization", credential)
				fullMethod := "/temporal.api.workflowservice.v1.WorkflowService/" + method.name
				var actual []byte
				err = conn.Invoke(ctx, fullMethod, &request, &actual, grpc.ForceCodec(rawCodec{}))
				require.Equal(t, code, status.Code(err))
				require.Equal(t, request, <-forwarded, "diagnostics must not rewrite the forwarded request")
				if code == codes.OK {
					require.Equal(t, response, actual, "diagnostics must not rewrite the provider response")
				}
				events := journal.snapshot()
				require.Len(t, events, 2)
				require.Equal(t, "provider_rpc", events[0]["kind"])
				require.Equal(t, fullMethod, events[0]["method"])
				require.Len(t, events[0], 3, "arrival evidence retains only kind, method and timestamp")
				terminal := events[1]
				require.Equal(t, "read_rpc", terminal["kind"])
				require.Equal(t, fullMethod, terminal["method"])
				require.Equal(t, code.String(), terminal["rpc_code"])
				require.Equal(t, true, terminal["request_decoded"])
				require.Equal(t, "workflow", terminal["request_workflow_id"])
				require.Equal(t, "pinned-run", terminal["request_run_id"])
				require.Positive(t, terminal["request_deadline_remaining_ns"])
				require.LessOrEqual(t, terminal["deadline_remaining_ns"], terminal["request_deadline_remaining_ns"])
				if method.name == "QueryWorkflow" && code == codes.OK {
					require.Equal(t, true, terminal["response_decoded"])
					require.Equal(t, false, terminal["query_rejected"])
					require.Len(t, terminal, 11, "successful Query adds only two response classification booleans")
				} else {
					require.Len(t, terminal, 9, "terminal evidence has an explicit safe field allowlist")
				}
				encoded, err := json.Marshal(events)
				require.NoError(t, err)
				for _, forbidden := range []string{credential, remoteError, payload} {
					require.NotContains(t, string(encoded), forbidden)
				}
			})
		}
	}
}

func TestFixtureProxyReadDiagnosticsPreserveOutageAndMalformedRequest(t *testing.T) {
	for _, test := range []struct {
		name    string
		outage  bool
		request []byte
		code    codes.Code
	}{
		{name: "outage", outage: true, code: codes.Unavailable},
		{name: "malformed", request: []byte{0xff}, code: codes.InvalidArgument},
	} {
		t.Run(test.name, func(t *testing.T) {
			journal, err := newJournal(filepath.Join(t.TempDir(), "events.jsonl"))
			require.NoError(t, err)
			f := &fixture{j: journal, outage: test.outage}
			request := test.request
			if test.outage {
				request, err = proto.Marshal(&workflowservice.QueryWorkflowRequest{Execution: &commonpb.WorkflowExecution{WorkflowId: "workflow", RunId: "pinned-run"}})
				require.NoError(t, err)
			}
			forwarded := make(chan []byte, 1)
			conn := proxyTestClient(t, f, func(_ any, stream grpc.ServerStream) error {
				var actual []byte
				if err := stream.RecvMsg(&actual); err != nil {
					return err
				}
				forwarded <- actual
				return status.Error(codes.InvalidArgument, "remote malformed request detail")
			})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var response []byte
			err = conn.Invoke(ctx, "/temporal.api.workflowservice.v1.WorkflowService/QueryWorkflow", &request, &response, grpc.ForceCodec(rawCodec{}))
			require.Equal(t, test.code, status.Code(err))
			events := journal.snapshot()
			terminal := events[len(events)-1]
			require.Equal(t, "provider_rpc", events[0]["kind"])
			require.Equal(t, "read_rpc", terminal["kind"])
			require.Equal(t, test.code.String(), terminal["rpc_code"])
			if test.outage {
				require.Len(t, events, 3)
				require.Equal(t, "proxy_interruption", events[1]["kind"])
				require.Equal(t, "pinned-run", terminal["request_run_id"])
				select {
				case <-forwarded:
					t.Fatal("outage must still refuse before forwarding")
				default:
				}
			} else {
				require.Equal(t, request, <-forwarded)
				require.Equal(t, false, terminal["request_decoded"])
				require.NotContains(t, terminal, "request_workflow_id")
				require.NotContains(t, terminal, "request_run_id")
			}
		})
	}
}

func TestFixtureProxyQueryRejectionAndUndecodableResponses(t *testing.T) {
	rejected, err := proto.Marshal(&workflowservice.QueryWorkflowResponse{QueryRejected: &querypb.QueryRejected{}})
	require.NoError(t, err)
	for _, test := range []struct {
		name     string
		response []byte
		decoded  bool
	}{
		{"rejected", rejected, true},
		{"undecodable", []byte{0xff}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			journal, err := newJournal(filepath.Join(t.TempDir(), "events.jsonl"))
			require.NoError(t, err)
			f := &fixture{j: journal}
			conn := proxyTestClient(t, f, func(_ any, stream grpc.ServerStream) error {
				var request []byte
				if err := stream.RecvMsg(&request); err != nil {
					return err
				}
				return stream.SendMsg(&test.response)
			})
			request, err := proto.Marshal(&workflowservice.QueryWorkflowRequest{Execution: &commonpb.WorkflowExecution{WorkflowId: "workflow", RunId: "pinned-run"}})
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var actual []byte
			require.NoError(t, conn.Invoke(ctx, "/temporal.api.workflowservice.v1.WorkflowService/QueryWorkflow", &request, &actual, grpc.ForceCodec(rawCodec{})))
			require.Equal(t, test.response, actual, "diagnostic decoding must not change forwarding")
			terminal := journal.snapshot()[1]
			require.Equal(t, "OK", terminal["rpc_code"])
			require.Equal(t, test.decoded, terminal["response_decoded"])
			if test.decoded {
				require.Equal(t, true, terminal["query_rejected"])
			} else {
				require.NotContains(t, terminal, "query_rejected", "undecodable response is not evidence of non-rejection")
			}
		})
	}
}

func TestFixtureReadDiagnosticsIgnoreOtherRPCsAndAbsentDeadline(t *testing.T) {
	require.Nil(t, readRPCObservation(context.Background(), "/temporal.api.workflowservice.v1.WorkflowService/GetSystemInfo", nil))
	event := readRPCObservation(context.Background(), "/temporal.api.workflowservice.v1.WorkflowService/DescribeWorkflowExecution", nil)
	require.NotContains(t, event, "request_deadline_remaining_ns")
	journal, err := newJournal(filepath.Join(t.TempDir(), "events.jsonl"))
	require.NoError(t, err)
	f := &fixture{j: journal}
	f.recordReadRPC(context.Background(), event, nil, nil)
	require.NotContains(t, journal.snapshot()[0], "deadline_remaining_ns")
}
