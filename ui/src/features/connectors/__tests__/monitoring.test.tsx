import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { api, ApiError } from "@/lib/api";
import type { PrincipalState } from "@/lib/auth";
import { canReadConnectors } from "@/features/auth/access";
import { ConnectionsPage, ObservationTime } from "../ConnectionsPage";
import { ExecutionDetailPage } from "../ExecutionDetailPage";
import { connectorRefreshMs, connectorRefreshSchedule, observationAge, temporalExecutionURL, type ConnectorConnection, type ExternalExecution } from "../types";

const { principal, search, navigate } = vi.hoisted(() => ({
  principal: { kind: "api_key", subject: "viewer", role: "viewer", isScoped: false, scopeKnown: true, canRunner: false } as PrincipalState,
  search: {} as Record<string, unknown>,
  navigate: vi.fn(),
}));
vi.mock("@/lib/auth", () => ({ usePrincipal: () => principal, withAuthHeaders: () => ({}) }));
vi.mock("@tanstack/react-router", () => ({
  useSearch: () => search,
  useParams: () => ({ connectionId: "fixture", executionId: "opaque/reference" }),
  useNavigate: () => navigate,
  Link: ({ children, params }: { children: ReactNode; params?: Record<string, string> }) => <a href={params?.executionId ? `/connectors/${params.connectionId}/executions/${params.executionId}` : "/connectors"}>{children}</a>,
}));

const connection: ConnectorConnection = { id: "fixture", provider: "temporal", scope: "team/a", enabled: true, capabilities: ["discovery", "inspection", "history"], ui_url: "https://temporal.example/ui", console_refresh_ms: 15_000 };
const execution: ExternalExecution = {
  id: "opaque/reference", reference: { connection_id: "fixture", coordinates: { workflow_id: "external/only", run_id: "actual-run" } },
  availability: "available", completeness: "complete", observed_at: "2026-10-16T12:00:00Z",
  metadata: { workflow_type: "external_only", workflow_id: "external/only", run_id: "actual-run", scope: "team/a", source_event_id: 12, native_status: "WORKFLOW_EXECUTION_STATUS_RUNNING", display_status: "running", terminal: false, failure: false, pending_activities: [{ activity_id: "activity-1", activity_type: "external-work", attempt: 3, maximum_attempts: 5, state: "ACTIVITY_STATE_STARTED" }] },
};

function show(children: ReactNode) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return { ...render(<QueryClientProvider client={client}>{children}</QueryClientProvider>), client };
}

beforeEach(() => {
  vi.restoreAllMocks();
  navigate.mockClear();
  Object.keys(search).forEach(key => delete search[key]);
  Object.assign(principal, { kind: "api_key", subject: "viewer", role: "viewer", isScoped: false, scopeKnown: true, canRunner: false });
  vi.spyOn(api, "getSystemFeatures").mockResolvedValue({ database_console_enabled: false, log_console_enabled: false, agent_remediation_enabled: false, freshness_enabled: false, contract_enforcement_enabled: false, connectors_enabled: true, connectors_ready: true });
  vi.spyOn(api, "getConnectorConnections").mockResolvedValue({ connections: [connection], ready: true, fingerprint: "epoch" });
  vi.spyOn(api, "getConnectorExecutions").mockResolvedValue({ executions: [execution], cursor: "next/executions" });
  vi.spyOn(api, "getConnectorExecution").mockResolvedValue(execution);
  vi.spyOn(api, "getConnectorHistory").mockResolvedValue({ workflow_id: "external/only", run_id: "actual-run", scope: "team/a", observed_at: "2026-10-16T12:00:00Z", events: [{ event_id: 1, event_type: "EVENT_TYPE_WORKFLOW_EXECUTION_STARTED" }], cursor: "next/history" });
});

describe("connector monitoring", () => {
  it.each([
    { kind: null, subject: null, role: null, isScoped: false },
    { kind: "api_key", subject: "scoped", role: "admin", isScoped: true },
    { kind: "user", subject: "namespace-only", role: null, isScoped: false },
    { kind: "agent_session", subject: "agent", role: "operator", isScoped: false },
    { kind: "api_key", subject: "unknown-scope", role: "viewer", isScoped: false, scopeKnown: false },
    { kind: "api_key", subject: "agent-like-unknown-scope", role: "operator", isScoped: false, scopeKnown: false },
  ])("does not request or render connector metadata for $subject", async denied => {
    Object.assign(principal, denied);
    show(<ExecutionDetailPage />);
    expect(await screen.findByRole("alert")).toHaveTextContent("unscoped cluster viewer");
    expect(api.getConnectorConnections).not.toHaveBeenCalled();
    expect(api.getConnectorExecution).not.toHaveBeenCalled();
    expect(screen.queryByText("actual-run")).not.toBeInTheDocument();
  });

  it("does not mount monitoring when the server feature is off", async () => {
    vi.mocked(api.getSystemFeatures).mockResolvedValue({ connectors_enabled: false } as Awaited<ReturnType<typeof api.getSystemFeatures>>);
    show(<ConnectionsPage />);
    expect(await screen.findByText("Connections disabled")).toBeVisible();
    expect(api.getConnectorConnections).not.toHaveBeenCalled();
  });

  it("renders actual native identity and observations and resets a cursor when filtering", async () => {
    search.cursor = "old/filter-bound-cursor";
    show(<ConnectionsPage />);
    expect(await screen.findByRole("link", { name: "external/only" })).toHaveAttribute("href", "/connectors/fixture/executions/opaque/reference");
    expect(screen.getByText("actual-run")).toBeVisible();
    expect(screen.getByText("WORKFLOW_EXECUTION_STATUS_RUNNING")).toBeVisible();
    expect(screen.getByText(/2026-10-16T12:00:00.000Z/)).toBeVisible();
    fireEvent.change(screen.getByLabelText("Workflow type"), { target: { value: " publication " } });
    fireEvent.click(screen.getByRole("button", { name: "Apply filters" }));
    expect(navigate).toHaveBeenLastCalledWith({ to: "/connectors", search: { connection: "fixture", workflow_type: "publication", cursor: undefined } });
    fireEvent.click(screen.getByRole("button", { name: "Next executions" }));
    expect(navigate).toHaveBeenLastCalledWith({ to: "/connectors", search: { connection: "fixture", cursor: "next/executions" } });
  });

  it("exposes exact selectable filter names independently of option text", async () => {
    show(<ConnectionsPage />);
    const connectionSelect = await screen.findByRole("combobox", { name: "Connection" });
    expect(connectionSelect).toHaveAccessibleName("Connection");
    expect(screen.getByLabelText("Connection", { exact: true })).toBe(connectionSelect);
    fireEvent.change(connectionSelect, { target: { value: "fixture" } });
    expect(navigate).toHaveBeenLastCalledWith({ to: "/connectors", search: { connection: "fixture", cursor: undefined } });
    const pageSizeSelect = screen.getByRole("combobox", { name: "Executions per page" });
    expect(pageSizeSelect).toHaveAccessibleName("Executions per page");
    fireEvent.change(pageSizeSelect, { target: { value: "5" } });
    expect(navigate).toHaveBeenLastCalledWith({ to: "/connectors", search: { page_size: 5, cursor: undefined } });
    expect(screen.getByRole("textbox", { name: "Workflow type" })).toBe(screen.getByLabelText("Workflow type", { exact: true }));
  });

  it("exposes an exact selectable history page-size name independently of option text", async () => {
    show(<ExecutionDetailPage />);
    const pageSizeSelect = await screen.findByRole("combobox", { name: "Requested history page size" });
    expect(pageSizeSelect).toHaveAccessibleName("Requested history page size");
    expect(screen.getByLabelText("Requested history page size", { exact: true })).toBe(pageSizeSelect);
    fireEvent.change(pageSizeSelect, { target: { value: "1" } });
    expect(navigate).toHaveBeenLastCalledWith({ to: "/connectors/$connectionId/executions/$executionId", params: { connectionId: "fixture", executionId: "opaque/reference" }, search: { page_size: 1 } });
  });

  it("retains the complete native event batch and cursor when it exceeds the requested history page size", async () => {
    search.page_size = 1;
    vi.mocked(api.getConnectorHistory).mockResolvedValue({ workflow_id: "external/only", run_id: "actual-run", scope: "team/a", observed_at: execution.observed_at, events: [{ event_id: 1, event_type: "EVENT_TYPE_WORKFLOW_EXECUTION_STARTED" }, { event_id: 2, event_type: "EVENT_TYPE_WORKFLOW_TASK_SCHEDULED" }], cursor: "native-batch-cursor" });
    show(<ExecutionDetailPage />);
    expect(await screen.findAllByTestId("external-history-event")).toHaveLength(2);
    expect(screen.getByLabelText("Requested history page size", { exact: true })).toHaveValue("1");
    expect(screen.getByText(/Temporal returns native event batches/)).toBeVisible();
    expect(screen.getAllByTestId("external-history-event").map(row => row.textContent)).toEqual(["#1EVENT_TYPE_WORKFLOW_EXECUTION_STARTED", "#2EVENT_TYPE_WORKFLOW_TASK_SCHEDULED"]);
    fireEvent.click(screen.getByRole("button", { name: "Next history" }));
    expect(navigate).toHaveBeenLastCalledWith({ to: "/connectors/$connectionId/executions/$executionId", params: { connectionId: "fixture", executionId: "opaque/reference" }, search: { page_size: 1, cursor: "native-batch-cursor" } });
  });

  it.each([ConnectionsPage, ExecutionDetailPage])("shows an unavailable observation time for a provider zero timestamp in %s", async Page => {
    const zeroTimestamp = "0001-01-01T00:00:00Z";
    vi.mocked(api.getConnectorExecutions).mockResolvedValue({ executions: [{ ...execution, observed_at: zeroTimestamp }] });
    vi.mocked(api.getConnectorExecution).mockResolvedValue({ ...execution, observed_at: zeroTimestamp });
    const { container } = show(<Page />);
    expect(await screen.findByText("Observation time unavailable")).toBeVisible();
    expect(screen.queryByText(/0001-01-01|1757713h/)).not.toBeInTheDocument();
    expect(container.querySelector(`time[datetime="${zeroTimestamp}"]`)).toBeNull();
  });

  it("shows partial history and reported attempts without claiming complete history or workflow failure", async () => {
    vi.mocked(api.getConnectorExecution).mockResolvedValue({ ...execution, completeness: "partial" });
    vi.mocked(api.getConnectorHistory).mockResolvedValue({ workflow_id: "external/only", run_id: "actual-run", scope: "team/a", observed_at: execution.observed_at, incomplete: true, events: [{ event_id: 7, event_type: "EVENT_TYPE_WORKFLOW_TASK_TIMED_OUT" }], problems: [{ event_id: 7, cause: "TIMEOUT_TYPE_START_TO_CLOSE" }] });
    show(<ExecutionDetailPage />);
    expect(await screen.findByText(/Partial execution metadata/)).toBeVisible();
    expect(await screen.findByText(/Partial history page/)).toBeVisible();
    expect(screen.getByText("3 / 5")).toBeVisible();
    expect(screen.getByText(/Workflow-task problems \(separate from workflow status\)/)).toBeVisible();
    expect(screen.getByTestId("external-native-status")).toHaveTextContent("WORKFLOW_EXECUTION_STATUS_RUNNING");
    expect(screen.getByRole("button", { name: "Next history" })).toBeDisabled();
    expect(screen.queryByText("End of reported history.")).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: /Open in Temporal UI/ })).toHaveAttribute("href", "https://temporal.example/ui/namespaces/team%2Fa/workflows/external%2Fonly/actual-run/history");
  });

  it("retains dated cached details through a read failure and recovers the same run", async () => {
    const { client } = show(<ExecutionDetailPage />);
    expect(await screen.findByTestId("external-run-id")).toHaveTextContent("actual-run");
    vi.mocked(api.getConnectorExecution).mockRejectedValueOnce(new ApiError(503, "connector unavailable"));
    await client.invalidateQueries({ queryKey: ["connectors", "viewer", "fixture", "opaque/reference", "detail"] });
    expect(await screen.findByTestId("execution-availability")).toHaveTextContent("Stale observation");
    expect(screen.getByTestId("external-run-id")).toHaveTextContent("actual-run");
    expect(screen.getAllByText(/2026-10-16T12:00:00.000Z/).length).toBeGreaterThan(0);
    await client.invalidateQueries({ queryKey: ["connectors", "viewer", "fixture", "opaque/reference", "detail"] });
    await waitFor(() => expect(screen.getByTestId("execution-availability")).toHaveTextContent("Live provider observation"));
  });

  it("hides cached metadata when refreshed authorization is refused", async () => {
    const { client } = show(<ExecutionDetailPage />);
    expect(await screen.findByTestId("external-run-id")).toBeVisible();
    vi.mocked(api.getConnectorExecution).mockRejectedValue(new ApiError(403, "forbidden", "insufficient_access"));
    await client.invalidateQueries({ queryKey: ["connectors", "viewer", "fixture", "opaque/reference", "detail"] });
    expect(await screen.findByRole("alert")).toHaveTextContent("permission");
    expect(screen.queryByText("actual-run")).not.toBeInTheDocument();
  });

  it("does not invoke unavailable history, Queries or actions", async () => {
    vi.mocked(api.getConnectorConnections).mockResolvedValue({ connections: [{ ...connection, capabilities: ["inspection"] }], ready: true, fingerprint: "epoch" });
    show(<ExecutionDetailPage />);
    expect(await screen.findByText(/History unavailable: this capability is not advertised/)).toBeVisible();
    expect(api.getConnectorHistory).not.toHaveBeenCalled();
    expect(screen.queryByRole("button", { name: /query|approve|retry|cancel/i })).not.toBeInTheDocument();
  });

  it.each([ConnectionsPage, ExecutionDetailPage])("disables overflowing provider poll timers and displays the configured interval in %s", async Page => {
    const interval = vi.spyOn(globalThis, "setInterval");
    // Thirty days is a legal configuration and exceeds the browser's signed
    // 32-bit timer range. Inspect the actual query timers, so a regression to
    // setInterval(30 days) cannot silently become a 1ms provider poll loop.
    vi.mocked(api.getConnectorConnections).mockResolvedValue({ connections: [{ ...connection, console_refresh_ms: 30 * 24 * 60 * 60 * 1000 }], ready: true, fingerprint: "epoch" });
    show(<Page />);
    expect(await screen.findByText(/Automatic refresh disabled: configured interval 2592000s exceeds the browser timer range/)).toBeVisible();
    expect(interval.mock.calls.every(([, delay]) => typeof delay !== "number" || delay <= 2_147_483_647)).toBe(true);
  });
});

describe("connector boundary helpers", () => {
  it.each([undefined, null, "", "invalid", "0001-01-01T00:00:00Z", "1969-12-31T23:59:59Z", "1970-01-01T00:00:00Z"])("renders unknown observation time without a date or age for %s", value => {
    const { container } = show(<ObservationTime value={value} />);
    expect(screen.getByText("Observation time unavailable")).toBeVisible();
    expect(container.querySelector("time")).toBeNull();
    expect(observationAge(value)).toBe("Observation time unavailable");
  });
  it("honors slower refresh and clamps invalid/faster refresh", () => {
    expect(connectorRefreshMs(connection)).toBe(15_000);
    expect(connectorRefreshMs({ ...connection, console_refresh_ms: 1000 })).toBe(10_000);
    expect(connectorRefreshMs({ ...connection, console_refresh_ms: NaN })).toBe(10_000);
    expect(connectorRefreshSchedule({ ...connection, console_refresh_ms: 2_147_483_647 }).interval).toBe(2_147_483_647);
    expect(connectorRefreshSchedule({ ...connection, console_refresh_ms: 2_147_483_648 }).interval).toBe(false);
  });
  it.each([undefined, "javascript:alert(1)", "https://user:secret@ui.example", "https://ui.example?token=secret", "https://ui.example/#secret"])("omits an absent/unsafe UI URL %s", ui_url => {
    expect(temporalExecutionURL({ ...connection, ui_url }, execution)).toBeUndefined();
  });
  it.each(["viewer", "runner", "operator", "admin"] as const)("allows an unscoped cluster %s", role => {
    expect(canReadConnectors({ ...principal, role })).toBe(true);
  });
});
