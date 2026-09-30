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

    const result = await api.getAllJobRuns("job-1");

    expect(result.runs).toHaveLength(1250);
    expect(result.runs[0].id).toBe("run-0");
    expect(result.runs[1249].id).toBe("run-1249");
    expect(result.total).toBe(1250);
    expect(result.truncated).toBe(false);

    // 1250 rows at the client's 500-row page size is three requests, and the
    // offsets must be the server's own cursors, not client-side arithmetic.
    expect(requested.map((url) => url.searchParams.get("offset"))).toEqual([null, "500", "1000"]);
    expect(requested.every((url) => url.searchParams.get("limit") === "500")).toBe(true);
  });

  it("makes exactly one request when the first page is the whole history", async () => {
    const requested: URL[] = [];
    mockFetch.mockImplementation(paginatedServer(3, requested));

    const result = await api.getAllJobRuns("job-1");

    expect(result.runs).toHaveLength(3);
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

    const result = await api.getAllJobRuns("job-1");

    expect(result.runs).toHaveLength(2);
    expect(mockFetch).toHaveBeenCalledTimes(1);
  });

  it("stops on a cursor that does not advance instead of spinning", async () => {
    mockFetch.mockResolvedValue({
      ok: true,
      status: 200,
      text: () => Promise.resolve(JSON.stringify([run("run-0")])),
      headers: new Headers({ "X-Caesium-Total-Count": "9", "X-Caesium-Next-Offset": "0" }),
    });

    const result = await api.getAllJobRuns("job-1");

    expect(mockFetch).toHaveBeenCalledTimes(1);
    expect(result.runs).toHaveLength(1);
  });

  // Codex review round 2, finding 1: the underlying list is offset-paginated
  // over a newest-first order that can mutate mid-walk. A run created
  // between the offset=0 and offset=500 requests shifts every older run down
  // by one position, so the offset=500 request re-returns the run that was
  // already the last entry of the first page (here "run-499") alongside the
  // genuinely new entry pushed into view ("run-500").
  it("deduplicates a run that shifts into an already-fetched page", async () => {
    const pages = [
      {
        runs: Array.from({ length: 500 }, (_unused, i) => run(`run-${i}`)),
        total: 501,
        next: 500 as number | null,
      },
      // A run was inserted before this request fired: newest-first ordering
      // shifted, so offset 500 now lands on the previous page's last entry
      // plus one new one, instead of cleanly picking up where page 1 left off.
      { runs: [run("run-499"), run("run-500")], total: 502, next: null },
    ];
    let call = 0;
    mockFetch.mockImplementation(() => {
      const page = pages[call++];
      return Promise.resolve({
        ok: true,
        status: 200,
        text: () => Promise.resolve(JSON.stringify(page.runs)),
        headers: new Headers({
          "X-Caesium-Total-Count": String(page.total),
          ...(page.next === null ? {} : { "X-Caesium-Next-Offset": String(page.next) }),
        }),
      });
    });

    const result = await api.getAllJobRuns("job-1");

    const ids = result.runs.map((r) => r.id);
    expect(new Set(ids).size).toBe(ids.length);
    expect(ids).toHaveLength(501);
    expect(ids.filter((id) => id === "run-499")).toHaveLength(1);
  });

  // Codex review round 2, finding 2: the walk's row cap (10,000, mirroring
  // getAllPartitions' fan-out safety valve) is an accepted, deliberate
  // limit — but it must be visible on the result, not a silent truncation.
  it("marks truncated when the walk hits the row cap with more server history left", async () => {
    mockFetch.mockImplementation(paginatedServer(10_001, []));

    const result = await api.getAllJobRuns("job-1");

    expect(result.runs).toHaveLength(10_000);
    expect(result.truncated).toBe(true);
    expect(result.total).toBe(10_001);
  });

  it("is not truncated when the row cap and the list's true end coincide", async () => {
    mockFetch.mockImplementation(paginatedServer(10_000, []));

    const result = await api.getAllJobRuns("job-1");

    expect(result.runs).toHaveLength(10_000);
    expect(result.truncated).toBe(false);
  });
});
