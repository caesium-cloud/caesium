import type { TaskRun } from "@/lib/api";
import { statusMeta } from "@/lib/status";
import { isTerminalRunStatus } from "./cache-utils";

/** Preserve the recorded outcome; a terminal parent cannot make a stale task live. */
export function taskPresentation(task: Pick<TaskRun, "status" | "started_at" | "completed_at" | "updated_at">, runStatus?: string, runCompletedAt?: string) {
  const status = statusMeta(task.status === "completed" ? "succeeded" : task.status).label;
  const incomplete = isTerminalRunStatus(runStatus) && ["running", "queued"].includes(status);
  const uncertain = incomplete && !!task.started_at;
  const label = incomplete ? uncertain ? "Outcome unknown" : "Did not start" : status;
  const observed = Date.parse(task.updated_at ?? "");
  const cutoff = Date.parse(runCompletedAt ?? "");
  const start = Date.parse(task.started_at ?? "");
  const observedEnd = Number.isFinite(observed) && Number.isFinite(start)
    ? new Date(Math.max(start, Math.min(observed, Number.isFinite(cutoff) ? cutoff : observed))).toISOString()
    : task.started_at;
  return {
    status: incomplete ? "unknown" : status,
    label,
    incomplete,
    uncertain,
    end: task.completed_at || (incomplete ? observedEnd : undefined),
    note: incomplete ? `Run ${runStatus}. Last reported task state: ${task.status}. ${uncertain ? "Final task outcome was not recorded; duration shows the last observation." : "No task start was recorded."}` : undefined,
  };
}
