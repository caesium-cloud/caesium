import {
  type AssertionResult,
  type AssertionVerdict,
  type TaskAssertions,
  type TaskRun,
} from "@/lib/api";

const reasonText: Record<string, string> = {
  log_unreadable: "the task's log could not be read",
  marker_stream_truncated: "the metric stream was truncated",
  no_baseline: "no clean baseline yet",
  zero_median: "the baseline median is zero",
};

/** One sentence saying what the evaluator decided; never "passed" for a check that decided nothing. */
export function resultHeadline(result: AssertionResult): string {
  switch (result.outcome) {
    case "passed":
      return result.seeding
        ? `Passed during cold start (${result.baseline_samples ?? 0} baseline samples, not enforced)`
        : "Passed";
    case "violated":
      switch (result.disposition) {
        case "fail":
          return "Violated, failed the task";
        case "hold":
          return "Violated, held the dataset";
        case "seeding":
          return "Violated, Seeding (not enforced)";
        default:
          return "Violated, warning only";
      }
    case "unavailable":
      return `Not observed: ${reasonText[result.reason ?? ""] ?? "the metric stream was lost"}`;
    default:
      return `Not decided: ${reasonText[result.reason ?? ""] ?? "nothing to compare against"}`;
  }
}

export const verdictLabel: Record<AssertionVerdict, string> = {
  violated: "violated",
  undecided: "decided nothing on some checks",
  seeding: "passed during cold start",
  passed: "passed",
  not_evaluated: "served from cache, no assertion ran",
  not_recorded: "no results recorded",
};

const verdictOrder: AssertionVerdict[] = [
  "violated",
  "undecided",
  "seeding",
  "passed",
  "not_evaluated",
  "not_recorded",
];

/** "1 violated, 2 passed": a group is described by its mix, never by one instance. */
export function verdictSummary(counts: TaskAssertions["verdict_counts"]): string {
  return verdictOrder
    .filter((verdict) => (counts[verdict] ?? 0) > 0)
    .map((verdict) => `${counts[verdict]} ${verdictLabel[verdict]}`)
    .join(", ");
}

/** Whether a run's recorded results include anything to show for this task. */
export function hasRecordedAssertions(
  assertions: { tasks: TaskAssertions[] } | undefined,
  task: TaskRun,
) {
  return !!assertions?.tasks.some((t) => t.task_id === task.task_id);
}
