import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { type ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { api, type RunAssertions } from "@/lib/api";
import { invalidateRunAssertions, useRunAssertions } from "../useDataAssertions";

const empty: RunAssertions = { run_id: "run-1", job_id: "job-1", tasks: [] };

function setup() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  return { client, wrapper };
}

beforeEach(() => {
  vi.restoreAllMocks();
});

describe("useRunAssertions", () => {
  it("reads the results once more when the run stops being live", async () => {
    vi.spyOn(api, "getSystemFeatures").mockResolvedValue({
      data_assertions_enabled: true,
    } as Awaited<ReturnType<typeof api.getSystemFeatures>>);
    const read = vi.spyOn(api, "getRunAssertions").mockResolvedValue(empty);
    const { wrapper } = setup();

    const { rerender } = renderHook(
      ({ live }) => useRunAssertions("job-1", "run-1", live),
      { wrapper, initialProps: { live: true } },
    );
    await waitFor(() => expect(read).toHaveBeenCalledTimes(1));

    // The terminal transition arrives with no further poll due: the final
    // read must still happen, or results written after the last poll stay
    // missing.
    rerender({ live: false });
    await waitFor(() => expect(read).toHaveBeenCalledTimes(2));

    // And it is only the transition: staying terminal does not refetch.
    rerender({ live: false });
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(read).toHaveBeenCalledTimes(2);
  });

  it("does not read at all while the circuit breaker is off", async () => {
    vi.spyOn(api, "getSystemFeatures").mockResolvedValue({
      data_assertions_enabled: false,
    } as Awaited<ReturnType<typeof api.getSystemFeatures>>);
    const read = vi.spyOn(api, "getRunAssertions").mockResolvedValue(empty);
    const { wrapper } = setup();

    const { rerender } = renderHook(
      ({ live }) => useRunAssertions("job-1", "run-1", live),
      { wrapper, initialProps: { live: true } },
    );
    await waitFor(() => expect(api.getSystemFeatures).toHaveBeenCalled());
    rerender({ live: false });
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(read).not.toHaveBeenCalled();
  });

  it("a retry or task event invalidates a terminal run's results", async () => {
    vi.spyOn(api, "getSystemFeatures").mockResolvedValue({
      data_assertions_enabled: true,
    } as Awaited<ReturnType<typeof api.getSystemFeatures>>);
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
