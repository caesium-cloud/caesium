//go:build integration

package robustness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/test/robustness/cluster"
	"github.com/caesium-cloud/caesium/test/robustness/faults"
	"github.com/caesium-cloud/caesium/test/robustness/history"
	"github.com/caesium-cloud/caesium/test/robustness/recorder"
	"github.com/google/uuid"
	"k8s.io/client-go/kubernetes"
)

const (
	firstStep  = "first"
	secondStep = "second"

	// leaseTTLSeconds mirrors CAESIUM_RUN_LEASE_TTL in the robustness values
	// file. It is only used to report whether a freeze outlasted a lease; the
	// resulting takeover behaviour is B3's scenario, not an assertion here.
	leaseTTLSeconds = 30.0
)

// completionEventTypes are the event types that assert a task finished. They
// are compared against the raw effect ledger, never substituted for it.
var completionEventTypes = []string{"task_succeeded", "task_failed"}

type faultEnv struct {
	env     cluster.Env
	kube    *kubernetes.Clientset
	sink    *recorder.Sink
	httpAPI *cluster.HTTP
	topo    cluster.Topology
	leader  cluster.Member
	host    faults.HostController
	testdir string
}

// TestTargetedFaults is B2's capability suite: each subtest proves ONE fault
// control with independent activation and heal evidence, or fails. It is not
// the failure suite — B3 composes these controls into the fenced recovery,
// dispatch and authorization scenarios.
func TestTargetedFaults(t *testing.T) {
	ctx := context.Background()
	env, err := cluster.LoadEnv()
	if err != nil {
		t.Fatalf("environment: %v", err)
	}
	kube, err := cluster.InClusterClient()
	if err != nil {
		t.Fatalf("kube client: %v", err)
	}
	sink := recorder.New()
	if err := sink.Start(); err != nil {
		t.Fatalf("recorder listen %s: %v", recorder.ListenAddr, err)
	}
	t.Cleanup(func() { _ = sink.Close(context.Background()) })
	if err := cluster.EnsureRecorderService(ctx, kube, env.Namespace); err != nil {
		t.Fatalf("recorder service: %v", err)
	}

	httpAPI := cluster.NewHTTP(env.ManualKey)
	topo := mustReadyTopology(t, ctx, kube, env)
	for _, m := range topo.Members {
		if err := httpAPI.Health(ctx, m.HTTPBase()); err != nil {
			t.Fatalf("pod %s /health: %v", m.Name, err)
		}
	}
	memberCtx, memberCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer memberCancel()
	membership, err := cluster.WaitMembership(memberCtx, dqliteAddresses(topo))
	if err != nil {
		t.Fatalf("dqlite membership: %v", err)
	}
	leader, ok := topo.ByIP(cluster.HostIP(membership.Leader.Address))
	if !ok {
		t.Fatalf("dqlite leader %s is not a caesium pod", membership.Leader.Address)
	}
	t.Logf("dqlite leader=%s members=%d", membership.Leader.Address, len(membership.Members))

	// The same task/sink connectivity precondition B1 requires: without it a
	// missing effect record is ambiguous rather than evidence.
	probeID := "probe-" + uuid.NewString()
	probeAlias := "fault-probe-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	if err := httpAPI.Apply(ctx, leader.HTTPBase(), []jobdef.Definition{cluster.ProbeDefinition(probeAlias, probeID, env.TaskImage)}); err != nil {
		t.Fatalf("apply probe job: %v", err)
	}
	probeJob, err := httpAPI.JobByAlias(ctx, leader.HTTPBase(), probeAlias)
	if err != nil {
		t.Fatalf("read probe job: %v", err)
	}
	if _, _, err := httpAPI.TriggerRun(ctx, leader.HTTPBase(), probeJob.ID); err != nil {
		t.Fatalf("trigger probe job: %v", err)
	}
	probeCtx, probeCancel := context.WithTimeout(ctx, 3*time.Minute)
	defer probeCancel()
	if err := cluster.WaitProbe(probeCtx, sink, probeID); err != nil {
		t.Fatalf("task/sink connectivity probe missing (inconclusive): %v", err)
	}
	t.Logf("connectivity probe %s observed", probeID)

	fe := &faultEnv{
		env:     env,
		kube:    kube,
		sink:    sink,
		httpAPI: httpAPI,
		topo:    topo,
		leader:  leader,
		host:    faults.HostController{Kube: kube, Namespace: env.Namespace},
		testdir: firstNonEmptyStr(os.Getenv("CAESIUM_ROBUSTNESS_TESTFAULT_DIR"), "/tmp/caesium-testfault"),
	}

	// Order matters in one place only: asymmetric_partition deliberately leaves
	// long-running dispatch work in flight (that is how it forces traffic
	// across the cut), and bus_publish_pause holds EVERY task_started
	// publication while it is armed. Running the partition last keeps the
	// publication pause from queueing behind another run's held event.
	t.Run("event_history_correlation", func(t *testing.T) { runEventHistory(t, fe) })
	t.Run("response_loss_possibly_committed", func(t *testing.T) { runResponseLoss(t, fe) })
	t.Run("external_pause_resume", func(t *testing.T) { runPauseResume(t, fe) })
	t.Run("bus_publish_pause", func(t *testing.T) { runBusPublishPause(t, fe) })
	t.Run("asymmetric_partition", func(t *testing.T) { runAsymmetricPartition(t, fe) })

	doneCtx, doneCancel := context.WithTimeout(ctx, 30*time.Second)
	defer doneCancel()
	if _, err := cluster.RequestHost(doneCtx, kube, env.Namespace, cluster.HostRequest{
		RequestID: uuid.NewString(),
		Action:    cluster.ActionDone,
	}); err != nil {
		t.Logf("host done: %v", err)
	}
	if err := cluster.WriteRecords(ctx, kube, env.Namespace, "fault_events", sink.Events()); err != nil {
		t.Fatalf("inconclusive: persist recorder events: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Public event history vs persisted rows vs the raw effect ledger
// ---------------------------------------------------------------------------

func runEventHistory(t *testing.T, fe *faultEnv) {
	ctx := context.Background()
	member := fe.leader
	job, alias := applyFaultFixture(t, fe, member, "history", 2)

	// Subscribe from the store's current tip so the subscription does not
	// replay the whole retained store. The cursor is a resume point, never a
	// claim that everything below it arrived.
	cursorCtx, cursorCancel := context.WithTimeout(ctx, 30*time.Second)
	defer cursorCancel()
	tip, err := latestSequence(cursorCtx, fe.httpAPI, member.HTTPBase())
	if err != nil {
		t.Fatalf("read store tip: %v", err)
	}

	sub := recorder.NewSSESubscriber(member.HTTPBase(), "", fe.env.ManualKey)
	streamCtx, streamCancel := context.WithCancel(ctx)
	streamDone := make(chan struct{})
	// The FIRST subscription's outcome is evidence too. A recorder that
	// delivers one event and then dies still leaves a non-empty delivery set,
	// and every row it missed would read as legal at-least-once loss; B2 says
	// recorder loss is inconclusive, so the connection is carried into the
	// comparison exactly as the reconnect attempt is.
	var firstErr error
	var firstEndedEarly bool
	go func() {
		defer close(streamDone)
		_, firstErr = sub.Connect(streamCtx, fmt.Sprint(tip))
		if streamCtx.Err() == nil {
			firstEndedEarly = true
		}
	}()
	// Give the subscription time to complete catch-up and go live before the
	// run exists, so its events are LIVE deliveries rather than backlog.
	time.Sleep(3 * time.Second)

	run, _, err := fe.httpAPI.TriggerRun(ctx, member.HTTPBase(), job.ID)
	if err != nil {
		t.Fatalf("trigger %s: %v", alias, err)
	}
	t.Logf("history fixture run %s on %s (store tip before subscribe=%d)", run.ID, member.Name, tip)

	runCtx, runCancel := context.WithTimeout(ctx, 5*time.Minute)
	defer runCancel()
	final := waitRunTerminal(t, runCtx, fe.httpAPI, member.HTTPBase(), job.ID, run.ID)
	if !strings.EqualFold(final.Status, "succeeded") {
		t.Fatalf("history fixture run %s ended %s, want succeeded", run.ID, final.Status)
	}

	// Settle, then close the stream BEFORE reading the store, so no delivery
	// can arrive outside the scope the read covers.
	time.Sleep(5 * time.Second)
	streamCancel()
	<-streamDone

	readCtx, readCancel := context.WithTimeout(ctx, 60*time.Second)
	defer readCancel()
	rows, scope, err := readPersistedEvents(readCtx, fe.httpAPI, member.HTTPBase(), run.ID, 2000)
	if err != nil {
		t.Fatalf("inconclusive: persisted event read: %v", err)
	}
	if len(rows) == 0 {
		t.Fatalf("inconclusive: no persisted events for run %s", run.ID)
	}
	t.Logf("persisted scope store=%s min=%d max=%d rows=%d complete=%t", scope.Store, scope.Min, scope.Max, len(rows), scope.Complete)

	delivered := deliveredForRun(sub.Events(), run.ID)
	if len(delivered) == 0 {
		t.Fatalf("inconclusive: SSE delivered nothing for run %s (connections=%s)", run.ID, jsonString(sub.Connections()))
	}
	firstConn := resumeConnection(sub.Connections(), firstErr)
	if firstEndedEarly {
		firstConn.Err = firstNonEmptyStr(firstConn.Err,
			"the subscription ended before the test closed it") +
			" (stream ended at its own initiative)"
	}
	t.Logf("first connection: %s (established=%t err=%q)", jsonString(sub.Connections()), firstConn.Established, firstConn.Err)

	rep := history.Compare(delivered, toHistoryPersisted(rows), scope, firstConn)
	t.Logf("history: delivered=%d distinct=%d persisted=%d duplicates=%v missing_from_delivery=%v out_of_order=%d",
		rep.DeliveredCount, rep.DeliveredDistinct, rep.PersistedCount, rep.Duplicates, rep.MissingFromDelivery, rep.OutOfOrder)
	if defects := rep.Defects(); len(defects) > 0 {
		t.Fatalf("event history defects: %v", defects)
	}
	if !rep.Conclusive() {
		t.Fatalf("inconclusive event history comparison: %v (connections=%s)", rep.Inconclusive, jsonString(sub.Connections()))
	}

	// Reconnect with the documented Last-Event-ID cursor and compare the
	// resumed stream as a SET against the persisted rows above it.
	//
	// The cursor is the run's LOWEST persisted sequence, not the highest
	// delivered one: resuming at the tip leaves no backlog, so the comparison
	// would succeed while proving nothing. Everything above it is real catch-up
	// this reconnection has to replay, and none of it assumes contiguity.
	cursor := rows[0].Sequence
	expectedCatchUp := sequencesAbove(rows, cursor, scope)
	if len(expectedCatchUp) == 0 {
		t.Fatalf("inconclusive: no persisted row above cursor %d, so the reconnection would exercise no catch-up (rows=%d)", cursor, len(rows))
	}
	t.Logf("reconnecting from cursor %d with %d persisted rows above it: %v", cursor, len(expectedCatchUp), expectedCatchUp)

	resumed := recorder.NewSSESubscriber(member.HTTPBase(), run.ID, fe.env.ManualKey)
	resumeCtx, resumeCancel := context.WithTimeout(ctx, 90*time.Second)
	resumeDone := make(chan struct{})
	var resumeErr error
	go func() {
		defer close(resumeDone)
		_, resumeErr = resumed.Connect(resumeCtx, fmt.Sprint(cursor))
	}()
	// Wait for the catch-up to land instead of sleeping a fixed interval: a
	// short sleep would turn a slow replay into a false "resumed nothing".
	waitCtx, waitCancel := context.WithTimeout(ctx, 60*time.Second)
	waitErr := cluster.Poll(waitCtx, time.Second, func() (bool, error) {
		return len(deliveredForRun(resumed.Events(), run.ID)) >= len(expectedCatchUp), nil
	})
	waitCancel()
	// Settle briefly so a replay that overshoots is retained too, then close.
	time.Sleep(2 * time.Second)
	resumeCancel()
	<-resumeDone
	if waitErr != nil {
		t.Logf("resumed stream had not replayed %d rows before the wait elapsed: %v", len(expectedCatchUp), waitErr)
	}

	conn := resumeConnection(resumed.Connections(), resumeErr)
	t.Logf("resumed connection: %s (err=%q)", jsonString(resumed.Connections()), conn.Err)
	reconnect := history.CompareReconnect(delivered, deliveredForRun(resumed.Events(), run.ID), toHistoryPersisted(rows), cursor, scope, conn)
	t.Logf("reconnect: cursor=%d established=%t status=%d first_distinct=%d resumed_distinct=%d catch_up_expected=%v replayed<=cursor=%v new>cursor=%v missing_from_resumed=%v never_delivered=%v",
		reconnect.Cursor, conn.Established, conn.Status, reconnect.FirstDistinct, reconnect.ResumedDistinct,
		reconnect.PersistedAboveCursor, reconnect.ReplayedAtOrBelowCursor, reconnect.NewAboveCursor,
		reconnect.MissingFromResumed, reconnect.PersistedAboveCursorNeverDelivered)
	if defects := reconnect.Defects(); len(defects) > 0 {
		t.Fatalf("reconnection defects: %v", defects)
	}
	if !reconnect.Conclusive() {
		t.Fatalf("inconclusive reconnection comparison: %v (connections=%s)", reconnect.Inconclusive, jsonString(resumed.Connections()))
	}

	// The separate raw ledger: SSE cannot reveal an external effect whose
	// completion event was lost, so the effect records stand on their own.
	// Correlation is PER TASK: run-wide totals let one step's duplicate
	// completions conceal another step's phantom completion.
	stepCtx, stepCancel := context.WithTimeout(ctx, 60*time.Second)
	taskSteps, err := taskStepNames(stepCtx, fe.httpAPI, member.HTTPBase(), job.ID)
	stepCancel()
	if err != nil {
		t.Fatalf("inconclusive: could not read the job's task identities: %v", err)
	}
	t.Logf("task identity map for %s: %v", alias, taskSteps)

	effects := effectsForRun(fe.sink.Events(), run.ID)
	effectRep := history.CorrelateEffects(run.ID, effects, delivered, toHistoryPersisted(rows), completionEventTypes, taskSteps)
	t.Logf("effects: raw_starts=%d raw_completions=%d delivered_completion_events=%d persisted_completion_events=%d unwitnessed=%d per_step=%s notes=%v",
		effectRep.RawStarts, effectRep.RawCompletions, effectRep.DeliveredCompletionEvents,
		effectRep.PersistedCompletionEvents, effectRep.UnwitnessedCompletions, jsonString(effectRep.PerStep), effectRep.Notes)
	if defects := effectRep.Defects(); len(defects) > 0 {
		t.Fatalf("effect correlation defects: %v", defects)
	}
	if !effectRep.Conclusive() {
		t.Fatalf("inconclusive effect correlation for run %s: %v", run.ID, effectRep.Inconclusive)
	}
	if effectRep.RawCompletions < 2 {
		t.Fatalf("fixture did not produce both external completions: %+v", effectRep)
	}
	// Per-step, not in total: both fixture steps must have left their own
	// external completion behind.
	for _, step := range []string{firstStep, secondStep} {
		if effectRep.PerStep[step].RawCompletions < 1 {
			t.Fatalf("step %s left no external completion record: %s", step, jsonString(effectRep.PerStep))
		}
		if effectRep.PerStep[step].PersistedCompletionEvents < 1 {
			t.Fatalf("step %s has no persisted completion event: %s", step, jsonString(effectRep.PerStep))
		}
	}

	writeFaultRecord(t, fe, "event_history_correlation", map[string]any{
		"run_id":             run.ID,
		"member":             member.Name,
		"scope":              scope,
		"compare":            rep,
		"reconnect":          reconnect,
		"reconnect_cursor":   cursor,
		"catch_up_expected":  expectedCatchUp,
		"effects":            effectRep,
		"task_steps":         taskSteps,
		"sse_connections":    sub.Connections(),
		"resume_connections": resumed.Connections(),
	})
}

// resumeConnection turns the subscriber's own record of the resumed attempt
// into the evidence the comparison needs. A stream that never returned HTTP 200
// is not "no events": it is no evidence. The caller's own cancellation is the
// one expected ending and is not reported as a failure.
func resumeConnection(conns []recorder.SSEConnection, connectErr error) history.Connection {
	out := history.Connection{}
	if len(conns) == 0 {
		out.Err = "the resumed subscription was never attempted"
		return out
	}
	last := conns[len(conns)-1]
	out.Status = last.Status
	out.Established = last.Status == http.StatusOK
	if connectErr != nil && !isOwnCancellation(connectErr) {
		out.Err = connectErr.Error()
	}
	return out
}

// isOwnCancellation recognises the one ending the caller arranged: the test
// closes the resumed stream itself once the catch-up has landed. Everything
// else — a refused dial, a non-200, a mid-stream parser failure — is retained
// and makes the comparison inconclusive. The string checks exist because the
// HTTP transport does not always wrap the context error in a matchable way.
func isOwnCancellation(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := err.Error()
	for _, s := range []string{
		"context canceled",
		"context deadline exceeded",
		"request canceled",
		"use of closed network connection",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// sequencesAbove returns the persisted sequences strictly above cursor that the
// read's own scope covers. It is a set, not a range: nothing here assumes the
// sequences are contiguous.
func sequencesAbove(rows []persistedEvent, cursor uint64, scope history.Scope) []uint64 {
	var out []uint64
	for _, r := range rows {
		if r.Sequence > cursor && r.Sequence >= scope.Min && r.Sequence <= scope.Max {
			out = append(out, r.Sequence)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Delayed / dropped response control on the runner's own route
// ---------------------------------------------------------------------------

func runResponseLoss(t *testing.T, fe *faultEnv) {
	ctx := context.Background()
	member := fe.leader
	survivor := otherMember(fe.topo, member)

	in, err := faults.NewInterposer(member.HTTPBase())
	if err != nil {
		t.Fatalf("interposer: %v", err)
	}
	if err := in.Start(); err != nil {
		t.Fatalf("interposer listen: %v", err)
	}
	t.Cleanup(func() { _ = in.Close(context.Background()) })
	t.Logf("client-side interposer %s -> %s", in.BaseURL(), in.Upstream())

	type variant struct {
		name   string
		policy faults.Policy
		client *http.Client
	}
	// Neither policy is `Once`: the fault stays armed until it is explicitly
	// disarmed, so the heal probe below is load-bearing evidence that the
	// disarm took effect rather than an assertion about a self-expiring fault.
	variants := []variant{
		{
			name:   "dropped_response",
			policy: faults.Policy{Mode: faults.ModeDropResponse, Method: http.MethodPost, PathContains: "/run"},
			client: &http.Client{Timeout: 30 * time.Second},
		},
		{
			name:   "delayed_response_past_client_deadline",
			policy: faults.Policy{Mode: faults.ModeDelayResponse, Method: http.MethodPost, PathContains: "/run", Delay: 12 * time.Second},
			client: &http.Client{Timeout: 4 * time.Second},
		},
	}

	var recorded []map[string]any
	for _, v := range variants {
		job, alias := applyFaultFixture(t, fe, member, "loss-"+strings.ReplaceAll(v.name, "_", "-"), 2)
		in.SetPolicy(v.policy)

		before := len(in.Operations())
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			in.BaseURL()+"/v1/jobs/"+job.ID+"/run", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("%s: build request: %v", v.name, err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Caesium-Manual-Trigger-Key", fe.env.ManualKey)

		resp, clientErr := v.client.Do(req)
		clientOutcomeAt := time.Now().UTC()
		if resp != nil {
			_ = resp.Body.Close()
		}
		if clientErr == nil {
			t.Fatalf("%s: the client received a usable response although the fault was armed", v.name)
		}
		t.Logf("%s: client observed %v at %s", v.name, clientErr, clientOutcomeAt.Format(time.RFC3339Nano))

		// Wait for the interposer to settle the operation it forwarded.
		var op faults.Operation
		deadline := time.Now().Add(60 * time.Second)
		for {
			ops := in.Operations()
			if len(ops) > before {
				op = ops[len(ops)-1]
				if !op.SettledAt.IsZero() {
					break
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: the interposer never settled an operation (ops=%s)", v.name, jsonString(in.Operations()))
			}
			time.Sleep(200 * time.Millisecond)
		}
		if !op.PossiblyCommitted {
			t.Fatalf("%s: a lost response must be recorded as possibly committed: %+v", v.name, op)
		}
		if op.UpstreamStatus != http.StatusAccepted {
			t.Fatalf("%s: the controller did not observe a committed 202 upstream: %+v", v.name, op)
		}
		// The INJECTED fault, not a slow upstream, must be what withheld the
		// response. Two independent observations establish it: the interposer
		// recorded applying exactly this mode, and it had already observed the
		// upstream's 202 BEFORE the client gave up. Without the ordering, a
		// three-second upstream and a two-second client deadline would satisfy
		// every other assertion here while proving nothing.
		if op.Applied != v.policy.Mode {
			t.Fatalf("%s: the interposer applied %q, not the armed fault %q: %+v", v.name, op.Applied, v.policy.Mode, op)
		}
		if op.UpstreamAt.IsZero() {
			t.Fatalf("%s: inconclusive: the interposer recorded no upstream acceptance time: %+v", v.name, op)
		}
		if !op.UpstreamAt.Before(clientOutcomeAt) {
			t.Fatalf("%s: inconclusive: the upstream 202 was observed at %s, at or after the client's failure at %s, "+
				"so the loss is not attributable to the injected fault: %+v",
				v.name, op.UpstreamAt.Format(time.RFC3339Nano), clientOutcomeAt.Format(time.RFC3339Nano), op)
		}
		commitLead := clientOutcomeAt.Sub(op.UpstreamAt)
		t.Logf("%s: ordering upstream_202_at=%s < client_failed_at=%s (lead %s), applied=%s",
			v.name, op.UpstreamAt.Format(time.RFC3339Nano), clientOutcomeAt.Format(time.RFC3339Nano), commitLead, op.Applied)
		note := faults.ReconcileNote(op)
		if !strings.Contains(note, "POSSIBLY COMMITTED") {
			t.Fatalf("%s: missing possibly-committed note", v.name)
		}
		t.Logf("%s: %s", v.name, note)

		// Reconcile by recorded identity, from a DIFFERENT member, exactly as
		// the contract requires. A client timeout is not a rejection.
		var acked cluster.Run
		if err := json.Unmarshal([]byte(op.UpstreamBody), &acked); err != nil {
			t.Fatalf("%s: controller-observed upstream body is not a run: %v (%s)", v.name, err, truncate([]byte(op.UpstreamBody), 512))
		}
		if _, err := uuid.Parse(acked.ID); err != nil {
			t.Fatalf("%s: controller-observed run id is not a uuid: %q", v.name, acked.ID)
		}
		reconCtx, reconCancel := context.WithTimeout(ctx, 2*time.Minute)
		var reconciled cluster.Run
		err = cluster.Poll(reconCtx, time.Second, func() (bool, error) {
			got, err := fe.httpAPI.GetRun(reconCtx, survivor.HTTPBase(), job.ID, acked.ID)
			if err != nil {
				return false, nil
			}
			reconciled = got
			return true, nil
		})
		reconCancel()
		if err != nil {
			t.Fatalf("%s: possibly-committed operation could not be reconciled on %s: %v", v.name, survivor.Name, err)
		}
		if reconciled.ID != acked.ID {
			t.Fatalf("%s: reconciled a different run %s", v.name, reconciled.ID)
		}
		t.Logf("%s: run %s IS present on %s despite the client's transport failure (status=%s)",
			v.name, reconciled.ID, survivor.Name, reconciled.Status)

		termCtx, termCancel := context.WithTimeout(ctx, 5*time.Minute)
		final := waitRunTerminal(t, termCtx, fe.httpAPI, survivor.HTTPBase(), job.ID, acked.ID)
		termCancel()

		effects := effectsForRun(fe.sink.Events(), acked.ID)
		if len(effects) == 0 {
			t.Fatalf("%s: run %s left no external effect records", v.name, acked.ID)
		}

		// Heal evidence, through the SAME interposer: disarming is a control
		// command, and a control command that returned success proves nothing.
		// A fresh matching request must come back usable, within a bound, on the
		// very route that just lost one.
		in.SetPolicy(faults.Policy{Mode: faults.ModePassThrough})
		healOp, healRun, healErr := probeInterposerHealed(ctx, fe, in, job.ID, 30*time.Second)
		if healErr != nil {
			t.Fatalf("%s: the response route did not recover after disarming the fault: %v (ops=%s)",
				v.name, healErr, jsonString(in.Operations()))
		}
		t.Logf("%s: heal proven through the same interposer: %s %s -> %d in %s (run %s)",
			v.name, healOp.Method, healOp.Path, healOp.UpstreamStatus,
			healOp.SettledAt.Sub(healOp.InvokedAt), healRun.ID)

		recorded = append(recorded, map[string]any{
			"variant":             v.name,
			"alias":               alias,
			"operation":           op,
			"client_error":        clientErr.Error(),
			"client_outcome_at":   clientOutcomeAt,
			"upstream_at":         op.UpstreamAt,
			"commit_lead":         commitLead.String(),
			"applied_fault":       string(op.Applied),
			"reconciled_run":      acked.ID,
			"reconciled_via":      survivor.Name,
			"final_status":        final.Status,
			"raw_effect_count":    len(effects),
			"reconcile_note":      note,
			"heal_operation":      healOp,
			"heal_run":            healRun.ID,
			"heal_latency_millis": healOp.SettledAt.Sub(healOp.InvokedAt).Milliseconds(),
		})
	}

	writeFaultRecord(t, fe, "response_loss_possibly_committed", map[string]any{
		"interposer_upstream": in.Upstream(),
		"variants":            recorded,
		"all_operations":      in.Operations(),
	})
}

// probeInterposerHealed sends a fresh request that WOULD have matched the fault
// through the same interposer and requires a usable response within bound. The
// returned operation is the heal evidence: it is the interposer's own record of
// forwarding a response it no longer withholds.
func probeInterposerHealed(ctx context.Context, fe *faultEnv, in *faults.Interposer, jobID string, bound time.Duration) (faults.Operation, cluster.Run, error) {
	before := len(in.Operations())
	reqCtx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		in.BaseURL()+"/v1/jobs/"+jobID+"/run", strings.NewReader("{}"))
	if err != nil {
		return faults.Operation{}, cluster.Run{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Caesium-Manual-Trigger-Key", fe.env.ManualKey)

	client := &http.Client{Timeout: bound}
	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return faults.Operation{}, cluster.Run{}, fmt.Errorf("the disarmed interposer still lost the response after %s: %w", time.Since(started), err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusAccepted {
		return faults.Operation{}, cluster.Run{}, fmt.Errorf("disarmed interposer returned status %d: %s", resp.StatusCode, truncate(body, 512))
	}
	var healRun cluster.Run
	if err := json.Unmarshal(body, &healRun); err != nil {
		return faults.Operation{}, cluster.Run{}, fmt.Errorf("disarmed interposer returned an undecodable body: %w (%s)", err, truncate(body, 512))
	}
	if _, err := uuid.Parse(healRun.ID); err != nil {
		return faults.Operation{}, cluster.Run{}, fmt.Errorf("disarmed interposer returned no usable run id: %q", healRun.ID)
	}

	ops := in.Operations()
	if len(ops) <= before {
		return faults.Operation{}, healRun, fmt.Errorf("the heal request did not traverse the interposer (ops before=%d after=%d)", before, len(ops))
	}
	op := ops[len(ops)-1]
	if op.Applied != faults.ModePassThrough {
		return op, healRun, fmt.Errorf("the heal request was still faulted (applied=%s)", op.Applied)
	}
	if op.PossiblyCommitted {
		return op, healRun, fmt.Errorf("the heal request was recorded as possibly committed: %+v", op)
	}
	if op.UpstreamStatus != http.StatusAccepted {
		return op, healRun, fmt.Errorf("the heal request's upstream status was %d", op.UpstreamStatus)
	}
	return op, healRun, nil
}

// ---------------------------------------------------------------------------
// External pause / resume of a real member process
// ---------------------------------------------------------------------------

func runPauseResume(t *testing.T, fe *faultEnv) {
	ctx := context.Background()
	// Freeze a non-leader so the blast radius of the capability proof is
	// bounded. Freezing an owner past its lease is B3's scenario.
	target := otherMember(fe.topo, fe.leader)
	live, err := cluster.RefreshMember(ctx, fe.kube, fe.env.Namespace, target.Name)
	if err != nil {
		t.Fatalf("refresh %s: %v", target.Name, err)
	}
	if live.ContainerID == "" {
		t.Fatalf("member %s has no container id", target.Name)
	}

	stateCtx, stateCancel := context.WithTimeout(ctx, 60*time.Second)
	before, beforeRaw, err := fe.host.TaskState(stateCtx, live.Node, live.ContainerID)
	stateCancel()
	if err != nil {
		t.Fatalf("inconclusive: could not read %s task state: %v (%s)", target.Name, err, truncate([]byte(beforeRaw), 400))
	}
	ev := faults.PauseEvidence{
		ContainerID:      live.ContainerID,
		StateBefore:      before,
		LeaseTTLSeconds:  leaseTTLSeconds,
		ObservedNodeAddr: live.NodeAddress,
	}
	ev.ReachableBefore = fe.httpAPI.Health(ctx, live.HTTPBase()) == nil

	resumed := false
	t.Cleanup(func() {
		if resumed {
			return
		}
		cleanCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if _, _, err := fe.host.Resume(cleanCtx, live.Node, live.ContainerID); err != nil {
			t.Logf("cleanup resume %s: %v", target.Name, err)
		}
	})

	pauseCtx, pauseCancel := context.WithTimeout(ctx, 2*time.Minute)
	pausedState, pausedRaw, err := fe.host.Pause(pauseCtx, live.Node, live.ContainerID)
	pauseCancel()
	if err != nil {
		t.Fatalf("pause %s: %v (%s)", target.Name, err, truncate([]byte(pausedRaw), 600))
	}
	ev.StatePaused = pausedState
	pausedAt := time.Now()

	// Second, independent observation: the frozen member stops answering the
	// runner. The container runtime alone could be reporting stale state.
	probe := &cluster.HTTP{Client: &http.Client{Timeout: 3 * time.Second}, ManualKey: fe.env.ManualKey}
	unreachCtx, unreachCancel := context.WithTimeout(ctx, 60*time.Second)
	var lastHealthErr error
	err = cluster.Poll(unreachCtx, time.Second, func() (bool, error) {
		lastHealthErr = probe.Health(unreachCtx, live.HTTPBase())
		return lastHealthErr != nil, nil
	})
	unreachCancel()
	if err != nil {
		t.Fatalf("frozen member %s still answered /health for 60s: the freeze had no observable effect", target.Name)
	}
	ev.DuringPauseErr = lastHealthErr.Error()
	t.Logf("pause activated: runtime=%s health=%v", ev.StatePaused, lastHealthErr)
	if err := ev.Activated(); err != nil {
		t.Fatalf("pause activation evidence: %v", err)
	}

	// Hold past the configured run lease so the resumed process really is a
	// stale one, then heal.
	hold := time.Duration(leaseTTLSeconds+12) * time.Second
	for time.Since(pausedAt) < hold {
		time.Sleep(time.Second)
	}
	ev.HeldSeconds = time.Since(pausedAt).Seconds()

	resumeCtx, resumeCancel := context.WithTimeout(ctx, 2*time.Minute)
	resumedState, resumedRaw, err := fe.host.Resume(resumeCtx, live.Node, live.ContainerID)
	resumeCancel()
	resumed = true
	if err != nil {
		t.Fatalf("resume %s: %v (%s)", target.Name, err, truncate([]byte(resumedRaw), 600))
	}
	ev.StateResumed = resumedState

	healCtx, healCancel := context.WithTimeout(ctx, 3*time.Minute)
	err = cluster.Poll(healCtx, time.Second, func() (bool, error) {
		return fe.httpAPI.Health(healCtx, live.HTTPBase()) == nil, nil
	})
	healCancel()
	ev.ReachableAfter = err == nil
	if err := ev.Healed(); err != nil {
		t.Fatalf("pause heal evidence: %v", err)
	}
	t.Logf("pause healed: runtime=%s held_s=%.1f past_lease=%t", ev.StateResumed, ev.HeldSeconds, ev.HeldPastLease())
	if !ev.HeldPastLease() {
		t.Fatalf("freeze lasted %.1fs, which did not outlast the %.0fs run lease", ev.HeldSeconds, leaseTTLSeconds)
	}

	memberCtx, memberCancel := context.WithTimeout(ctx, 3*time.Minute)
	membership, err := cluster.WaitMembership(memberCtx, dqliteAddresses(fe.topo))
	memberCancel()
	if err != nil {
		t.Fatalf("cluster did not recover membership after resume: %v", err)
	}
	t.Logf("membership after resume: leader=%s members=%d", membership.Leader.Address, len(membership.Members))

	writeFaultRecord(t, fe, "external_pause_resume", map[string]any{
		"member":            target.Name,
		"node":              live.Node,
		"container_id":      live.ContainerID,
		"evidence":          ev,
		"state_before_raw":  truncate([]byte(beforeRaw), 2000),
		"state_paused_raw":  truncate([]byte(pausedRaw), 2000),
		"state_resumed_raw": truncate([]byte(resumedRaw), 2000),
		"leader_after":      membership.Leader.Address,
		"members_after":     len(membership.Members),
	})
}

// ---------------------------------------------------------------------------
// Asymmetric network partition on the discovered peer addresses
// ---------------------------------------------------------------------------

func runAsymmetricPartition(t *testing.T, fe *faultEnv) {
	ctx := context.Background()

	// Refresh membership: the addresses under test must be the ones the
	// product actually discovered, not ones the harness assumed.
	discCtx, discCancel := context.WithTimeout(ctx, 2*time.Minute)
	membership, err := cluster.DiscoverMembership(discCtx, dqliteAddresses(fe.topo))
	discCancel()
	if err != nil {
		t.Fatalf("discover membership: %v", err)
	}
	dst, ok := fe.topo.ByIP(cluster.HostIP(membership.Leader.Address))
	if !ok {
		t.Fatalf("discovered leader %s is not a caesium pod", membership.Leader.Address)
	}

	// Put real dispatch work in flight BEFORE the cut, and let the product pick
	// the worker. The owner stamps its own discovered dispatch address
	// (https://<owner pod ip>:8443) onto every dispatch, and the worker reports
	// the task's completion back to exactly that address — so partitioning the
	// worker's route to the owner puts the DISCOVERED dispatch address inside
	// the cut, and the blocked rule's own packet counter becomes the traversal
	// evidence for it. Choosing the source this way is what makes the dispatch
	// route provably exercised instead of merely rule-covered.
	src, fixture := triggerInFlightDispatch(t, fe, dst, 60, 4)

	// The third member is unpartitioned and is the open-direction oracle: if the
	// destination still answers IT on the same port, the destination is healthy
	// and only the src->dst direction is cut.
	var third cluster.Member
	for _, m := range fe.topo.Members {
		if m.Name != src.Name && m.Name != dst.Name {
			third = m
			break
		}
	}
	if third.Name == "" {
		t.Fatalf("no unpartitioned third member to use as the open-direction oracle")
	}
	thirdLive, err := cluster.RefreshMember(ctx, fe.kube, fe.env.Namespace, third.Name)
	if err != nil {
		t.Fatalf("refresh %s: %v", third.Name, err)
	}

	srcLive, err := cluster.RefreshMember(ctx, fe.kube, fe.env.Namespace, src.Name)
	if err != nil {
		t.Fatalf("refresh %s: %v", src.Name, err)
	}
	dstLive, err := cluster.RefreshMember(ctx, fe.kube, fe.env.Namespace, dst.Name)
	if err != nil {
		t.Fatalf("refresh %s: %v", dst.Name, err)
	}

	// The addresses come from the dqlite Cluster RPC, so the rules are keyed by
	// the real advertised peer address; dispatch derives https://<same host>:8443
	// from it, which is why both ports are counted.
	discovered := map[string]string{}
	for _, m := range membership.Members {
		discovered[cluster.HostIP(m.Address)] = m.Address
	}
	if _, ok := discovered[srcLive.IP]; !ok {
		t.Fatalf("source pod %s (%s) is not in the discovered membership %v", src.Name, srcLive.IP, discovered)
	}
	if _, ok := discovered[dstLive.IP]; !ok {
		t.Fatalf("destination pod %s (%s) is not in the discovered membership %v", dst.Name, dstLive.IP, discovered)
	}

	plan := faults.PartitionPlan{
		Tag:        "rbp-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10],
		SrcPod:     src.Name,
		DstPod:     dst.Name,
		SrcIP:      srcLive.IP,
		DstIP:      dstLive.IP,
		SrcNode:    srcLive.Node,
		DstNode:    dstLive.Node,
		DropPorts:  []int{faults.PortAPI, faults.PortInternal, faults.PortRaft},
		CountPorts: []int{faults.PortRaft, faults.PortInternal},
	}
	if err := plan.Validate(); err != nil {
		t.Fatalf("partition plan: %v", err)
	}
	t.Logf("partition plan %s: %s(%s on %s) -/-> %s(%s on %s)", plan.Tag,
		plan.SrcPod, plan.SrcIP, plan.SrcNode, plan.DstPod, plan.DstIP, plan.DstNode)

	target := fmt.Sprintf("http://%s:%d/health", plan.DstIP, faults.PortAPI)
	reverse := fmt.Sprintf("http://%s:%d/health", plan.SrcIP, faults.PortAPI)

	preCtx, preCancel := context.WithTimeout(ctx, 2*time.Minute)
	preFwd, err := fe.host.Probe(preCtx, srcLive.Node, srcLive.ContainerID, src.Name, target, 4)
	if err != nil {
		preCancel()
		t.Fatalf("inconclusive: baseline probe %s->%s failed to run: %v", src.Name, dst.Name, err)
	}
	preRev, err := fe.host.Probe(preCtx, dstLive.Node, dstLive.ContainerID, dst.Name, reverse, 4)
	if err != nil {
		preCancel()
		t.Fatalf("inconclusive: baseline probe %s->%s failed to run: %v", dst.Name, src.Name, err)
	}
	preOpen, err := fe.host.Probe(preCtx, thirdLive.Node, thirdLive.ContainerID, third.Name, target, 4)
	preCancel()
	if err != nil {
		t.Fatalf("inconclusive: baseline probe %s->%s failed to run: %v", third.Name, dst.Name, err)
	}
	if !preFwd.Success || !preRev.Success || !preOpen.Success {
		t.Fatalf("inconclusive: the members could not reach each other before the fault (fwd=%s rev=%s open=%s)",
			preFwd.Err(), preRev.Err(), preOpen.Err())
	}

	healed := false
	t.Cleanup(func() {
		if healed {
			return
		}
		cleanCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if _, err := fe.host.Heal(cleanCtx, plan); err != nil {
			t.Logf("cleanup heal %s: %v", plan.Tag, err)
		}
	})

	partCtx, partCancel := context.WithTimeout(ctx, 2*time.Minute)
	installed, err := fe.host.Partition(partCtx, plan)
	partCancel()
	if err != nil {
		t.Fatalf("install partition: %v (%s)", err, truncate([]byte(installed), 1000))
	}

	// Activation evidence 1: the rule keyed by the DISCOVERED Raft address
	// must match real packets. A zero counter means the route bypasses the
	// injector, which is exactly the failure mode A1 warned about for proxies.
	var blocked, open map[string]faults.Counter
	var counterRaw string
	countCtx, countCancel := context.WithTimeout(ctx, 3*time.Minute)
	err = cluster.Poll(countCtx, 2*time.Second, func() (bool, error) {
		b, o, raw, err := fe.host.Counters(countCtx, plan)
		if err != nil {
			return false, nil
		}
		blocked, open, counterRaw = b, o, raw
		return b[plan.DropRuleComment(faults.PortRaft)].Packets > 0, nil
	})
	countCancel()
	if err != nil {
		t.Fatalf("the discovered Raft peer address never traversed the injector: counters=%v raw=%s", blocked, truncate([]byte(counterRaw), 1500))
	}

	// Activation evidence 2 and 3: the blocked direction fails and the open
	// direction still works, from inside the members' own namespaces.
	probeCtx, probeCancel := context.WithTimeout(ctx, 3*time.Minute)
	fwd, err := fe.host.Probe(probeCtx, srcLive.Node, srcLive.ContainerID, src.Name, target, 4)
	if err != nil {
		probeCancel()
		t.Fatalf("inconclusive: blocked-direction probe failed to run: %v", err)
	}
	rev, err := fe.host.Probe(probeCtx, dstLive.Node, dstLive.ContainerID, dst.Name, reverse, 4)
	if err != nil {
		probeCancel()
		t.Fatalf("inconclusive: reverse-direction probe failed to run: %v", err)
	}
	openProbe, err := fe.host.Probe(probeCtx, thirdLive.Node, thirdLive.ContainerID, third.Name, target, 4)
	probeCancel()
	if err != nil {
		t.Fatalf("inconclusive: open-direction probe failed to run: %v", err)
	}

	evidence := faults.ActivationEvidence{
		BlockedCounters: blocked,
		OpenCounters:    open,
		BlockedProbeErr: fwd.Err(),
		OpenProbeOK:     openProbe.Success,
		OpenProbeFrom:   third.Name,
		ReverseProbeErr: rev.Err(),
	}
	if err := plan.Activated(evidence); err != nil {
		t.Fatalf("partition activation evidence: %v (blocked=%v open=%v)", err, blocked, open)
	}
	t.Logf("partition active: blocked %s->%s (%s); %s still reachable from unpartitioned %s; reverse %s->%s: %q; raft_counter=%d dispatch_counter=%d",
		src.Name, dst.Name, strings.TrimSpace(fwd.Err()), dst.Name, third.Name, dst.Name, src.Name, strings.TrimSpace(rev.Err()),
		blocked[plan.DropRuleComment(faults.PortRaft)].Packets,
		blocked[plan.DropRuleComment(faults.PortInternal)].Packets)

	// Activation evidence 4: the DISPATCH route, not just the Raft one. The
	// in-flight task claimed by this source must report its completion to the
	// owner's discovered dispatch address, which now sits behind the cut, so
	// the BLOCKED direction's own counter for :8443 must become positive. A
	// route that never traverses the injector is reported unproven here — as a
	// failure, never as a pass — because that is precisely the bypass A1 warned
	// about.
	dispatchRule := plan.DropRuleComment(faults.PortInternal)
	dispCtx, dispCancel := context.WithTimeout(ctx, 4*time.Minute)
	var dispatchBlocked faults.Counter
	err = cluster.Poll(dispCtx, 3*time.Second, func() (bool, error) {
		b, o, raw, cerr := fe.host.Counters(dispCtx, plan)
		if cerr != nil {
			return false, nil
		}
		blocked, open, counterRaw = b, o, raw
		dispatchBlocked = b[dispatchRule]
		return dispatchBlocked.Packets > 0, nil
	})
	dispCancel()
	if err != nil {
		t.Fatalf("the discovered dispatch address %s:%d never traversed the injector during the fault window: "+
			"the dispatch route is UNPROVEN, not proven (in-flight task %s on %s owned by %s, runs %v; counters=%v raw=%s)",
			plan.DstIP, faults.PortInternal, fixture.TaskRunID, src.Name, dst.Name, fixture.RunIDs,
			blocked, truncate([]byte(counterRaw), 1200))
	}
	t.Logf("dispatch route traversal proven: rule %s matched %d packets / %d bytes from %s toward the owner's discovered dispatch address %s:%d (task %s)",
		dispatchRule, dispatchBlocked.Packets, dispatchBlocked.Bytes, src.Name, plan.DstIP, faults.PortInternal, fixture.TaskRunID)

	// Hold briefly, then re-read the counters so the retained evidence shows
	// traffic accumulating against the rule rather than a single sample.
	time.Sleep(20 * time.Second)
	heldCtx, heldCancel := context.WithTimeout(ctx, 2*time.Minute)
	heldBlocked, heldOpen, heldRaw, err := fe.host.Counters(heldCtx, plan)
	heldCancel()
	if err != nil {
		t.Fatalf("re-read counters: %v", err)
	}

	healCtx, healCancel := context.WithTimeout(ctx, 2*time.Minute)
	healEvidence, err := fe.host.Heal(healCtx, plan)
	healCancel()
	healed = true
	if err != nil {
		t.Fatalf("heal partition: %v (%s)", err, truncate([]byte(healEvidence), 1000))
	}

	postCtx, postCancel := context.WithTimeout(ctx, 3*time.Minute)
	var postFwd, postRev faults.ProbeResult
	err = cluster.Poll(postCtx, 3*time.Second, func() (bool, error) {
		f, ferr := fe.host.Probe(postCtx, srcLive.Node, srcLive.ContainerID, src.Name, target, 4)
		if ferr != nil {
			return false, nil
		}
		r, rerr := fe.host.Probe(postCtx, dstLive.Node, dstLive.ContainerID, dst.Name, reverse, 4)
		if rerr != nil {
			return false, nil
		}
		postFwd, postRev = f, r
		return f.Success && r.Success, nil
	})
	postCancel()
	if err != nil {
		t.Fatalf("connectivity did not recover after heal (fwd=%s rev=%s)", postFwd.Err(), postRev.Err())
	}
	if err := plan.Healed(postFwd.Success, postRev.Success); err != nil {
		t.Fatalf("partition heal evidence: %v", err)
	}

	memberCtx, memberCancel := context.WithTimeout(ctx, 3*time.Minute)
	after, err := cluster.WaitMembership(memberCtx, dqliteAddresses(fe.topo))
	memberCancel()
	if err != nil {
		t.Fatalf("membership did not recover after heal: %v", err)
	}
	t.Logf("partition healed: leader=%s members=%d", after.Leader.Address, len(after.Members))

	// The blocked direction's own counter is the dispatch-route evidence; the
	// open direction's counter is recorded beside it but never substituted for
	// it, because reverse-direction traffic says nothing about the cut route.
	heldDispatchBlocked := heldBlocked[dispatchRule].Packets
	if heldDispatchBlocked < dispatchBlocked.Packets {
		t.Fatalf("the dispatch rule's counter went backwards (%d -> %d): the rule was replaced mid-window and its evidence cannot be trusted",
			dispatchBlocked.Packets, heldDispatchBlocked)
	}
	t.Logf("dispatch route counters: blocked=%d packets (required), open direction=%d packets (recorded only)",
		heldDispatchBlocked, heldOpen[plan.OpenRuleComment(faults.PortInternal)].Packets)

	// The fixture runs are not asserted on: re-dispatch after a dropped
	// completion is B3's fenced-recovery scenario, not this capability proof.
	// Their observed state is retained as context for the evidence record.
	fixtureStatus := map[string]string{}
	for _, id := range fixture.RunIDs {
		statusCtx, statusCancel := context.WithTimeout(ctx, 30*time.Second)
		got, gerr := fe.httpAPI.GetRun(statusCtx, dst.HTTPBase(), fixture.JobID, id)
		statusCancel()
		if gerr != nil {
			fixtureStatus[id] = "unread: " + gerr.Error()
			continue
		}
		fixtureStatus[id] = got.Status
	}
	t.Logf("partition fixture runs after heal: %v", fixtureStatus)

	writeFaultRecord(t, fe, "asymmetric_partition", map[string]any{
		"plan":                     plan,
		"discovered_membership":    discovered,
		"baseline_forward":         preFwd,
		"baseline_reverse":         preRev,
		"blocked_counters":         blocked,
		"open_counters":            open,
		"held_blocked_counters":    heldBlocked,
		"held_open_counters":       heldOpen,
		"blocked_probe":            fwd,
		"baseline_open":            preOpen,
		"open_probe":               openProbe,
		"open_probe_from":          third.Name,
		"reverse_probe_during":     rev,
		"post_heal_forward":        postFwd,
		"post_heal_reverse":        postRev,
		"dispatch_rule":            dispatchRule,
		"dispatch_blocked_packets": heldDispatchBlocked,
		"dispatch_open_packets":    heldOpen[plan.OpenRuleComment(faults.PortInternal)].Packets,
		"dispatch_fixture":         fixture,
		"dispatch_fixture_status":  fixtureStatus,
		"dispatch_worker":          src.Name,
		"dispatch_owner":           dst.Name,
		"install_evidence":         truncate([]byte(installed), 4000),
		"held_evidence":            truncate([]byte(heldRaw), 4000),
		"heal_evidence":            truncate([]byte(healEvidence), 4000),
		"leader_after":             after.Leader.Address,
		"members_after":            len(after.Members),
	})
}

// inFlightDispatch records the work that must cross the cut: the fixture job,
// the runs that were triggered, and the run whose OWNER is the partition
// destination while a DIFFERENT member holds a running task claim.
type inFlightDispatch struct {
	JobID     string   `json:"job_id"`
	Alias     string   `json:"alias"`
	RunIDs    []string `json:"run_ids"`
	RunID     string   `json:"run_id"`
	TaskRunID string   `json:"task_run_id"`
	ClaimedBy string   `json:"claimed_by"`
	OwnerNode string   `json:"owner_node"`
	HoldSecs  int      `json:"hold_seconds"`
}

// triggerInFlightDispatch starts fixture runs and returns the member the
// PRODUCT chose to execute a task of a run the intended owner actually owns.
// That member becomes the partition source, so the worker's completion report
// to the owner's discovered dispatch address has to cross the cut.
//
// Both halves are read from the product, never assumed: the run's owner comes
// from its `run_leases` row (the node that triggered a run is not necessarily
// the node that owns it) and the worker comes from the task row's own claim. If
// no run satisfies both within the attempts allowed, the dispatch route would
// be rule-covered but unexercised, and that is reported as inconclusive rather
// than partitioning an idle member and calling it proof.
func triggerInFlightDispatch(t *testing.T, fe *faultEnv, owner cluster.Member, holdSeconds, attempts int) (cluster.Member, inFlightDispatch) {
	t.Helper()
	ctx := context.Background()
	job, alias := applyFaultFixture(t, fe, owner, "partition", holdSeconds)
	rec := inFlightDispatch{JobID: job.ID, Alias: alias, HoldSecs: holdSeconds}

	var worker cluster.Member
	lastReason := "no run was triggered"
	for i := 0; i < attempts && worker.Name == ""; i++ {
		run, _, err := fe.httpAPI.TriggerRun(ctx, owner.HTTPBase(), job.ID)
		if err != nil {
			t.Fatalf("trigger dispatch fixture run %d: %v", i+1, err)
		}
		rec.RunIDs = append(rec.RunIDs, run.ID)

		waitCtx, cancel := context.WithTimeout(ctx, 75*time.Second)
		_ = cluster.Poll(waitCtx, 2*time.Second, func() (bool, error) {
			lease, lerr := fe.httpAPI.QueryLease(waitCtx, owner.HTTPBase(), run.ID)
			if lerr != nil {
				lastReason = fmt.Sprintf("run %s has no readable lease yet: %v", run.ID, lerr)
				return false, nil
			}
			if cluster.HostIP(lease.OwnerNode) != owner.IP {
				// Another member owns it, so its dispatch address is not the
				// one under test. Stop waiting and trigger another run.
				lastReason = fmt.Sprintf("run %s is owned by %s, not by %s", run.ID, lease.OwnerNode, owner.Name)
				return true, nil
			}
			got, gerr := fe.httpAPI.GetRun(waitCtx, owner.HTTPBase(), job.ID, run.ID)
			if gerr != nil {
				lastReason = fmt.Sprintf("run %s unreadable: %v", run.ID, gerr)
				return false, nil
			}
			for _, task := range got.Tasks {
				if !strings.EqualFold(strings.TrimSpace(task.Status), "running") {
					continue
				}
				claimed := strings.TrimSpace(task.ClaimedBy)
				if claimed == "" {
					continue
				}
				m, ok := fe.topo.ByIP(cluster.HostIP(claimed))
				if !ok || m.Name == owner.Name {
					continue
				}
				worker = m
				rec.RunID, rec.TaskRunID, rec.ClaimedBy, rec.OwnerNode = run.ID, task.ID, claimed, lease.OwnerNode
				return true, nil
			}
			lastReason = fmt.Sprintf("run %s (owned by %s) has no task claimed by a peer yet", run.ID, lease.OwnerNode)
			return false, nil
		})
		cancel()
		if worker.Name == "" {
			t.Logf("dispatch fixture attempt %d did not produce cross-member work: %s", i+1, lastReason)
		}
	}
	if worker.Name == "" {
		t.Fatalf("inconclusive: after %d runs of %s no task of a run owned by %s was claimed by another member, "+
			"so no dispatch work would have to cross the cut (last observation: %s)",
			len(rec.RunIDs), alias, owner.Name, lastReason)
	}
	t.Logf("dispatch work in flight: task %s of run %s (owner %s = %s) claimed by %s (%s), holding ~%ds",
		rec.TaskRunID, rec.RunID, owner.Name, rec.OwnerNode, worker.Name, rec.ClaimedBy, holdSeconds)
	return worker, rec
}

// ---------------------------------------------------------------------------
// Durable-event-before-publication pause (instrumented image only)
// ---------------------------------------------------------------------------

func runBusPublishPause(t *testing.T, fe *faultEnv) {
	if os.Getenv("CAESIUM_ROBUSTNESS_INSTRUMENTED") != "1" {
		t.Skip("release image: the durable-event-before-publication control is compiled out. " +
			"scripts/robustness.sh requires this subtest to PASS when it deploys the instrumented image, " +
			"so this skip can never stand in for the instrumented result.")
	}
	ctx := context.Background()
	member := fe.leader
	dir := fe.testdir

	live := make([]cluster.Member, 0, len(fe.topo.Members))
	for _, m := range fe.topo.Members {
		refreshed, err := cluster.RefreshMember(ctx, fe.kube, fe.env.Namespace, m.Name)
		if err != nil {
			t.Fatalf("refresh %s: %v", m.Name, err)
		}
		if refreshed.ContainerID == "" {
			t.Fatalf("member %s has no container id", m.Name)
		}
		live = append(live, refreshed)
	}

	// Arm EVERY publisher process: each member runs its own BusDispatcher
	// against the shared store, so an unarmed member would publish the row and
	// the pause would silently do nothing.
	// task_started is chosen deliberately: internal/run/store.go appends it
	// inside the dispatch transaction and publishes it from the owner/dispatch
	// loop, NOT from the HTTP trigger handler. Holding a publication that the
	// trigger request waits on would only prove that a blocked request times
	// out. The run id is not known before the trigger, so the predicate is
	// keyed by type here and correlated to the run through the hook's own
	// record afterwards.
	directive := `{"armed":true,"types":["task_started"],"max_hold_ms":120000}`
	disarm := func() {
		for _, m := range live {
			cleanCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			if _, err := fe.host.DisarmBusPublishPause(cleanCtx, m.Node, m.ContainerID, dir); err != nil {
				t.Logf("disarm %s: %v", m.Name, err)
			}
			cancel()
		}
	}
	t.Cleanup(disarm)
	for _, m := range live {
		armCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		ev, err := fe.host.ArmBusPublishPause(armCtx, m.Node, m.ContainerID, dir, directive)
		cancel()
		if err != nil {
			t.Fatalf("arm %s: %v (%s)", m.Name, err, truncate([]byte(ev), 400))
		}
	}
	t.Logf("armed durable-event-before-publication pause on %d members (dir=%s)", len(live), dir)

	// Subscribe from the store tip so any delivery observed afterwards is live.
	tipCtx, tipCancel := context.WithTimeout(ctx, 30*time.Second)
	tip, err := latestSequence(tipCtx, fe.httpAPI, member.HTTPBase())
	tipCancel()
	if err != nil {
		t.Fatalf("read store tip: %v", err)
	}
	sub := recorder.NewSSESubscriber(member.HTTPBase(), "", fe.env.ManualKey)
	streamCtx, streamCancel := context.WithCancel(ctx)
	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		if _, err := sub.Connect(streamCtx, fmt.Sprint(tip)); err != nil && streamCtx.Err() == nil {
			t.Logf("sse connection ended: %v", err)
		}
	}()
	defer func() { streamCancel(); <-streamDone }()
	time.Sleep(3 * time.Second)

	job, _ := applyFaultFixture(t, fe, member, "buspause", 2)
	run, _, err := fe.httpAPI.TriggerRun(ctx, member.HTTPBase(), job.ID)
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	t.Logf("armed run %s", run.ID)

	// The server's OWN record that it entered the hook. A pause inferred from
	// a delivery gap would prove nothing.
	var hookEvidence []faults.HookEvidence
	var held uint64
	hookCtx, hookCancel := context.WithTimeout(ctx, 3*time.Minute)
	err = cluster.Poll(hookCtx, 2*time.Second, func() (bool, error) {
		hookEvidence = nil
		held = 0
		for _, m := range live {
			raw, err := fe.host.BusPublishHookLog(hookCtx, m.Node, m.ContainerID, dir)
			if err != nil {
				return false, nil
			}
			entries, perr := faults.ParseHookLog(raw)
			if perr != nil {
				return false, fmt.Errorf("inconclusive: %s hook log unreadable: %w", m.Name, perr)
			}
			ev := faults.SummariseHookLog(m.Name, run.ID, entries)
			if len(ev.Entered) > 0 {
				hookEvidence = append(hookEvidence, ev)
				if seqs := ev.HeldSequences(); len(seqs) > 0 {
					held = seqs[0]
				}
			}
		}
		return held != 0, nil
	})
	hookCancel()
	if err != nil {
		t.Fatalf("no member recorded entering the publication hook for run %s: %v", run.ID, err)
	}
	for _, ev := range hookEvidence {
		if err := ev.Activated(); err != nil {
			t.Fatalf("hook activation evidence: %v", err)
		}
		t.Logf("hook entered on %s: paths=%v sequences=%v", ev.Member, ev.Paths, ev.HeldSequences())
	}

	// The row is durably committed and still pending: publication is held
	// AFTER the commit, and marking stays after publication.
	rowCtx, rowCancel := context.WithTimeout(ctx, 60*time.Second)
	rows, _, err := readPersistedEvents(rowCtx, fe.httpAPI, member.HTTPBase(), run.ID, 2000)
	rowCancel()
	if err != nil {
		t.Fatalf("inconclusive: persisted event read while held: %v", err)
	}
	row, ok := findPersisted(rows, held)
	if !ok {
		t.Fatalf("the held event %d has no durably committed row: the hook is on the wrong side of the commit", held)
	}
	if !row.BusPending || row.DispatchedAt != "" {
		t.Fatalf("held event %d is already marked dispatched (pending=%t dispatched_at=%q): marking must stay after publication",
			held, row.BusPending, row.DispatchedAt)
	}
	t.Logf("held event sequence=%d type=%s committed and still pending", row.Sequence, row.Type)

	// It must not have been delivered live while held.
	for _, ev := range sub.Events() {
		if ev.Sequence == held {
			t.Fatalf("event %d was delivered live while the publication hook held it", held)
		}
	}

	disarm()
	// Reconcile the releases against the holds ACTIVATION recorded, member by
	// member and sequence by sequence. Skipping a member whose log now reads
	// empty would let a truncated or vanished log stand in for "this member
	// never entered the hook", which is exactly the evidence the activation
	// step already disproved.
	relCtx, relCancel := context.WithTimeout(ctx, 2*time.Minute)
	var lastRelErr error
	var released []faults.HookEvidence
	err = cluster.Poll(relCtx, 2*time.Second, func() (bool, error) {
		current := make([]faults.HookEvidence, 0, len(live))
		for _, m := range live {
			raw, rerr := fe.host.BusPublishHookLog(relCtx, m.Node, m.ContainerID, dir)
			if rerr != nil {
				lastRelErr = fmt.Errorf("inconclusive: %s hook log could not be read: %w", m.Name, rerr)
				return false, nil
			}
			entries, perr := faults.ParseHookLog(raw)
			if perr != nil {
				return false, fmt.Errorf("inconclusive: %s hook log unreadable: %w", m.Name, perr)
			}
			current = append(current, faults.SummariseHookLog(m.Name, run.ID, entries))
		}
		if rerr := faults.ReconcileReleases(hookEvidence, current); rerr != nil {
			lastRelErr = rerr
			return false, nil
		}
		released = current
		return true, nil
	})
	relCancel()
	if err != nil {
		t.Fatalf("the recorded holds were not all released under test control: %v (last observation: %v)", err, lastRelErr)
	}
	for _, ev := range released {
		if len(ev.Entered) == 0 {
			continue
		}
		t.Logf("released on %s: held=%v releases=%d", ev.Member, ev.HeldSequences(), len(ev.Released))
	}

	// The durable, deterministic consequence: the row becomes marked dispatched
	// once publication is allowed. SSE delivery is supporting evidence only,
	// because at-least-once delivery may legally drop it.
	markCtx, markCancel := context.WithTimeout(ctx, 3*time.Minute)
	var after persistedEvent
	err = cluster.Poll(markCtx, 2*time.Second, func() (bool, error) {
		rows, _, err := readPersistedEvents(markCtx, fe.httpAPI, member.HTTPBase(), run.ID, 2000)
		if err != nil {
			return false, nil
		}
		row, ok := findPersisted(rows, held)
		if !ok {
			return false, fmt.Errorf("the committed row for %d disappeared", held)
		}
		after = row
		return !row.BusPending, nil
	})
	markCancel()
	if err != nil {
		t.Fatalf("the released event %d was never marked dispatched: %v", held, err)
	}
	deliveredLive := false
	for _, ev := range sub.Events() {
		if ev.Sequence == held {
			deliveredLive = true
		}
	}
	t.Logf("after disarm: event %d pending=%t dispatched_at=%q delivered_live=%t",
		held, after.BusPending, after.DispatchedAt, deliveredLive)

	finalCtx, finalCancel := context.WithTimeout(ctx, 5*time.Minute)
	final := waitRunTerminal(t, finalCtx, fe.httpAPI, member.HTTPBase(), job.ID, run.ID)
	finalCancel()

	writeFaultRecord(t, fe, "bus_publish_pause", map[string]any{
		"run_id":           run.ID,
		"control_dir":      dir,
		"directive":        directive,
		"held_sequence":    held,
		"held_type":        row.Type,
		"row_while_held":   row,
		"row_after_disarm": after,
		"hook_evidence":    hookEvidence,
		"delivered_live":   deliveredLive,
		"final_status":     final.Status,
		"members_armed":    len(live),
	})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func applyFaultFixture(t *testing.T, fe *faultEnv, member cluster.Member, kind string, holdSeconds int) (cluster.Job, string) {
	t.Helper()
	ctx := context.Background()
	alias := fmt.Sprintf("robustness-%s-%s", kind, strings.ReplaceAll(uuid.NewString(), "-", "")[:8])
	def := faultFixtureDefinition(alias, fe.env.TaskImage, holdSeconds)
	if err := def.Validate(); err != nil {
		t.Fatalf("fixture %s invalid: %v", alias, err)
	}
	if err := fe.httpAPI.Apply(ctx, member.HTTPBase(), []jobdef.Definition{def}); err != nil {
		t.Fatalf("apply fixture %s: %v", alias, err)
	}
	job, err := fe.httpAPI.JobByAlias(ctx, member.HTTPBase(), alias)
	if err != nil {
		t.Fatalf("read applied job %s: %v", alias, err)
	}
	return job, alias
}

// faultFixtureDefinition is a two-step job whose tasks report a start and a
// completion, each with a fresh nonce, to the runner's effect sink. Unlike
// B1's owner-crash fixture it does not block on the sink barrier: these
// scenarios need a run that completes on its own.
//
// holdSeconds is how long each task stays running before it completes. The
// partition case needs a task still executing when the cut goes in, so that the
// worker's completion report to the owner's discovered dispatch address is
// forced across it; the other cases keep it short.
func faultFixtureDefinition(alias, taskImage string, holdSeconds int) jobdef.Definition {
	if holdSeconds <= 0 {
		holdSeconds = 2
	}
	script := func(step string) string {
		return fmt.Sprintf(`set -eu
RECORDER=%q
STEP=%q
NONCE="$(cat /proc/sys/kernel/random/uuid)"
wget -qO- --header='Content-Type: application/json' --post-data="{\"run_id\":\"${CAESIUM_RUN_ID}\",\"step\":\"${STEP}\",\"nonce\":\"${NONCE}\",\"event\":\"start\"}" "${RECORDER}/start"
sleep %d
wget -qO- --header='Content-Type: application/json' --post-data="{\"run_id\":\"${CAESIUM_RUN_ID}\",\"step\":\"${STEP}\",\"nonce\":\"${NONCE}\",\"event\":\"complete\"}" "${RECORDER}/effect"
`, cluster.RecorderURL(), step, holdSeconds)
	}
	return jobdef.Definition{
		APIVersion: jobdef.APIVersionV1,
		Kind:       jobdef.KindJob,
		Metadata: jobdef.Metadata{
			Alias:  alias,
			Labels: map[string]string{"caesium-robustness": "faults"},
		},
		Trigger: jobdef.Trigger{
			Type:          jobdef.TriggerHTTP,
			Configuration: map[string]any{"path": "robustness-faults-" + alias},
		},
		Steps: []jobdef.Step{
			{
				Name:    firstStep,
				Type:    jobdef.StepTypeTask,
				Engine:  jobdef.EngineKubernetes,
				Image:   taskImage,
				Command: []string{"sh", "-c", script(firstStep)},
				Next:    []string{secondStep},
			},
			{
				Name:      secondStep,
				Type:      jobdef.StepTypeTask,
				Engine:    jobdef.EngineKubernetes,
				Image:     taskImage,
				Command:   []string{"sh", "-c", script(secondStep)},
				DependsOn: []string{firstStep},
			},
		},
	}
}

func waitRunTerminal(t *testing.T, ctx context.Context, h *cluster.HTTP, base, jobID, runID string) cluster.Run {
	t.Helper()
	var final cluster.Run
	if err := cluster.Poll(ctx, 2*time.Second, func() (bool, error) {
		got, err := h.GetRun(ctx, base, jobID, runID)
		if err != nil {
			return false, nil
		}
		switch strings.ToLower(strings.TrimSpace(got.Status)) {
		case "succeeded", "failed", "cancelled", "skipped":
			final = got
			return true, nil
		}
		return false, nil
	}); err != nil {
		t.Fatalf("run %s did not reach a terminal state: %v", runID, err)
	}
	return final
}

func deliveredForRun(events []recorder.SSEEvent, runID string) []history.Delivered {
	var out []history.Delivered
	for _, ev := range events {
		if ev.RunID != runID {
			continue
		}
		out = append(out, history.Delivered{
			Sequence: ev.Sequence,
			Type:     ev.Type,
			RunID:    ev.RunID,
			TaskID:   ev.TaskID,
			ConnGen:  ev.ConnGen,
		})
	}
	return out
}

func effectsForRun(events []recorder.Event, runID string) []history.Effect {
	var out []history.Effect
	for _, ev := range events {
		if ev.RunID != runID {
			continue
		}
		if ev.Kind != "start" && ev.Kind != "complete" {
			continue
		}
		out = append(out, history.Effect{
			RunID: ev.RunID,
			Step:  ev.Step,
			Nonce: ev.Nonce,
			Kind:  ev.Kind,
		})
	}
	return out
}

func otherMember(topo cluster.Topology, not cluster.Member) cluster.Member {
	for _, m := range topo.Members {
		if m.Name != not.Name {
			return m
		}
	}
	return not
}

func writeFaultRecord(t *testing.T, fe *faultEnv, key string, record map[string]any) {
	t.Helper()
	if err := cluster.WriteRecords(context.Background(), fe.kube, fe.env.Namespace, key, record); err != nil {
		t.Fatalf("inconclusive: persist %s records: %v", key, err)
	}
}

func jsonString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

func firstNonEmptyStr(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
