import { statusMeta } from "@/lib/status";
import { cn } from "@/lib/utils";
import { useUTCTick } from "./utc-clock";

export interface RunSummary { status: string; duration?: number | null; startedAt?: string }

export function RunStripAxis({ windowSeconds = 900 }: { windowSeconds?: number }) {
  const hours = windowSeconds >= 3600;
  const label = (part: number) => `-${Math.round(windowSeconds * part / (hours ? 3600 : 60))}${hours ? "h" : "m"}`;
  return <div className="relative flex h-[30px] items-center justify-between text-[11px] text-text-3"><span>{label(1)}</span><span>{label(2 / 3)}</span><span>{label(1 / 3)}</span><span>now</span></div>;
}

/** A shared time window; missing timestamps stay explicitly in the historical fallback. */
export function RunStrip({ runs, windowSeconds = 900, className }: { runs: RunSummary[]; windowSeconds?: number; className?: string }) {
  const now = useUTCTick(tick => runs.some(run => run.startedAt && Number.isFinite(Date.parse(run.startedAt)) &&
    (statusMeta(run.status).label === "running" || tick - Date.parse(run.startedAt) <= windowSeconds * 1000))).getTime();
  const timed = runs.map((run) => ({ ...run, start: run.startedAt ? Date.parse(run.startedAt) : NaN }));
  const recent = timed.filter(run => Number.isFinite(run.start) && (now - run.start <= windowSeconds * 1000 || statusMeta(run.status).label === "running"));
  const fallback = recent.length === 0 && runs.length > 0;
  const displayed = fallback ? timed.slice(-10) : recent;
  const oldest = fallback && Number.isFinite(displayed[0]?.start) ? Math.max(0, now - displayed[0].start) : null;
  const age = oldest === null ? "time unknown" : oldest < 3_600_000 ? `${Math.floor(oldest / 60_000)}m ago` : `${Math.floor(oldest / 3_600_000)}h ago`;
  return <div role="img" aria-label={runs.length ? `${runs.length} recent runs${fallback ? ", outside the current window" : ""}: ${runs.map(run => `${statusMeta(run.status).label}, ${run.startedAt || "time unknown"}`).join("; ")}` : "no runs"} className={cn("relative h-14 min-w-0", className)} style={{ containerType: "inline-size" }}>
    {[0, 1 / 3, 2 / 3, 1].map(position => <span key={position} aria-hidden="true" className={cn("absolute bottom-0 top-0 border-l", position === 1 ? "border-cyan" : "border-border")} style={{ left: `${position * 100}%` }} />)}
    <span aria-hidden="true" className="cs-now-head absolute -right-[3px] top-0 h-[7px] w-[7px] rounded-full bg-running" />
    {displayed.map((run, index) => {
      const meta = statusMeta(run.status);
      const running = meta.label === "running" && !fallback;
      const left = fallback ? 60 + (index + .5) * 40 / displayed.length : Math.max(0, Math.min(100, (1 - (now - run.start) / (windowSeconds * 1000)) * 100));
      const elapsed = Number.isFinite(run.start) ? Math.max(0, (now - run.start) / 1000) : 0;
      const height = 6 + Math.min(Math.max(0, run.duration ?? elapsed), 10) * 2;
      return <span key={`${run.startedAt}-${index}`} aria-hidden="true" data-status={meta.label} className={cn("cs-time-mark absolute bottom-3 left-0 transition-transform duration-1000 ease-linear", running && "shadow-[0_0_10px_hsl(var(--running)/.5)]")}
        style={{ transform: `translateX(${left}cqw)`, height: running ? 3 : height, width: running ? `${Math.max(0.5, 100 - left)}%` : 3, background: running ? "linear-gradient(90deg,hsl(var(--running)/.2),hsl(var(--running)))" : meta.fg, opacity: fallback ? .6 : 1 }}>
        {running ? <span className="absolute -top-4 right-1 text-[11px] text-running">{elapsed.toFixed(0)}s</span> : null}
      </span>;
    })}
    {fallback ? <span className="absolute left-0 top-1 text-[11px] text-text-3">last {displayed.length}, oldest {age}</span> : null}
  </div>;
}
