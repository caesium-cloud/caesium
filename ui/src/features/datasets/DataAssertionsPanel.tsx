import { Link } from "@tanstack/react-router";
import {
  type AssertionInstance,
  type AssertionResult,
  type DataViolation,
  type TaskAssertions,
  type TaskRun,
} from "@/lib/api";
import {
  hasRecordedAssertions,
  resultHeadline,
  verdictLabel,
  verdictSummary,
} from "./assertion-utils";
import { BaselineSparkline } from "./BaselineSparkline";
import { useDataAssertionsEnabled, useRunAssertions } from "./useDataAssertions";

const boundLabel = (assertion: string) =>
  assertion === "deltaFromBaseline"
    ? "Allowed deviation"
    : assertion === "maxLag"
      ? "Maximum lag (seconds)"
      : "Bound";

export function AssertionEvidence({ violation }: { violation: DataViolation }) {
  return (
    <div className="space-y-1 text-xs" data-testid="assertion-evidence">
      <div className="font-bold text-text-1">
        {violation.metric}.{violation.assertion}
        {violation.seeding ? ", Seeding (not enforced)" : ", Violated"}
      </div>
      <p className="text-text-2">{violation.message}</p>
      <p className="text-text-3">
        Observed: {violation.observed ?? "missing"},{" "}
        {boundLabel(violation.assertion)}: {violation.bound ?? "—"}
      </p>
      {violation.assertion === "deltaFromBaseline" ? (
        <>
          <p className="text-text-3">
            Recorded baseline: median{" "}
            {violation.baseline_median ?? "unavailable"},{" "}
            {violation.baseline_samples ?? 0} samples, tolerance{" "}
            {violation.delta_from_baseline ?? "—"}
          </p>
          <BaselineSparkline violation={violation} />
        </>
      ) : null}
    </div>
  );
}

export function AssertionResultRow({ result }: { result: AssertionResult }) {
  const decided = result.outcome === "passed" || result.outcome === "violated";
  return (
    <li
      className="space-y-1 text-xs"
      data-testid="assertion-result"
      data-outcome={result.outcome}
      data-disposition={result.disposition ?? ""}
      data-seeding={result.seeding ? "true" : "false"}
    >
      <div
        className={`font-bold ${result.outcome === "violated" ? "text-warning" : "text-text-1"}`}
      >
        {result.metric}.{result.assertion}: {resultHeadline(result)}
      </div>
      {result.outcome === "violated" ? (
        <Link
          to="/datasets/holds"
          search={{ namespace: result.namespace ?? "", name: result.dataset }}
          className="text-cyan-glow hover:underline"
        >
          {result.dataset}
        </Link>
      ) : null}
      {result.message ? <p className="text-text-2">{result.message}</p> : null}
      {decided || result.assertion === "missing" ? (
        <p className="text-text-3">
          Observed: {result.observed ?? "missing"},{" "}
          {boundLabel(result.assertion)}:{" "}
          {result.bound_withheld ? "withheld" : (result.bound ?? "—")}
        </p>
      ) : null}
      {result.assertion === "deltaFromBaseline" && decided ? (
        <>
          <p className="text-text-3">
            Recorded baseline: median{" "}
            {result.baseline_median ?? "unavailable"},{" "}
            {result.baseline_samples ?? 0} samples, tolerance{" "}
            {result.delta_from_baseline ?? "—"}
          </p>
          <BaselineSparkline
            violation={result}
            when={result.outcome === "violated" ? "breach" : "check"}
          />
        </>
      ) : null}
    </li>
  );
}

function InstanceResults({ instance }: { instance: AssertionInstance }) {
  if (instance.verdict === "not_evaluated") {
    return (
      <p className="text-xs text-text-3" data-testid="assertion-not-evaluated">
        Served from cache: no assertion ran for this execution.
      </p>
    );
  }
  if (!instance.results?.length) {
    return (
      <div className="space-y-2" data-testid="assertion-legacy">
        <p className="text-xs text-text-3">
          {instance.verdict === "violated"
            ? "Violations were recorded before per-assertion results existed; which checks held was not recorded."
            : "No per-assertion results were recorded for this partition."}
        </p>
        {instance.violations?.length ? (
          <Violations violations={instance.violations} />
        ) : null}
      </div>
    );
  }
  return (
    <ul className="space-y-2">
      {instance.results.map((result, i) => (
        <AssertionResultRow key={i} result={result} />
      ))}
    </ul>
  );
}

function RecordedResults({
  recorded,
  fallback,
}: {
  recorded: TaskAssertions;
  fallback?: DataViolation[];
}) {
  const single = !recorded.fanned && recorded.instances.length === 1;
  if (single) {
    const only = recorded.instances[0];
    // A row recorded before per-check results existed keeps showing its own
    // violations (or the run payload's); it never claims anything passed.
    if (!only.results?.length && !only.violations?.length && fallback?.length)
      return <Violations violations={fallback} />;
    return <InstanceResults instance={only} />;
  }
  return (
    <div className="space-y-2" data-testid="assertion-group">
      <p className="text-xs text-text-2" data-testid="assertion-group-summary">
        {recorded.instance_count} partitions:{" "}
        {verdictSummary(recorded.verdict_counts)}
      </p>
      {recorded.instances.map((instance) => (
        <details
          key={instance.task_run_id}
          className="rounded border border-border/50 p-2"
          data-testid="assertion-instance"
          data-verdict={instance.verdict}
          open={instance.verdict === "violated"}
        >
          <summary className="cursor-pointer text-xs text-text-1">
            {instance.partition_value || `#${instance.partition_index ?? 0}`}:{" "}
            {verdictLabel[instance.verdict]}
          </summary>
          <div className="mt-2">
            <InstanceResults instance={instance} />
          </div>
        </details>
      ))}
      {recorded.instances_truncated ? (
        <p className="text-xs text-text-3">
          Showing the {recorded.instances.length} most notable of{" "}
          {recorded.instance_count} partitions; the counts above cover all of
          them.
        </p>
      ) : null}
    </div>
  );
}

function Violations({ violations }: { violations: DataViolation[] }) {
  return (
    <>
      {violations.map((violation, i) => (
        <div key={i} className="space-y-2 border-t border-border/50 pt-2">
          <Link
            to="/datasets/holds"
            search={{
              namespace: violation.namespace ?? "",
              name: violation.dataset,
            }}
            className="text-xs text-cyan-glow hover:underline"
          >
            {violation.dataset}
          </Link>
          <AssertionEvidence violation={violation} />
        </div>
      ))}
    </>
  );
}

function isFanned(task: TaskRun) {
  return (task.partition_count ?? 0) >= 1 || !!task.partition_value;
}

export function DataAssertionsPanel({
  task,
  jobId,
  runId,
  live = false,
}: {
  task: TaskRun;
  jobId?: string;
  runId?: string;
  live?: boolean;
}) {
  const enabled = useDataAssertionsEnabled();
  const assertions = useRunAssertions(jobId, runId, live);
  const recorded = assertions.data?.tasks.find(
    (t) => t.task_id === task.task_id,
  );
  // A collapsed fan-out group carries one arbitrary instance's violations, so
  // the group is described only by the per-instance read.
  const legacyViolations = isFanned(task) ? undefined : task.data_violations;
  if (
    !task.schema_violations?.length &&
    (!enabled || (!legacyViolations?.length && !recorded))
  )
    return null;
  return (
    <section
      className="space-y-3 rounded-md border border-warning/30 bg-warning/5 p-3"
      data-testid="data-assertions-panel"
    >
      <h3 className="text-xs font-bold text-text-1">Recorded assertions</h3>
      {task.schema_violations?.map((violation, i) => (
        <p className="text-xs text-warning" key={`schema-${i}`}>
          {violation.key}: {violation.message}
        </p>
      ))}
      {enabled && recorded ? (
        <RecordedResults recorded={recorded} fallback={legacyViolations} />
      ) : null}
      {enabled && !recorded && legacyViolations?.length ? (
        <Violations violations={legacyViolations} />
      ) : null}
    </section>
  );
}

/** One panel per task of a run that has assertion evidence to show. */
export function RunAssertionPanels({
  jobId,
  runId,
  tasks,
  live = false,
}: {
  jobId: string;
  runId: string;
  tasks: TaskRun[];
  live?: boolean;
}) {
  const assertions = useRunAssertions(jobId, runId, live);
  return (
    <>
      {tasks
        .filter(
          (task) =>
            task.data_violations?.length ||
            task.schema_violations?.length ||
            hasRecordedAssertions(assertions.data, task),
        )
        .map((task) => (
          <DataAssertionsPanel
            key={task.id}
            task={task}
            jobId={jobId}
            runId={runId}
            live={live}
          />
        ))}
    </>
  );
}
