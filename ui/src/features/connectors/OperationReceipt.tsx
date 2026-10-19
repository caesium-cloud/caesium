import { useRef, useState } from "react";
import { Button } from "@/components/ui/button";
import { usePrincipal } from "@/lib/auth";
import type { ConnectorOperation } from "./types";

interface Props {
  operation: ConnectorOperation;
  allowResend: boolean;
  refreshing: boolean;
  receiptUnavailable: boolean;
  onRefresh: () => Promise<ConnectorOperation | undefined>;
  onResubmit: (revision: number) => Promise<void>;
}

const stateLabels: Record<string, string> = {
  submitted: "Submitted — awaiting acceptance",
  accepted: "Accepted — awaiting completion",
  completed: "Completed",
  rejected: "Rejected",
  not_dispatched: "Not dispatched",
  unknown: "Unknown — outcome unresolved",
};

export function OperationReceipt({ operation, allowResend, refreshing, receiptUnavailable, onRefresh, onResubmit }: Props) {
  const principal = usePrincipal();
  const [reviewedRevision, setReviewedRevision] = useState<number>();
  const [confirmed, setConfirmed] = useState(false);
  const [busy, setBusy] = useState(false);
  const sending = useRef(false);
  const originalPrincipal = !!principal.stableId && principal.stableId === operation.actor.stable_id && principal.kind === operation.actor.principal_kind;
  const canResend = originalPrincipal && allowResend && operation.state === "unknown" && !receiptUnavailable;
  const reviewing = canResend && reviewedRevision === operation.revision;
  const coordinates = operation.execution?.coordinates;

  const review = async () => {
    setConfirmed(false);
    setReviewedRevision(undefined);
    const fresh = await onRefresh();
    if (fresh?.id === operation.id && fresh.state === "unknown") setReviewedRevision(fresh.revision);
  };
  const resend = async () => {
    if (sending.current || !reviewing || !confirmed || refreshing) return;
    sending.current = true;
    setBusy(true);
    setConfirmed(false);
    setReviewedRevision(undefined);
    try { await onResubmit(operation.revision); }
    finally { sending.current = false; setBusy(false); }
  };

  return <article aria-label={`Operation ${operation.id}`} data-testid="operation-receipt" data-operation-id={operation.id} data-state={operation.state} data-revision={operation.revision} data-attempt={operation.attempt} className="space-y-3 rounded-md border border-border p-4 text-sm">
    <h3 className="break-all font-mono text-text-1">Operation {operation.id}</h3>
    <p role="status" data-testid="operation-state" className={operation.state === "unknown" || operation.state === "rejected" || operation.state === "not_dispatched" ? "text-warning" : "text-text-2"}>{stateLabels[operation.state] ?? `Unrecognized receipt state: ${operation.state}`}</p>
    {operation.state === "submitted" && <p>The request is stored. Provider acceptance has not been established.</p>}
    {operation.state === "accepted" && <p>The provider accepted this Update. Its completion and business result are still pending.</p>}
    {operation.state === "unknown" && <p>The outcome remains unresolved. A timeout or missing lookup does not prove rejection, success, or absence of effects. Refresh only looks up this Update; it never resends it.</p>}
    {operation.state === "rejected" && <p>The provider supplied definitive rejection evidence. This receipt is terminal and will not be resent.</p>}
    {operation.state === "not_dispatched" && <p>Initial preflight refused this request before an Update was dispatched. This is not business rejection.</p>}
    <dl className="grid gap-2 sm:grid-cols-[150px_minmax(0,1fr)]">
      <dt className="text-text-3">Action</dt><dd>{operation.action}</dd>
      <dt className="text-text-3">Connection</dt><dd>{operation.connection_id}</dd>
      <dt className="text-text-3">Execution ID</dt><dd className="break-all font-mono">{operation.execution_id}</dd>
      {coordinates && <><dt className="text-text-3">Workflow ID</dt><dd className="break-all font-mono">{coordinates.workflow_id}</dd><dt className="text-text-3">Run ID</dt><dd className="break-all font-mono">{coordinates.run_id}</dd></>}
      <dt className="text-text-3">Binding</dt><dd>{operation.binding_name} · version {operation.binding_version}</dd>
      <dt className="text-text-3">Original principal</dt><dd className="break-all">{operation.actor.principal_kind} · {operation.actor.subject ?? operation.actor.stable_id} · {operation.actor.stable_id}</dd>
      <dt className="text-text-3">External Update ID</dt><dd className="break-all font-mono">{operation.external_update_id}</dd>
      <dt className="text-text-3">Revision / attempt</dt><dd>{operation.revision} / {operation.attempt}</dd>
      {operation.evidence?.native_stage && <><dt className="text-text-3">Provider stage</dt><dd>{operation.evidence.native_stage}</dd></>}
      {operation.evidence?.failure_kind && <><dt className="text-text-3">Failure evidence</dt><dd>{operation.evidence.failure_kind}</dd></>}
      {operation.evidence?.rejection_proof && <><dt className="text-text-3">Rejection evidence</dt><dd>{operation.evidence.rejection_proof}</dd></>}
    </dl>
    {operation.evidence?.failure_kind === "failure_origin_unknown" && <p className="text-warning">The provider returned a failure whose origin is unknown. This does not establish validator rejection.</p>}
    {operation.state === "completed" && <div><h4>Validated result</h4>{operation.evidence?.result !== undefined ? <pre data-testid="operation-result" className="mt-2 overflow-x-auto whitespace-pre-wrap break-all rounded bg-surface-2 p-3">{JSON.stringify(operation.evidence.result, null, 2)}</pre> : <p>{operation.evidence?.result_omitted || operation.metadata_omitted ? "The validated result was omitted to fit the response budget. Completion remains established." : "No result body is present in this receipt."}</p>}</div>}
    {operation.metadata_omitted && <p className="text-warning">Descriptive metadata was omitted to fit the response budget. The immutable execution ID above remains the target.</p>}
    {receiptUnavailable && <p role="alert" className="text-warning">Receipt refresh unavailable. This is the last stored observation; it cannot authorize a resend.</p>}
    <div className="flex flex-wrap gap-3"><Button variant="outline" disabled={refreshing || busy} onClick={() => { setConfirmed(false); setReviewedRevision(undefined); void onRefresh(); }}>Refresh receipt</Button>{originalPrincipal && operation.state === "unknown" && <Button variant="outline" disabled={!canResend || refreshing || busy} onClick={() => void review()}>Review resend</Button>}</div>
    {operation.state === "unknown" && !originalPrincipal && <p className="text-text-3">Only the original principal can confirm a resend of this operation.</p>}
    {reviewing && <div className="space-y-3 rounded border border-warning p-3">
      <p className="text-warning">This may deliver the original request again. It keeps the original principal, target, input and Update ID. Eligibility remains the remote validator's decision.</p>
      <label className="flex items-start gap-2"><input type="checkbox" checked={confirmed} disabled={busy || refreshing} onChange={event => setConfirmed(event.target.checked)} /><span>I confirm resending operation {operation.id} at revision {operation.revision} to its original run.</span></label>
      <Button disabled={!confirmed || busy || refreshing} onClick={() => void resend()}>Confirm resend</Button>
    </div>}
  </article>;
}
