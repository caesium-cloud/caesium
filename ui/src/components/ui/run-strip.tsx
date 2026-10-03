import type { ReactNode } from "react";
import { statusMeta } from "@/lib/status";
import { cn } from "@/lib/utils";
import { useUTCTick } from "./utc-clock";

export interface RunSummary { status: string; duration?: number | null; startedAt?: string }
const GRID = [0, 1 / 3, 2 / 3, 1];

function windowLabel(seconds: number) {
  return seconds >= 3600 ? `${Number((seconds / 3600).toFixed(1))}h` : `${Number((seconds / 60).toFixed(1))}m`;
}

function markHeight(duration?: number | null) {
  return 6 + Math.min(Math.max(0, Number.isFinite(duration) ? duration! : 0), 10) * 2;
}

export function RunStripAxis({ windowSeconds = 900, className, label = "Run history" }: { windowSeconds?: number; className?: string; label?: string }) {
  return <div className={cn("bg-midnight px-3 pb-2 pt-2 text-[11px] text-text-3", className)}>
    <div className="mb-2 flex items-center justify-between gap-2"><span className="text-xs text-text-2">{label}</span><span>UTC</span></div>
    <div className="relative h-4">{GRID.map((position, index) => <span key={position} data-history-tick={position} className={cn("absolute whitespace-nowrap", position === 1 && "text-running")} style={{ left: `${position * 100}%`, transform: `translateX(${index === 0 ? 0 : index === GRID.length - 1 ? -100 : -50}%)` }}>{position === 1 ? "now" : `−${windowLabel(windowSeconds * (1 - position))}`}</span>)}</div>
    {/* One clock reference for the whole table, distinct from run-event marks. */}
    <div className="relative"><span aria-hidden="true" data-testid="history-now-line" className="cs-history-now-line pointer-events-none absolute left-full top-0 h-2 border-l border-running/55" /></div>
  </div>;
}

/** Recent events use time coordinates. Archived history is an explicitly ordinal list. */
export function RunStrip({ runs, windowSeconds = 900, className, historyLink }: { runs: RunSummary[]; windowSeconds?: number; className?: string; historyLink?: ReactNode }) {
  const now = useUTCTick(tick => runs.some(run => run.startedAt && Number.isFinite(Date.parse(run.startedAt)) &&
    (statusMeta(run.status).label === "running" || tick - Date.parse(run.startedAt) <= windowSeconds * 1000))).getTime();
  const timed = runs.map(run => ({ ...run, start: run.startedAt ? Date.parse(run.startedAt) : NaN }));
  const recent = timed.filter(run => Number.isFinite(run.start) && (now - run.start <= windowSeconds * 1000 || statusMeta(run.status).label === "running"));
  const fallback = recent.length === 0 && runs.length > 0;
  const displayed = fallback ? timed.slice(-10) : recent;
  const dated = displayed.map(run => run.start).filter(Number.isFinite);
  const oldest = dated.length ? Math.max(0, now - Math.min(...dated)) : null;
  const age = oldest === null ? "time unknown" : oldest < 3_600_000 ? `${Math.floor(oldest / 60_000)}m ago` : oldest < 86_400_000 ? `${Math.floor(oldest / 3_600_000)}h ago` : `${Math.floor(oldest / 86_400_000)}d ago`;
  const description = runs.length ? `${displayed.length} ${fallback ? "archived runs, outside the current window" : "runs in the current window"}: ${displayed.map(run => `${statusMeta(run.status).label}, ${run.startedAt || "time unknown"}, ${run.duration == null ? "duration unknown" : `${run.duration}s`}`).join("; ")}` : "no runs";
  if (fallback) return <div data-history="archived" className={cn("grid h-full min-h-16 grid-rows-[minmax(28px,1fr)_16px] border-x border-dashed border-border bg-midnight/40 px-3 py-2", className)}>
    <div role="img" aria-label={description} className="flex items-end gap-2">{displayed.map((run, index) => <span key={index} aria-hidden="true" data-status={statusMeta(run.status).label} className="w-[3px] rounded-t-sm" style={{ height: markHeight(run.duration), background: statusMeta(run.status).fg }} />)}</div>
    <div className="flex min-w-0 items-end justify-between gap-2 text-[11px]">
      <span className="truncate text-text-3" title={`Last ${displayed.length} runs, oldest ${age}. Ordered by start time, outside the selected window.`}>{dated.length ? `Outside ${windowLabel(windowSeconds)}` : "Time unknown"} · {displayed.length}</span>
      {historyLink}
    </div>
  </div>;
  return <div role="img" aria-label={description} data-history={runs.length ? "recent" : "empty"} className={cn("h-full min-h-16 bg-midnight px-3", className)}>
    <div className="relative h-full min-h-16 min-w-0" style={{ containerType: "inline-size" }}>
      {GRID.map(position => <span key={position} aria-hidden="true" data-history-grid={position} className={cn("pointer-events-none absolute inset-y-0 border-l", position === 1 ? "cs-history-now-line border-running/55" : "border-border")} style={{ left: `${position * 100}%` }} />)}
      {!runs.length ? <span className="absolute inset-y-0 left-2 flex items-center text-xs text-text-3"><span className="bg-midnight px-1">No runs yet</span></span> : <span className="absolute right-0 top-0 text-[11px] text-text-3 md:hidden">−{windowLabel(windowSeconds)} → now</span>}
      {displayed.map((run, index) => {
        const meta = statusMeta(run.status);
        const running = meta.label === "running";
        const left = Math.max(0, Math.min(100, (1 - (now - run.start) / (windowSeconds * 1000)) * 100));
        const elapsed = Math.max(0, (now - run.start) / 1000);
        return <span key={`${run.startedAt}-${index}`} aria-hidden="true" data-status={meta.label} className={cn("cs-time-mark absolute bottom-6 left-0 rounded-sm transition-transform duration-1000 ease-linear", running && "cs-live-bar")}
          style={{ opacity: 1, transform: `translateX(${left}cqw)`, marginLeft: left === 100 ? -3 : undefined, height: running ? 10 : markHeight(run.duration), width: running ? `${100 - left}%` : 3, minWidth: 3, background: running ? "linear-gradient(90deg,hsl(var(--running)/.25),hsl(var(--running)))" : meta.fg }}>
          {running ? <span className="absolute -top-5 right-0 text-[11px] text-running">{elapsed.toFixed(0)}s</span> : null}
        </span>;
      })}
    </div>
  </div>;
}
