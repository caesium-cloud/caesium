import { expect, it } from "vitest";
import type { JobRun } from "@/lib/api";
import { mergeLatestRun, mergeRetriedRun } from "../run-updates";

it("clears terminal metadata when the latest execution is retried", () => {
  const failed = run({ status: "failed", completed_at: "2026-10-02T10:01:00Z", error: "failed" });
  expect(mergeLatestRun(failed, run({ status: "running" }))).toMatchObject({ status: "running", completed_at: undefined, error: undefined });
});

const run = (overrides: Partial<JobRun> = {}): JobRun => ({
  id: "new", job_id: "job", status: "running", started_at: "2026-10-02T10:00:00Z",
  created_at: "2026-10-02T10:00:00Z", updated_at: "2026-10-02T10:00:10Z", ...overrides,
});

it("keeps the newer concurrent run when an older execution finishes", () => {
  const latest = run();
  expect(mergeLatestRun(latest, run({ id: "old", started_at: "2026-10-02T09:00:00Z", status: "succeeded" }))).toBe(latest);
  expect(mergeLatestRun(latest, run({ status: "succeeded" })).status).toBe("succeeded");
  expect(mergeLatestRun(latest, run({ id: "newest", started_at: "2026-10-02T11:00:00Z" })).id).toBe("newest");
});

it("falls back to creation time and does not guess when a different run has no valid time", () => {
  const latest = run();
  expect(mergeLatestRun(latest, run({ id: "unknown", started_at: "bad", created_at: "bad" }))).toBe(latest);
  expect(mergeLatestRun(latest, run({ id: "fallback", started_at: "", created_at: "2026-10-02T11:00:00Z" })).id).toBe("fallback");
});

it("reopens only an explicit current retry snapshot, clearing terminal metadata", () => {
  const failed = run({ status: "failed", error: "failed", completed_at: "2026-10-02T10:00:10Z" });
  const retried = mergeRetriedRun(failed, run({ updated_at: "2026-10-02T10:01:00Z" }));
  expect(retried.status).toBe("running");
  expect(retried.completed_at).toBeUndefined();
  expect(retried.error).toBeUndefined();
  expect(mergeRetriedRun(failed, run({ id: "another" }))).toBe(failed);
  expect(mergeRetriedRun(failed, run({ updated_at: "2026-10-02T10:00:00Z" }))).toBe(failed);
  expect(mergeRetriedRun(failed, undefined)).toBe(failed);
});
