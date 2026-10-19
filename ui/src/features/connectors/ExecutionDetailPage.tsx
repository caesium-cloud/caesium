import { Link, useNavigate, useParams, useSearch } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { Button } from "@/components/ui/button";
import { EmptyState } from "@/components/ui/empty-state";
import { InsufficientAccess } from "@/features/auth/InsufficientAccess";
import { isInsufficientAccessError } from "@/features/auth/access";
import { api, ApiError } from "@/lib/api";
import { usePrincipal } from "@/lib/auth";
import { ConnectorGate, ObservationTime } from "./ConnectionsPage";
import { ActionPanel } from "./ActionPanel";
import { RelationshipsPanel } from "./RelationshipsPanel";
import { connectorRefreshSchedule, temporalExecutionURL, type ConnectorPageQuery } from "./types";

export function ExecutionDetailPage() {
  return <ConnectorGate><ExecutionObservation /></ConnectorGate>;
}

function ExecutionObservation() {
  const { connectionId, executionId } = useParams({ strict: false }) as { connectionId: string; executionId: string };
  const search = useSearch({ strict: false }) as ConnectorPageQuery;
  const navigate = useNavigate();
  const principal = usePrincipal();
  const connections = useQuery({ queryKey: ["connectors", principal.subject, "connections"], queryFn: ({ signal }) => api.getConnectorConnections(signal), retry: false, refetchInterval: 60_000 });
  const connection = connections.data?.connections.find(c => c.id === connectionId);
  const refresh = connectorRefreshSchedule(connection);
  const retryDescription = refresh.interval === false ? "Automatic provider refresh is disabled for this configured interval." : "Automatic refresh will retry.";
  const enabled = !!connection?.enabled && connection.capabilities.includes("inspection");
  const detail = useQuery({ queryKey: ["connectors", principal.subject, connectionId, executionId, "detail"], queryFn: ({ signal }) => api.getConnectorExecution(connectionId, executionId, signal), enabled, retry: false, refetchInterval: refresh.interval, refetchOnWindowFocus: false });
  const historyParams = { cursor: search.cursor, page_size: search.page_size ?? 25 };
  const history = useQuery({ queryKey: ["connectors", principal.subject, connectionId, executionId, "history", historyParams], queryFn: ({ signal }) => api.getConnectorHistory(connectionId, executionId, historyParams, signal), enabled: enabled && connection.capabilities.includes("history"), retry: false, refetchInterval: refresh.interval, refetchOnWindowFocus: false });
  if (isInsufficientAccessError(connections.error) || isInsufficientAccessError(detail.error) || isInsufficientAccessError(history.error)) return <InsufficientAccess message="You do not have permission to inspect this execution." />;
  if (connections.isPending) return <p role="status" className="p-6">Loading connection…</p>;
  if (connections.error) return <EmptyState title="Connection unavailable" subtitle="The connector configuration could not be read. Automatic refresh will retry." />;
  if (!enabled) return <EmptyState title="Inspection unavailable" subtitle="This connection is missing, disabled, or does not advertise inspection." />;
  if (detail.error instanceof ApiError && detail.error.status === 404) return <EmptyState title="Execution not found" subtitle="This exact workflow run could not be found. A newer run is a separate execution." />;
  const execution = detail.data;
  const stale = !!detail.error || execution?.availability !== "available";
  const remoteURL = execution ? temporalExecutionURL(connection, execution) : undefined;
  const changePage = (next: ConnectorPageQuery) => void navigate({ to: "/connectors/$connectionId/executions/$executionId", params: { connectionId, executionId }, search: next });
  return <div className="space-y-6 p-6">
    <Link to="/connectors" search={{ connection: connectionId }} className="text-sm text-cyan hover:underline">← Connections</Link>
    <div className="flex flex-wrap items-start justify-between gap-4"><div><h1 className="text-xl text-text-1">External execution</h1><p className="mt-1 text-sm text-text-3">{connectionId} · {connection.provider} · {connection.scope}</p></div>{remoteURL && <a className="text-sm text-cyan hover:underline" href={remoteURL} target="_blank" rel="noopener noreferrer">Open in Temporal UI ↗</a>}</div>
    {detail.isPending && <p role="status">Loading execution…</p>}
    <Button variant="outline" disabled={detail.isFetching} onClick={() => void detail.refetch()}>Refresh observation</Button>
    {!execution && detail.error && <p role="alert" className="text-warning">Execution unavailable. No cached observation is available. {retryDescription}</p>}
    {execution && <>
      <section aria-label="Execution observation" className="space-y-3 rounded-md border border-border p-4 text-sm">
        <p role="status" data-testid="execution-availability" className={stale ? "text-warning" : "text-text-2"}>{stale ? "Stale observation — refresh unavailable" : "Live provider observation"}</p>
        {stale && <p className="text-warning">Cached details describe a previous observation. They do not establish the workflow's current state. {retryDescription}</p>}
        {(execution.completeness !== "complete" || execution.metadata.incomplete) && <p role="alert" className="text-warning">Partial execution metadata. The reported activity list may be incomplete.</p>}
        <dl className="grid gap-3 sm:grid-cols-[150px_minmax(0,1fr)]">
          <dt className="text-text-3">Workflow ID</dt><dd className="break-all font-mono" data-testid="external-workflow-id">{execution.metadata.workflow_id}</dd>
          <dt className="text-text-3">Run ID</dt><dd className="break-all font-mono" data-testid="external-run-id">{execution.metadata.run_id}</dd>
          <dt className="text-text-3">Workflow type</dt><dd>{execution.metadata.workflow_type}</dd>
          <dt className="text-text-3">Native status</dt><dd data-testid="external-native-status">{execution.metadata.native_status}</dd>
          <dt className="text-text-3">Display status</dt><dd>{execution.metadata.display_status}</dd>
          <dt className="text-text-3">Observed</dt><dd><ObservationTime value={execution.observed_at} /></dd>
          <dt className="text-text-3">Source event</dt><dd>{execution.metadata.source_event_id}</dd>
        </dl>
        <p className="text-xs text-text-3">{refresh.description} Each Run ID remains a distinct execution. Workflow inputs/results and undeclared payloads are unavailable in this metadata view.</p>
      </section>
      <section aria-label="Reported activities" className="space-y-3"><h2 className="text-base text-text-1">Reported pending activities</h2>
        {execution.metadata.pending_activities?.length ? <div className="overflow-x-auto"><table className="w-full text-left text-sm"><thead className="text-text-3"><tr><th className="p-2">Activity ID</th><th className="p-2">Type</th><th className="p-2">Reported attempt</th><th className="p-2">Native state</th></tr></thead><tbody>{execution.metadata.pending_activities.map(activity => <tr key={activity.activity_id}><td className="break-all p-2">{activity.activity_id}</td><td className="p-2">{activity.activity_type}</td><td className="p-2">{activity.attempt}{activity.maximum_attempts ? ` / ${activity.maximum_attempts}` : ""}</td><td className="p-2">{activity.state}</td></tr>)}</tbody></table></div> : <p className="text-sm text-text-3">No pending activity metadata reported.</p>}
      </section>
    </>}
    {execution && <ActionPanel key={`${connectionId}:${executionId}:${principal.kind}:${principal.subject}:${principal.stableId}`} connection={connection} execution={execution} executionId={executionId} observationStale={stale} />}
    {execution && <RelationshipsPanel key={`${connectionId}:${executionId}`} connection={connection} executionId={executionId} observationStale={stale} />}
    <section aria-label="Execution history" className="space-y-3">
      <div className="flex flex-wrap items-center justify-between gap-3"><h2 className="text-base text-text-1">History metadata</h2><div className="flex items-center gap-2 text-sm"><label htmlFor="connector-history-events-per-page">Requested history page size</label><select id="connector-history-events-per-page" aria-describedby="connector-history-page-size-help" className="h-9 rounded-md border border-border bg-background px-3" value={historyParams.page_size} onChange={event => changePage({ page_size: Number(event.target.value) })}>{[1, 5, 10, 25, 50, 100].map(size => <option key={size}>{size}</option>)}</select></div></div>
      <p id="connector-history-page-size-help" className="text-xs text-text-3">Temporal returns native event batches, so a page may contain more or fewer events than requested.</p>
      {!connection.capabilities.includes("history") ? <p className="text-sm text-text-3">History unavailable: this capability is not advertised.</p> : <>
        {history.isPending && <p role="status">Loading history…</p>}
        {history.error && <p role="alert" className="text-warning">History unavailable. {history.data ? "Previously observed history is stale." : "No history page is available."} {retryDescription}</p>}
        {history.data && <>
          <p className="text-xs text-text-3">History observed <ObservationTime value={history.data.observed_at} />. Event attributes and payloads are unavailable.</p>
          {history.data.incomplete && <p role="alert" className="text-warning">Partial history page. Some events are unavailable at the configured limits. This page does not prove complete history.</p>}
          <ol className="divide-y divide-border rounded-md border border-border">{history.data.events?.map(event => <li key={event.event_id} data-testid="external-history-event" className="flex gap-3 p-3 text-sm"><span className="font-mono text-text-3">#{event.event_id}</span><span>{event.event_type}</span></li>)}</ol>
          {!history.data.events?.length && <p className="text-sm text-text-3">No event metadata on this page.</p>}
          {!!history.data.activities?.length && <div className="text-sm text-text-2"><h3>Reported activity attempts on this page</h3>{history.data.activities.map(activity => <p key={activity.event_id}>Event #{activity.event_id} · {activity.activity_id || `Scheduled event #${activity.scheduled_event_id}`} · {activity.activity_type || "Type unavailable on this page"} · attempt {activity.attempt}</p>)}</div>}
          {!!history.data.problems?.length && <div className="text-sm text-warning"><h3>Workflow-task problems (separate from workflow status)</h3>{history.data.problems.map(problem => <p key={problem.event_id}>Event #{problem.event_id} · {problem.cause}</p>)}</div>}
          <div className="flex gap-3"><Button variant="outline" disabled={!search.cursor} onClick={() => changePage({ page_size: historyParams.page_size })}>First history page</Button><Button variant="outline" disabled={!history.data.cursor || history.data.cursor === search.cursor || history.isFetching || !!history.error} onClick={() => changePage({ ...search, cursor: history.data!.cursor })}>Next history</Button></div>
          <p className="text-xs text-text-3">{history.data.cursor ? "More history available." : history.data.incomplete ? "History is incomplete; no continuation cursor was supplied." : "End of reported history."}</p>
        </>}
      </>}
    </section>
    <p className="text-xs text-text-3">Server capabilities: {connection.capabilities.join(", ") || "None"}. Queries are never invoked automatically. Actions require explicit submission. Unbound workflows remain inspectable.</p>
  </div>;
}
