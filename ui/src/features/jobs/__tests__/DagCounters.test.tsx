import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { DagCounters } from "../DagCounters";
import type { TaskRun } from "@/lib/api";

function task(status: string): TaskRun {
  return {
    id: `${status}-task`,
    job_run_id: "run-1",
    task_id: `${status}-task`,
    atom_id: "atom-1",
    engine: "docker",
    image: "alpine:3.23",
    command: ["true"],
    status,
    created_at: "2026-07-07T00:00:00Z",
    updated_at: "2026-07-07T00:00:00Z",
  };
}

describe("DagCounters", () => {
  it("surfaces failed and blocked buckets explicitly", () => {
    render(<DagCounters tasks={[task("failed"), task("skipped"), task("blocked")]} />);

    const counters = screen.getByTestId("dag-counters");
    expect(counters).toHaveTextContent("0 done");
    expect(counters).toHaveTextContent("1 failed");
    expect(counters).toHaveTextContent("2 blocked");
    expect(counters).not.toHaveTextContent("queued");
  });

  it("keeps running and cached counts visible", () => {
    render(<DagCounters tasks={[task("succeeded"), task("running"), task("cached"), task("pending")]} />);

    const counters = screen.getByTestId("dag-counters");
    expect(counters).toHaveTextContent("1 done");
    expect(counters).toHaveTextContent("1 running");
    expect(counters).toHaveTextContent("1 cached");
    expect(counters).toHaveTextContent("1 waiting");
  });
});
