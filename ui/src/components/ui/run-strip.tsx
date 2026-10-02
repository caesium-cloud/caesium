import { statusMeta } from "@/lib/status";
import { cn } from "@/lib/utils";
import { useUTCTick } from "./utc-clock";

export interface RunSummary { status: string; duration?: number | null; startedAt?: string }

export function RunStripAxis({ windowSeconds = 900 }: { windowSeconds?: number }) {
  const hours = windowSeconds >= 3600;
  const label = (part: number) => `−${Number((windowSeconds * part / (hours ? 3600 : 60)).toFixed(1))}${hours ? "h" : "m"}`;
  return <div className="py-2 text-xs text-text-3"><div className="mb-1 text-text-2">Run history</div><div className="flex justify-between"><span>{label(1)}</span><span>{label(.5)}</span><span>now</span></div></div>;
}

/** Recent events use time coordinates. Archived history is an explicitly ordinal list. */
export function RunStrip({ runs, windowSeconds = 900, className }: { runs: RunSummary[]; windowSeconds?: number; className?: string }) {
  const now = useUTCTick(tick => runs.some(run => run.startedAt && Number.isFinite(Date.parse(run.startedAt)) &&
    (statusMeta(run.status).label === "running" || tick - Date.parse(run.startedAt) <= windowSeconds * 1000))).getTime();
  const timed = runs.map((run) => ({ ...run, start: run.startedAt ? Date.parse(run.startedAt) : NaN }));
  const recent = timed.filter(run => Number.isFinite(run.start) && (now - run.start <= windowSeconds * 1000 || statusMeta(run.status).label === "running"));
  const fallback = recent.length === 0 && runs.length > 0;
  const displayed = fallback ? timed.slice(-10) : recent;
  const dated = displayed.map(run => run.start).filter(Number.isFinite);
  const oldest = dated.length ? Math.max(0, now - Math.min(...dated)) : null;
  const age = oldest === null ? "time unknown" : oldest < 3_600_000 ? `${Math.floor(oldest / 60_000)}m ago` : oldest < 86_400_000 ? `${Math.floor(oldest / 3_600_000)}h ago` : `${Math.floor(oldest / 86_400_000)}d ago`;
  const description = runs.length ? `${displayed.length} ${fallback ? "archived runs, outside the current window" : "runs in the current window"}: ${displayed.map(run => `${statusMeta(run.status).label}, ${run.startedAt || "time unknown"}`).join("; ")}` : "no runs";
  if (!runs.length) return <div role="img" aria-label="no runs" className={cn("flex h-14 items-center text-xs text-text-3", className)}>No runs yet</div>;
  if (fallback) return <div role="img" aria-label={description} data-history="archived" className={cn("flex min-h-14 flex-col justify-center gap-1.5 py-2", className)}>
    <div className="flex items-center gap-2"><span className="text-xs text-text-3">Older</span><span className="flex items-center gap-1 border-l border-border pl-2">{displayed.map((run, index) => <span key={index} aria-hidden="true" data-status={statusMeta(run.status).label} className="h-2 w-2 rounded-sm" style={{ background: statusMeta(run.status).fg }} />)}</span></div>
    <span className="text-xs text-text-3">last {displayed.length} · oldest {age}</span>
  </div>;
  return <div role="img" aria-label={description} data-history="recent" className={cn("relative h-14 min-w-0", className)} style={{ containerType: "inline-size" }}>
    {[0, .5, 1].map(position => <span key={position} aria-hidden="true" className="absolute bottom-0 top-0 border-l border-border" style={{ left: `${position * 100}%` }} />)}
    <span className="absolute right-0 top-0 text-[10px] text-text-3 md:hidden">−{Math.round(windowSeconds / 60)}m → now</span>
    {displayed.map((run, index) => {
      const meta = statusMeta(run.status);
      const running = meta.label === "running";
      const left = Math.max(0, Math.min(100, (1 - (now - run.start) / (windowSeconds * 1000)) * 100));
      const elapsed = Math.max(0, (now - run.start) / 1000);
      const height = 6 + Math.min(Math.max(0, run.duration ?? elapsed), 10) * 2;
      return <span key={`${run.startedAt}-${index}`} aria-hidden="true" data-status={meta.label} className={cn("cs-time-mark absolute bottom-3 left-0 transition-transform duration-1000 ease-linear", running && "shadow-[0_0_10px_hsl(var(--running)/.5)]")}
        style={{ opacity: 1, transform: `translateX(${left}cqw)`, height: running ? 3 : height, width: running ? `${Math.max(0.5, 100 - left)}%` : 3, background: running ? "linear-gradient(90deg,hsl(var(--running)/.2),hsl(var(--running)))" : meta.fg }}>
        {running ? <span className="absolute -top-4 right-1 text-xs text-running">{elapsed.toFixed(0)}s</span> : null}
      </span>;
    })}
  </div>;
}
