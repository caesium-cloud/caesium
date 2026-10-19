import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api, ApiError } from "@/lib/api";
import type { PrincipalState } from "@/lib/auth";
import { ActionPanel } from "../ActionPanel";
import type { ConnectorConnection, ConnectorOperation, ConnectorOperationResult, ExternalExecution } from "../types";

const { principal } = vi.hoisted(() => ({ principal: { kind: "api_key", stableId: "key-1", subject: "operator", role: "operator", isScoped: false, scopeKnown: true, canRunner: true } as PrincipalState }));
vi.mock("@/lib/auth", () => ({ usePrincipal: () => principal, withAuthHeaders: () => ({}) }));
const connection: ConnectorConnection = { id: "fixture", provider: "temporal", scope: "default", enabled: true, capabilities: ["inspection", "action_submission", "receipt_lookup"], console_refresh_ms: 10_000 };
function execution(): ExternalExecution {
  return { id: "opaque/run-1", reference: { connection_id: "fixture", coordinates: { workflow_id: "publication-1", run_id: "run-1" } }, availability: "available", completeness: "complete", observed_at: new Date().toISOString(), metadata: { workflow_type: "publication", workflow_id: "publication-1", run_id: "run-1", scope: "default", source_event_id: 9, native_status: "WORKFLOW_EXECUTION_STATUS_RUNNING", display_status: "running", terminal: false, failure: false }, binding: { name: "publication", version: "1", display_name: "Publication", available: true, actions: [{ name: "approve_publication", minimum_role: "operator", available: true, input_schema: { type: "object", additionalProperties: false, properties: { note: { type: "string" } } }, result_schema: { type: "object", additionalProperties: false, properties: { approved: { type: "boolean" } } } }] } };
}
function operation(overrides: Partial<ConnectorOperation> = {}): ConnectorOperation {
  return { id: "op-1", connection_id: "fixture", execution_id: "opaque/run-1", execution: execution().reference, action: "approve_publication", binding_name: "publication", binding_version: "1", state: "unknown", revision: 5, attempt: 1, actor: { principal_kind: "api_key", stable_id: "key-1", subject: "operator" }, external_update_id: "update-immutable", evidence: { state: "unknown" }, ...overrides };
}
function result(value = operation()): ConnectorOperationResult { return { operation: value, created: false, conflict: false, pending: false }; }
function show(value = execution(), options: { stale?: boolean; connection?: ConnectorConnection } = {}) {
  // Keep a newly seeded receipt until its observer attaches, as production does.
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 60_000 } } });
  const panel = (stale: boolean) => <QueryClientProvider client={client}><ActionPanel connection={options.connection ?? connection} execution={value} executionId="opaque/run-1" observationStale={stale} /></QueryClientProvider>;
  const mounted = render(panel(options.stale ?? false));
  return { ...mounted, client, rerenderStale: (stale: boolean) => mounted.rerender(panel(stale)) };
}
async function loadedButton() {
  const button = await screen.findByRole("button", { name: "Approve publication" });
  await waitFor(() => expect(button).toBeEnabled());
  return button;
}
async function loadReceipt(value = operation()) {
  vi.mocked(api.getConnectorOperations).mockResolvedValue({ operations: [value] });
  vi.mocked(api.getConnectorOperation).mockResolvedValue(result(value));
  const mounted = show();
  await screen.findByTestId("operation-receipt");
  return mounted;
}

beforeEach(() => {
  vi.restoreAllMocks();
  Object.assign(principal, { kind: "api_key", stableId: "key-1", subject: "operator", role: "operator", isScoped: false, scopeKnown: true, canRunner: true });
  vi.spyOn(api, "getConnectorOperations").mockResolvedValue({ operations: [] });
  vi.spyOn(api, "getConnectorOperation").mockResolvedValue(result());
  vi.spyOn(api, "submitConnectorAction").mockResolvedValue({ ...result(operation({ state: "accepted" })), created: true });
  vi.spyOn(api, "resubmitConnectorOperation").mockResolvedValue(result(operation({ state: "accepted", revision: 7 })));
});

afterEach(() => { vi.useRealTimers(); });

describe("declared connector approval", () => {
  it("submits the declared input to an immutable run and binding once during duplicate clicks", async () => {
    let finish!: (value: ConnectorOperationResult) => void;
    vi.mocked(api.submitConnectorAction).mockReturnValue(new Promise(resolve => { finish = resolve; }));
    show();
    const button = await loadedButton();
    expect(screen.getByRole("region", { name: "Declared actions" })).toHaveTextContent("run-1");
    fireEvent.change(screen.getByLabelText("note", { exact: true }), { target: { value: "Reviewed publication" } });
    fireEvent.click(button); fireEvent.click(button);
    expect(api.submitConnectorAction).toHaveBeenCalledTimes(1);
    expect(api.submitConnectorAction).toHaveBeenCalledWith("fixture", "opaque/run-1", "approve_publication", { binding_name: "publication", binding_version: "1", input: { note: "Reviewed publication" } }, expect.any(String));
    await act(async () => finish({ ...result(operation({ state: "accepted" })), created: true }));
    await screen.findByTestId("operation-receipt");
    expect(api.resubmitConnectorOperation).not.toHaveBeenCalled();
  });

  it.each(["viewer", "runner"] as const)("lets %s recover receipts while disabling approval", async role => {
    principal.role = role;
    show();
    await waitFor(() => expect(api.getConnectorOperations).toHaveBeenCalled());
    expect(screen.getByRole("button", { name: "Approve publication" })).toBeDisabled();
    expect(screen.getByText(/Approval requires an unscoped operator/)).toBeVisible();
    expect(api.submitConnectorAction).not.toHaveBeenCalled();
  });

  it.each([
    { kind: "agent_session", isScoped: false, scopeKnown: true },
    { kind: "api_key", isScoped: true, scopeKnown: true },
    { kind: "api_key", isScoped: false, scopeKnown: false },
  ])("does not read or act for disallowed principal $kind scoped=$isScoped known=$scopeKnown", async denied => {
    Object.assign(principal, denied);
    show();
    expect(screen.queryByRole("region", { name: "Declared actions" })).not.toBeInTheDocument();
    expect(api.getConnectorOperations).not.toHaveBeenCalled();
    expect(api.submitConnectorAction).not.toHaveBeenCalled();
  });

  it.each(["stale", "expired", "unavailable", "binding", "version", "target", "terminal", "capability"])("disables submission when %s", async failure => {
    const value = execution();
    if (failure === "expired") value.observed_at = new Date(Date.now() - 30_000).toISOString();
    if (failure === "unavailable") value.availability = "unavailable";
    if (failure === "binding") value.binding!.name = "other-workflow";
    if (failure === "version") value.binding!.version = "";
    if (failure === "target") value.reference.coordinates.run_id = "different-run";
    if (failure === "terminal") value.metadata.terminal = true;
    show(value, { stale: failure === "stale", connection: failure === "capability" ? { ...connection, capabilities: ["inspection", "receipt_lookup"] } : connection });
    await waitFor(() => expect(api.getConnectorOperations).toHaveBeenCalled());
    expect(screen.getByRole("button", { name: "Approve publication" })).toBeDisabled();
  });

  it("keeps unbound workflows inspectable with no approval form", async () => {
    const value = execution(); delete value.binding;
    show(value);
    expect(screen.getByText("No declared binding")).toBeVisible();
    expect(screen.queryByRole("form", { name: "Approval form" })).not.toBeInTheDocument();
  });

  it("requires a successful server operation list before a new submission", async () => {
    vi.mocked(api.getConnectorOperations).mockRejectedValue(new ApiError(503, "unavailable"));
    show();
    expect(await screen.findByRole("alert")).toHaveTextContent("Operation list unavailable");
    expect(screen.getByRole("button", { name: "Approve publication" })).toBeDisabled();
  });

  it("does not retry a lost reply, and explicitly recovers the admitted server receipt", async () => {
    vi.mocked(api.submitConnectorAction).mockRejectedValue(new TypeError("network reply lost"));
    show();
    fireEvent.click(await loadedButton());
    expect(await screen.findByText(/Submission reply unavailable or refused/)).toBeVisible();
    expect(screen.getByRole("button", { name: "Approve publication" })).toBeDisabled();
    vi.mocked(api.getConnectorOperations).mockResolvedValue({ operations: [operation()] });
    fireEvent.click(screen.getByRole("button", { name: "Refresh receipts" }));
    expect(await screen.findByTestId("operation-receipt")).toHaveAttribute("data-state", "unknown");
    expect(api.submitConnectorAction).toHaveBeenCalledTimes(1);
    expect(api.resubmitConnectorOperation).not.toHaveBeenCalled();
  });

  it("shows submission failure evidence until normal refresh and lets explicit lookup replace it", async () => {
    const failed = operation({ evidence: { state: "unknown", failure_kind: "failure_origin_unknown" } });
    vi.mocked(api.submitConnectorAction).mockResolvedValue({ ...result(failed), created: true });
    vi.mocked(api.getConnectorOperation).mockResolvedValue(result(operation({ revision: 6, evidence: { state: "unknown", rpc_code: "NotFound" } })));
    show();
    fireEvent.click(await loadedButton());
    await screen.findByTestId("operation-receipt");
    expect(screen.getByText(/does not establish validator rejection/)).toBeVisible();
    expect(screen.getByTestId("operation-receipt")).toHaveTextContent("failure_origin_unknown");
    expect(api.getConnectorOperation).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Refresh receipt" }));
    await waitFor(() => expect(screen.getByTestId("operation-receipt")).toHaveAttribute("data-revision", "6"));
    expect(screen.queryByText(/does not establish validator rejection/)).not.toBeInTheDocument();
    expect(screen.getByTestId("operation-state")).toHaveTextContent("Unknown — outcome unresolved");
    expect(api.getConnectorOperation).toHaveBeenCalledTimes(1);
    expect(api.submitConnectorAction).toHaveBeenCalledTimes(1);
    expect(api.resubmitConnectorOperation).not.toHaveBeenCalled();
  });

  it("polls a submitted unknown receipt on the configured cadence without sending another Update", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    vi.mocked(api.submitConnectorAction).mockResolvedValue({ ...result(), created: true });
    show();
    fireEvent.click(await loadedButton());
    await screen.findByTestId("operation-receipt");
    expect(api.getConnectorOperation).not.toHaveBeenCalled();
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    await waitFor(() => expect(api.getConnectorOperation).toHaveBeenCalledTimes(1));
    expect(api.submitConnectorAction).toHaveBeenCalledTimes(1);
    expect(api.resubmitConnectorOperation).not.toHaveBeenCalled();
  });

  it("recovers a conflicting operator's open receipt instead of reporting a second submission", async () => {
    const existing = operation({ actor: { principal_kind: "api_key", stable_id: "other-key" } });
    vi.mocked(api.submitConnectorAction).mockResolvedValue({ ...result(existing), conflict: true });
    vi.mocked(api.getConnectorOperation).mockResolvedValue(result(existing));
    show();
    fireEvent.click(await loadedButton());
    expect(await screen.findByText(/Another request already owns this action/)).toBeVisible();
    expect(await screen.findByTestId("operation-receipt")).toHaveAttribute("data-operation-id", "op-1");
    expect(screen.queryByRole("button", { name: "Review resend" })).not.toBeInTheDocument();
    expect(api.getConnectorOperation).not.toHaveBeenCalled();
    expect(api.submitConnectorAction).toHaveBeenCalledTimes(1);
  });

  it("follows server receipt pagination without sending Updates", async () => {
    vi.mocked(api.getConnectorOperations).mockResolvedValueOnce({ operations: [], cursor: "next-receipts" }).mockResolvedValueOnce({ operations: [operation()] });
    show();
    const next = screen.getByRole("button", { name: "Next receipts" });
    await waitFor(() => expect(next).toBeEnabled());
    fireEvent.click(next);
    await screen.findByTestId("operation-receipt");
    expect(api.getConnectorOperations).toHaveBeenLastCalledWith("fixture", "opaque/run-1", { cursor: "next-receipts", page_size: 10 }, expect.any(AbortSignal));
    expect(api.submitConnectorAction).not.toHaveBeenCalled();
    expect(api.resubmitConnectorOperation).not.toHaveBeenCalled();
  });
});

describe("server operation receipts", () => {
  it.each([
    ["submitted", "Submitted — awaiting acceptance"],
    ["accepted", "Accepted — awaiting completion"],
    ["completed", "Completed"],
    ["rejected", "Rejected"],
    ["not_dispatched", "Not dispatched"],
    ["unknown", "Unknown — outcome unresolved"],
  ])("renders %s as %s without inferring another state", async (state, label) => {
    await loadReceipt(operation({ state, evidence: { state, result: state === "completed" ? { approved: true } : undefined } }));
    expect(screen.getByTestId("operation-state")).toHaveTextContent(label);
    if (state === "completed") expect(screen.getByTestId("operation-result")).toHaveTextContent('"approved": true');
    else expect(screen.queryByTestId("operation-result")).not.toBeInTheDocument();
    expect(api.submitConnectorAction).not.toHaveBeenCalled();
    expect(api.resubmitConnectorOperation).not.toHaveBeenCalled();
  });

  it("can explicitly recover an initial selected-receipt failure without reloading or sending", async () => {
    vi.mocked(api.getConnectorOperations).mockResolvedValue({ operations: [operation()] });
    vi.mocked(api.getConnectorOperation).mockRejectedValueOnce(new ApiError(503, "provider unavailable")).mockResolvedValue(result());
    show();
    expect(await screen.findByText("Receipt unavailable. No provider outcome has been established.")).toBeVisible();
    expect(screen.queryByTestId("operation-receipt")).not.toBeInTheDocument();
    const refresh = screen.getByRole("button", { name: "Refresh receipt" });
    expect(refresh).toBeEnabled();
    fireEvent.click(refresh);
    expect(await screen.findByTestId("operation-receipt")).toHaveAttribute("data-operation-id", "op-1");
    expect(api.getConnectorOperation).toHaveBeenCalledTimes(2);
    expect(screen.queryByText("Receipt unavailable. No provider outcome has been established.")).not.toBeInTheDocument();
    expect(api.submitConnectorAction).not.toHaveBeenCalled();
    expect(api.resubmitConnectorOperation).not.toHaveBeenCalled();
  });

  it("keeps polling a selected receipt after its initial GET failed", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    vi.mocked(api.getConnectorOperations).mockResolvedValue({ operations: [operation()] });
    vi.mocked(api.getConnectorOperation).mockRejectedValueOnce(new ApiError(503, "provider unavailable")).mockResolvedValue(result());
    show();
    await screen.findByText("Receipt unavailable. No provider outcome has been established.");
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(await screen.findByTestId("operation-receipt")).toHaveAttribute("data-operation-id", "op-1");
    expect(api.getConnectorOperation).toHaveBeenCalledTimes(2);
    expect(api.submitConnectorAction).not.toHaveBeenCalled();
    expect(api.resubmitConnectorOperation).not.toHaveBeenCalled();
  });

  it("keeps failed lookup evidence with unknown origin unresolved", async () => {
    await loadReceipt(operation({ evidence: { state: "unknown", failure_kind: "failure_origin_unknown" } }));
    expect(screen.getByTestId("operation-state")).toHaveTextContent("Unknown — outcome unresolved");
    expect(screen.getByText(/does not establish validator rejection/)).toBeVisible();
    expect(screen.queryByText("Rejected", { exact: true })).not.toBeInTheDocument();
  });

  it("recovers after a new mount without browser key storage", async () => {
    const get = vi.spyOn(Storage.prototype, "getItem");
    const set = vi.spyOn(Storage.prototype, "setItem");
    const first = await loadReceipt(); first.unmount();
    show();
    await screen.findByTestId("operation-receipt");
    expect(api.getConnectorOperations).toHaveBeenCalledTimes(2);
    expect(get).not.toHaveBeenCalled(); expect(set).not.toHaveBeenCalled();
    expect(api.resubmitConnectorOperation).not.toHaveBeenCalled();
  });

  it("uses fresh displayed revision and a separate original-principal confirmation before resend", async () => {
    await loadReceipt();
    vi.mocked(api.getConnectorOperation).mockResolvedValue(result(operation({ revision: 6 })));
    fireEvent.click(screen.getByRole("button", { name: "Review resend" }));
    const confirm = await screen.findByRole("checkbox", { name: /operation op-1 at revision 6 to its original run/ });
    expect(screen.getByRole("button", { name: "Confirm resend" })).toBeDisabled();
    expect(api.resubmitConnectorOperation).not.toHaveBeenCalled();
    fireEvent.click(confirm);
    fireEvent.click(screen.getByRole("button", { name: "Confirm resend" }));
    await waitFor(() => expect(api.resubmitConnectorOperation).toHaveBeenCalledWith("fixture", "op-1", 6));
    expect(api.resubmitConnectorOperation).toHaveBeenCalledTimes(1);
    expect(await screen.findByTestId("operation-state")).toHaveTextContent("Accepted — awaiting completion");
  });

  it.each([
    { kind: "api_key", stableId: "different-id" },
    { kind: "user", stableId: "key-1" },
    { kind: "api_key", stableId: undefined },
  ])("does not grant resend using subject or missing stable identity $kind $stableId", async identity => {
    Object.assign(principal, identity);
    await loadReceipt();
    expect(screen.queryByRole("button", { name: "Review resend" })).not.toBeInTheDocument();
    expect(screen.getByText(/Only the original principal/)).toBeVisible();
  });

  it("clears confirmation when a stronger receipt revision is observed", async () => {
    const { client } = await loadReceipt();
    fireEvent.click(screen.getByRole("button", { name: "Review resend" }));
    fireEvent.click(await screen.findByRole("checkbox"));
    vi.mocked(api.getConnectorOperation).mockResolvedValue(result(operation({ revision: 6, state: "accepted" })));
    await act(async () => { await client.invalidateQueries({ queryKey: ["connectors"] }); });
    await waitFor(() => expect(screen.queryByRole("checkbox")).not.toBeInTheDocument());
    expect(screen.getByTestId("operation-state")).toHaveTextContent("Accepted — awaiting completion");
    expect(api.resubmitConnectorOperation).not.toHaveBeenCalled();
  });

  it("requires a new confirmation after a live observation becomes stale and recovers", async () => {
    const { rerenderStale } = await loadReceipt();
    fireEvent.click(screen.getByRole("button", { name: "Review resend" }));
    fireEvent.click(await screen.findByRole("checkbox"));
    expect(screen.getByRole("button", { name: "Confirm resend" })).toBeEnabled();
    rerenderStale(true);
    expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
    rerenderStale(false);
    expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Review resend" })).toBeEnabled();
    expect(api.resubmitConnectorOperation).not.toHaveBeenCalled();
  });

  it("blocks resend when refresh fails and preserves the last uncertain receipt", async () => {
    await loadReceipt();
    vi.mocked(api.getConnectorOperation).mockRejectedValue(new ApiError(503, "unavailable"));
    fireEvent.click(screen.getByRole("button", { name: "Review resend" }));
    expect(await screen.findByText(/Receipt refresh unavailable/)).toBeVisible();
    expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Review resend" })).toBeDisabled();
    expect(api.resubmitConnectorOperation).not.toHaveBeenCalled();
  });

  it("uses opaque immutable target and actor from compact receipts", async () => {
    const value = operation({ state: "completed", metadata_omitted: true }); delete value.execution; delete value.evidence;
    await loadReceipt(value);
    expect(screen.getByTestId("operation-receipt")).toHaveTextContent("opaque/run-1");
    expect(screen.getByText(/Descriptive metadata was omitted/)).toBeVisible();
    expect(screen.getByText(/Completion remains established/)).toBeVisible();
  });

  it("does not offer resend for receipts with an incompatible frozen binding", async () => {
    await loadReceipt(operation({ binding_version: "old-version" }));
    expect(screen.getByRole("button", { name: "Review resend" })).toBeDisabled();
    expect(api.resubmitConnectorOperation).not.toHaveBeenCalled();
  });
});
