import { render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { type ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  api,
  type AssertionInstance,
  type AssertionResult,
  type RunAssertions,
  type TaskAssertions,
  type TaskRun,
} from "@/lib/api";
import { resultHeadline } from "../assertion-utils";
import { DataAssertionsPanel, RunAssertionPanels } from "../DataAssertionsPanel";

vi.mock("@/lib/auth", () => ({
  usePrincipal: () => ({ subject: "viewer", role: "viewer", isScoped: false }),
  withAuthHeaders: () => ({}),
}));
vi.mock("@tanstack/react-router", () => ({
  Link: ({ children }: { children: ReactNode }) => <a href="#">{children}</a>,
}));

const dataset = "warehouse/orders";
const task = { id: "tr-1", task_id: "task-1" } as TaskRun;

const passMin: AssertionResult = {
  dataset,
  metric: "rowCount",
  assertion: "min",
  outcome: "passed",
  observed: 1200,
  bound: 1000,
};
const violatedMax: AssertionResult = {
  dataset,
  metric: "rowCount",
  assertion: "max",
  outcome: "violated",
  disposition: "warn",
  observed: 1200,
  bound: 1100,
  message: "observed 1200 exceeds the declared max 1100",
};
const deltaPass: AssertionResult = {
  dataset,
  metric: "rowCount",
  assertion: "deltaFromBaseline",
  outcome: "passed",
  observed: 105,
  bound: 50,
  baseline_median: 100,
  baseline_samples: 9,
  delta_from_baseline: "50%",
};

function instance(
  results: AssertionResult[] | undefined,
  patch: Partial<AssertionInstance> = {},
): AssertionInstance {
  return {
    task_run_id: "tr-1",
    status: "succeeded",
    attempt: 1,
    cache_hit: false,
    verdict: "passed",
    results,
    ...patch,
  };
}
function recordedFor(patch: Partial<TaskAssertions>): RunAssertions {
  return {
    run_id: "run-1",
    job_id: "job-1",
    tasks: [
      {
        task_id: "task-1",
        fanned: false,
        instance_count: 1,
        verdict_counts: { passed: 1 },
        instances: [instance([passMin])],
        ...patch,
      },
    ],
  };
}

function show(children: ReactNode) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>{children}</QueryClientProvider>,
  );
}
function enable(on = true, recorded?: RunAssertions) {
  vi.spyOn(api, "getSystemFeatures").mockResolvedValue({
    data_assertions_enabled: on,
  } as Awaited<ReturnType<typeof api.getSystemFeatures>>);
  return vi
    .spyOn(api, "getRunAssertions")
    .mockResolvedValue(recorded ?? recordedFor({}));
}
function panel(extra: Partial<TaskRun> = {}) {
  return show(
    <DataAssertionsPanel
      task={{ ...task, ...extra }}
      jobId="job-1"
      runId="run-1"
    />,
  );
}

beforeEach(() => {
  vi.restoreAllMocks();
});

describe("passing assertion results", () => {
  it("shows a passing check with its outcome, observed value and bound on a task with no violations", async () => {
    enable();
    panel();
    const row = await screen.findByTestId("assertion-result");
    expect(row).toHaveAttribute("data-outcome", "passed");
    expect(row).toHaveTextContent("rowCount.min: Passed");
    expect(row).toHaveTextContent("Observed: 1200");
    expect(row).toHaveTextContent("Bound: 1000");
  });

  it("shows passes beside the violation, and a passing deltaFromBaseline keeps its sparkline", async () => {
    enable(
      true,
      recordedFor({
        verdict_counts: { violated: 1 },
        instances: [
          instance([passMin, violatedMax, deltaPass], { verdict: "violated" }),
        ],
      }),
    );
    vi.spyOn(api, "getDatasetMetrics").mockResolvedValue({
      namespace: "",
      name: dataset,
      metric: "rowCount",
      series: [],
      total: 9,
      limit: 200,
      offset: 0,
      baseline: {
        values: [95, 100, 105],
        samples: 3,
        median: 100,
        p10: 96,
        p90: 104,
        as_of: "2026-09-09T12:00:00Z",
      },
      window: 20,
      min_samples: 5,
      seeding: false,
    });
    panel();
    const rows = await screen.findAllByTestId("assertion-result");
    expect(rows.map((r) => r.getAttribute("data-outcome"))).toEqual([
      "passed",
      "violated",
      "passed",
    ]);
    expect(rows[1]).toHaveTextContent("Violated, warning only");
    expect(rows[1]).toHaveTextContent("exceeds the declared max");
    expect(rows[2]).toHaveTextContent("Recorded baseline: median 100");
    expect(await within(rows[2]).findByTestId("baseline-sparkline")).toBeVisible();
    expect(screen.getByTestId("baseline-recorded-bound")).toBeInTheDocument();
  });

  it("does not describe a cold-start pass, an abstention or a lost metric as a clean pass", () => {
    expect(
      resultHeadline({ ...deltaPass, seeding: true, baseline_samples: 3 }),
    ).toBe("Passed during cold start (3 baseline samples, not enforced)");
    expect(
      resultHeadline({
        ...deltaPass,
        outcome: "undecided",
        reason: "no_baseline",
      }),
    ).toBe("Not decided: no clean baseline yet");
    expect(
      resultHeadline({
        ...passMin,
        assertion: "unavailable",
        outcome: "unavailable",
        reason: "marker_stream_truncated",
      }),
    ).toBe("Not observed: the metric stream was truncated");
    expect(
      resultHeadline({
        ...passMin,
        assertion: "missing",
        outcome: "violated",
        disposition: "hold",
      }),
    ).toBe("Violated, held the dataset");
    expect(resultHeadline(passMin)).toBe("Passed");
  });

  it("says a bound was withheld rather than showing none", async () => {
    enable(
      true,
      recordedFor({
        instances: [
          instance([
            {
              ...deltaPass,
              bound: undefined,
              bound_withheld: true,
              baseline_median: undefined,
              baseline_samples: undefined,
            },
          ]),
        ],
      }),
    );
    vi.spyOn(api, "getDatasetMetrics").mockRejectedValue(new Error("no"));
    panel();
    const row = await screen.findByTestId("assertion-result");
    expect(row).toHaveTextContent("Allowed deviation: withheld");
    expect(row).not.toHaveTextContent("Allowed deviation: —");
  });

  it("states that a cache hit ran no assertion", async () => {
    enable(
      true,
      recordedFor({
        verdict_counts: { not_evaluated: 1 },
        instances: [
          instance(undefined, { verdict: "not_evaluated", cache_hit: true }),
        ],
      }),
    );
    panel();
    expect(await screen.findByTestId("assertion-not-evaluated")).toHaveTextContent(
      "no assertion ran",
    );
    expect(screen.queryByText("Passed")).toBeNull();
  });
});

describe("fan-out groups", () => {
  it("summarises every partition's own verdict and never the head row's violations", async () => {
    enable(
      true,
      recordedFor({
        fanned: true,
        instance_count: 3,
        verdict_counts: { violated: 1, passed: 2 },
        instances: [
          instance([passMin], {
            task_run_id: "a",
            partition_value: "eu",
            partition_index: 0,
          }),
          instance([{ ...passMin, outcome: "violated", disposition: "warn", message: "us is short" }], {
            task_run_id: "b",
            partition_value: "us",
            partition_index: 1,
            verdict: "violated",
          }),
          instance([passMin], {
            task_run_id: "c",
            partition_value: "ap",
            partition_index: 2,
          }),
        ],
      }),
    );
    panel({
      partition_count: 3,
      // The collapsed head carries partition 0's violations; the group view
      // must not present them as the group's.
      data_violations: [
        {
          dataset,
          metric: "rowCount",
          assertion: "min",
          message: "head violation",
        },
      ],
    });
    expect(await screen.findByTestId("assertion-group-summary")).toHaveTextContent(
      "3 partitions: 1 violated, 2 passed",
    );
    const instances = screen.getAllByTestId("assertion-instance");
    expect(instances.map((i) => i.getAttribute("data-verdict"))).toEqual([
      "passed",
      "violated",
      "passed",
    ]);
    expect(instances[1]).toHaveTextContent("us: violated");
    expect(instances[1]).toHaveTextContent("us is short");
    expect(screen.queryByText("head violation")).toBeNull();
  });
});

describe("old rows, the gate and unrelated tasks", () => {
  it("still shows violations recorded before results existed and claims nothing passed", async () => {
    enable(true, { run_id: "run-1", job_id: "job-1", tasks: [] });
    panel({
      data_violations: [
        {
          dataset,
          metric: "rowCount",
          assertion: "min",
          observed: 12,
          bound: 1000,
          message: "legacy breach",
        },
      ],
    });
    expect(await screen.findByTestId("assertion-evidence")).toHaveTextContent(
      "legacy breach",
    );
    await waitFor(() => expect(api.getRunAssertions).toHaveBeenCalled());
    expect(screen.queryByTestId("assertion-result")).toBeNull();
    expect(screen.queryByText(/Passed/)).toBeNull();
  });

  it("falls back to the run payload's violations for a single row whose results were not recorded", async () => {
    enable(
      true,
      recordedFor({
        verdict_counts: { violated: 1 },
        instances: [instance(undefined, { verdict: "violated" })],
      }),
    );
    panel({
      data_violations: [
        {
          dataset,
          metric: "rowCount",
          assertion: "min",
          message: "legacy breach",
        },
      ],
    });
    expect(await screen.findByTestId("assertion-evidence")).toHaveTextContent(
      "legacy breach",
    );
  });

  it("renders nothing and requests nothing while the circuit breaker is off", async () => {
    const recorded = enable(false);
    panel();
    await waitFor(() => expect(api.getSystemFeatures).toHaveBeenCalled());
    expect(screen.queryByTestId("data-assertions-panel")).toBeNull();
    expect(recorded).not.toHaveBeenCalled();
  });

  it("renders nothing for a task with no assertions", async () => {
    enable(true, { run_id: "run-1", job_id: "job-1", tasks: [] });
    panel();
    await waitFor(() => expect(api.getRunAssertions).toHaveBeenCalled());
    expect(screen.queryByTestId("data-assertions-panel")).toBeNull();
  });

  it("run page lists a panel only for tasks that have evidence", async () => {
    enable(true, recordedFor({}));
    show(
      <RunAssertionPanels
        jobId="job-1"
        runId="run-1"
        tasks={[
          task,
          { id: "tr-2", task_id: "task-2" } as TaskRun,
        ]}
      />,
    );
    expect(await screen.findAllByTestId("data-assertions-panel")).toHaveLength(1);
  });
});
