import { beforeEach, expect, it, vi } from "vitest";
import { api } from "../api";
import { clearApiKey, setApiKey } from "../auth";
const fetchMock = vi.fn();
beforeEach(() => {
  clearApiKey();
  fetchMock.mockReset();
  globalThis.fetch = fetchMock;
  fetchMock.mockResolvedValue({
    ok: true,
    status: 200,
    text: async () => "{}",
  });
});
it("keeps empty namespace exact in hold feeds and encodes each dataset path segment once", async () => {
  await api.getDatasetHolds({
    namespace: "",
    name: "warehouse/orders%2Fraw",
    offset: 20,
    limit: 20,
    status: "all",
  });
  expect(fetchMock.mock.calls[0][0]).toBe(
    "/v1/datasets/holds?namespace=&name=warehouse%2Forders%252Fraw&offset=20&limit=20&status=all",
  );
  await api.getDatasetMetrics("", "warehouse/orders%2Fraw", "rowCount");
  expect(fetchMock.mock.calls[1][0]).toBe(
    "/v1/datasets/_/warehouse%2Forders%252Fraw/metrics?metric=rowCount&limit=200",
  );
});
it("releases the explicit hold ID using the existing authenticated request path", async () => {
  setApiKey("test-operator-key");
  await api.releaseDatasetHold("hold-1", "seasonal", {
    deltaFromBaseline: "24h",
  });
  expect(fetchMock.mock.calls[0][0]).toBe("/v1/datasets/holds/hold-1/release");
  expect(fetchMock.mock.calls[0][1]).toMatchObject({
    method: "POST",
    headers: { Authorization: "Bearer test-operator-key" },
    body: JSON.stringify({
      reason: "seasonal",
      tolerate: { deltaFromBaseline: "24h" },
    }),
  });
});

it("encodes reserved namespace and dataset characters on separate axes", async () => {
  await api.getDatasetHolds({
    namespace: "tenant/blue%2F",
    name: "orders hold=raw%2F",
  });
  const url = new URL(fetchMock.mock.calls[0][0], "http://localhost");
  expect(url.searchParams.get("namespace")).toBe("tenant/blue%2F");
  expect(url.searchParams.get("name")).toBe("orders hold=raw%2F");
  await api.getDatasetMetrics(
    "tenant/blue%2F",
    "orders hold=raw%2F",
    "rowCount",
  );
  expect(fetchMock.mock.calls[1][0]).toBe(
    "/v1/datasets/tenant%2Fblue%252F/orders%20hold%3Draw%252F/metrics?metric=rowCount&limit=200",
  );
});
