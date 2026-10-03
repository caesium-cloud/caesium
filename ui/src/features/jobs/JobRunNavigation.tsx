import type { ReactNode } from "react";
import { Link } from "@tanstack/react-router";
import { cn } from "@/lib/utils";

/** The job remains the parent while an individual execution is being inspected. */
export function JobRunNavigation({ jobId, active, children, trailing }: {
  jobId: string;
  active: string;
  children?: ReactNode;
  trailing?: ReactNode;
}) {
  const linkClass = (selected: boolean) => cn(
    "inline-flex h-9 shrink-0 items-center border-b-2 px-2.5 text-xs transition-colors hover:bg-obsidian hover:text-text-1",
    selected ? "border-cyan font-bold text-text-1" : "border-transparent text-text-3",
  );
  return <nav aria-label="Job navigation" data-testid="job-detail-view-tabs" className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 border-b border-border">
    <div className="flex min-w-0 flex-wrap items-center gap-1">
      <Link to="/jobs/$jobId" params={{ jobId }} aria-current={active === "overview" ? "page" : undefined} className={linkClass(active === "overview")}>Job overview</Link>
      <Link to="/jobs/$jobId/runs" params={{ jobId }} resetScroll={false} aria-current={active === "runs" ? "page" : undefined} className={linkClass(active === "runs")} data-testid={active === "run" ? "all-runs-link" : undefined} aria-keyshortcuts={active === "run" ? "A" : undefined} title={active === "run" ? "Run history (A)" : undefined}>Run history</Link>
      {active === "run" ? <span aria-current="page" className={linkClass(true)}>Execution</span> : children}
    </div>
    {trailing ? <div className="pb-1">{trailing}</div> : null}
  </nav>;
}
