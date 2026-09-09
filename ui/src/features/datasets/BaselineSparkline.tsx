import { useQuery } from "@tanstack/react-query";
import { api, type DataViolation } from "@/lib/api";

const number = (value: number) => Number(value.toPrecision(5)).toLocaleString();

export function BaselineSparkline({ violation }: { violation: DataViolation }) {
  const query = useQuery({
    queryKey: [
      "datasets",
      "metrics",
      violation.namespace ?? "",
      violation.dataset,
      violation.metric,
    ],
    queryFn: () =>
      api.getDatasetMetrics(
        violation.namespace ?? "",
        violation.dataset,
        violation.metric,
      ),
    staleTime: 30_000,
  });
  if (query.isPending)
    return <p className="text-xs text-text-3">Loading clean baseline…</p>;
  if (query.error)
    return (
      <p role="status" className="text-xs text-warning">
        Baseline unavailable: {query.error.message}
      </p>
    );
  const baseline = query.data.baseline;
  const values = baseline?.values?.filter(Number.isFinite) ?? [];
  if (!baseline || values.length === 0)
    return (
      <p className="text-xs text-text-3">No clean baseline samples yet.</p>
    );
  const current = violation.observed;
  const points = current == null ? values : [...values, current];
  const recordedLow =
    violation.baseline_median != null && violation.bound != null
      ? violation.baseline_median - violation.bound
      : undefined;
  const recordedHigh =
    violation.baseline_median != null && violation.bound != null
      ? violation.baseline_median + violation.bound
      : undefined;
  const min = Math.min(...points, baseline.p10, recordedLow ?? baseline.p10);
  const max = Math.max(...points, baseline.p90, recordedHigh ?? baseline.p90);
  const span = max - min || Math.max(1, Math.abs(max) * 0.1);
  const x = (index: number) =>
    12 + (index / Math.max(1, points.length - 1)) * 296;
  const y = (value: number) => 82 - ((value - min) / span) * 64;
  const label = `Current clean baseline for ${violation.metric}: ${values.length} samples, median ${number(baseline.median)}, p10 ${number(baseline.p10)}, p90 ${number(baseline.p90)}. Recorded observation ${current == null ? "missing" : number(current)}.`;
  return (
    <figure
      className="mt-2 rounded border border-border/50 bg-obsidian/30 p-2"
      data-testid="baseline-sparkline"
    >
      <svg
        viewBox="0 0 320 96"
        role="img"
        aria-label={label}
        className="h-28 w-full"
      >
        {recordedLow != null && recordedHigh != null ? (
          <rect
            data-testid="baseline-recorded-bound"
            x="12"
            y={y(recordedHigh)}
            width="296"
            height={Math.max(2, y(recordedLow) - y(recordedHigh))}
            fill="none"
            stroke="hsl(var(--warning))"
            strokeDasharray="2 3"
          >
            <title>
              Allowed range at breach: {recordedLow}–{recordedHigh}
            </title>
          </rect>
        ) : null}
        <rect
          data-testid="baseline-band"
          x="12"
          y={y(baseline.p90)}
          width="296"
          height={Math.max(2, y(baseline.p10) - y(baseline.p90))}
          fill="hsl(var(--cyan) / 0.16)"
        />
        <line
          x1="12"
          x2="308"
          y1={y(baseline.median)}
          y2={y(baseline.median)}
          stroke="hsl(var(--cyan))"
          strokeDasharray="4 3"
        />
        <polyline
          points={values.map((value, i) => `${x(i)},${y(value)}`).join(" ")}
          fill="none"
          stroke="hsl(var(--text-2))"
          strokeWidth="2"
        />
        {values.map((value, i) => (
          <circle
            key={i}
            cx={x(i)}
            cy={y(value)}
            r="2.5"
            fill="hsl(var(--cyan))"
          />
        ))}
        {current != null ? (
          <circle
            data-testid="baseline-observation"
            cx={x(points.length - 1)}
            cy={y(current)}
            r="5"
            fill="hsl(var(--warning))"
            stroke="hsl(var(--text-1))"
          >
            <title>Recorded observation: {number(current)}</title>
          </circle>
        ) : null}
      </svg>
      <figcaption className="text-[11px] text-text-3">
        Current clean baseline · last {values.length}/{query.data.window}{" "}
        samples · p10–p90 band · gold point: recorded observation; dashed gold
        box: allowed range at breach.
        {query.data.seeding
          ? ` Seeding (${values.length}/${query.data.min_samples} samples).`
          : ""}
        <span className="mt-1 block">
          This live history may differ from the baseline at this breach; the
          recorded verdict below preserves its median and sample count.
        </span>
      </figcaption>
    </figure>
  );
}
