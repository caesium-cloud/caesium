import { describe, expect, it } from "vitest";
import { mergeTerminalRunUpdate } from "../cache-utils";
import type { CallbackRun, JobRun } from "@/lib/api";

const callback: CallbackRun = {
  id: "callback-run-1",
  callback_id: "callback-1",
  status: "failed",
  error: "connection refused",
  started_at: "2026-09-14T00:00:00Z",
  completed_at: "2026-09-14T00:00:01Z",
};

const persistedRun: JobRun = {
  id: "run-1",
  job_id: "job-1",
  status: "running",
  started_at: "2026-09-14T00:00:00Z",
  created_at: "2026-09-14T00:00:00Z",
  updated_at: "2026-09-14T00:00:00Z",
  callbacks: [callback],
  tasks: [{
    id: "task-run-1",
    job_run_id: "run-1",
    task_id: "task-1",
    atom_id: "atom-1",
    engine: "docker",
    image: "alpine:3.23",
    command: ["true"],
    status: "running",
    created_at: "2026-09-14T00:00:00Z",
    updated_at: "2026-09-14T00:00:00Z",
  }],
};

describe("mergeTerminalRunUpdate", () => {
  it("keeps persisted callback deliveries when an earlier terminal SSE snapshot is empty", () => {
    // Synthetic event-order boundary: the persisted run is the real REST
    // shape after callback delivery. The terminal snapshot is created before
    // dispatch and therefore serializes callbacks as an explicit empty array.
    const terminalEventRun: JobRun = {
      ...persistedRun,
      status: "succeeded",
      completed_at: "2026-09-14T00:00:02Z",
      updated_at: "2026-09-14T00:00:02Z",
      callbacks: [],
      tasks: persistedRun.tasks?.map((task) => ({ ...task, status: "succeeded" })),
    };

    const merged = mergeTerminalRunUpdate(persistedRun, terminalEventRun);

    expect(merged.status).toBe("succeeded");
    expect(merged.tasks?.[0]?.status).toBe("succeeded");
    expect(merged.callbacks).toEqual([callback]);
  });

  it("ignores a terminal payload for a different run", () => {
    const foreignRun = { ...persistedRun, id: "other-run", status: "succeeded" };

    expect(mergeTerminalRunUpdate(persistedRun, foreignRun)).toBe(persistedRun);
  });

  it("keeps newer REST callback rows while retaining unseen event rows", () => {
    const terminalEventRun: JobRun = {
      ...persistedRun,
      status: "succeeded",
      callbacks: [
        { ...callback, status: "running", error: undefined },
        {
          ...callback,
          id: "callback-run-2",
          callback_id: "callback-2",
          status: "succeeded",
        },
      ],
    };

    const merged = mergeTerminalRunUpdate(persistedRun, terminalEventRun);

    expect(merged.callbacks).toEqual([
      callback,
      expect.objectContaining({ id: "callback-run-2", status: "succeeded" }),
    ]);
  });
});
