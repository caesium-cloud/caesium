import { describe, it, expect, vi, beforeEach } from "vitest";
import { ApiError, api, type Receipt } from "@/lib/api";
import { clearApiKey, setApiKey } from "@/lib/auth";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

function okResponse(body: unknown, status = 200) {
  return {
    ok: true,
    status,
    text: () => Promise.resolve(JSON.stringify(body)),
  };
}

function errorResponse(status: number, body: unknown) {
  return {
    ok: false,
    status,
    text: () => Promise.resolve(typeof body === "string" ? body : JSON.stringify(body)),
  };
}

const committedReceipt: Receipt = {
  receipt_version: 1,
  run_id: "run-1",
  job_id: "job-1",
  job_alias: "daily",
  git_commit: "abc123",
  manifest_content_hash: "manifest-sha",
  tasks: [
    {
      task_name: "extract",
      identity_hash: "task-hash",
      image: "alpine@sha256:abc",
      resolved_image_digest: "sha256:abc",
      digest_pinned: true,
      degraded: false,
    },
  ],
  degraded: false,
  receipt_digest: "receipt-sha",
};

beforeEach(() => {
  vi.clearAllMocks();
  clearApiKey();
});

describe("data verb api methods", () => {
  it.each([
    {
      name: "getRunDiff",
      call: () => api.getRunDiff("job-1", "left-run", "right-run"),
      response: {
        jobId: "job-1",
        leftRunId: "left-run",
        rightRunId: "right-run",
        leftStatus: "succeeded",
        rightStatus: "succeeded",
        leftTrigger: {},
        rightTrigger: {},
        tasks: [],
        generatedAt: "2026-06-26T00:00:00Z",
      },
      expectedUrl: "/v1/jobs/job-1/runs/diff?left=left-run&right=right-run",
      expectedMethod: undefined,
    },
    {
      name: "postReplay",
      call: () => api.postReplay("job-1", "run-1", { set: { day: "2026-06-26" } }, "idem-123"),
      response: { run_id: "replay-run", status: "running", quarantine: true },
      status: 202,
      expectedUrl: "/v1/jobs/job-1/runs/run-1/replay",
      expectedMethod: "POST",
      expectedBody: { set: { day: "2026-06-26" } },
      expectedExtraHeaders: { "Idempotency-Key": "idem-123" },
    },
    {
      name: "getTaskWhy",
      call: () => api.getTaskWhy("job-1", "run-1", "extract data"),
      response: {
        runId: "run-1",
        jobId: "job-1",
        taskId: "task-1",
        taskName: "extract data",
        taskRunId: "task-run-1",
        verdict: "CACHE_MISS",
        status: "succeeded",
        cacheEnabled: true,
        summary: "CACHE_MISS",
        trigger: {},
        baseline: { kind: "prior_run" },
      },
      expectedUrl: "/v1/jobs/job-1/runs/run-1/why?task=extract+data",
      expectedMethod: undefined,
    },
    {
      name: "getBlame",
      call: () => api.getBlame("job-1", { from: "abc123", to: "def456" }),
      response: {
        job_id: "job-1",
        coverage: "topology+image+command",
        from_commit: "abc123",
        to_commit: "def456",
        tasks: [],
        edges: [],
      },
      expectedUrl: "/v1/jobs/job-1/blame?from=abc123&to=def456",
      expectedMethod: undefined,
    },
    {
      name: "getReceipt",
      call: () => api.getReceipt("job-1", "run-1"),
      response: committedReceipt,
      expectedUrl: "/v1/jobs/job-1/runs/run-1/receipt",
      expectedMethod: undefined,
    },
    {
      name: "postVerify",
      call: () => api.postVerify(committedReceipt),
      response: {
        run_id: "run-1",
        match: true,
        degraded: false,
        expected_digest: "receipt-sha",
        actual_digest: "receipt-sha",
        rederived: committedReceipt,
      },
      expectedUrl: "/v1/jobs/job-1/runs/run-1/receipt/verify",
      expectedMethod: "POST",
      expectedBody: committedReceipt,
    },
    {
      name: "getLineageImpact",
      call: () => api.getLineageImpact({ namespace: "warehouse", name: "analytics.fact orders", maxDepth: 3 }),
      response: {
        root_namespace: "warehouse",
        root_name: "analytics.fact orders",
        downstream: [],
      },
      expectedUrl: "/v1/lineage/impact?namespace=warehouse&name=analytics.fact+orders&max_depth=3",
      expectedMethod: undefined,
    },
  ])(
    "$name builds the expected request and includes auth headers",
    async ({ call, response, status, expectedUrl, expectedMethod, expectedBody, expectedExtraHeaders }) => {
      setApiKey("csk_test_secret");
      mockFetch.mockResolvedValue(okResponse(response, status));

      const result = await call();
      // Round-trip: the method must return the parsed response body, not drop/transform it.
      expect(result).toEqual(response);

      const [url, init] = mockFetch.mock.calls[0] as [string, RequestInit];
      expect(url).toBe(expectedUrl);
      expect(init.method).toBe(expectedMethod);
      expect(init).toEqual(
        expect.objectContaining({
          credentials: "include",
          headers: expect.objectContaining({
            "Content-Type": "application/json",
            Authorization: "Bearer csk_test_secret",
            ...(expectedExtraHeaders ?? {}),
          }),
        }),
      );
      if (expectedBody) {
        expect(JSON.parse(String(init.body))).toEqual(expectedBody);
      }
    },
  );

  it.each([
    { name: "getRunDiff", call: () => api.getRunDiff("job-1", "left-run", "right-run") },
    { name: "postReplay", call: () => api.postReplay("job-1", "run-1", { set: {} }, "idem-123") },
    { name: "getTaskWhy", call: () => api.getTaskWhy("job-1", "run-1", "extract") },
    { name: "getBlame", call: () => api.getBlame("job-1") },
    { name: "getReceipt", call: () => api.getReceipt("job-1", "run-1") },
    { name: "postVerify", call: () => api.postVerify(committedReceipt) },
    { name: "getLineageImpact", call: () => api.getLineageImpact({ namespace: "warehouse", name: "raw.orders" }) },
  ])("$name maps 403 to a typed insufficient-access error", async ({ call }) => {
    mockFetch.mockResolvedValue(errorResponse(403, { message: "insufficient permissions" }));

    await expect(call()).rejects.toMatchObject({
      status: 403,
      kind: "insufficient_access",
      message: "insufficient permissions",
    });
  });

  it.each([
    { status: 400, kind: "replay_missing_idempotency_key", message: "Idempotency-Key header is required" },
    { status: 404, kind: "replay_target_not_found", message: "Not Found" },
    {
      status: 409,
      kind: "replay_requires_distributed_execution",
      message: "replay requires distributed execution mode for re-executing tasks",
    },
    { status: 422, kind: "replay_safe_refusal", message: "replay: baseline task is not replay safe" },
    // Overloaded codes: same status, different cause -> neutral kind (not the specific one).
    { status: 400, kind: "replay_bad_request", message: "bad request" },
    { status: 409, kind: "replay_conflict", message: "replay: baseline run is quarantined" },
    { status: 413, kind: "replay_request_too_large", message: "request body too large" },
    { status: 422, kind: "replay_refused", message: "replay: missing execution descriptor" },
  ])("postReplay maps $status to $kind", async ({ status, kind, message }) => {
    mockFetch.mockResolvedValue(errorResponse(status, { message }));

    await expect(api.postReplay("job-1", "run-1", { set: {} }, "idem-123")).rejects.toMatchObject({
      status,
      kind,
      message,
    });
  });

  it("throws ApiError instances for typed data-verb failures", async () => {
    mockFetch.mockResolvedValue(errorResponse(409, { message: "replay requires distributed execution mode" }));

    await expect(api.postReplay("job-1", "run-1", { set: {} }, "idem-123")).rejects.toBeInstanceOf(ApiError);
  });

  it("clears the API key and throws authentication_required on 401", async () => {
    setApiKey("csk_test_secret");
    mockFetch.mockResolvedValue(errorResponse(401, { message: "unauthorized" }));

    await expect(api.getRunDiff("job-1", "left", "right")).rejects.toMatchObject({
      status: 401,
      kind: "authentication_required",
    });

    // The 401 cleared the key: a subsequent call carries no Authorization header.
    mockFetch.mockResolvedValue(okResponse({}, 200));
    await api.getReceipt("job-1", "run-1");
    const [, init] = mockFetch.mock.calls.at(-1) as [string, RequestInit];
    const headers = (init.headers ?? {}) as Record<string, string>;
    expect(headers.Authorization).toBeUndefined();
  });
});
