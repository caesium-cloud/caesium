import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { api, ApiError, type JobRun } from "@/lib/api";
import type { PrincipalState } from "@/lib/auth";
import { RunDetailPage } from "@/features/jobs/RunDetailPage";
import { ExecutionDetailPage } from "../ExecutionDetailPage";
import { RelationshipsPanel, RunDataEvidence, RunExternalExecutions } from "../RelationshipsPanel";
import type { ConnectorConnection, ExternalDelegation, ExternalRelationshipsPage } from "../types";

const { principal } = vi.hoisted(() => ({ principal: { kind: "api_key", subject: "viewer", role: "viewer", isScoped: false, scopeKnown: true, canRunner: false } as PrincipalState }));
vi.mock("@/lib/auth", () => ({ usePrincipal: () => principal, withAuthHeaders: () => ({}) }));
vi.mock("@tanstack/react-router", () => ({
  useSearch: () => ({ cursor: "history-only-cursor", page_size: 5 }),
  useParams: () => ({ connectionId: "fixture", executionId: "http-execution", jobId: "job-1", runId: "local-run" }),
  useNavigate: () => vi.fn(),
  Link: ({ children, to, params, search, ...props }: { children: ReactNode; to: string; params?: Record<string, string>; search?: Record<string, string | undefined>; "data-testid"?: string }) => {
    const path = to.replace(/\$(\w+)/g, (_, key: string) => encodeURIComponent(params?.[key] ?? ""));
    const query = Object.fromEntries(Object.entries(search ?? {}).filter((entry): entry is [string, string] => entry[1] !== undefined));
    return <a href={`${path}${search ? `?${new URLSearchParams(query)}` : ""}`} {...props}>{children}</a>;
  },
}));
vi.mock("@/hooks/useDagHeight", () => ({ useDagHeight: () => [vi.fn(), 300] }));
vi.mock("@/lib/events", () => ({ events: { isHealthy: () => true, subscribe: vi.fn(), unsubscribe: vi.fn(), subscribeConnection: vi.fn(), unsubscribeConnection: vi.fn() } }));
vi.mock("@/features/jobs/JobDAG", () => ({ JobDAG: ({ dag, taskDefinitions, atoms, onNodeClick }: { dag: { nodes: { id: string }[] }; taskDefinitions: Record<string, { name: string }>; atoms: Record<string, unknown>; onNodeClick: (taskId: string) => void }) => <div data-testid="local-dag" data-atom-count={Object.keys(atoms).length}><pre>{JSON.stringify(atoms)}</pre>{dag.nodes.map(node => <button key={node.id} onClick={() => onNodeClick(node.id)}>{taskDefinitions[node.id]?.name} node</button>)}</div> }));
vi.mock("@/features/jobs/LogViewer", () => ({ LogViewer: ({ jobId, runId, taskId }: { jobId: string; runId: string; taskId: string }) => <p data-testid="local-log-target">{jobId}/{runId}/{taskId}</p> }));
vi.mock("@/features/jobs/RunTimeline", () => ({ RunTimeline: () => null }));
vi.mock("@/features/jobs/RunProposalSummary", () => ({ RunProposalSummary: () => null }));
vi.mock("@/features/jobs/RunPicker", () => ({ RunPicker: () => null }));
vi.mock("@/features/jobs/JobRunNavigation", () => ({ JobRunNavigation: () => null }));
vi.mock("@/features/jobs/ReceiptPanel", () => ({ ReceiptPanel: () => <p>Existing run receipt</p> }));

const connection: ConnectorConnection = { id: "fixture", provider: "temporal", scope: "default", enabled: true, capabilities: ["inspection", "history", "relationships"], console_refresh_ms: 15_000 };
const delegation: ExternalDelegation = {
  id: "stable-correlation", opaque_id: "catalog-digest-not-a-route", execution_id: "http-execution", reference: { connection_id: "fixture", coordinates: { workflow_id: "publication", run_id: "external-run" } },
  connection_id: "fixture", activity_id: "start-job", activity_type: "caesium.start", relation_type: "delegation", binding_version: "1", admission_id: "stable-admission", job_id: "job-1", idempotency_key: "external-run/start-job", outcome: "queued", queue_id: "same-queue", source_event_id: 12, verified_at: "2026-10-17T12:00:00Z",
};
const page: ExternalRelationshipsPage = { relationships: [], correlations: [delegation], incomplete: false };
const baseline: JobRun = { id: "local-run", job_id: "job-1", status: "succeeded", tasks: [], started_at: "2026-10-17T12:00:00Z", created_at: "2026-10-17T12:00:00Z", updated_at: "2026-10-17T12:00:01Z" };

function show(children: ReactNode, prepare?: (client: QueryClient) => void) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  prepare?.(client);
  const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>;
  return { ...render(children, { wrapper }), client };
}

beforeEach(() => {
  vi.restoreAllMocks();
  Object.assign(principal, { kind: "api_key", subject: "viewer", role: "viewer", isScoped: false, scopeKnown: true, canRunner: false });
  vi.spyOn(api, "getSystemFeatures").mockResolvedValue({ database_console_enabled: false, log_console_enabled: false, agent_remediation_enabled: false, freshness_enabled: true, contract_enforcement_enabled: false, connectors_enabled: true, connectors_ready: true });
  vi.spyOn(api, "getConnectorConnections").mockResolvedValue({ connections: [connection], ready: true, fingerprint: "epoch" });
  vi.spyOn(api, "getConnectorRelationships").mockResolvedValue(page);
  vi.spyOn(api, "getRunExternalExecutions").mockResolvedValue({ executions: [{ ...delegation, run_id: "local-run", outcome: "created" }] });
  vi.spyOn(api, "getJobManifest").mockResolvedValue("apiVersion: v1\nkind: Job\nmetadata:\n  alias: publish\nsteps:\n  - name: publish\n    datasets:\n      produces:\n        - name: publication.output\n      consumes:\n        - source.input\n");
});

describe("execution relationships", () => {
  it("renders native relationship kinds with each exact workflow/run identity", async () => {
    vi.mocked(api.getConnectorRelationships).mockResolvedValue({ ...page, relationships: ["parent", "child", "continuation"].map(type => ({ type, id: `exact-${type}`, reference: { connection_id: "fixture", coordinates: { workflow_id: `${type}-workflow`, run_id: `${type}-run` } }, event_id: 13 })) });
    show(<RelationshipsPanel connection={connection} executionId="http-execution" observationStale={false} />);
    const rows = await screen.findAllByTestId("external-relationship");
    expect(rows.map(row => row.getAttribute("data-relationship-type"))).toEqual(["parent", "child", "continuation"]);
    for (const type of ["parent", "child", "continuation"]) expect(screen.getByRole("link", { name: `${type}-workflow · ${type}-run` })).toHaveAttribute("href", `/connectors/fixture/executions/exact-${type}`);
    expect(screen.getByText(/previous or next run/)).toBeVisible();
  });

  it("retains one verified admission and queue identity as promotion adds the admitted run", async () => {
    const { client } = show(<RelationshipsPanel connection={connection} executionId="http-execution" observationStale={false} />);
    const queued = await screen.findByTestId("verified-delegation");
    expect(queued).toHaveAttribute("data-correlation-id", "stable-correlation");
    expect(queued).toHaveAttribute("data-admission-id", "stable-admission");
    expect(screen.getByTestId("delegation-outcome")).toHaveTextContent("queued");
    expect(screen.queryByTestId("delegated-run-link")).not.toBeInTheDocument();
    expect(screen.getByText("No admitted run is available for this admission.")).toBeVisible();
    vi.mocked(api.getConnectorRelationships).mockResolvedValue({ ...page, correlations: [{ ...delegation, run_id: "local-run", outcome: "created" }] });
    await client.invalidateQueries({ queryKey: ["connectors", "viewer", "fixture", "http-execution", "relationships"] });
    const runLink = await screen.findByRole("link", { name: "Open Caesium run local-run" });
    expect(runLink).toHaveAttribute("href", "/jobs/job-1/runs/local-run");
    expect(screen.getAllByTestId("verified-delegation")).toHaveLength(1);
    expect(screen.getByTestId("verified-delegation")).toHaveAttribute("data-admission-id", "stable-admission");
    expect(screen.getByTestId("delegation-queue-id")).toHaveTextContent("same-queue");
  });

  it("paginates independently of history and resets its cursor on page size changes", async () => {
    vi.spyOn(api, "getConnectorExecution").mockResolvedValue({ id: "http-execution", reference: { connection_id: "fixture", coordinates: {} }, availability: "available", completeness: "complete", observed_at: delegation.verified_at, metadata: { workflow_type: "publication", workflow_id: "publication", run_id: "external-run", scope: "default", source_event_id: 1, native_status: "RUNNING", display_status: "running", terminal: false, failure: false } });
    vi.spyOn(api, "getConnectorHistory").mockResolvedValue({ workflow_id: "publication", run_id: "external-run", scope: "default", observed_at: delegation.verified_at, events: [] });
    vi.mocked(api.getConnectorRelationships).mockResolvedValue({ ...page, incomplete: true, cursor: "relationships-next" });
    show(<ExecutionDetailPage />);
    const next = await screen.findByRole("button", { name: "Next relationships" });
    await waitFor(() => expect(next).toBeEnabled());
    expect(api.getConnectorHistory).toHaveBeenCalledWith("fixture", "http-execution", { cursor: "history-only-cursor", page_size: 5 }, expect.any(AbortSignal));
    expect(api.getConnectorRelationships).toHaveBeenLastCalledWith("fixture", "http-execution", { cursor: undefined, page_size: 25 }, expect.any(AbortSignal));
    fireEvent.click(next);
    await waitFor(() => expect(api.getConnectorRelationships).toHaveBeenLastCalledWith("fixture", "http-execution", { cursor: "relationships-next", page_size: 25 }, expect.any(AbortSignal)));
    fireEvent.change(screen.getByLabelText("Requested relationships page size", { exact: true }), { target: { value: "1" } });
    await waitFor(() => expect(api.getConnectorRelationships).toHaveBeenLastCalledWith("fixture", "http-execution", { cursor: undefined, page_size: 1 }, expect.any(AbortSignal)));
  });

  it("shows partial empty results without manufacturing local evidence or asserting absence", async () => {
    vi.mocked(api.getConnectorRelationships).mockResolvedValue({ relationships: [], correlations: [], incomplete: true });
    show(<RelationshipsPanel connection={connection} executionId="opaque-work" observationStale={false} />);
    expect(await screen.findByText(/This page does not prove their absence/)).toBeVisible();
    expect(screen.getByText(/External work has no inferred Caesium run/)).toBeVisible();
    expect(screen.queryByRole("link")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Next relationships" })).toBeDisabled();
    expect(screen.queryByText("End of reported relationships.")).not.toBeInTheDocument();
  });

  it("does not request an unadvertised relationship capability", async () => {
    show(<RelationshipsPanel connection={{ ...connection, capabilities: ["inspection"] }} executionId="http-execution" observationStale={false} />);
    expect(screen.getByText(/Relationships unavailable: this capability is not advertised/)).toBeVisible();
    expect(api.getConnectorRelationships).not.toHaveBeenCalled();
  });

  it("marks retained evidence stale on refresh failure and hides it after authorization denial", async () => {
    const { client } = show(<RelationshipsPanel connection={connection} executionId="http-execution" observationStale={false} />);
    expect(await screen.findByText("stable-admission")).toBeVisible();
    vi.mocked(api.getConnectorRelationships).mockRejectedValue(new ApiError(503, "unavailable"));
    await client.invalidateQueries({ queryKey: ["connectors", "viewer", "fixture", "http-execution", "relationships"] });
    expect(await screen.findByText(/Stale relationships describe previous evidence/)).toBeVisible();
    expect(screen.getByText("stable-admission")).toBeVisible();
    vi.mocked(api.getConnectorRelationships).mockRejectedValue(new ApiError(403, "forbidden", "insufficient_access"));
    await client.invalidateQueries({ queryKey: ["connectors", "viewer", "fixture", "http-execution", "relationships"] });
    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("permission"));
    expect(screen.queryByText("stable-admission")).not.toBeInTheDocument();
  });
});

describe("local run external links and data evidence", () => {
  it("uses the server's HTTP execution identity and preserves historical links without connection discovery", async () => {
    show(<RunExternalExecutions jobId="job-1" runId="local-run" />);
    expect(await screen.findByRole("link", { name: "Open external execution fixture · activity start-job" })).toHaveAttribute("href", "/connectors/fixture/executions/http-execution");
    expect(screen.getByText(/removed or disabled connection may prevent inspection/)).toBeVisible();
    expect(api.getConnectorConnections).not.toHaveBeenCalled();
  });

  it("never invents a route from the catalog digest when HTTP reference fields are unavailable", async () => {
    vi.mocked(api.getRunExternalExecutions).mockResolvedValue({ executions: [{ ...delegation, execution_id: undefined, reference: undefined }] });
    show(<RunExternalExecutions jobId="job-1" runId="local-run" />);
    expect(await screen.findByText(/historical admission cannot be opened/)).toBeVisible();
    expect(screen.queryByRole("link")).not.toBeInTheDocument();
  });

  it("retains stale reverse links on outage and hides them after a current authorization refusal", async () => {
    const { client } = show(<RunExternalExecutions jobId="job-1" runId="local-run" />);
    expect(await screen.findByTestId("external-execution-link")).toBeVisible();
    vi.mocked(api.getRunExternalExecutions).mockRejectedValue(new ApiError(503, "unavailable"));
    await client.invalidateQueries({ queryKey: ["connectors", "viewer", "run-links", "job-1", "local-run"] });
    expect(await screen.findByRole("alert")).toHaveTextContent("Previously read links are stale");
    expect(screen.getByTestId("external-execution-link")).toBeVisible();
    vi.mocked(api.getRunExternalExecutions).mockRejectedValue(new ApiError(403, "forbidden", "insufficient_access"));
    await client.invalidateQueries({ queryKey: ["connectors", "viewer", "run-links", "job-1", "local-run"] });
    await waitFor(() => expect(screen.queryByTestId("run-external-executions")).not.toBeInTheDocument());
  });

  it("does not reuse another principal's cached external identity", async () => {
    const { rerender } = show(<RunExternalExecutions jobId="job-1" runId="local-run" />);
    expect(await screen.findByTestId("external-execution-link")).toBeVisible();
    principal.subject = "other-viewer";
    vi.mocked(api.getRunExternalExecutions).mockResolvedValue({ executions: [] });
    rerender(<RunExternalExecutions jobId="job-1" runId="local-run" />);
    await waitFor(() => expect(screen.queryByTestId("external-execution-link")).not.toBeInTheDocument());
    expect(await screen.findByText("No verified external execution links returned for this run.")).toBeVisible();
  });

  it.each([{ isScoped: true }, { scopeKnown: false }, { kind: "agent_session" }, { role: null }])("does not request or disclose external links for denied principal %s", async denied => {
    Object.assign(principal, denied);
    const { container } = show(<RunExternalExecutions jobId="job-1" runId="local-run" />);
    expect(container).toBeEmptyDOMElement();
    expect(api.getRunExternalExecutions).not.toHaveBeenCalled();
    expect(api.getSystemFeatures).not.toHaveBeenCalled();
  });

  it("keeps gate-off runs independent of connector requests", async () => {
    vi.mocked(api.getSystemFeatures).mockResolvedValue({ connectors_enabled: false } as Awaited<ReturnType<typeof api.getSystemFeatures>>);
    const { container } = show(<RunExternalExecutions jobId="job-1" runId="local-run" />);
    await waitFor(() => expect(api.getSystemFeatures).toHaveBeenCalled());
    expect(container).toBeEmptyDOMElement();
    expect(api.getRunExternalExecutions).not.toHaveBeenCalled();
  });

  it("labels current declarations separately and links to the existing dataset evidence view", async () => {
    const openReceipt = vi.fn();
    show(<RunDataEvidence jobId="job-1" freshnessEnabled onOpenReceipt={openReceipt} />);
    expect(await screen.findByRole("link", { name: "Inspect dataset publication.output" })).toHaveAttribute("href", "/datasets?name=publication.output");
    expect(screen.getByText(/may differ from the definition used by this run/)).toBeVisible();
    expect(screen.getByText(/A declaration alone does not prove/)).toBeVisible();
    fireEvent.click(screen.getByRole("link", { name: "Run receipt" }));
    expect(openReceipt).toHaveBeenCalledOnce();
  });

  it("reports manifest fetch and parse failure without claiming there are no declarations", async () => {
    vi.mocked(api.getJobManifest).mockResolvedValue("invalid: [yaml");
    show(<RunDataEvidence jobId="job-1" freshnessEnabled onOpenReceipt={vi.fn()} />);
    expect(await screen.findByRole("alert")).toHaveTextContent("manifest could not be read or parsed");
    expect(screen.queryByText("No dataset declarations in the current job manifest.")).not.toBeInTheDocument();
  });

  it.each([{ scoped: true, warmCatalog: false, namespaceOnly: false }, { scoped: false, warmCatalog: false, namespaceOnly: false }, { scoped: false, warmCatalog: true, namespaceOnly: false }, { scoped: false, warmCatalog: true, namespaceOnly: true }])("keeps local evidence usable after global denials (scoped=$scoped, warm catalog=$warmCatalog, namespace only=$namespaceOnly)", async ({ scoped, warmCatalog, namespaceOnly }) => {
    principal.isScoped = scoped;
    if (namespaceOnly) Object.assign(principal, { kind: "user", role: null });
    vi.mocked(api.getSystemFeatures).mockRejectedValue(new ApiError(403, "insufficient permissions", "insufficient_access"));
    vi.spyOn(api, "getJob").mockResolvedValue({ id: "job-1", alias: "publish", labels: {}, annotations: {}, trigger_id: "trigger-1", paused: false, created_at: baseline.created_at, updated_at: baseline.updated_at });
    vi.spyOn(api, "getJobRun").mockResolvedValue({ ...baseline, tasks: [{ id: "task-run-1", job_run_id: baseline.id, task_id: "task-1", atom_id: "atom-1", engine: "docker", image: "alpine:3.23", command: "echo", status: "succeeded", created_at: baseline.created_at, updated_at: baseline.updated_at, output: { rows: "42", processed_through: "2026-10-17T12:00:00Z" } }] });
    vi.spyOn(api, "getReceipt").mockRejectedValue(new Error("No receipt"));
    vi.spyOn(api, "getJobDAG").mockResolvedValue({ job_id: "job-1", nodes: [{ id: "task-1", atom_id: "atom-1" }], edges: [] });
    vi.spyOn(api, "getJobTasks").mockResolvedValue([{ id: "task-1", job_id: "job-1", atom_id: "atom-1", name: "work", retries: 0, retry_delay: 0, retry_backoff: false, trigger_rule: "all_success", created_at: baseline.created_at, updated_at: baseline.updated_at }]);
    vi.spyOn(api, "getJobRuns").mockResolvedValue({ runs: [baseline], total: 1, nextOffset: null });
    vi.spyOn(api, "getAtoms").mockRejectedValue(new ApiError(403, "insufficient permissions", "insufficient_access"));
    // A known namespace-only principal must not expose retained cluster data
    // while waiting for a denial, or even issue the global request.
    if (namespaceOnly) vi.mocked(api.getAtoms).mockImplementation(() => new Promise(() => {}));
    show(<RunDetailPage />, client => {
      if (warmCatalog) client.setQueryData(["atoms"], [{ id: "atom-1", engine: "docker", image: "alpine:3.23", command: "retained-global-command-secret", spec: { token: "retained-global-spec-secret" }, created_at: baseline.created_at, updated_at: baseline.updated_at }]);
    });
    const evidence = await screen.findByTestId("run-data-evidence");
    expect(await within(evidence).findByText("publish · produces · publication.output")).toBeVisible();
    expect(api.getJobManifest).toHaveBeenCalledWith("job-1");
    expect(within(evidence).queryByTestId("declared-dataset-link")).not.toBeInTheDocument();
    expect(screen.queryByTestId("run-external-executions")).not.toBeInTheDocument();
    expect(api.getRunExternalExecutions).not.toHaveBeenCalled();
    expect(await screen.findByText(/^Global atom metadata is unavailable/)).toBeVisible();
    await waitFor(() => expect(screen.getByTestId("local-dag")).toHaveAttribute("data-atom-count", "0"));
    expect(screen.queryByText(/retained-global-command-secret|retained-global-spec-secret/)).not.toBeInTheDocument();
    if (scoped || namespaceOnly) expect(api.getAtoms).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "work node" }));
    expect(await screen.findByTestId("local-log-target")).toHaveTextContent("job-1/local-run/task-1");
    const task = screen.getByTestId("task-detail-panel");
    fireEvent.click(within(task).getByRole("button", { name: "Details" }));
    expect(await within(task).findByText("42")).toBeVisible();
    expect(within(task).getByText("2026-10-17T12:00:00Z")).toBeVisible();
    expect(task).not.toHaveTextContent(/retained-global-command-secret|retained-global-spec-secret/);
    fireEvent.click(within(task).getByRole("button", { name: "Close task panel" }));
    fireEvent.click(within(evidence).getByRole("link", { name: "Run receipt" }));
    expect(await screen.findByText("Existing run receipt")).toBeVisible();
  });
});
