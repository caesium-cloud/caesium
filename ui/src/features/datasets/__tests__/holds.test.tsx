import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { type ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { api, ApiError, type DatasetHold, type TaskRun } from "@/lib/api";
import { HoldPanel } from "../HoldPanel";
import { DataAssertionsPanel } from "../DataAssertionsPanel";
import { parseHoldSkipReason, readIdentityHold } from "../hold-utils";

const { principal } = vi.hoisted(() => ({
  principal: { subject: "operator", role: "operator", isScoped: false },
}));
vi.mock("@/lib/auth", () => ({
  usePrincipal: () => principal,
  withAuthHeaders: () => ({}),
}));
vi.mock("@tanstack/react-router", () => ({
  Link: ({
    children,
    params,
  }: {
    children: ReactNode;
    params?: Record<string, string>;
  }) => (
    <a
      href={params?.runId ? `/jobs/${params.jobId}/runs/${params.runId}` : "#"}
    >
      {children}
    </a>
  ),
}));

const hold: DatasetHold = {
  id: "original",
  namespace: "",
  name: "warehouse/orders%2Fraw",
  status: "active",
  reason: "min",
  occurrence_count: 2,
  opened_at: "2026-09-09T00:00:00Z",
  held_by_job_id: "producer",
  held_by_run_id: "run",
  violations: [
    {
      dataset: "warehouse/orders%2Fraw",
      metric: "rowCount",
      assertion: "min",
      observed: 0,
      bound: 50,
      message: "Too few rows",
    },
  ],
};
function show(children: ReactNode) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>{children}</QueryClientProvider>,
  );
}
beforeEach(() => {
  vi.restoreAllMocks();
  principal.subject = "operator";
  principal.role = "operator";
  principal.isScoped = false;
});

describe("hold release boundary", () => {
  it.each(["viewer", "runner", "anonymous", "scoped"])(
    "does not offer release to %s",
    (role) => {
      principal.role = role === "scoped" ? "operator" : role;
      principal.isScoped = role === "scoped";
      principal.subject = role === "anonymous" ? "" : role;
      const release = vi.spyOn(api, "releaseDatasetHold");
      show(<HoldPanel hold={hold} />);
      expect(
        screen.getByRole("button", { name: "Release hold" }),
      ).toBeDisabled();
      expect(release).not.toHaveBeenCalled();
    },
  );
  it("requires a trimmed reason, preserves a real zero observation, and submits advisory assertion-kind tolerances", async () => {
    const release = vi.spyOn(api, "releaseDatasetHold").mockResolvedValue({
      hold: {
        ...hold,
        status: "released",
        released_by: "operator",
        release_note: "seasonal",
      },
    });
    show(<HoldPanel hold={hold} />);
    expect(screen.getByText(/Observed: 0/)).toBeVisible();
    const button = screen.getByRole("button", { name: "Release hold" });
    fireEvent.change(screen.getByLabelText("Release reason"), {
      target: { value: "  " },
    });
    expect(button).toBeDisabled();
    fireEvent.change(screen.getByLabelText("Release reason"), {
      target: { value: " seasonal " },
    });
    fireEvent.change(screen.getByLabelText("Tolerance for min"), {
      target: { value: "24h" },
    });
    fireEvent.click(button);
    await waitFor(() =>
      expect(release).toHaveBeenCalledWith("original", "seasonal", {
        min: "24h",
      }),
    );
    expect(await screen.findByText(/Dataset hold · released/)).toBeVisible();
  });
  it("refuses to retry or retarget a conflicting historical hold", async () => {
    const release = vi
      .spyOn(api, "releaseDatasetHold")
      .mockRejectedValue(new ApiError(409, "hold not active"));
    show(<HoldPanel hold={hold} />);
    fireEvent.change(screen.getByLabelText("Release reason"), {
      target: { value: "reviewed" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Release hold" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "review any new active hold separately",
    );
    expect(screen.getByRole("button", { name: "Release hold" })).toBeDisabled();
    expect(release).toHaveBeenCalledTimes(1);
    expect(release.mock.calls[0][0]).toBe(hold.id);
  });
});

describe("hold producing-run evidence", () => {
  it("keeps the opening run link and shows a later producer's run without an invented job", () => {
    show(
      <HoldPanel
        hold={{ ...hold, last_breach_run_id: "different-producer-run" }}
      />,
    );
    expect(
      screen.getByRole("link", { name: "producer · run" }),
    ).toHaveAttribute("href", "/jobs/producer/runs/run");
    const latest = screen.getByTestId("hold-latest-breach-run");
    expect(latest).toHaveTextContent("different-producer-run");
    expect(latest.querySelector("a")).toBeNull();
    expect(
      screen.queryByRole("link", { name: "different-producer-run" }),
    ).toBeNull();
  });
});

describe("identity and pagination", () => {
  const holdID = "47e69470-a0b9-49aa-a1b1-e343c71b1fa6";
  it.each([
    ["/warehouse/orders%2Fraw", "", "warehouse/orders%2Fraw"],
    ["/orders hold=raw", "", "orders hold=raw"],
    [
      "tenant%2Fblue/warehouse/orders%2Fraw hold=source%26a",
      "tenant%2Fblue",
      "warehouse/orders%2Fraw hold=source%26a",
    ],
  ])(
    "preserves identity %s in bare and UUID-suffixed reasons",
    (identity, namespace, name) => {
      expect(parseHoldSkipReason(`dataset_hold:${identity}`)).toEqual({
        namespace,
        name,
        hold: undefined,
      });
      expect(
        parseHoldSkipReason(`dataset_hold:${identity} hold=${holdID}`),
      ).toEqual({ namespace, name, hold: holdID });
    },
  );
  it("does not strip nonterminal or malformed hold suffixes", () => {
    expect(
      parseHoldSkipReason(`dataset_hold:/orders hold=${holdID} trailing`),
    ).toEqual({
      namespace: "",
      name: `orders hold=${holdID} trailing`,
      hold: undefined,
    });
    expect(parseHoldSkipReason("dataset_hold:/orders hold=not-a-uuid")).toEqual(
      { namespace: "", name: "orders hold=not-a-uuid", hold: undefined },
    );
    expect(parseHoldSkipReason("trigger rule not satisfied")).toBeNull();
  });
  it("pages exact-identity history without substituting a newer active hold", async () => {
    const replacement = { ...hold, id: "replacement" };
    const read = vi
      .spyOn(api, "getDatasetHolds")
      .mockResolvedValueOnce({
        holds: [replacement],
        total: 2,
        limit: 1,
        offset: 0,
      })
      .mockResolvedValueOnce({
        holds: [{ ...hold, status: "released" }],
        total: 2,
        limit: 1,
        offset: 1,
      });
    expect((await readIdentityHold("", hold.name, hold.id))?.id).toBe(
      "original",
    );
    expect(read.mock.calls.map(([params]) => params?.offset)).toEqual([0, 1]);
    expect(
      read.mock.calls.every(
        ([params]) =>
          params?.namespace === "" &&
          params.name === hold.name &&
          params.status === "all",
      ),
    ).toBe(true);
  });
});

describe("assertion evidence", () => {
  it("does not fetch or show a disabled assertion surface", async () => {
    vi.spyOn(api, "getSystemFeatures").mockResolvedValue({
      data_assertions_enabled: false,
    } as Awaited<ReturnType<typeof api.getSystemFeatures>>);
    const metrics = vi.spyOn(api, "getDatasetMetrics");
    show(
      <DataAssertionsPanel
        task={{ data_violations: hold.violations } as TaskRun}
      />,
    );
    await waitFor(() => expect(api.getSystemFeatures).toHaveBeenCalled());
    expect(screen.queryByTestId("data-assertions-panel")).toBeNull();
    expect(metrics).not.toHaveBeenCalled();
  });
  it("uses real clean baseline values and distinguishes a recorded observation from current history", async () => {
    vi.spyOn(api, "getSystemFeatures").mockResolvedValue({
      data_assertions_enabled: true,
    } as Awaited<ReturnType<typeof api.getSystemFeatures>>);
    vi.spyOn(api, "getDatasetMetrics").mockResolvedValue({
      namespace: "",
      name: hold.name,
      metric: "rowCount",
      series: [],
      total: 30,
      limit: 200,
      offset: 0,
      baseline: {
        values: [90, 100, 110],
        samples: 3,
        median: 100,
        p10: 92,
        p90: 108,
        as_of: "2026-09-09T12:00:00Z",
      },
      window: 20,
      min_samples: 5,
      seeding: true,
    });
    show(
      <DataAssertionsPanel
        task={
          {
            data_violations: [
              {
                dataset: hold.name,
                metric: "rowCount",
                assertion: "deltaFromBaseline",
                observed: 1000,
                bound: 25,
                baseline_median: 50,
                baseline_samples: 6,
                message: "10x",
                delta_from_baseline: "50%",
              },
            ],
          } as TaskRun
        }
      />,
    );
    const chart = await screen.findByRole("img");
    expect(chart).toHaveAccessibleName(
      /3 samples, median 100.*Recorded observation 1,000/,
    );
    expect(screen.getByText(/Recorded baseline: median 50/)).toBeVisible();
    expect(screen.getByText(/live history may differ/)).toBeVisible();
    expect(screen.getByTestId("baseline-band")).toBeInTheDocument();
    expect(screen.getByTestId("baseline-observation")).toBeInTheDocument();
  });
});
