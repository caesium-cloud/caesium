import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { type ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { api, type RunAssertions, type TaskRun } from "@/lib/api";
import {
  invalidateRunAssertions,
  taskEvidenceSignature,
  useRunAssertions,
  useSyncRunAssertions,
} from "../useDataAssertions";

const empty: RunAssertions = { run_id: "run-1", job_id: "job-1", tasks: [] };

function resultsOf(outcome: "violated" | "passed"): RunAssertions {
  return {
    run_id: "run-1",
    job_id: "job-1",
    tasks: [
      {
        task_id: "task-1",
        fanned: false,
        instance_count: 1,
        verdict_counts: { [outcome]: 1 },
        instances: [
          {
            task_run_id: "tr-1",
            status: "succeeded",
            attempt: 1,
            cache_hit: false,
            verdict: outcome,
            results: [
              {
                dataset: "d",
                metric: "rowCount",
                assertion: "min",
                outcome,
              },
            ],
          },
        ],
      },
    ],
  };
}

function task(patch: Partial<TaskRun> = {}): TaskRun {
  return {
    id: "tr-1",
    task_id: "task-1",
    status: "failed",
    attempt: 1,
    started_at: "2026-11-02T10:00:00Z",
    completed_at: "2026-11-02T10:00:05Z",
    ...patch,
  } as TaskRun;
}

function setup() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  return { client, wrapper };
}

function enable(on = true) {
  vi.spyOn(api, "getSystemFeatures").mockResolvedValue({
    data_assertions_enabled: on,
  } as Awaited<ReturnType<typeof api.getSystemFeatures>>);
}

// The page's two hooks, as RunDetailPage uses them: the read, and the sync that
// ties it to whatever the run's tasks show.
function usePage(runId: string, tasks: TaskRun[] | undefined, live = false) {
  useSyncRunAssertions("job-1", runId, tasks);
  return useRunAssertions("job-1", runId, live);
}

beforeEach(() => {
  vi.restoreAllMocks();
});

describe("useSyncRunAssertions", () => {
  it("reads a retry that started and finished between two polls, with the stream down", async () => {
    enable();
    const read = vi
      .spyOn(api, "getRunAssertions")
      .mockResolvedValueOnce(resultsOf("violated"))
      .mockResolvedValue(resultsOf("passed"));
    const { wrapper } = setup();

    // live stays false throughout: no event, no run in flight on screen.
    const { result, rerender } = renderHook(
      ({ tasks }) => usePage("run-1", tasks),
      { wrapper, initialProps: { tasks: [task()] } },
    );
    await waitFor(() => expect(read).toHaveBeenCalledTimes(1));
    expect(result.current.data?.tasks[0].instances[0].verdict).toBe("violated");

    // The next poll shows the run already finished again: same task, a new
    // attempt's timestamps, a new status.
    rerender({
      tasks: [
        task({
          status: "succeeded",
          attempt: 2,
          started_at: "2026-11-02T10:00:20Z",
          completed_at: "2026-11-02T10:00:25Z",
        }),
      ],
    });
    await waitFor(() => expect(read).toHaveBeenCalledTimes(2));
    await waitFor(() =>
      expect(result.current.data?.tasks[0].instances[0].verdict).toBe("passed"),
    );
  });

  it("notices a retry that ends in the status the previous attempt had", async () => {
    enable();
    const read = vi.spyOn(api, "getRunAssertions").mockResolvedValue(empty);
    const { wrapper } = setup();

    const { rerender } = renderHook(({ tasks }) => usePage("run-1", tasks), {
      wrapper,
      initialProps: { tasks: [task()] },
    });
    await waitFor(() => expect(read).toHaveBeenCalledTimes(1));

    // failed again, one attempt later
    rerender({
      tasks: [
        task({
          attempt: 2,
          started_at: "2026-11-02T10:00:20Z",
          completed_at: "2026-11-02T10:00:25Z",
        }),
      ],
    });
    await waitFor(() => expect(read).toHaveBeenCalledTimes(2));
  });

  it("reads again when the run stops being live (the terminal transition)", async () => {
    enable();
    const read = vi.spyOn(api, "getRunAssertions").mockResolvedValue(empty);
    const { wrapper } = setup();

    const { rerender } = renderHook(
      ({ tasks, live }) => usePage("run-1", tasks, live),
      {
        wrapper,
        initialProps: {
          tasks: [task({ status: "running", completed_at: undefined })],
          live: true,
        },
      },
    );
    await waitFor(() => expect(read).toHaveBeenCalledTimes(1));

    rerender({ tasks: [task({ status: "succeeded" })], live: false });
    await waitFor(() => expect(read).toHaveBeenCalledTimes(2));
  });

  it("does not read on the first load, nor on a poll that changed nothing", async () => {
    enable();
    const read = vi.spyOn(api, "getRunAssertions").mockResolvedValue(empty);
    const { wrapper } = setup();

    const { rerender } = renderHook(({ tasks }) => usePage("run-1", tasks), {
      wrapper,
      initialProps: { tasks: undefined as TaskRun[] | undefined },
    });
    await waitFor(() => expect(read).toHaveBeenCalledTimes(1));

    // The run loads: the first signature is recorded, not acted on.
    rerender({ tasks: [task()] });
    await new Promise((resolve) => setTimeout(resolve, 30));
    expect(read).toHaveBeenCalledTimes(1);

    // Polls return fresh objects with the same evidence, and a lease renewal
    // moves fields that are not evidence.
    for (let i = 0; i < 3; i++) {
      rerender({
        tasks: [
          {
            ...task(),
            claim_expires_at: `2026-11-02T10:01:0${i}Z`,
            updated_at: `2026-11-02T10:01:0${i}Z`,
          } as TaskRun,
        ],
      });
    }
    await new Promise((resolve) => setTimeout(resolve, 30));
    expect(read).toHaveBeenCalledTimes(1);
  });

  it("does not carry one run's evidence over to another", async () => {
    enable();
    const read = vi.spyOn(api, "getRunAssertions").mockResolvedValue(empty);
    const { wrapper } = setup();

    const { rerender } = renderHook(
      ({ runId, tasks }) => usePage(runId, tasks),
      { wrapper, initialProps: { runId: "run-1", tasks: [task()] } },
    );
    await waitFor(() => expect(read).toHaveBeenCalledTimes(1));

    rerender({ runId: "run-2", tasks: [task({ status: "succeeded" })] });
    await waitFor(() => expect(read).toHaveBeenCalledTimes(2)); // run-2's own first read
    await new Promise((resolve) => setTimeout(resolve, 30));
    expect(read).toHaveBeenCalledTimes(2);

    // Back to run-1, whose results are cached and fresh: it was not changed by
    // having looked at another run in between.
    rerender({ runId: "run-1", tasks: [task()] });
    await new Promise((resolve) => setTimeout(resolve, 30));
    expect(read).toHaveBeenCalledTimes(2);
  });

  it("notices a change of status alone", async () => {
    enable();
    const read = vi.spyOn(api, "getRunAssertions").mockResolvedValue(empty);
    const { wrapper } = setup();
    const bare = { started_at: undefined, completed_at: undefined };

    const { rerender } = renderHook(({ tasks }) => usePage("run-1", tasks), {
      wrapper,
      initialProps: { tasks: [task({ ...bare, status: "pending" })] },
    });
    await waitFor(() => expect(read).toHaveBeenCalledTimes(1));

    rerender({ tasks: [task({ ...bare, status: "running" })] });
    await waitFor(() => expect(read).toHaveBeenCalledTimes(2));
  });

  it("makes no request while the circuit breaker is off", async () => {
    enable(false);
    const read = vi.spyOn(api, "getRunAssertions").mockResolvedValue(empty);
    const { wrapper } = setup();

    const { rerender } = renderHook(({ tasks }) => usePage("run-1", tasks), {
      wrapper,
      initialProps: { tasks: [task()] },
    });
    await waitFor(() => expect(api.getSystemFeatures).toHaveBeenCalled());
    rerender({ tasks: [task({ status: "succeeded", attempt: 2 })] });
    await new Promise((resolve) => setTimeout(resolve, 30));
    expect(read).not.toHaveBeenCalled();
  });
});

describe("taskEvidenceSignature", () => {
  it("moves with an attempt and ignores what is not evidence", () => {
    const base = taskEvidenceSignature([task()]);
    expect(taskEvidenceSignature([task()])).toBe(base);
    expect(
      taskEvidenceSignature([
        { ...task(), claim_expires_at: "x", updated_at: "y" } as TaskRun,
      ]),
    ).toBe(base);
    expect(taskEvidenceSignature([task({ attempt: 2 })])).not.toBe(base);
    expect(
      taskEvidenceSignature([task({ completed_at: "2026-11-02T10:09:00Z" })]),
    ).not.toBe(base);
    expect(
      taskEvidenceSignature([task({ partition_status_counts: { failed: 1 } })]),
    ).not.toBe(base);
  });
});

describe("a first read still in flight when the run changes", () => {
  it("is replaced, not reused, so the older response never stays on screen", async () => {
    enable();
    let answerFirst: (value: RunAssertions) => void = () => undefined;
    const read = vi
      .spyOn(api, "getRunAssertions")
      .mockImplementationOnce(
        () => new Promise<RunAssertions>((resolve) => (answerFirst = resolve)),
      )
      .mockResolvedValue(resultsOf("passed"));
    const { wrapper } = setup();

    // The page opens on a running run and asks for its results; the request
    // is slow, and there is no cached data to fall back on.
    const { result, rerender } = renderHook(
      ({ tasks, live }) => usePage("run-1", tasks, live),
      {
        wrapper,
        initialProps: {
          tasks: [task({ status: "running", completed_at: undefined })],
          live: true,
        },
      },
    );
    await waitFor(() => expect(read).toHaveBeenCalledTimes(1));

    // The run finishes while that request is open: polling stops with it.
    rerender({ tasks: [task({ status: "succeeded" })], live: false });
    await waitFor(() => expect(read).toHaveBeenCalledTimes(2));
    await waitFor(() =>
      expect(result.current.data?.tasks[0].instances[0].verdict).toBe("passed"),
    );

    // The older request answers last, with what was true before the change.
    answerFirst(resultsOf("violated"));
    await new Promise((resolve) => setTimeout(resolve, 30));
    expect(result.current.data?.tasks[0].instances[0].verdict).toBe("passed");
    expect(read).toHaveBeenCalledTimes(2);
  });
});

describe("invalidateRunAssertions", () => {
  it("marks only that run's results stale", async () => {
    enable();
    const read = vi.spyOn(api, "getRunAssertions").mockResolvedValue(empty);
    const { client, wrapper } = setup();

    renderHook(() => useRunAssertions("job-1", "run-1", false), { wrapper });
    await waitFor(() => expect(read).toHaveBeenCalledTimes(1));

    await invalidateRunAssertions(client, "job-1", "run-1");
    await waitFor(() => expect(read).toHaveBeenCalledTimes(2));

    await invalidateRunAssertions(client, "job-1", "another-run");
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(read).toHaveBeenCalledTimes(2);
  });
});
