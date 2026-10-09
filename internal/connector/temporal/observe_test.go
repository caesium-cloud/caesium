package temporal

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/connector"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	failurepb "go.temporal.io/api/failure/v1"
	historypb "go.temporal.io/api/history/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"google.golang.org/grpc"
)

const (
	apiKeySentinel  = "sentinel-secret-value"
	pemSentinel     = "FAKEPEM-BODY-not-a-certificate"
	payloadSentinel = "payload-sentinel-9f3c2a-do-not-copy"
)

func TestClientOptionsTransport(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.crt")
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")
	writeCert(t, caPath, "", true)
	writeCert(t, certPath, keyPath, false)

	plaintext := []string{"localhost:7233", "127.0.0.1:7233", "[::1]:7233"}
	for _, endpoint := range plaintext {
		opts, err := ClientOptions(DialConfig{
			Endpoint:  endpoint,
			Namespace: "default",
			APIKey:    apiKeySentinel,
		})
		if err != nil {
			t.Fatalf("%s: %v", endpoint, err)
		}
		if opts.HostPort != endpoint || opts.Namespace != "default" {
			t.Fatalf("%s host/namespace = %s %s", endpoint, opts.HostPort, opts.Namespace)
		}
		if opts.ConnectionOptions.TLS != nil || !opts.ConnectionOptions.TLSDisabled {
			t.Fatalf("%s: plaintext requires a nil TLS config and TLSDisabled", endpoint)
		}
		if opts.Credentials == nil {
			t.Fatalf("%s: API key credentials were not set", endpoint)
		}
		if !strings.Contains(reflect.TypeOf(opts.Credentials).String(), "apiKey") {
			t.Fatalf("%s: credentials type %s", endpoint, reflect.TypeOf(opts.Credentials))
		}
		if strings.Contains(fmt.Sprintf("%+v %#v", opts, opts), apiKeySentinel) {
			t.Fatalf("%s: API key appeared in options", endpoint)
		}
	}

	remote, err := ClientOptions(DialConfig{Endpoint: "example.temporal.io:7233", Namespace: "payments"})
	if err != nil {
		t.Fatal(err)
	}
	if remote.ConnectionOptions.TLS == nil || remote.ConnectionOptions.TLS.RootCAs != nil || remote.ConnectionOptions.TLSDisabled {
		t.Fatalf("remote without certificates = %#v", remote.ConnectionOptions.TLS)
	}
	if remote.Credentials != nil {
		t.Fatal("credentials set without an API key")
	}

	loopbackTLS, err := ClientOptions(DialConfig{
		Endpoint:         "localhost:7233",
		Namespace:        "default",
		CertificatePaths: []string{caPath},
	})
	if err != nil {
		t.Fatal(err)
	}
	if loopbackTLS.ConnectionOptions.TLS == nil || loopbackTLS.ConnectionOptions.TLS.RootCAs == nil || loopbackTLS.ConnectionOptions.TLSDisabled {
		t.Fatal("loopback with a certificate must use TLS")
	}

	mtls, err := ClientOptions(DialConfig{
		Endpoint:         "example.temporal.io:7233",
		Namespace:        "payments",
		APIKey:           apiKeySentinel,
		CertificatePaths: []string{caPath, certPath, keyPath},
	})
	if err != nil {
		t.Fatal(err)
	}
	if mtls.ConnectionOptions.TLS == nil || mtls.ConnectionOptions.TLS.RootCAs == nil || len(mtls.ConnectionOptions.TLS.Certificates) != 1 {
		t.Fatalf("mTLS config = %#v", mtls.ConnectionOptions.TLS)
	}

	clientCert := filepath.Join(dir, "nested", "client.crt")
	clientKey := filepath.Join(dir, "nested", "client.key")
	if err := os.MkdirAll(filepath.Dir(clientCert), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCert(t, clientCert, clientKey, false)
	alt, err := ClientOptions(DialConfig{
		Endpoint:         "127.0.0.2:7233",
		Namespace:        "default",
		CertificatePaths: []string{clientCert, clientKey},
	})
	if err != nil {
		t.Fatal(err)
	}
	if alt.ConnectionOptions.TLS == nil || len(alt.ConnectionOptions.TLS.Certificates) != 1 || alt.ConnectionOptions.TLS.RootCAs != nil {
		t.Fatal("client.crt/client.key on a non-loopback host must be TLS with system roots")
	}

	cases := []struct {
		name  string
		paths []string
	}{
		{name: "cert without key", paths: []string{certPath}},
		{name: "key without cert", paths: []string{keyPath}},
		{name: "unknown basename", paths: []string{filepath.Join(dir, "server.crt")}},
	}
	for _, tc := range cases {
		_, err := ClientOptions(DialConfig{
			Endpoint:         "example.temporal.io:7233",
			Namespace:        "payments",
			APIKey:           apiKeySentinel,
			CertificatePaths: tc.paths,
		})
		if err == nil {
			t.Fatalf("%s: expected error", tc.name)
		}
		mustNotLeak(t, err.Error(), apiKeySentinel)
	}
}

func TestClientOptionsRedactsKeyAndPEM(t *testing.T) {
	dir := t.TempDir()
	body := "-----BEGIN CERTIFICATE-----\n" + apiKeySentinel + "-" + pemSentinel + "\n-----END CERTIFICATE-----\n"
	certPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(certPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ClientOptions(DialConfig{
		Endpoint:         "example.temporal.io:7233",
		Namespace:        "payments",
		APIKey:           apiKeySentinel,
		CertificatePaths: []string{certPath},
	})
	if err == nil {
		t.Fatal("expected PEM rejection")
	}
	mustNotLeak(t, err.Error(), apiKeySentinel, pemSentinel, body)
	if !strings.Contains(err.Error(), "ca.pem") || strings.Contains(err.Error(), "BEGIN CERTIFICATE") {
		t.Fatalf("certificate error = %s", err)
	}

	keyDir := t.TempDir()
	clientCert := filepath.Join(keyDir, "tls.crt")
	clientKey := filepath.Join(keyDir, "tls.key")
	writeCert(t, clientCert, "", false)
	keyBody := "-----BEGIN PRIVATE KEY-----\n" + apiKeySentinel + "-" + pemSentinel + "\n-----END PRIVATE KEY-----\n"
	if err := os.WriteFile(clientKey, []byte(keyBody), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = ClientOptions(DialConfig{
		Endpoint:         "example.temporal.io:7233",
		Namespace:        "payments",
		APIKey:           apiKeySentinel,
		CertificatePaths: []string{clientCert, clientKey},
	})
	if err == nil {
		t.Fatal("expected key rejection")
	}
	mustNotLeak(t, err.Error(), apiKeySentinel, pemSentinel, keyBody)
	if !strings.Contains(err.Error(), "tls.crt") {
		t.Fatalf("key-pair error = %s", err)
	}
}

func TestListPagesAreDisjoint(t *testing.T) {
	const query = "WorkflowType = 'order' AND ExecutionStatus = 'Running'"
	token := []byte{0x00, 0xff, 'p', '2'}
	fake := &fakeService{
		pages: map[string]*workflowservice.ListWorkflowExecutionsResponse{
			"": {
				Executions: []*workflowpb.WorkflowExecutionInfo{
					executionInfo("alpha", "run-a", enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING),
					executionInfo("beta", "run-b", enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED),
				},
				NextPageToken: token,
			},
			string(token): {
				Executions: []*workflowpb.WorkflowExecutionInfo{
					executionInfo("gamma", "run-c", enumspb.WORKFLOW_EXECUTION_STATUS_FAILED),
				},
			},
		},
	}
	obs := observerFor(t, fake, apiKeySentinel, 0)
	first, err := obs.List(context.Background(), query, nil, 500)
	if err != nil {
		t.Fatal(err)
	}
	second, err := obs.List(context.Background(), query, first.NextPageToken, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.lists) != 2 {
		t.Fatalf("list calls = %d", len(fake.lists))
	}
	if fake.lists[0].GetQuery() != query || fake.lists[1].GetQuery() != query {
		t.Fatalf("query was rewritten: %#v", fake.lists)
	}
	if fake.lists[0].GetPageSize() != connector.MaxPageEntries || fake.lists[1].GetPageSize() != 3 {
		t.Fatalf("page sizes = %d %d", fake.lists[0].GetPageSize(), fake.lists[1].GetPageSize())
	}
	if fake.lists[0].GetNamespace() != "payments" || fake.lists[1].GetNamespace() != "payments" {
		t.Fatal("namespace was not the configured scope")
	}
	if !bytes.Equal(first.NextPageToken, token) || second.NextPageToken != nil {
		t.Fatalf("tokens = %q %q", first.NextPageToken, second.NextPageToken)
	}
	if !bytes.Equal(fake.lists[1].GetNextPageToken(), token) {
		t.Fatal("second request did not advance the token")
	}
	seen := map[string]struct{}{}
	for _, page := range []Page{first, second} {
		for _, execution := range page.Executions {
			id := execution.Coordinates["workflow_id"] + "/" + execution.Coordinates["run_id"]
			if _, ok := seen[id]; ok {
				t.Fatalf("duplicate id %s", id)
			}
			seen[id] = struct{}{}
			if _, ok := execution.Coordinates["namespace"]; ok {
				t.Fatal("namespace is not a coordinate")
			}
			if execution.Scope != "payments" {
				t.Fatalf("scope = %s", execution.Scope)
			}
			if !execution.ObservedAt.IsZero() {
				t.Fatal("list stored an observation time")
			}
		}
	}
	if len(seen) != 3 {
		t.Fatalf("ids = %#v", seen)
	}
	if first.Executions[0].DisplayStatus != "running" || first.Executions[1].DisplayStatus != "completed" || second.Executions[0].DisplayStatus != "failed" {
		t.Fatalf("display = %s %s %s", first.Executions[0].DisplayStatus, first.Executions[1].DisplayStatus, second.Executions[0].DisplayStatus)
	}
	if first.Executions[0].NativeStatus != "WORKFLOW_EXECUTION_STATUS_RUNNING" || second.Executions[0].Failure != true {
		t.Fatalf("native/failure = %s %v", first.Executions[0].NativeStatus, second.Executions[0].Failure)
	}

	zero, err := obs.List(context.Background(), query, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if fake.lists[len(fake.lists)-1].GetPageSize() != connector.MaxPageEntries || zero.Executions[0].WorkflowID != "alpha" {
		t.Fatal("non-positive page size was not clamped to the ceiling")
	}

	fake.listErr = fmt.Errorf("visibility %s", apiKeySentinel)
	_, err = obs.List(context.Background(), query, nil, 1)
	if err == nil {
		t.Fatal("expected list error")
	}
	mustNotLeak(t, err.Error(), apiKeySentinel)
}

func TestDescribeParentDeadlineAndContinued(t *testing.T) {
	observed := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	fake := &fakeService{
		describe: &workflowservice.DescribeWorkflowExecutionResponse{
			WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{
				Execution: &commonpb.WorkflowExecution{WorkflowId: "order", RunId: "run-1"},
				Status:    enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING,
				ParentExecution: &commonpb.WorkflowExecution{
					WorkflowId: "parent-wf",
					RunId:      "parent-run",
				},
				RootExecution: &commonpb.WorkflowExecution{WorkflowId: "root-wf", RunId: "root-run"},
				Memo: &commonpb.Memo{Fields: map[string]*commonpb.Payload{
					"note": {Data: []byte(payloadSentinel)},
				}},
			},
		},
	}
	obs := observerFor(t, fake, apiKeySentinel, 0)
	obs.now = func() time.Time { return observed }
	got, err := obs.Describe(context.Background(), "order", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.ObservedAt.Equal(observed) {
		t.Fatalf("observed = %s", got.ObservedAt)
	}
	if got.Scope != "payments" || got.Coordinates["workflow_id"] != "order" || got.Coordinates["run_id"] != "run-1" {
		t.Fatalf("identity = %#v", got)
	}
	if got.DisplayStatus != "running" || got.Terminal || got.Failure {
		t.Fatalf("running status = %s terminal=%v failure=%v", got.DisplayStatus, got.Terminal, got.Failure)
	}
	if got.Parent == nil || got.Parent.Type != RelationParent || got.Parent.WorkflowID != "parent-wf" || got.Parent.RunID != "parent-run" {
		t.Fatalf("parent = %#v", got.Parent)
	}
	if got.Parent.Type == RelationChild || got.Parent.Type == "delegation" || got.Parent.WorkflowID == "root-wf" {
		t.Fatal("parent was recorded as a child, delegation, or root")
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	mustNotLeak(t, string(encoded), payloadSentinel)

	fake.describe.WorkflowExecutionInfo.Status = enumspb.WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW
	fake.describe.WorkflowExecutionInfo.ParentExecution = nil
	continued, err := obs.Describe(context.Background(), "order", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if continued.DisplayStatus != "continued" || !continued.Terminal || continued.Failure || continued.Parent != nil {
		t.Fatalf("continued = %#v", continued)
	}
	if continued.NativeStatus != "WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW" {
		t.Fatalf("native = %s", continued.NativeStatus)
	}

	blocking := &fakeService{block: true, apiKey: apiKeySentinel}
	short := observerFor(t, blocking, apiKeySentinel, 30*time.Millisecond)
	started := time.Now()
	_, err = short.Describe(context.Background(), "order", "run-1")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline error = %v", err)
	}
	mustNotLeak(t, err.Error(), apiKeySentinel)
	if time.Since(started) > time.Second {
		t.Fatal("configured deadline was not applied")
	}

	parent, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	long := observerFor(t, blocking, apiKeySentinel, connector.MaxRPCDeadline)
	started = time.Now()
	_, err = long.Describe(parent, "order", "run-1")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("caller deadline error = %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("shorter caller deadline was replaced")
	}
	mustNotLeak(t, err.Error(), apiKeySentinel)

	_, err = NewObserver(fake, "payments", apiKeySentinel, connector.MaxRPCDeadline+time.Nanosecond, 0)
	if err == nil {
		t.Fatal("expected deadline ceiling rejection")
	}
	mustNotLeak(t, err.Error(), apiKeySentinel)
}

func TestHistoryMetadataOmitsPayloads(t *testing.T) {
	payload := &commonpb.Payloads{Payloads: []*commonpb.Payload{{Data: []byte(payloadSentinel)}}}
	events := []*historypb.HistoryEvent{
		{
			EventId:   1,
			EventType: enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_STARTED,
			Attributes: &historypb.HistoryEvent_WorkflowExecutionStartedEventAttributes{
				WorkflowExecutionStartedEventAttributes: &historypb.WorkflowExecutionStartedEventAttributes{Input: payload},
			},
		},
		{
			EventId:   10,
			EventType: enumspb.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED,
			Attributes: &historypb.HistoryEvent_ActivityTaskScheduledEventAttributes{
				ActivityTaskScheduledEventAttributes: &historypb.ActivityTaskScheduledEventAttributes{
					ActivityId:   "act-1",
					ActivityType: &commonpb.ActivityType{Name: "Charge"},
					Input:        payload,
				},
			},
		},
		{
			EventId:   11,
			EventType: enumspb.EVENT_TYPE_ACTIVITY_TASK_STARTED,
			Attributes: &historypb.HistoryEvent_ActivityTaskStartedEventAttributes{
				ActivityTaskStartedEventAttributes: &historypb.ActivityTaskStartedEventAttributes{
					ScheduledEventId: 10,
					Attempt:          2,
					LastFailure:      &failurepb.Failure{Message: payloadSentinel},
				},
			},
		},
		{
			EventId:   12,
			EventType: enumspb.EVENT_TYPE_WORKFLOW_TASK_FAILED,
			Attributes: &historypb.HistoryEvent_WorkflowTaskFailedEventAttributes{
				WorkflowTaskFailedEventAttributes: &historypb.WorkflowTaskFailedEventAttributes{
					Cause:   enumspb.WORKFLOW_TASK_FAILED_CAUSE_UNHANDLED_COMMAND,
					Failure: &failurepb.Failure{Message: payloadSentinel},
				},
			},
		},
		{
			EventId:   13,
			EventType: enumspb.EVENT_TYPE_WORKFLOW_TASK_TIMED_OUT,
			Attributes: &historypb.HistoryEvent_WorkflowTaskTimedOutEventAttributes{
				WorkflowTaskTimedOutEventAttributes: &historypb.WorkflowTaskTimedOutEventAttributes{
					TimeoutType: enumspb.TIMEOUT_TYPE_START_TO_CLOSE,
				},
			},
		},
		{
			EventId:   14,
			EventType: enumspb.EVENT_TYPE_CHILD_WORKFLOW_EXECUTION_STARTED,
			Attributes: &historypb.HistoryEvent_ChildWorkflowExecutionStartedEventAttributes{
				ChildWorkflowExecutionStartedEventAttributes: &historypb.ChildWorkflowExecutionStartedEventAttributes{
					WorkflowExecution: &commonpb.WorkflowExecution{WorkflowId: "child-wf", RunId: "child-run"},
				},
			},
		},
		{
			EventId:   15,
			EventType: enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_CONTINUED_AS_NEW,
			Attributes: &historypb.HistoryEvent_WorkflowExecutionContinuedAsNewEventAttributes{
				WorkflowExecutionContinuedAsNewEventAttributes: &historypb.WorkflowExecutionContinuedAsNewEventAttributes{
					NewExecutionRunId: "next-run",
					Input:             payload,
				},
			},
		},
		{
			EventId:   16,
			EventType: enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_COMPLETED,
			Attributes: &historypb.HistoryEvent_WorkflowExecutionCompletedEventAttributes{
				WorkflowExecutionCompletedEventAttributes: &historypb.WorkflowExecutionCompletedEventAttributes{Result: payload},
			},
		},
	}
	token := []byte("hist-2")
	fake := &fakeService{
		describe: &workflowservice.DescribeWorkflowExecutionResponse{
			WorkflowExecutionInfo: executionInfo("order", "run-1", enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING),
		},
		history: &workflowservice.GetWorkflowExecutionHistoryResponse{
			History:       &historypb.History{Events: events},
			NextPageToken: token,
			RawHistory:    []*commonpb.DataBlob{{Data: []byte(payloadSentinel)}},
		},
	}
	obs := observerFor(t, fake, apiKeySentinel, 0)
	described, err := obs.Describe(context.Background(), "order", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	describes := len(fake.describes)
	page, err := obs.History(context.Background(), "order", "run-1", nil, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if described.DisplayStatus != "running" || described.Failure {
		t.Fatalf("workflow-task problems changed describe: %#v", described)
	}
	if fake.histories[0].GetMaximumPageSize() != connector.MaxPageEntries {
		t.Fatalf("history page size = %d", fake.histories[0].GetMaximumPageSize())
	}
	if len(fake.describes) != describes {
		t.Fatal("history described a caller-supplied run id")
	}
	if fake.histories[0].GetWaitNewEvent() {
		t.Fatal("history followed new events")
	}
	if len(page.Activities) != 1 {
		t.Fatalf("activities = %#v", page.Activities)
	}
	activity := page.Activities[0]
	if activity.EventID != 11 || activity.ScheduledEventID != 10 || activity.ActivityID != "act-1" || activity.ActivityType != "Charge" || activity.Attempt != 2 {
		t.Fatalf("activity = %#v", activity)
	}
	if len(page.Problems) != 2 || page.Problems[0].EventID != 12 || page.Problems[1].EventID != 13 {
		t.Fatalf("problems = %#v", page.Problems)
	}
	if page.Problems[0].Cause != "WORKFLOW_TASK_FAILED_CAUSE_UNHANDLED_COMMAND" || page.Problems[1].Cause != "TIMEOUT_TYPE_START_TO_CLOSE" {
		t.Fatalf("causes = %#v", page.Problems)
	}
	var child, continuation *Relation
	for i := range page.Relations {
		rel := &page.Relations[i]
		if rel.Type == "delegation" {
			t.Fatalf("delegation relation %#v", rel)
		}
		switch rel.Type {
		case RelationChild:
			child = rel
		case RelationContinuation:
			continuation = rel
		default:
			t.Fatalf("unexpected relation %#v", rel)
		}
	}
	if child == nil || child.WorkflowID != "child-wf" || child.RunID != "child-run" || child.EventID != 14 {
		t.Fatalf("child = %#v", child)
	}
	if continuation == nil || continuation.RunID != "next-run" || continuation.WorkflowID != "order" || continuation.EventID != 15 {
		t.Fatalf("continuation = %#v", continuation)
	}
	if !bytes.Equal(page.NextPageToken, token) {
		t.Fatalf("history token = %q", page.NextPageToken)
	}
	encoded, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	mustNotLeak(t, string(encoded), payloadSentinel)
	describedEncoded, err := json.Marshal(described)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(describedEncoded), `"display_status":"failed"`) {
		t.Fatalf("describe became failed: %s", describedEncoded)
	}
}

func TestStatusMap(t *testing.T) {
	cases := []struct {
		status   enumspb.WorkflowExecutionStatus
		native   string
		display  string
		terminal bool
		failure  bool
	}{
		{enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "WORKFLOW_EXECUTION_STATUS_RUNNING", "running", false, false},
		{enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, "WORKFLOW_EXECUTION_STATUS_COMPLETED", "completed", true, false},
		{enumspb.WORKFLOW_EXECUTION_STATUS_FAILED, "WORKFLOW_EXECUTION_STATUS_FAILED", "failed", true, true},
		{enumspb.WORKFLOW_EXECUTION_STATUS_CANCELED, "WORKFLOW_EXECUTION_STATUS_CANCELED", "canceled", true, false},
		{enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED, "WORKFLOW_EXECUTION_STATUS_TERMINATED", "terminated", true, false},
		{enumspb.WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW, "WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW", "continued", true, false},
		{enumspb.WORKFLOW_EXECUTION_STATUS_TIMED_OUT, "WORKFLOW_EXECUTION_STATUS_TIMED_OUT", "timed_out", true, false},
		{enumspb.WORKFLOW_EXECUTION_STATUS_UNSPECIFIED, "WORKFLOW_EXECUTION_STATUS_UNSPECIFIED", "unknown", false, false},
		{enumspb.WORKFLOW_EXECUTION_STATUS_PAUSED, "WORKFLOW_EXECUTION_STATUS_PAUSED", "unknown", false, false},
		{enumspb.WorkflowExecutionStatus(99), "99", "unknown", false, false},
	}
	fake := &fakeService{}
	obs := observerFor(t, fake, "", 0)
	for _, tc := range cases {
		fake.describe = &workflowservice.DescribeWorkflowExecutionResponse{
			WorkflowExecutionInfo: executionInfo("wf", "run", tc.status),
		}
		got, err := obs.Describe(context.Background(), "wf", "run")
		if err != nil {
			t.Fatal(err)
		}
		if got.NativeStatus != tc.native || got.DisplayStatus != tc.display || got.Terminal != tc.terminal || got.Failure != tc.failure {
			t.Fatalf("%s: got native=%s display=%s terminal=%v failure=%v", tc.native, got.NativeStatus, got.DisplayStatus, got.Terminal, got.Failure)
		}
	}
}

func TestHealthIsAvailability(t *testing.T) {
	fake := &fakeService{healthOK: true}
	obs := observerFor(t, fake, apiKeySentinel, 0)
	if err := obs.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.systemInfos != 1 || len(fake.namespaces) != 1 || fake.namespaces[0] != "payments" {
		t.Fatalf("health calls = system %d namespaces %#v", fake.systemInfos, fake.namespaces)
	}
	fake.healthOK = false
	fake.healthErr = fmt.Errorf("system info %s: backend unavailable", apiKeySentinel)
	err := obs.Health(context.Background())
	if err == nil {
		t.Fatal("expected health error")
	}
	mustNotLeak(t, err.Error(), apiKeySentinel)
	if strings.Contains(err.Error(), "WORKFLOW_EXECUTION_STATUS_FAILED") || strings.Contains(err.Error(), "display_status") {
		t.Fatalf("health mapped to a workflow failure: %s", err.Error())
	}
	if !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("health error = %s", err.Error())
	}
	if fake.systemInfos != 2 || len(fake.namespaces) != 1 {
		t.Fatal("namespace was checked after GetSystemInfo failed")
	}

	fake.healthOK = true
	fake.healthErr = nil
	fake.namespaceErr = fmt.Errorf("namespace %s is not found", apiKeySentinel)
	err = obs.Health(context.Background())
	if err == nil {
		t.Fatal("expected namespace health error")
	}
	mustNotLeak(t, err.Error(), apiKeySentinel)
	if strings.Contains(err.Error(), "WORKFLOW_EXECUTION_STATUS_FAILED") || strings.Contains(err.Error(), "display_status") {
		t.Fatalf("namespace health mapped to a workflow failure: %s", err.Error())
	}
	if !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("namespace health error = %s", err.Error())
	}
	if fake.systemInfos != 3 || len(fake.namespaces) != 2 || fake.namespaces[1] != "payments" {
		t.Fatalf("namespace check = system %d %#v", fake.systemInfos, fake.namespaces)
	}
}

func TestObservedAtOmitsZeroTime(t *testing.T) {
	zeroExec, err := json.Marshal(Execution{WorkflowID: "wf", RunID: "run"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(zeroExec), "observed_at") {
		t.Fatalf("zero execution time was encoded: %s", zeroExec)
	}
	zeroPage, err := json.Marshal(HistoryPage{WorkflowID: "wf"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(zeroPage), "observed_at") {
		t.Fatalf("zero history time was encoded: %s", zeroPage)
	}
	stamp := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	presentExec, err := json.Marshal(Execution{ObservedAt: stamp})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(presentExec), `"observed_at"`) {
		t.Fatalf("execution time missing: %s", presentExec)
	}
	presentPage, err := json.Marshal(HistoryPage{ObservedAt: stamp})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(presentPage), `"observed_at"`) {
		t.Fatalf("history time missing: %s", presentPage)
	}
}

func TestPageSizeClampsToObserverMaximum(t *testing.T) {
	fake := &fakeService{
		pages: map[string]*workflowservice.ListWorkflowExecutionsResponse{
			"": {Executions: []*workflowpb.WorkflowExecutionInfo{
				executionInfo("alpha", "run-a", enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING),
			}},
		},
		history: &workflowservice.GetWorkflowExecutionHistoryResponse{History: &historypb.History{}},
	}
	obs, err := NewObserver(fake, "payments", apiKeySentinel, 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := obs.List(context.Background(), "WorkflowType = 'order'", nil, 50); err != nil {
		t.Fatal(err)
	}
	if _, err := obs.List(context.Background(), "WorkflowType = 'order'", nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := obs.List(context.Background(), "WorkflowType = 'order'", nil, 2); err != nil {
		t.Fatal(err)
	}
	if fake.lists[0].GetPageSize() != 4 || fake.lists[1].GetPageSize() != 4 || fake.lists[2].GetPageSize() != 2 {
		t.Fatalf("list sizes = %d %d %d", fake.lists[0].GetPageSize(), fake.lists[1].GetPageSize(), fake.lists[2].GetPageSize())
	}
	if _, err := obs.History(context.Background(), "alpha", "run-a", nil, 80); err != nil {
		t.Fatal(err)
	}
	if _, err := obs.History(context.Background(), "alpha", "run-a", nil, 1); err != nil {
		t.Fatal(err)
	}
	if fake.histories[0].GetMaximumPageSize() != 4 || fake.histories[1].GetMaximumPageSize() != 1 {
		t.Fatalf("history sizes = %d %d", fake.histories[0].GetMaximumPageSize(), fake.histories[1].GetMaximumPageSize())
	}
	if len(fake.describes) != 0 {
		t.Fatal("history described a caller-supplied run id")
	}
	if _, err := NewObserver(fake, "payments", apiKeySentinel, 0, connector.MaxPageEntries+1); err == nil {
		t.Fatal("expected page ceiling rejection")
	} else {
		mustNotLeak(t, err.Error(), apiKeySentinel)
	}
	if _, err := NewObserver(fake, "payments", apiKeySentinel, 0, -1); err == nil {
		t.Fatal("expected negative page rejection")
	} else {
		mustNotLeak(t, err.Error(), apiKeySentinel)
	}
}

func TestActivityIdentityStaysOnThePage(t *testing.T) {
	startedOnly := &workflowservice.GetWorkflowExecutionHistoryResponse{
		History: &historypb.History{Events: []*historypb.HistoryEvent{{
			EventId:   21,
			EventType: enumspb.EVENT_TYPE_ACTIVITY_TASK_STARTED,
			Attributes: &historypb.HistoryEvent_ActivityTaskStartedEventAttributes{
				ActivityTaskStartedEventAttributes: &historypb.ActivityTaskStartedEventAttributes{
					ScheduledEventId: 5,
					Attempt:          1,
				},
			},
		}}},
	}
	fake := &fakeService{history: startedOnly}
	obs := observerFor(t, fake, "", 0)
	page, err := obs.History(context.Background(), "order", "run-1", []byte("page-2"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Activities) != 1 {
		t.Fatalf("activities = %#v", page.Activities)
	}
	activity := page.Activities[0]
	if activity.ScheduledEventID != 5 || activity.ActivityID != "" || activity.ActivityType != "" || activity.Attempt != 1 {
		t.Fatalf("activity = %#v", activity)
	}

	fake.history = &workflowservice.GetWorkflowExecutionHistoryResponse{
		History: &historypb.History{Events: []*historypb.HistoryEvent{
			{
				EventId:   5,
				EventType: enumspb.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED,
				Attributes: &historypb.HistoryEvent_ActivityTaskScheduledEventAttributes{
					ActivityTaskScheduledEventAttributes: &historypb.ActivityTaskScheduledEventAttributes{
						ActivityId:   "act-from-other-page",
						ActivityType: &commonpb.ActivityType{Name: "Remembered"},
					},
				},
			},
		}},
	}
	if _, err := obs.History(context.Background(), "order", "run-1", nil, 10); err != nil {
		t.Fatal(err)
	}
	fake.history = startedOnly
	again, err := obs.History(context.Background(), "order", "run-1", []byte("page-2"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if again.Activities[0].ActivityID != "" || again.Activities[0].ActivityType != "" || again.Activities[0].ScheduledEventID != 5 {
		t.Fatalf("identity leaked across pages: %#v", again.Activities[0])
	}
}

func TestParentMatchesListAndHistory(t *testing.T) {
	info := executionInfo("order", "run-2", enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING)
	info.ParentExecution = &commonpb.WorkflowExecution{WorkflowId: "parent-wf", RunId: "parent-run"}
	fake := &fakeService{
		pages: map[string]*workflowservice.ListWorkflowExecutionsResponse{
			"": {Executions: []*workflowpb.WorkflowExecutionInfo{info}},
		},
		describe: &workflowservice.DescribeWorkflowExecutionResponse{WorkflowExecutionInfo: info},
		history: &workflowservice.GetWorkflowExecutionHistoryResponse{
			History: &historypb.History{Events: []*historypb.HistoryEvent{
				{
					EventId:   1,
					EventType: enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_STARTED,
					Attributes: &historypb.HistoryEvent_WorkflowExecutionStartedEventAttributes{
						WorkflowExecutionStartedEventAttributes: &historypb.WorkflowExecutionStartedEventAttributes{
							ParentWorkflowExecution: &commonpb.WorkflowExecution{WorkflowId: "parent-wf", RunId: "parent-run"},
							ContinuedExecutionRunId: "run-1",
							OriginalExecutionRunId:  "run-2",
							FirstExecutionRunId:     "run-0",
						},
					},
				},
				{
					EventId:   2,
					EventType: enumspb.EVENT_TYPE_CHILD_WORKFLOW_EXECUTION_STARTED,
					Attributes: &historypb.HistoryEvent_ChildWorkflowExecutionStartedEventAttributes{
						ChildWorkflowExecutionStartedEventAttributes: &historypb.ChildWorkflowExecutionStartedEventAttributes{
							WorkflowExecution: &commonpb.WorkflowExecution{WorkflowId: "child-wf", RunId: "child-run"},
						},
					},
				},
			}},
		},
	}
	obs := observerFor(t, fake, "", 0)
	page, err := obs.History(context.Background(), "order", "", nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.describes) != 1 || fake.describes[0].workflowID != "order" || fake.describes[0].runID != "" {
		t.Fatalf("history describe = %#v", fake.describes)
	}
	if page.RunID != "run-2" {
		t.Fatalf("run id = %s", page.RunID)
	}
	listed, err := obs.List(context.Background(), "", nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	described, err := obs.Describe(context.Background(), "order", "run-2")
	if err != nil {
		t.Fatal(err)
	}
	parent := relationByType(page.Relations, RelationParent)
	if parent == nil || listed.Executions[0].Parent == nil || described.Parent == nil {
		t.Fatalf("parents list=%#v describe=%#v history=%#v", listed.Executions[0].Parent, described.Parent, parent)
	}
	if parent.Type != RelationParent || parent.WorkflowID != "parent-wf" || parent.RunID != "parent-run" || parent.EventID != 1 {
		t.Fatalf("history parent = %#v", parent)
	}
	if listed.Executions[0].Parent.Type != parent.Type || listed.Executions[0].Parent.WorkflowID != parent.WorkflowID || listed.Executions[0].Parent.RunID != parent.RunID {
		t.Fatalf("list parent = %#v history = %#v", listed.Executions[0].Parent, parent)
	}
	if described.Parent.Type != parent.Type || described.Parent.WorkflowID != parent.WorkflowID || described.Parent.RunID != parent.RunID {
		t.Fatalf("describe parent = %#v", described.Parent)
	}
	continued := relationByType(page.Relations, RelationContinuation)
	if continued == nil || continued.WorkflowID != "order" || continued.RunID != "run-1" || continued.EventID != 1 {
		t.Fatalf("continuation = %#v", continued)
	}
	child := relationByType(page.Relations, RelationChild)
	if child == nil || child.WorkflowID != "child-wf" || child.RunID != "child-run" {
		t.Fatalf("child = %#v", child)
	}
	for _, rel := range page.Relations {
		if rel.Type == "delegation" || rel.RunID == "run-0" || rel.WorkflowID == "root-wf" {
			t.Fatalf("unexpected relation %#v", rel)
		}
	}
}

func TestHistoryRunIDFallsBackToDescribe(t *testing.T) {
	fake := &fakeService{
		describe: &workflowservice.DescribeWorkflowExecutionResponse{
			WorkflowExecutionInfo: executionInfo("order", "run-current", enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING),
		},
		history: &workflowservice.GetWorkflowExecutionHistoryResponse{
			History: &historypb.History{Events: []*historypb.HistoryEvent{{
				EventId:   1,
				EventType: enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_STARTED,
				Attributes: &historypb.HistoryEvent_WorkflowExecutionStartedEventAttributes{
					WorkflowExecutionStartedEventAttributes: &historypb.WorkflowExecutionStartedEventAttributes{
						ContinuedExecutionRunId: "run-previous",
						FirstExecutionRunId:     "run-first",
					},
				},
			}}},
		},
	}
	obs := observerFor(t, fake, "", 0)
	page, err := obs.History(context.Background(), "order", "", nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if page.RunID != "run-current" {
		t.Fatalf("run id = %s", page.RunID)
	}
	if len(fake.describes) != 1 || fake.describes[0].workflowID != "order" || fake.describes[0].runID != "" {
		t.Fatalf("describe calls = %#v", fake.describes)
	}
	continued := relationByType(page.Relations, RelationContinuation)
	if continued == nil || continued.RunID != "run-previous" || continued.WorkflowID != "order" {
		t.Fatalf("continuation = %#v", continued)
	}

	fake.describes = nil
	named, err := obs.History(context.Background(), "order", "run-explicit", nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if named.RunID != "run-explicit" || len(fake.describes) != 0 {
		t.Fatalf("named run = %s describes = %#v", named.RunID, fake.describes)
	}

	fake.history = &workflowservice.GetWorkflowExecutionHistoryResponse{
		History: &historypb.History{Events: []*historypb.HistoryEvent{{
			EventId:   40,
			EventType: enumspb.EVENT_TYPE_ACTIVITY_TASK_STARTED,
			Attributes: &historypb.HistoryEvent_ActivityTaskStartedEventAttributes{
				ActivityTaskStartedEventAttributes: &historypb.ActivityTaskStartedEventAttributes{ScheduledEventId: 9},
			},
		}}},
	}
	later, err := obs.History(context.Background(), "order", "", []byte("page-2"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if later.RunID != "run-current" || len(fake.describes) != 1 {
		t.Fatalf("later run = %s describes = %#v", later.RunID, fake.describes)
	}
}

func relationByType(relations []Relation, kind string) *Relation {
	for i := range relations {
		if relations[i].Type == kind {
			return &relations[i]
		}
	}
	return nil
}

type listCall struct {
	namespace string
	query     string
	pageSize  int32
	token     []byte
}

func (c listCall) GetQuery() string         { return c.query }
func (c listCall) GetPageSize() int32       { return c.pageSize }
func (c listCall) GetNamespace() string     { return c.namespace }
func (c listCall) GetNextPageToken() []byte { return c.token }

type historyCall struct {
	pageSize int32
	wait     bool
}

func (c historyCall) GetMaximumPageSize() int32 { return c.pageSize }
func (c historyCall) GetWaitNewEvent() bool     { return c.wait }

type fakeService struct {
	pages        map[string]*workflowservice.ListWorkflowExecutionsResponse
	lists        []listCall
	listErr      error
	describe     *workflowservice.DescribeWorkflowExecutionResponse
	describes    []describeCall
	history      *workflowservice.GetWorkflowExecutionHistoryResponse
	histories    []historyCall
	healthOK     bool
	healthErr    error
	namespaceErr error
	namespaces   []string
	systemInfos  int
	block        bool
	apiKey       string
}

type describeCall struct {
	workflowID string
	runID      string
}

func (f *fakeService) ListWorkflowExecutions(ctx context.Context, in *workflowservice.ListWorkflowExecutionsRequest, _ ...grpc.CallOption) (*workflowservice.ListWorkflowExecutionsResponse, error) {
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	f.lists = append(f.lists, listCall{
		namespace: in.GetNamespace(),
		query:     in.GetQuery(),
		pageSize:  in.GetPageSize(),
		token:     append([]byte(nil), in.GetNextPageToken()...),
	})
	if f.listErr != nil {
		err := f.listErr
		f.listErr = nil
		return nil, err
	}
	page := f.pages[string(in.GetNextPageToken())]
	return page, nil
}

func (f *fakeService) DescribeWorkflowExecution(ctx context.Context, in *workflowservice.DescribeWorkflowExecutionRequest, _ ...grpc.CallOption) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	call := describeCall{}
	if in != nil && in.GetExecution() != nil {
		call.workflowID = in.GetExecution().GetWorkflowId()
		call.runID = in.GetExecution().GetRunId()
	}
	f.describes = append(f.describes, call)
	return f.describe, nil
}

func (f *fakeService) GetWorkflowExecutionHistory(ctx context.Context, in *workflowservice.GetWorkflowExecutionHistoryRequest, _ ...grpc.CallOption) (*workflowservice.GetWorkflowExecutionHistoryResponse, error) {
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	f.histories = append(f.histories, historyCall{pageSize: in.GetMaximumPageSize(), wait: in.GetWaitNewEvent()})
	return f.history, nil
}

func (f *fakeService) GetSystemInfo(ctx context.Context, _ *workflowservice.GetSystemInfoRequest, _ ...grpc.CallOption) (*workflowservice.GetSystemInfoResponse, error) {
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	f.systemInfos++
	if f.healthErr != nil {
		return nil, f.healthErr
	}
	if !f.healthOK {
		return nil, errors.New("health not configured")
	}
	return &workflowservice.GetSystemInfoResponse{}, nil
}

func (f *fakeService) DescribeNamespace(ctx context.Context, in *workflowservice.DescribeNamespaceRequest, _ ...grpc.CallOption) (*workflowservice.DescribeNamespaceResponse, error) {
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	if in != nil {
		f.namespaces = append(f.namespaces, in.GetNamespace())
	}
	if f.namespaceErr != nil {
		return nil, f.namespaceErr
	}
	return &workflowservice.DescribeNamespaceResponse{}, nil
}

func (f *fakeService) wait(ctx context.Context) error {
	if !f.block {
		return nil
	}
	<-ctx.Done()
	return fmt.Errorf("upstream %s: %w", f.apiKey, ctx.Err())
}

func observerFor(t *testing.T, service Service, apiKey string, deadline time.Duration) *Observer {
	t.Helper()
	obs, err := NewObserver(service, "payments", apiKey, deadline, 0)
	if err != nil {
		t.Fatal(err)
	}
	return obs
}

func executionInfo(workflowID, runID string, status enumspb.WorkflowExecutionStatus) *workflowpb.WorkflowExecutionInfo {
	return &workflowpb.WorkflowExecutionInfo{
		Execution: &commonpb.WorkflowExecution{WorkflowId: workflowID, RunId: runID},
		Status:    status,
	}
}

func mustNotLeak(t *testing.T, message string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if secret != "" && strings.Contains(message, secret) {
			t.Fatalf("secret leaked into %q", message)
		}
	}
}

func writeCert(t *testing.T, certPath, keyPath string, isCA bool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: filepath.Base(certPath)},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  isCA,
	}
	if isCA {
		tmpl.KeyUsage |= x509.KeyUsageCertSign
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if keyPath == "" {
		return
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
}
