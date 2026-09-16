import { beforeEach, describe, expect, it, vi } from "vitest";
import { api, type JobRun } from "@/lib/api";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

function run(id: string, overrides: Partial<JobRun> = {}): JobRun {
  return {
    id,
    job_id: "job-1",
    status: "succeeded",
    quarantine: false,
    started_at: "2026-01-01T00:00:00Z",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    tasks: [],
    ...overrides,
  };
}

/**
 * A server that serves `total` runs newest-first through the bare-array body
 * + `X-Caesium-Total-Count` / `X-Caesium-Next-Offset` headers, honoring
 * limit/offset — the contract in api/rest/controller/job/run/list.go.
 */
function paginatedServer(total: number, requested: URL[]) {
  return (input: string) => {
    const url = new URL(input, "http://localhost");
    requested.push(url);
    const limit = Number(url.searchParams.get("limit") ?? 100);
    const offset = Number(url.searchParams.get("offset") ?? 0);
    const count = Math.max(0, Math.min(limit, total - offset));
    const runs = Array.from({ length: count }, (_unused, i) => run(`run-${offset + i}`));
    const next = offset + runs.length;
    return Promise.resolve({
      ok: true,
      status: 200,
      text: () => Promise.resolve(JSON.stringify(runs)),
      headers: new Headers({
        "X-Caesium-Total-Count": String(total),
        ...(next >= total ? {} : { "X-Caesium-Next-Offset": String(next) }),
      }),
    });
  };
}

describe("api.getJobRuns", () => {
  beforeEach(() => {
    mockFetch.mockReset();
  });

  it("surfaces total and nextOffset from the response headers", async () => {
    mockFetch.mockImplementation(paginatedServer(250, []));

    const page = await api.getJobRuns("job-1", { limit: 100 });

    expect(page.runs).toHaveLength(100);
    expect(page.total).toBe(250);
    expect(page.nextOffset).toBe(100);
  });

  it("reports nextOffset null on the final page", async () => {
    mockFetch.mockImplementation(paginatedServer(3, []));

    const page = await api.getJobRuns("job-1", { limit: 100 });

    expect(page.runs).toHaveLength(3);
    expect(page.nextOffset).toBeNull();
  });
});

describe("api.getAllJobRuns", () => {
  beforeEach(() => {
    mockFetch.mockReset();
  });

  // This is the regression a codex review caught: the console's job detail
  // page called the (now-paginated) list endpoint with no params and
  // rendered the bare array directly, so a job with more than the server's
  // default page size (100) silently lost its older run history. Every
  // caller that needs the WHOLE history must walk pages instead.
  it("follows next_offset until a job's whole run history is collected", async () => {
    const requested: URL[] = [];
    mockFetch.mockImplementation(paginatedServer(1250, requested));

    const runs = await api.getAllJobRuns("job-1");

    expect(runs).toHaveLength(1250);
    expect(runs[0].id).toBe("run-0");
    expect(runs[1249].id).toBe("run-1249");

    // 1250 rows at the client's 500-row page size is three requests, and the
    // offsets must be the server's own cursors, not client-side arithmetic.
    expect(requested.map((url) => url.searchParams.get("offset"))).toEqual([null, "500", "1000"]);
    expect(requested.every((url) => url.searchParams.get("limit") === "500")).toBe(true);
  });

  it("makes exactly one request when the first page is the whole history", async () => {
    const requested: URL[] = [];
    mockFetch.mockImplementation(paginatedServer(3, requested));

    const runs = await api.getAllJobRuns("job-1");

    expect(runs).toHaveLength(3);
    expect(requested).toHaveLength(1);
  });

  it("treats an absent next-offset header as the end of the list", async () => {
    // The shape a server that predates pagination headers returns. Reading
    // it as "offset 0, keep going" would loop forever.
    mockFetch.mockResolvedValue({
      ok: true,
      status: 200,
      text: () => Promise.resolve(JSON.stringify([run("run-0"), run("run-1")])),
      headers: new Headers(),
    });

    const runs = await api.getAllJobRuns("job-1");

    expect(runs).toHaveLength(2);
    expect(mockFetch).toHaveBeenCalledTimes(1);
  });

  it("stops on a cursor that does not advance instead of spinning", async () => {
    mockFetch.mockResolvedValue({
      ok: true,
      status: 200,
      text: () => Promise.resolve(JSON.stringify([run("run-0")])),
      headers: new Headers({ "X-Caesium-Total-Count": "9", "X-Caesium-Next-Offset": "0" }),
    });

    const runs = await api.getAllJobRuns("job-1");

    expect(mockFetch).toHaveBeenCalledTimes(1);
    expect(runs).toHaveLength(1);
  });
});
