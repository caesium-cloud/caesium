import { beforeEach, describe, expect, it, vi } from "vitest";
import { api, taskLogsURL, type PartitionInstance } from "@/lib/api";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

function okResponse(body: unknown) {
  return {
    ok: true,
    status: 200,
    text: () => Promise.resolve(JSON.stringify(body)),
  };
}

function instance(index: number, overrides: Partial<PartitionInstance> = {}): PartitionInstance {
  return {
    value: `p${index}`,
    index,
    status: "succeeded",
    attempt: 1,
    cache_hit: false,
    task_run_id: `tr-${index}`,
    ...overrides,
  };
}

/**
 * A server that serves `total` instances through the paginated envelope,
 * honoring limit/offset and reporting `next_offset: null` on the last page —
 * the contract in api/rest/controller/job/run/partitions.go.
 */
function paginatedServer(total: number, requested: URL[]) {
  return (input: string) => {
    const url = new URL(input, "http://localhost");
    requested.push(url);
    const limit = Number(url.searchParams.get("limit") ?? 100);
    const offset = Number(url.searchParams.get("offset") ?? 0);
    const partitions = Array.from(
      { length: Math.max(0, Math.min(limit, total - offset)) },
      (_unused, i) => instance(offset + i),
    );
    const next = offset + partitions.length;
    return Promise.resolve(
      okResponse({
        partitions,
        total,
        limit,
        offset,
        next_offset: next >= total ? null : next,
        status_counts: { succeeded: total },
      }),
    );
  };
}

describe("api.getAllPartitions", () => {
  beforeEach(() => {
    mockFetch.mockReset();
  });

  it("follows next_offset until the list is exhausted", async () => {
    const requested: URL[] = [];
    mockFetch.mockImplementation(paginatedServer(1250, requested));

    const result = await api.getAllPartitions("job-1", "run-1", "task-1");

    expect(result.partitions).toHaveLength(1250);
    expect(result.partitions[0].value).toBe("p0");
    expect(result.partitions[1249].value).toBe("p1249");
    expect(result.total).toBe(1250);
    expect(result.status_counts).toEqual({ succeeded: 1250 });

    // 1250 rows at the client's 500-row page size is three requests, and the
    // offsets must be the server's own cursors — not client-side arithmetic.
    expect(requested.map((url) => url.searchParams.get("offset"))).toEqual([null, "500", "1000"]);
    expect(requested.every((url) => url.searchParams.get("limit") === "500")).toBe(true);
  });

  it("makes exactly one request when the first page is the whole group", async () => {
    const requested: URL[] = [];
    mockFetch.mockImplementation(paginatedServer(3, requested));

    const result = await api.getAllPartitions("job-1", "run-1", "task-1");

    expect(result.partitions).toHaveLength(3);
    expect(requested).toHaveLength(1);
  });

  it("passes the status filter through on every page", async () => {
    const requested: URL[] = [];
    mockFetch.mockImplementation(paginatedServer(700, requested));

    await api.getAllPartitions("job-1", "run-1", "task-1", "failed");

    expect(requested).toHaveLength(2);
    expect(requested.every((url) => url.searchParams.get("status") === "failed")).toBe(true);
  });

  it("treats an absent next_offset as the end of the list", async () => {
    // The shape a server that predates pagination returns. Reading it as
    // "offset 0, keep going" would loop forever.
    mockFetch.mockResolvedValue(okResponse({ partitions: [instance(0), instance(1)] }));

    const result = await api.getAllPartitions("job-1", "run-1", "task-1");

    expect(result.partitions).toHaveLength(2);
    expect(result.total).toBe(2);
    expect(mockFetch).toHaveBeenCalledTimes(1);
  });

  it("stops on a cursor that does not advance instead of spinning", async () => {
    mockFetch.mockResolvedValue(
      okResponse({ partitions: [instance(0)], total: 9, limit: 500, offset: 0, next_offset: 0 }),
    );

    const result = await api.getAllPartitions("job-1", "run-1", "task-1");

    expect(mockFetch).toHaveBeenCalledTimes(1);
    // The shortfall stays visible: total still reports the group the server
    // claims, so a caller can tell the walk did not complete.
    expect(result.partitions).toHaveLength(1);
    expect(result.total).toBe(9);
  });
});

describe("taskLogsURL", () => {
  it("names only the catalog task for an unfanned task", () => {
    expect(taskLogsURL("job-1", "run-1", "task-1")).toBe(
      "/v1/jobs/job-1/runs/run-1/logs?task_id=task-1",
    );
  });

  it("selects one instance by task_run_id", () => {
    const url = new URL(taskLogsURL("job-1", "run-1", "task-1", { taskRunId: "tr-7" }), "http://x");
    expect(url.searchParams.get("task_id")).toBe("task-1");
    expect(url.searchParams.get("task_run_id")).toBe("tr-7");
  });

  it("selects one instance by partition value", () => {
    const url = new URL(taskLogsURL("job-1", "run-1", "task-1", { partition: "eu-west-1" }), "http://x");
    expect(url.searchParams.get("partition")).toBe("eu-west-1");
    expect(url.searchParams.get("task_run_id")).toBeNull();
  });

  it("prefers task_run_id when both selectors are supplied", () => {
    // task_run_id is the primary key and is authoritative; sending both would
    // let the server pick, and the two could disagree after a retry.
    const url = new URL(
      taskLogsURL("job-1", "run-1", "task-1", { taskRunId: "tr-7", partition: "alpha" }),
      "http://x",
    );
    expect(url.searchParams.get("task_run_id")).toBe("tr-7");
    expect(url.searchParams.get("partition")).toBeNull();
  });
});
