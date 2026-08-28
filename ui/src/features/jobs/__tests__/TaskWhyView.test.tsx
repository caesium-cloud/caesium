import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactElement, ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { TaskWhyView } from "../TaskWhyView";
import type { WhyExplanation } from "@/lib/api";

const { mockFetch } = vi.hoisted(() => ({ mockFetch: vi.fn() }));

vi.mock("@/lib/auth", () => ({
  withAuthHeaders: () => ({}),
}));

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children }: { children: ReactNode }) => <a href="#">{children}</a>,
}));

function jsonResponse(body: unknown) {
  return {
    ok: true,
    status: 200,
    headers: { get: () => "application/json" },
    json: () => Promise.resolve(body),
    text: () => Promise.resolve(JSON.stringify(body)),
  };
}

/**
 * valuesModeHit is the shape spec §4.3 is about: a CACHE HIT under
 * `cache.chain: values`, where the two blobs disagree about predecessorHashes
 * but the keys are equal. The server reports the exclusion as a note plus a
 * kind:"excluded" entry, never as a discriminating field.
 */
const valuesModeHit: WhyExplanation = {
  runId: "run-1",
  jobId: "job-1",
  taskId: "task-1",
  taskName: "mid",
  taskRunId: "taskrun-1",
  verdict: "CACHE_HIT",
  status: "cached",
  cacheEnabled: true,
  hash: "abc123",
  summary:
    "CACHE HIT — every hashed input identical to the cached run; predecessor hashes excluded (chain: values)",
  trigger: { type: "cron", alias: "nightly" },
  baseline: { kind: "cache_origin", runId: "run-0" },
  diff: {
    hashEqual: true,
    subjectHash: "abc123",
    baselineHash: "abc123",
    notes: ["predecessor hashes excluded (chain: values)"],
    changes: [
      { field: "predecessorHashes", kind: "excluded", note: "excluded (chain: values)" },
    ],
  },
};

describe("TaskWhyView chain exclusion", () => {
  function renderView(component: ReactElement) {
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    return render(
      <QueryClientProvider client={queryClient}>{component}</QueryClientProvider>,
    );
  }

  beforeEach(() => {
    mockFetch.mockReset();
    globalThis.fetch = mockFetch as unknown as typeof fetch;
  });

  it("names the chain: values exclusion", async () => {
    mockFetch.mockResolvedValue(jsonResponse(valuesModeHit));

    renderView(<TaskWhyView jobId="job-1" runId="run-1" taskName="mid" />);

    const note = await screen.findByTestId("task-why-chain-exclusion");
    expect(note.textContent).toContain("predecessor hashes excluded (chain: values)");
  });

  it("does not report an excluded input as a changed field", async () => {
    mockFetch.mockResolvedValue(jsonResponse(valuesModeHit));

    renderView(<TaskWhyView jobId="job-1" runId="run-1" taskName="mid" />);

    await screen.findByTestId("task-why-chain-exclusion");
    expect(screen.queryByText("Changed fields")).toBeNull();
    expect(screen.queryByText("Additional changed fields")).toBeNull();
    // hashEqual=true still wins the discriminating-field slot.
    expect(screen.getByTestId("task-why-discriminating-field").textContent).toContain(
      "hashEqual=true",
    );
  });

  it("names the exclusion for a fanned group, which carries no diff", async () => {
    // Spec §5.5 puts `fanOut` and `chain: values` on the same step, and a group
    // answer has N hashes and N baselines — so it has no `diff` to hang the note
    // on. It rides on `group.notes` instead, and the panel must still show it.
    mockFetch.mockResolvedValue(
      jsonResponse({
        ...valuesModeHit,
        verdict: "CACHE_MISS",
        status: "succeeded",
        summary:
          'FANNED GROUP — task "apply" ran 3 partition(s): 2 cached, 1 succeeded; predecessor hashes excluded (chain: values)',
        diff: undefined,
        group: {
          partitionCount: 3,
          statusCounts: { cached: 2, succeeded: 1 },
          cacheHits: 2,
          notes: ["predecessor hashes excluded (chain: values)"],
        },
      }),
    );

    renderView(<TaskWhyView jobId="job-1" runId="run-1" taskName="apply" />);

    const note = await screen.findByTestId("task-why-chain-exclusion");
    expect(note.textContent).toContain("predecessor hashes excluded (chain: values)");
  });

  it("omits the note entirely for a transitive explanation", async () => {
    mockFetch.mockResolvedValue(
      jsonResponse({
        ...valuesModeHit,
        summary: "CACHE HIT — every hashed input identical to the cached run",
        diff: { hashEqual: true, subjectHash: "abc123", baselineHash: "abc123" },
      }),
    );

    renderView(<TaskWhyView jobId="job-1" runId="run-1" taskName="mid" />);

    await screen.findByTestId("task-why-container");
    await waitFor(() =>
      expect(screen.queryByTestId("task-why-degraded-reason")).toBeNull(),
    );
    expect(screen.queryByTestId("task-why-chain-exclusion")).toBeNull();
  });
});
