import { Link } from "@tanstack/react-router";
import { ChevronDown, ArrowRight } from "lucide-react";
import { Button } from "@/components/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuLabel, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { StatusBadge } from "@/components/ui/status-badge";
import { Duration } from "@/components/duration";
import type { JobRun } from "@/lib/api";
import { formatUTCTimestamp, shortId } from "@/lib/utils";

export function RunPicker({ jobId, currentRun, runs = [], isLoading = false, label = "Switch run" }: {
  jobId: string;
  currentRun: JobRun;
  runs?: JobRun[];
  isLoading?: boolean;
  label?: string;
}) {
  // Keep the inspected run pinned even when it is older than the fetched page.
  const recent = runs.filter(run => run.id !== currentRun.id)
    .sort((a, b) => (Date.parse(b.started_at) || 0) - (Date.parse(a.started_at) || 0)).slice(0, 8);
  if (!isLoading && recent.length === 0) return null;
  return <DropdownMenu>
    <DropdownMenuTrigger asChild>
      <Button size="sm" variant="outline" disabled={isLoading} data-testid="run-picker-trigger">{isLoading ? "Loading runs…" : label}<ChevronDown aria-hidden="true" /></Button>
    </DropdownMenuTrigger>
    <DropdownMenuContent align="end" collisionPadding={16} className="w-[23rem] max-w-[calc(100vw-2rem)]">
      <DropdownMenuLabel className="text-xs text-text-3">Selected run</DropdownMenuLabel>
      <RunOption jobId={jobId} run={currentRun} current />
      <DropdownMenuSeparator />
      <DropdownMenuLabel className="text-xs text-text-3">Other recent runs</DropdownMenuLabel>
      <div className="max-h-[min(22rem,45vh)] overflow-y-auto">
        {recent.map(run => <RunOption key={run.id} jobId={jobId} run={run} />)}
      </div>
      <DropdownMenuSeparator />
      <DropdownMenuItem asChild><Link to="/jobs/$jobId/runs" params={{ jobId }} className="justify-between">All run history<ArrowRight className="h-3.5 w-3.5" aria-hidden="true" /></Link></DropdownMenuItem>
    </DropdownMenuContent>
  </DropdownMenu>;
}

function RunOption({ jobId, run, current = false }: { jobId: string; run: JobRun; current?: boolean }) {
  const timestamp = formatUTCTimestamp(run.started_at, "Unknown time");
  return <DropdownMenuItem asChild className="block p-2.5">
    <Link to="/jobs/$jobId/runs/$runId" params={{ jobId, runId: run.id }} aria-current={current ? "page" : undefined} aria-label={`Open run ${run.id}, ${timestamp}, ${run.status}`} title={run.id}>
      <div className="flex flex-wrap items-center justify-between gap-2"><span className="text-xs text-text-1">{timestamp}</span><StatusBadge status={run.status} size="sm" /></div>
      <div className="mt-1 flex items-center justify-between gap-3 text-xs text-text-3"><span>{shortId(run.id)}{current ? " · selected" : ""}</span><Duration start={run.started_at} end={run.completed_at} /></div>
    </Link>
  </DropdownMenuItem>;
}
