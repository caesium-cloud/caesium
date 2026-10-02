import { describe, expect, it } from "vitest";
import { taskPresentation } from "../task-presentation";

const task = { status: "running", started_at: "2026-10-02T00:00:00Z", updated_at: "2026-10-02T00:00:30Z" };
describe("terminal task presentation", () => {
  it.each(["failed", "succeeded", "cancelled", "skipped"])("does not infer a task outcome from a %s parent", status => {
    expect(taskPresentation(task, status, "2026-10-02T00:00:20Z")).toMatchObject({ status: "unknown", label: "Outcome unknown", end: "2026-10-02T00:00:20.000Z", uncertain: true });
    expect(task.status).toBe("running");
  });
  it("keeps actual terminal task outcomes and completion times", () => {
    expect(taskPresentation({ ...task, status: "succeeded", completed_at: task.updated_at }, "failed")).toMatchObject({ status: "succeeded", end: task.updated_at, incomplete: false });
  });
  it("distinguishes never-started tasks and missing observation timestamps", () => {
    expect(taskPresentation({ status: "pending", updated_at: "" }, "failed")).toMatchObject({ status: "unknown", label: "Did not start", end: undefined });
    expect(taskPresentation({ ...task, updated_at: "invalid" }, "failed").end).toBe(task.started_at);
  });
  it("retains live state until the run has ended", () => {
    expect(taskPresentation(task, "running")).toMatchObject({ status: "running", end: undefined, incomplete: false });
  });
});
