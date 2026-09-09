import { Link } from "@tanstack/react-router";
import { type DataViolation, type TaskRun } from "@/lib/api";
import { BaselineSparkline } from "./BaselineSparkline";
import { useDataAssertionsEnabled } from "./useDataAssertions";

export function AssertionEvidence({ violation }: { violation: DataViolation }) {
  return (
    <div className="space-y-1 text-xs" data-testid="assertion-evidence">
      <div className="font-mono font-semibold text-text-1">
        {violation.metric}.{violation.assertion}
        {violation.seeding ? " · Seeding (not enforced)" : " · Violated"}
      </div>
      <p className="text-text-2">{violation.message}</p>
      <p className="text-text-3">
        Observed: {violation.observed ?? "missing"} ·{" "}
        {violation.assertion === "deltaFromBaseline"
          ? "Allowed deviation"
          : violation.assertion === "maxLag"
            ? "Maximum lag (seconds)"
            : "Bound"}
        : {violation.bound ?? "—"}
      </p>
      {violation.assertion === "deltaFromBaseline" ? (
        <>
          <p className="text-text-3">
            Recorded baseline: median{" "}
            {violation.baseline_median ?? "unavailable"} ·{" "}
            {violation.baseline_samples ?? 0} samples · tolerance{" "}
            {violation.delta_from_baseline ?? "—"}
          </p>
          <BaselineSparkline violation={violation} />
        </>
      ) : null}
    </div>
  );
}

export function DataAssertionsPanel({ task }: { task: TaskRun }) {
  const enabled = useDataAssertionsEnabled();
  if (
    !task.schema_violations?.length &&
    (!enabled || !task.data_violations?.length)
  )
    return null;
  return (
    <section
      className="space-y-3 rounded-lg border border-warning/30 bg-warning/5 p-3"
      data-testid="data-assertions-panel"
    >
      <h3 className="text-xs font-semibold text-text-1">Recorded assertions</h3>
      {task.schema_violations?.map((violation, i) => (
        <p className="text-xs text-warning" key={`schema-${i}`}>
          {violation.key}: {violation.message}
        </p>
      ))}
      {enabled &&
        task.data_violations?.map((violation, i) => (
          <div key={i} className="space-y-2 border-t border-border/50 pt-2">
            <Link
              to="/datasets/holds"
              search={{
                namespace: violation.namespace ?? "",
                name: violation.dataset,
              }}
              className="font-mono text-xs text-cyan-glow hover:underline"
            >
              {violation.dataset}
            </Link>
            <AssertionEvidence violation={violation} />
          </div>
        ))}
    </section>
  );
}
