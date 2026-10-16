import { useEffect, useState, type ReactNode } from "react";
import { Link, useNavigate, useSearch } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { Button } from "@/components/ui/button";
import { EmptyState } from "@/components/ui/empty-state";
import { InsufficientAccess } from "@/features/auth/InsufficientAccess";
import { canReadConnectors, isInsufficientAccessError } from "@/features/auth/access";
import { api } from "@/lib/api";
import { usePrincipal } from "@/lib/auth";
import { connectorRefreshSchedule, observationAge, type ConnectorSearch } from "./types";

const fieldClass = "h-9 rounded-md border border-border bg-background px-3 text-sm text-text-1";

export function ConnectorGate({ children }: { children: ReactNode }) {
  const principal = usePrincipal();
  const mayRead = canReadConnectors(principal);
  const features = useQuery({ queryKey: ["system-features"], queryFn: api.getSystemFeatures, enabled: mayRead, staleTime: 0, retry: false, refetchInterval: 60_000 });
  if (!mayRead || isInsufficientAccessError(features.error)) return <InsufficientAccess message="Connections require an unscoped cluster viewer or higher." />;
  if (features.isPending) return <p role="status" className="p-6 text-text-3">Checking connector availability…</p>;
  if (features.error) return <EmptyState title="Connections unavailable" subtitle="Server features could not be read. Try again when the server is reachable." />;
  if (!features.data?.connectors_enabled) return <EmptyState title="Connections disabled" subtitle="Execution connectors are not enabled on this server." />;
  if (!features.data.connectors_ready) return <EmptyState title="Connections unavailable" subtitle="Connector configuration is not ready. Local job health is reported separately." />;
  return children;
}

export function ObservationTime({ value }: { value: string }) {
  const [now, setNow] = useState(Date.now);
  useEffect(() => { const timer = window.setInterval(() => setNow(Date.now()), 1000); return () => window.clearInterval(timer); }, []);
  const valid = Number.isFinite(Date.parse(value));
  return <span>{valid ? <><time dateTime={value} title={value}>{new Date(value).toISOString()}</time> · {observationAge(value, now)}</> : "Observation time unavailable"}</span>;
}

export function ConnectionsPage() {
  return <ConnectorGate><ConnectionBrowser /></ConnectorGate>;
}

function ConnectionBrowser() {
  const principal = usePrincipal();
  const navigate = useNavigate();
  const search = useSearch({ strict: false }) as ConnectorSearch;
  const connections = useQuery({ queryKey: ["connectors", principal.subject, "connections"], queryFn: ({ signal }) => api.getConnectorConnections(signal), retry: false, refetchInterval: 60_000 });
  const connection = connections.data?.connections.find(c => c.id === search.connection) ?? (!search.connection ? connections.data?.connections.find(c => c.enabled) : undefined);
  const refresh = connectorRefreshSchedule(connection);
  const params = { workflow_type: search.workflow_type, cursor: search.cursor, page_size: search.page_size ?? 25 };
  const executions = useQuery({
    queryKey: ["connectors", principal.subject, connection?.id, "executions", params],
    queryFn: ({ signal }) => api.getConnectorExecutions(connection!.id, params, signal),
    enabled: !!connection?.enabled && connection.capabilities.includes("discovery"),
    retry: false, refetchInterval: refresh.interval, refetchOnWindowFocus: false,
  });
  const update = (next: ConnectorSearch) => void navigate({ to: "/connectors", search: next });
  if (isInsufficientAccessError(connections.error) || isInsufficientAccessError(executions.error)) return <InsufficientAccess message="You do not have permission to read connections." />;
  if (connections.isPending) return <p role="status" className="p-6 text-text-3">Loading connections…</p>;
  if (connections.error || !connections.data?.ready) return <EmptyState title="Connections unavailable" subtitle="Connector configuration could not be read. This view retries automatically." />;
  return <div className="space-y-6 p-6">
    <div><h1 className="text-xl text-text-1">Connections</h1><p className="mt-1 text-sm text-text-3">Inspect external workflow runs alongside Caesium. Discovery can lag behind direct observations.</p></div>
    <form key={search.workflow_type ?? ""} className="flex flex-wrap items-end gap-3" onSubmit={event => { event.preventDefault(); const workflowType = String(new FormData(event.currentTarget).get("workflow_type") ?? ""); update({ ...search, connection: connection?.id, workflow_type: workflowType.trim() || undefined, cursor: undefined }); }}>
      <label className="flex flex-col gap-1 text-sm">Connection<select className={fieldClass} value={connection?.id ?? ""} onChange={event => update({ ...search, connection: event.target.value, cursor: undefined })}>
        <option value="" disabled>Select a connection</option>{connections.data.connections.map(c => <option key={c.id} value={c.id}>{c.id} · {c.provider} · {c.scope}{c.enabled ? "" : " (disabled)"}</option>)}
      </select></label>
      <label className="flex flex-col gap-1 text-sm">Workflow type<input name="workflow_type" className={fieldClass} defaultValue={search.workflow_type ?? ""} placeholder="All workflow types" /></label>
      <label className="flex flex-col gap-1 text-sm">Executions per page<select className={fieldClass} value={params.page_size} onChange={event => update({ ...search, page_size: Number(event.target.value), cursor: undefined })}>{[1, 5, 10, 25, 50, 100].map(size => <option key={size}>{size}</option>)}</select></label>
      <Button type="submit" variant="outline">Apply filters</Button>
    </form>
    {!connection ? <EmptyState title="No connection selected" subtitle="Select a configured connection to discover executions." /> : !connection.enabled ? <EmptyState title="Connection disabled" subtitle="This connection does not make provider requests." /> : <>
      <section aria-label="Connection health" className="rounded-md border border-border p-4 text-sm">
        <h2 className="text-text-1">{connection.id} · {connection.provider} · {connection.scope}</h2>
        <p className="mt-2 text-text-2">Configuration ready · {executions.error ? "Provider observation unavailable" : executions.data ? "Provider observation available" : "Provider observation pending"}</p>
        <p className="mt-1 text-text-3">Observation health reflects this connection's discovery request. {refresh.description}</p>
        <p className="mt-1 text-text-3">Server capabilities: {connection.capabilities.join(", ") || "None"}</p>
      </section>
      {!connection.capabilities.includes("discovery") ? <EmptyState title="Discovery unavailable" subtitle="This connection does not advertise discovery." /> : <>
        {executions.isPending && <p role="status">Loading executions…</p>}
        {executions.error && <p role="alert" className="text-warning">Discovery unavailable. {executions.data ? "The previous discovery page is stale; its dated observations are retained below." : "No execution observations are available."} {refresh.interval === false ? "Automatic provider refresh is disabled for this configured interval." : "Automatic refresh will retry."}</p>}
        {executions.data?.incomplete && <p role="alert" className="text-warning">Partial discovery page. Metadata was truncated; reduce the page size to inspect the omitted entries.</p>}
        {executions.data && <>
          <div className="overflow-x-auto"><table className="w-full text-left text-sm"><thead className="border-b border-border text-text-3"><tr><th className="p-2">Workflow / Run ID</th><th className="p-2">Workflow type</th><th className="p-2">Native status</th><th className="p-2">Observed</th></tr></thead><tbody className="divide-y divide-border">
            {executions.data.executions.map(execution => <tr key={execution.id} data-testid="external-execution-row"><td className="max-w-sm break-all p-2"><Link className="text-cyan hover:underline" to="/connectors/$connectionId/executions/$executionId" params={{ connectionId: connection.id, executionId: execution.id }}>{execution.metadata.workflow_id}</Link><div className="mt-1 font-mono text-xs text-text-3">{execution.metadata.run_id}</div></td><td className="p-2">{execution.metadata.workflow_type}</td><td className="p-2">{execution.metadata.native_status}<div className="text-xs text-text-3">Display: {execution.metadata.display_status}</div></td><td className="p-2 text-xs text-text-3"><ObservationTime value={execution.observed_at} />{execution.completeness !== "complete" && <div>Partial metadata</div>}</td></tr>)}
          </tbody></table></div>
          {executions.data.executions.length === 0 && <EmptyState title="No executions on this page" subtitle="Adjust the workflow type filter. Visibility may take time to discover new workflows." />}
          <div className="flex gap-3"><Button variant="outline" disabled={!search.cursor} onClick={() => update({ ...search, cursor: undefined })}>First executions page</Button><Button variant="outline" disabled={!executions.data.cursor || executions.data.cursor === search.cursor || executions.isFetching || !!executions.error} onClick={() => update({ ...search, connection: connection.id, cursor: executions.data!.cursor })}>Next executions</Button></div>
          <p className="text-xs text-text-3">{executions.data.cursor ? "More executions available." : executions.data.incomplete ? "Discovery is incomplete; no continuation cursor was supplied." : "End of this discovery result."} Unbound workflows are included. Workflow inputs and results are unavailable in this metadata view.</p>
        </>}
      </>}
    </>}
  </div>;
}
