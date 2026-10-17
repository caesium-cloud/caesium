import { useState } from "react";
import { Link } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { parse } from "yaml";
import { Button } from "@/components/ui/button";
import { InsufficientAccess } from "@/features/auth/InsufficientAccess";
import { canReadConnectors, isInsufficientAccessError } from "@/features/auth/access";
import { api } from "@/lib/api";
import { usePrincipal } from "@/lib/auth";
import { ObservationTime } from "./ConnectionsPage";
import { connectorRefreshSchedule, type ConnectorConnection, type ExternalDelegation } from "./types";

const linkClass = "text-cyan hover:underline";

export function RelationshipsPanel({ connection, executionId, observationStale }: { connection: ConnectorConnection; executionId: string; observationStale: boolean }) {
  const principal = usePrincipal();
  const [cursor, setCursor] = useState<string>();
  const [pageSize, setPageSize] = useState(25);
  const refresh = connectorRefreshSchedule(connection);
  const params = { cursor, page_size: pageSize };
  const supported = connection.capabilities.includes("relationships");
  const relationships = useQuery({
    queryKey: ["connectors", principal.subject, connection.id, executionId, "relationships", params],
    queryFn: ({ signal }) => api.getConnectorRelationships(connection.id, executionId, params, signal),
    enabled: canReadConnectors(principal) && connection.enabled && supported,
    retry: false, refetchInterval: refresh.interval, refetchOnWindowFocus: false,
  });
  if (!canReadConnectors(principal) || isInsufficientAccessError(relationships.error)) return <InsufficientAccess message="You do not have permission to read execution relationships." />;
  const stale = observationStale || !!relationships.error;
  return <section aria-label="External relationships" data-testid="external-relationships" className="space-y-4 rounded-md border border-border p-4 text-sm">
    <div className="flex flex-wrap items-center justify-between gap-3"><h2 className="text-base text-text-1">External relationships</h2>{supported && <div className="flex items-center gap-2"><label htmlFor="connector-relationships-page-size">Requested relationships page size</label><select id="connector-relationships-page-size" className="h-9 rounded-md border border-border bg-background px-3" value={pageSize} onChange={event => { setPageSize(Number(event.target.value)); setCursor(undefined); }}>{[1, 5, 10, 25, 50, 100].map(size => <option key={size}>{size}</option>)}</select></div>}</div>
    {!supported ? <p className="text-text-3">Relationships unavailable: this capability is not advertised.</p> : <>
      {relationships.isPending && <p role="status">Loading relationships…</p>}
      {relationships.error && <p role="alert" className="text-warning">Relationships unavailable. {relationships.data ? "Previously read relationships are stale." : "No relationship page is available."} {refresh.interval === false ? "Automatic provider refresh is disabled for this configured interval." : "Automatic refresh will retry."}</p>}
      {relationships.data && <>
        {stale && <p role="status" className="text-warning">Stale relationships describe previous evidence; current workflow state and admission outcomes may have changed.</p>}
        <p className="text-xs text-text-3">Last successful relationship read <ObservationTime value={new Date(relationships.dataUpdatedAt).toISOString()} />. Relationships retain exact Run IDs.</p>
        {relationships.data.incomplete && <p role="alert" className="text-warning">Partial relationship page. Other relationships or unverified delegations may be unavailable. This page does not prove their absence.</p>}
        <div className="space-y-2"><h3 className="text-text-1">Workflow relationships</h3>
          <ul className="space-y-2">{relationships.data.relationships.map(relation => <li key={`${relation.type}:${relation.id}`} data-testid="external-relationship" data-relationship-type={relation.type} className="rounded-md border border-border p-3">
            <p>{relation.type === "parent" ? "Parent" : relation.type === "child" ? "Child" : relation.type === "continuation" ? "Continuation" : `Reported relationship (${relation.type})`}</p>
            <Link className={`${linkClass} break-all`} to="/connectors/$connectionId/executions/$executionId" params={{ connectionId: relation.reference.connection_id, executionId: relation.id }}>{relation.reference.coordinates.workflow_id} · {relation.reference.coordinates.run_id}</Link>
            {relation.event_id !== undefined && <p className="text-xs text-text-3">Source event #{relation.event_id}</p>}
          </li>)}</ul>
          {!relationships.data.relationships.length && <p className="text-text-3">No workflow relationships reported on this page.</p>}
          <p className="text-xs text-text-3">Parent, child and continuation runs are distinct executions. A continuation link can refer to the previous or next run.</p>
        </div>
        <section aria-label="Verified Caesium delegations" data-testid="verified-delegations" className="space-y-2"><h3 className="text-text-1">Verified Caesium delegations</h3>
          <ul className="space-y-3">{relationships.data.correlations.map(delegation => <DelegationRow key={delegation.id} delegation={delegation} />)}</ul>
          {!relationships.data.correlations.length && <p className="text-text-3">No verified Caesium delegations returned on this page. External work has no inferred Caesium run, dataset lineage or freshness evidence.</p>}
          <p className="text-xs text-text-3">Verification joins trusted activity identity to one local admission. It does not prove data materialization, freshness, cache coverage or reproducibility. Retry attempts and queue promotion retain the same admission.</p>
        </section>
        <div className="flex gap-3"><Button variant="outline" disabled={!cursor} onClick={() => setCursor(undefined)}>First relationships page</Button><Button variant="outline" disabled={!relationships.data.cursor || relationships.data.cursor === cursor || relationships.isFetching || !!relationships.error} onClick={() => setCursor(relationships.data!.cursor)}>Next relationships</Button></div>
        <p className="text-xs text-text-3">{relationships.data.cursor ? "More relationships available." : relationships.data.incomplete ? "Relationships are incomplete; no continuation cursor was supplied." : "End of reported relationships."}</p>
      </>}
    </>}
  </section>;
}

function DelegationRow({ delegation }: { delegation: ExternalDelegation }) {
  return <li data-testid="verified-delegation" data-correlation-id={delegation.id} data-admission-id={delegation.admission_id} className="space-y-2 rounded-md border border-border p-3">
    <p className="break-all">Delegated Caesium admission · activity {delegation.activity_id} · {delegation.activity_type}</p>
    <dl className="grid gap-1 sm:grid-cols-[140px_minmax(0,1fr)]">
      <dt className="text-text-3">Verified admission</dt><dd className="break-all font-mono">{delegation.admission_id}</dd>
      <dt className="text-text-3">Job ID</dt><dd className="break-all font-mono">{delegation.job_id}</dd>
      <dt className="text-text-3">Outcome</dt><dd data-testid="delegation-outcome">{delegation.outcome}</dd>
      {delegation.queue_id && <><dt className="text-text-3">Queue ID</dt><dd className="break-all font-mono" data-testid="delegation-queue-id">{delegation.queue_id}</dd></>}
      {delegation.run_id && <><dt className="text-text-3">Run ID</dt><dd className="break-all font-mono" data-testid="delegation-run-id">{delegation.run_id}</dd></>}
      <dt className="text-text-3">Binding version</dt><dd>{delegation.binding_version}</dd>
      <dt className="text-text-3">Verified</dt><dd><ObservationTime value={delegation.verified_at} /></dd>
      <dt className="text-text-3">Source event</dt><dd>#{delegation.source_event_id}</dd>
    </dl>
    {delegation.run_id ? <Link data-testid="delegated-run-link" className={`${linkClass} break-all`} to="/jobs/$jobId/runs/$runId" params={{ jobId: delegation.job_id, runId: delegation.run_id }}>Open Caesium run {delegation.run_id}</Link> : <p className="text-text-3">No admitted run is available for this admission.</p>}
  </li>;
}

export function RunExternalExecutions({ jobId, runId }: { jobId: string; runId: string }) {
  const principal = usePrincipal();
  const allowed = canReadConnectors(principal);
  const features = useQuery({ queryKey: ["system-features"], queryFn: api.getSystemFeatures, enabled: allowed, staleTime: 0, retry: false, refetchInterval: 60_000 });
  const enabled = allowed && features.data?.connectors_enabled === true && !features.error;
  const links = useQuery({
    queryKey: ["connectors", principal.subject, "run-links", jobId, runId],
    queryFn: ({ signal }) => api.getRunExternalExecutions(jobId, runId, signal),
    enabled, retry: false, refetchInterval: 30_000, refetchOnWindowFocus: false,
  });
  // Local job/run permissions remain independent. Never disclose cached external
  // identities after the current principal loses connector access.
  if (!allowed || isInsufficientAccessError(features.error) || isInsufficientAccessError(links.error) || !features.data?.connectors_enabled) return null;
  return <section aria-label="External execution links" data-testid="run-external-executions" className="space-y-3 rounded-md border border-border p-4 text-sm">
    <h2 className="text-base text-text-1">External execution links</h2>
    {(features.error || links.error || !features.data.connectors_ready) && <p role="alert" className="text-warning">External execution links unavailable. {links.data ? "Previously read links are stale." : "Local run evidence remains available."}</p>}
    {links.isPending && enabled && <p role="status">Loading external execution links…</p>}
    {links.data && <>
      <ul className="space-y-2">{links.data.executions.map(link => <li key={link.id} className="space-y-1 rounded-md border border-border p-3" data-correlation-id={link.id}>
        <p className="break-all">Verified delegation · {link.connection_id} · activity {link.activity_id}</p>
        {link.execution_id && link.reference?.connection_id === link.connection_id ? <Link data-testid="external-execution-link" className={`${linkClass} break-all`} to="/connectors/$connectionId/executions/$executionId" params={{ connectionId: link.connection_id, executionId: link.execution_id }}>Open external execution {link.connection_id} · activity {link.activity_id}</Link> : <p className="text-text-3">External execution identity is unavailable; this historical admission cannot be opened.</p>}
        <p className="text-xs text-text-3">Admission {link.admission_id} · verified <ObservationTime value={link.verified_at} /></p>
      </li>)}</ul>
      {!links.data.executions.length && <p className="text-text-3">No verified external execution links returned for this run.</p>}
      <p className="text-xs text-text-3">Historical links retain the original external run. A removed or disabled connection may prevent inspection. Local run evidence keeps its existing permissions.</p>
    </>}
  </section>;
}

interface DeclaredDataset { name: string; direction: "produces" | "consumes" | "source"; step?: string }

function readDeclarations(manifest: string): DeclaredDataset[] {
  const definition = parse(manifest, { maxAliasCount: 100 }) as {
    metadata?: { datasets?: { sources?: { name: string }[] } };
    steps?: { name: string; datasets?: { produces?: { name: string }[]; consumes?: (string | { name: string })[] } }[];
  };
  if (!definition || !Array.isArray(definition.steps)) throw new Error("Invalid job manifest");
  const declarations: DeclaredDataset[] = [];
  for (const source of definition.metadata?.datasets?.sources ?? []) declarations.push({ name: source.name, direction: "source" });
  for (const step of definition.steps) {
    for (const produced of step.datasets?.produces ?? []) declarations.push({ name: produced.name, direction: "produces", step: step.name });
    for (const consumed of step.datasets?.consumes ?? []) declarations.push({ name: typeof consumed === "string" ? consumed : consumed.name, direction: "consumes", step: step.name });
  }
  if (declarations.some(declaration => typeof declaration.name !== "string" || !declaration.name.trim())) throw new Error("Invalid dataset declaration");
  return declarations;
}

export function RunDataEvidence({ jobId, freshnessEnabled, onOpenReceipt }: { jobId: string; freshnessEnabled: boolean; onOpenReceipt: () => void }) {
  const principal = usePrincipal();
  const declarations = useQuery({ queryKey: ["job", principal.subject, jobId, "current-dataset-declarations"], queryFn: async () => readDeclarations(await api.getJobManifest(jobId)), retry: false, staleTime: 30_000 });
  return <section aria-label="Caesium data evidence" data-testid="run-data-evidence" className="space-y-3 rounded-md border border-border p-4 text-sm">
    <h2 className="text-base text-text-1">Caesium data evidence</h2>
    <p className="text-text-3">Inspect this run's observed task logs, outputs, data assertions and reproducibility receipt through the existing run views.</p>
    <div className="flex flex-wrap gap-4"><a className={linkClass} href="#run-interactive-dag-section">Task logs, outputs and data assertions</a><a className={linkClass} href="#run-reproducibility-section" onClick={onOpenReceipt}>Run receipt</a></div>
    <h3 className="text-text-1">Current job dataset declarations</h3>
    <p className="text-xs text-text-3">These declarations come from the current job manifest, which may differ from the definition used by this run. A declaration alone does not prove materialization, lineage, freshness, cache coverage or reproducibility.</p>
    {declarations.isPending && <p role="status">Loading current dataset declarations…</p>}
    {declarations.error && <p role="alert" className="text-warning">Current dataset declarations unavailable. The manifest could not be read or parsed.</p>}
    {!declarations.error && declarations.data && <>
      <ul className="space-y-2">{declarations.data.map((declaration, index) => <li key={`${declaration.direction}:${declaration.step}:${declaration.name}:${index}`} data-testid="run-dataset-declaration" className="space-y-1">
        <p>{declaration.step ? `${declaration.step} · ` : ""}{declaration.direction} · {declaration.name}</p>
        {freshnessEnabled && !principal.isScoped && principal.scopeKnown && <Link data-testid="declared-dataset-link" className={linkClass} to="/datasets" search={{ name: declaration.name, namespace: undefined, status: undefined }}>Inspect dataset {declaration.name}</Link>}
      </li>)}</ul>
      {!declarations.data.length && <p className="text-text-3">No dataset declarations in the current job manifest.</p>}
    </>}
    {principal.isScoped && <p className="text-xs text-text-3">Dataset views require their own permission. This scoped credential retains its permitted local run logs, outputs, assertions and receipt.</p>}
  </section>;
}
