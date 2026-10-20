import { useEffect, useRef, useState, type FormEvent } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Button } from "@/components/ui/button";
import { canReadConnectors } from "@/features/auth/access";
import { api } from "@/lib/api";
import { usePrincipal } from "@/lib/auth";
import { OperationReceipt } from "./OperationReceipt";
import { connectorRefreshMs, connectorRefreshSchedule, observationTimestamp, type ConnectorAction, type ConnectorBinding, type ConnectorConnection, type ConnectorOperation, type ExternalExecution } from "./types";

interface Props {
  connection: ConnectorConnection;
  execution: ExternalExecution;
  executionId: string;
  observationStale: boolean;
}

function isOpen(operation: ConnectorOperation) {
  return ["submitted", "accepted", "unknown"].includes(operation.state);
}

export function ActionPanel({ connection, execution, executionId, observationStale }: Props) {
  const principal = usePrincipal();
  const client = useQueryClient();
  const [now, setNow] = useState(Date.now);
  const [cursor, setCursor] = useState<string>();
  const [selected, setSelected] = useState<string>();
  const [message, setMessage] = useState<string>();
  const [uncertainReply, setUncertainReply] = useState(false);
  const [mutationBusy, setMutationBusy] = useState(false);
  const sending = useRef(false);
  const readAllowed = canReadConnectors(principal);
  const authorized = readAllowed && (principal.role === "operator" || principal.role === "admin");
  const schedule = connectorRefreshSchedule(connection);
  useEffect(() => { const timer = window.setInterval(() => setNow(Date.now()), 1000); return () => window.clearInterval(timer); }, []);
  const observed = observationTimestamp(execution.observed_at);
  const live = !observationStale && execution.availability === "available" && observed !== undefined && now - observed <= 2 * connectorRefreshMs(connection);
  const binding = execution.binding;
  const exactTarget = execution.id === executionId && execution.reference.connection_id === connection.id && !!execution.metadata.workflow_id && !!execution.metadata.run_id && execution.reference.coordinates.workflow_id === execution.metadata.workflow_id && execution.reference.coordinates.run_id === execution.metadata.run_id;
  const action = binding?.actions.find(value => value.name === "approve_publication");
  const compatible = exactTarget && !!binding?.version && binding.name === execution.metadata.workflow_type && binding.available && !!action?.available;
  const actionCapable = connection.enabled && connection.capabilities.includes("action_submission");
  const receiptCapable = readAllowed && connection.enabled && connection.capabilities.includes("receipt_lookup");
  const prefix = ["connectors", principal.subject, principal.kind, principal.stableId, connection.id, executionId, "actions"];
  const list = useQuery({ queryKey: [...prefix, "operations", cursor], queryFn: ({ signal }) => api.getConnectorOperations(connection.id, executionId, { cursor, page_size: 10 }, signal), enabled: receiptCapable, retry: false, refetchInterval: schedule.interval, refetchOnWindowFocus: false });
  const operations = list.data?.operations.filter(value => value.connection_id === connection.id && value.execution_id === executionId) ?? [];
  const selectedId = selected ?? operations[0]?.id;
  const receiptKey = [...prefix, "receipt", selectedId];
  // A just-returned submission/conflict already carries the current server receipt.
  // Display it until the configured refresh, while fresh tabs fetch immediately.
  const receipt = useQuery({ queryKey: receiptKey, staleTime: connectorRefreshMs(connection), queryFn: ({ signal }) => api.getConnectorOperation(connection.id, selectedId!, signal), enabled: receiptCapable && !!selectedId, retry: false, refetchInterval: query => query.state.data && !isOpen(query.state.data.operation) ? false : schedule.interval, refetchOnWindowFocus: false });
  const current = receipt.data?.operation;
  const matchingReceipt = current?.connection_id === connection.id && current.execution_id === executionId && current.id === selectedId ? current : undefined;
  const open = operations.some(isOpen) || !!matchingReceipt && isOpen(matchingReceipt);
  const actionRoleAllowed = action?.minimum_role === "operator" ? authorized : action?.minimum_role === "admin" && principal.role === "admin";
  const allowAction = authorized && actionRoleAllowed && actionCapable && receiptCapable && live && compatible && !execution.metadata.terminal;
  const allowSubmit = allowAction && !open && !mutationBusy && !uncertainReply && list.isSuccess && !list.isFetching && !receipt.error;
  const allowResend = allowAction && !!matchingReceipt && matchingReceipt.binding_name === binding?.name && matchingReceipt.binding_version === binding?.version && matchingReceipt.action === action?.name && (!matchingReceipt.execution || (matchingReceipt.execution.connection_id === execution.reference.connection_id && Object.keys(matchingReceipt.execution.coordinates).length === Object.keys(execution.reference.coordinates).length && Object.entries(matchingReceipt.execution.coordinates).every(([key, value]) => execution.reference.coordinates[key] === value)));

  const recover = async () => {
    const result = await list.refetch();
    if (!result.isError) {
      setUncertainReply(false);
      const openReceipt = result.data?.operations.find(isOpen);
      if (openReceipt) setSelected(openReceipt.id);
    }
  };
  const refreshReceipt = async () => {
    const result = await receipt.refetch();
    return result.isError ? undefined : result.data?.operation;
  };
  const remember = (result: Awaited<ReturnType<typeof api.submitConnectorAction>>) => {
    if (result.operation.connection_id !== connection.id || result.operation.execution_id !== executionId) throw new Error("Receipt target mismatch");
    client.setQueryData([...prefix, "receipt", result.operation.id], result);
    setSelected(result.operation.id);
    setCursor(undefined);
    setUncertainReply(false);
    setMessage(result.conflict ? "Another request already owns this action. Recovered its existing receipt; this request did not create another Update." : result.pending ? "The request is pending. Its receipt is authoritative; no completed outcome has been established." : "The server stored this operation. Read its receipt for the provider outcome.");
    void client.invalidateQueries({ queryKey: [...prefix, "operations"] });
  };
  const submit = async (input: Record<string, unknown>) => {
    if (!allowSubmit || !binding || !action || sending.current) return;
    sending.current = true;
    setMutationBusy(true);
    setMessage(undefined);
    try {
      remember(await api.submitConnectorAction(connection.id, executionId, action.name, { binding_name: binding.name, binding_version: binding.version, input }, crypto.randomUUID()));
    } catch {
      setUncertainReply(true);
      setMessage("Submission reply unavailable or refused. This does not establish success, rejection, or absence of effects. Refresh receipts to recover server state before another submission.");
      void client.invalidateQueries({ queryKey: [...prefix, "operations"] });
    } finally { sending.current = false; setMutationBusy(false); }
  };
  const resubmit = async (revision: number) => {
    if (!allowResend || !matchingReceipt || matchingReceipt.state !== "unknown" || revision !== matchingReceipt.revision || sending.current) return;
    sending.current = true;
    setMutationBusy(true);
    setMessage(undefined);
    try { remember(await api.resubmitConnectorOperation(connection.id, matchingReceipt.id, revision)); }
    catch {
      setMessage("Resend reply unavailable or refused. The outcome remains unresolved. Refresh the receipt; another resend requires a new explicit confirmation.");
      void refreshReceipt();
    } finally { sending.current = false; setMutationBusy(false); }
  };

  if (!readAllowed) return null;
  return <section aria-label="Declared actions" data-testid="connector-action-panel" className="space-y-4 rounded-md border border-border p-4 text-sm">
    <h2 className="text-base text-text-1">Declared actions</h2>
    <p>Actions target this exact workflow run and binding version. The remote validator decides business eligibility.</p>
    <dl className="grid gap-2 sm:grid-cols-[150px_minmax(0,1fr)]"><dt className="text-text-3">Workflow ID</dt><dd className="break-all font-mono">{execution.metadata.workflow_id}</dd><dt className="text-text-3">Run ID</dt><dd className="break-all font-mono">{execution.metadata.run_id}</dd><dt className="text-text-3">Binding</dt><dd>{binding ? `${binding.name} · version ${binding.version}` : "No declared binding"}</dd></dl>
    {!authorized && <p className="text-warning">Approval requires an unscoped operator or administrator.</p>}
    {!actionCapable && <p className="text-warning">Action submission is not advertised by this connection.</p>}
    {!receiptCapable && <p className="text-warning">Receipt lookup is unavailable; actions cannot be recovered safely.</p>}
    {!live && <p className="text-warning">Actions are disabled until a fresh live execution observation is available.</p>}
    {!compatible && <p className="text-warning">No available compatible approval binding is declared for this exact run.</p>}
    {execution.metadata.terminal && <p className="text-warning">This run is terminal. Approval cannot target a newer run.</p>}
    {open && <p className="text-warning">An operation is still open for this run. Recover its receipt before taking another action.</p>}
    {binding && action && <ApprovalForm key={`${connection.id}:${executionId}:${binding.name}:${binding.version}:${JSON.stringify(action.input_schema)}`} action={action} binding={binding} disabled={!allowSubmit} onSubmit={submit} />}
    {message && <p role="status" className="text-warning">{message}</p>}
    <section aria-label="Operation receipts" className="space-y-3">
      <div className="flex flex-wrap items-center justify-between gap-3"><h3 className="text-base text-text-1">Operation receipts</h3><Button variant="outline" disabled={!receiptCapable || list.isFetching || mutationBusy} onClick={() => void recover()}>Refresh receipts</Button></div>
      <p className="text-text-3">Recovered from the server, including after refresh or in a new tab. Listing and polling never submit or resend an Update.</p>
      {receiptCapable && list.isPending && <p role="status">Loading operation receipts…</p>}
      {list.error && <p role="alert" className="text-warning">Operation list unavailable. Previously listed receipts may be stale.</p>}
      {list.isSuccess && !operations.length && <p>No stored operations on this page.</p>}
      {!!operations.length && <ul className="space-y-2">{operations.map(value => <li key={value.id}><button className="break-all text-left text-cyan hover:underline" aria-pressed={selectedId === value.id} onClick={() => setSelected(value.id)}>Open receipt {value.id}</button><span className="ml-2 text-text-3">{value.state} · {value.action}</span></li>)}</ul>}
      {selectedId && receipt.isPending && <p role="status">Loading receipt…</p>}
      {selectedId && receipt.error && !matchingReceipt && <div className="space-y-2"><p role="alert" className="text-warning">Receipt unavailable. No provider outcome has been established.</p><Button variant="outline" disabled={!receiptCapable || receipt.isFetching} onClick={() => void refreshReceipt()}>Refresh receipt</Button></div>}
      {matchingReceipt && <OperationReceipt key={`${matchingReceipt.id}:${allowResend && !mutationBusy}:${!!receipt.error}`} operation={matchingReceipt} allowResend={allowResend && !mutationBusy} refreshing={receipt.isFetching} receiptUnavailable={!!receipt.error} onRefresh={refreshReceipt} onResubmit={resubmit} />}
      <div className="flex gap-3"><Button variant="outline" disabled={!cursor || list.isFetching || mutationBusy} onClick={() => { setCursor(undefined); setSelected(undefined); }}>First receipts</Button><Button variant="outline" disabled={!list.data?.cursor || list.data.cursor === cursor || list.isFetching || !!list.error || mutationBusy} onClick={() => { setCursor(list.data?.cursor); setSelected(undefined); }}>Next receipts</Button></div>
    </section>
  </section>;
}

function ApprovalForm({ action, binding, disabled, onSubmit }: { action: ConnectorAction; binding: ConnectorBinding; disabled: boolean; onSubmit: (input: Record<string, unknown>) => Promise<void> }) {
  const [values, setValues] = useState<Record<string, string>>({});
  const [error, setError] = useState<string>();
  const fields = Object.entries(action.input_schema.properties ?? {}).filter(([name]) => name !== "_caesium_actor");
  const send = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (disabled) return;
    const input: Record<string, unknown> = {};
    try {
      for (const [name, schema] of fields) {
        const raw = values[name] ?? "";
        if (raw === "" && !action.input_schema.required?.includes(name)) continue;
        if (schema.enum || schema.type !== "string") input[name] = JSON.parse(raw);
        else input[name] = raw;
      }
    } catch { setError("Enter valid JSON for structured fields, and a value for each required field."); return; }
    setError(undefined);
    void onSubmit(input);
  };
  return <form aria-label="Approval form" className="space-y-3" onSubmit={send}>
    <p className="text-text-3">{binding.display_name || binding.name} · approve_publication · minimum role {action.minimum_role}. Input is validated against the declared schema by the server.</p>
    {fields.map(([name, schema]) => {
      const id = `connector-action-${name}`;
      const required = action.input_schema.required?.includes(name) ?? false;
      const update = (value: string) => setValues(previous => ({ ...previous, [name]: value }));
      return <div key={name} className="space-y-1"><label htmlFor={id} className="block">{name}</label>
        {schema.enum || schema.type === "boolean" ? <select id={id} disabled={disabled} required={required} className="w-full rounded-md border border-border bg-background p-2" value={values[name] ?? ""} onChange={event => update(event.target.value)}><option value="">Select a value</option>{(schema.enum ?? [true, false]).map((value, index) => <option key={index} value={JSON.stringify(value)}>{typeof value === "string" ? value : JSON.stringify(value)}</option>)}</select> : <textarea id={id} disabled={disabled} required={required} minLength={schema.type === "string" ? schema.minLength : undefined} maxLength={schema.type === "string" ? schema.maxLength : undefined} className="w-full rounded-md border border-border bg-background p-2" value={values[name] ?? ""} onChange={event => update(event.target.value)} aria-describedby={`${id}-help`} />}
        <p id={`${id}-help`} className="text-xs text-text-3">{schema.description}{schema.type === "string" ? "" : " Enter JSON matching this declared field."}{required ? " Required." : " Optional."}</p>
      </div>;
    })}
    {error && <p role="alert" className="text-warning">{error}</p>}
    <Button type="submit" disabled={disabled}>Approve publication</Button>
  </form>;
}
