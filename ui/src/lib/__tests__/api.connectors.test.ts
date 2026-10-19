import { beforeEach, expect, it, vi } from "vitest";
import { api, ApiError } from "../api";
import { clearApiKey, setApiKey } from "../auth";

const fetchMock = vi.fn();
beforeEach(() => {
  clearApiKey();
  fetchMock.mockReset();
  globalThis.fetch = fetchMock;
  fetchMock.mockResolvedValue({ ok: true, status: 200, text: async () => "{}" });
});

it("encodes the pinned execution and action separately with an explicit idempotency key", async () => {
  setApiKey("operator-key");
  const body = { binding_name: "publication", binding_version: "frozen-1", input: { note: "Reviewed" } };
  await api.submitConnectorAction("connection/a", "opaque/run%2Fid", "approve_publication", body, "fresh-key");
  expect(fetchMock).toHaveBeenCalledWith("/v1/connectors/connection%2Fa/executions/opaque%2Frun%252Fid/actions/approve_publication", expect.objectContaining({ method: "POST", headers: expect.objectContaining({ Authorization: "Bearer operator-key", "Idempotency-Key": "fresh-key" }), body: JSON.stringify(body) }));
});

it("recovers a 409 operation conflict receipt and keeps ordinary conflicts as errors", async () => {
  const winner = { operation: { id: "existing-op", execution_id: "pinned-run" }, created: false, conflict: true, pending: false };
  fetchMock.mockResolvedValueOnce({ ok: false, status: 409, text: async () => JSON.stringify(winner) });
  await expect(api.submitConnectorAction("c", "pinned-run", "approve_publication", { binding_name: "b", binding_version: "1", input: {} }, "k")).resolves.toEqual(winner);
  fetchMock.mockResolvedValueOnce({ ok: false, status: 409, text: async () => JSON.stringify({ message: "revision conflicts" }) });
  await expect(api.resubmitConnectorOperation("c", "op", 5)).rejects.toEqual(expect.objectContaining({ status: 409 }));
});

it("recovers server list/receipt state with GET and sends confirmed revision only on explicit resubmit", async () => {
  const controller = new AbortController();
  await api.getConnectorOperations("c", "opaque/run", { cursor: "next/token", page_size: 10 }, controller.signal);
  expect(fetchMock.mock.calls[0]).toEqual(["/v1/connectors/c/executions/opaque%2Frun/operations?cursor=next%2Ftoken&page_size=10", expect.objectContaining({ signal: controller.signal })]);
  await api.getConnectorOperation("c", "op/id", controller.signal);
  expect(fetchMock.mock.calls[1][0]).toBe("/v1/connectors/c/operations/op%2Fid");
  expect(fetchMock.mock.calls[1][1].method).toBeUndefined();
  await api.resubmitConnectorOperation("c", "op/id", 7);
  expect(fetchMock.mock.calls[2]).toEqual(["/v1/connectors/c/operations/op%2Fid/resubmit", expect.objectContaining({ method: "POST", body: JSON.stringify({ confirmed: true, revision: 7 }) })]);
});

it("preserves authentication refusal without inventing a receipt", async () => {
  fetchMock.mockResolvedValueOnce({ ok: false, status: 403, text: async () => '{"message":"Forbidden"}' });
  await expect(api.getConnectorOperation("c", "op")).rejects.toBeInstanceOf(ApiError);
  expect(fetchMock).toHaveBeenCalledTimes(1);
});
