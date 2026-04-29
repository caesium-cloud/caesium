import { Link, useNavigate } from "@tanstack/react-router";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Pause, Play, Search } from "lucide-react";
import { toast } from "sonner";
import { Duration } from "@/components/duration";
import { RelativeTime } from "@/components/relative-time";
import { Button } from "@/components/ui/button";
import { EmptyState } from "@/components/ui/empty-state";
import { Sparkline } from "@/components/ui/sparkline";
import { StatusBadge } from "@/components/ui/status-badge";
import { api, type Job, type JobRun } from "@/lib/api";
import { cn, shortId } from "@/lib/utils";
import { useJobsView, type ActivityEntry, type JobCounts, type StatusFilter, type SortKey } from "./useJobsView";

export function JobsPage() {
  return <JobsPageInner />;
}

function JobsPageInner() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { rows, counts, search, setSearch, statusFilter, setStatusFilter, sort, setSort, isLoading, error, activity } =
    useJobsView();

  const triggerMutation = useMutation({
    mutationFn: ({ jobId }: { jobId: string }) => api.triggerJob(jobId),
    onSuccess: (run) => {
      queryClient.invalidateQueries({ queryKey: ["jobs"] });
      toast.success("Job triggered");
      if (run?.job_id && run?.id) {
        navigate({ to: "/jobs/$jobId/runs/$runId", params: { jobId: run.job_id, runId: run.id } });
      }
    },
    onError: (err: Error) => toast.error(`Failed to trigger: ${err.message}`),
  });

  const pauseMutation = useMutation({
    mutationFn: ({ jobId, paused }: { jobId: string; paused: boolean; hasActiveRun: boolean }) =>
      paused ? api.pauseJob(jobId) : api.unpauseJob(jobId),
    onSuccess: (job, vars) => {
      queryClient.setQueryData(["jobs"], (old: Job[] | undefined) =>
        old?.map((e) => (e.id === job.id ? { ...e, paused: job.paused } : e)),
      );
      queryClient.setQueryData(["job", job.id], (old: Job | undefined) =>
        old ? { ...old, paused: job.paused } : job,
      );
      if (job.paused) {
        toast.success(
          vars.hasActiveRun
            ? "Job paused — active run will finish, new runs blocked."
            : "Job paused — new runs blocked.",
        );
      } else {
        toast.success("Job unpaused");
      }
    },
    onError: (err: Error) => toast.error(`Failed to update: ${err.message}`),
  });

  if (isLoading) {
    return (
      <div className="space-y-4">
        <PageHeader />
        <div className="h-10 rounded-md bg-muted/30 animate-pulse" />
        <div className="rounded-md border border-border/50 bg-card divide-y divide-border/50">
          {Array.from({ length: 5 }).map((_, i) => (
            <div key={i} className="h-14 animate-pulse bg-muted/20" />
          ))}
        </div>
      </div>
    );
  }

  if (error) {
    return (
      <div className="space-y-4">
        <PageHeader />
        <div className="rounded-md border border-danger/30 bg-danger/5 p-6 text-sm text-danger">
          Failed to load jobs: {error.message}
        </div>
      </div>
    );
  }

  return (
    <div className="space-y-5">
      <PageHeader />

      <FilterBar
        counts={counts}
        statusFilter={statusFilter}
        onStatusFilter={setStatusFilter}
        search={search}
        onSearch={setSearch}
        sort={sort}
        onSort={setSort}
      />

      {/* Job grid */}
      <div className="rounded-md border border-border/50 bg-card overflow-hidden">
        {/* Column headers */}
        <div
          className="grid items-center px-4 py-2 border-b border-border/50 bg-obsidian/30"
          style={{ gridTemplateColumns: "1fr 130px 120px 90px 96px 72px" }}
        >
          <span className="text-[10px] font-semibold uppercase tracking-[0.1em] text-text-3">Pipeline</span>
          <span className="text-[10px] font-semibold uppercase tracking-[0.1em] text-text-3">Status</span>
          <span className="text-[10px] font-semibold uppercase tracking-[0.1em] text-text-3">Last run</span>
          <span className="text-[10px] font-semibold uppercase tracking-[0.1em] text-text-3">Duration</span>
          <span className="text-[10px] font-semibold uppercase tracking-[0.1em] text-text-3">History</span>
          <span className="sr-only">Actions</span>
        </div>

        {rows.length === 0 ? (
          <EmptyState
            title={search || statusFilter !== "all" ? "No pipelines match" : "No pipelines yet"}
            subtitle={
              search || statusFilter !== "all"
                ? "Try clearing your filter or search term."
                : "Apply a job definition to get started."
            }
            className="py-20"
          />
        ) : null}

        {rows.map((job) => {
          const lr = job.latest_run;
          const isRunning = lr?.status === "running";
          const isPaused = job.paused;

          return (
            <div
              key={job.id}
              className={cn(
                "group grid items-center px-4 py-0 border-b border-border/40 last:border-0 transition-colors",
                "hover:bg-obsidian/60",
                isRunning && [
                  "border-l-2 border-l-cyan-glow/60",
                  "bg-running/5",
                ],
                isPaused && !isRunning && "bg-warning/5",
              )}
              style={{ gridTemplateColumns: "1fr 130px 120px 90px 96px 72px", minHeight: "52px" }}
            >
              {/* Alias column */}
              <div className="min-w-0 py-3 pr-3">
                <div className="flex items-center gap-2">
                  <Link
                    to="/jobs/$jobId"
                    params={{ jobId: job.id }}
                    className="font-medium text-text-1 hover:text-cyan-glow truncate transition-colors"
                  >
                    {job.alias}
                  </Link>
                  {isPaused && (
                    <StatusBadge status="paused" variant="soft" size="sm" />
                  )}
                </div>
                <div className="text-[10px] font-mono text-text-4 mt-0.5">{shortId(job.id)}</div>
              </div>

              {/* Status column */}
              <div className="py-3">
                {lr ? (
                  <StatusBadge status={lr.status} size="sm" />
                ) : (
                  <span className="text-[11px] text-text-4">—</span>
                )}
              </div>

              {/* Last run column */}
              <div className="py-3 text-sm text-text-2 tabular-nums">
                {lr ? <RelativeTime date={lr.started_at} /> : <span className="text-text-4">—</span>}
              </div>

              {/* Duration column */}
              <div className="py-3 text-sm font-mono text-text-2 tabular-nums">
                {lr ? (
                  <Duration start={lr.started_at} end={lr.completed_at} />
                ) : (
                  <span className="text-text-4">—</span>
                )}
              </div>

              {/* Sparkline column */}
              <div className="py-3">
                <Sparkline runs={job.lastRuns} width={84} height={20} />
              </div>

              {/* Actions column */}
              <div className="py-2 flex items-center justify-end gap-0.5 opacity-0 group-hover:opacity-100 transition-opacity">
                <Button
                  variant="ghost"
                  size="icon"
                  className="h-7 w-7"
                  onClick={() => triggerMutation.mutate({ jobId: job.id })}
                  disabled={triggerMutation.isPending || isPaused}
                  title={isPaused ? "Unpause before triggering" : "Trigger run"}
                >
                  <Play className="h-3.5 w-3.5" />
                </Button>
                <Button
                  variant="ghost"
                  size="icon"
                  className="h-7 w-7"
                  onClick={() =>
                    pauseMutation.mutate({ jobId: job.id, paused: !isPaused, hasActiveRun: isRunning })
                  }
                  disabled={pauseMutation.isPending}
                  title={isPaused ? "Unpause" : "Pause future runs"}
                >
                  <Pause className="h-3.5 w-3.5" />
                </Button>
              </div>
            </div>
          );
        })}
      </div>

      {/* Activity feed */}
      {activity.length > 0 && <ActivityFeed entries={activity} />}
    </div>
  );
}

/* ── Sub-components ── */

function PageHeader() {
  return (
    <div>
      <div className="text-[10px] font-semibold uppercase tracking-[0.18em] text-text-3 mb-1">
        Pipelines
      </div>
      <h1 className="text-xl font-semibold text-text-1 tracking-tight">Jobs</h1>
    </div>
  );
}

interface FilterBarProps {
  counts: JobCounts;
  statusFilter: StatusFilter;
  onStatusFilter: (f: StatusFilter) => void;
  search: string;
  onSearch: (s: string) => void;
  sort: SortKey;
  onSort: (s: SortKey) => void;
}

const FILTER_CHIPS: { key: StatusFilter; label: string }[] = [
  { key: "all", label: "All" },
  { key: "running", label: "Running" },
  { key: "succeeded", label: "Succeeded" },
  { key: "failed", label: "Failed" },
  { key: "paused", label: "Paused" },
];

function FilterBar({ counts, statusFilter, onStatusFilter, search, onSearch, sort, onSort }: FilterBarProps) {
  return (
    <div className="flex flex-wrap items-center gap-3">
      {/* Status chips */}
      <div className="flex items-center gap-1 rounded-md border border-border/50 bg-card p-1">
        {FILTER_CHIPS.map(({ key, label }) => {
          const count = counts[key];
          const isActive = statusFilter === key;
          return (
            <button
              key={key}
              type="button"
              onClick={() => onStatusFilter(key)}
              className={cn(
                "flex items-center gap-1.5 rounded px-2.5 py-1 text-[11px] font-medium transition-colors",
                isActive
                  ? "bg-obsidian text-text-1 shadow-sm"
                  : "text-text-3 hover:text-text-2 hover:bg-obsidian/50",
              )}
            >
              {label}
              {count > 0 && (
                <span
                  className={cn(
                    "inline-flex items-center justify-center rounded-full px-1.5 py-px text-[9px] font-semibold tabular-nums min-w-[16px]",
                    isActive ? "bg-graphite text-text-2" : "bg-obsidian/60 text-text-4",
                    key === "running" && count > 0 && "text-cyan-glow/80",
                    key === "failed" && count > 0 && "text-danger/80",
                  )}
                >
                  {count}
                </span>
              )}
            </button>
          );
        })}
      </div>

      {/* Search */}
      <div className="relative flex-1 min-w-[160px] max-w-xs">
        <Search className="absolute left-2.5 top-1/2 -translate-y-1/2 h-3.5 w-3.5 text-text-4 pointer-events-none" />
        <input
          type="search"
          value={search}
          onChange={(e) => onSearch(e.target.value)}
          placeholder="Filter pipelines…"
          className={cn(
            "w-full pl-8 pr-3 py-1.5 text-[12px] rounded-md border border-border/50",
            "bg-card text-text-1 placeholder:text-text-4",
            "focus:outline-none focus:ring-1 focus:ring-cyan/40 focus:border-cyan/40",
          )}
        />
      </div>

      {/* Sort */}
      <div className="ml-auto flex items-center gap-1.5 text-[11px] text-text-3">
        <span>Sort</span>
        <select
          value={sort}
          onChange={(e) => onSort(e.target.value as SortKey)}
          className={cn(
            "rounded border border-border/50 bg-card text-text-2 text-[11px] py-1 px-2",
            "focus:outline-none focus:ring-1 focus:ring-cyan/40",
          )}
        >
          <option value="alias">Name</option>
          <option value="status">Status</option>
          <option value="last_run">Last run</option>
        </select>
      </div>
    </div>
  );
}

const ACTIVITY_LABELS: Record<string, string> = {
  run_started: "Run started",
  run_completed: "Run completed",
  run_terminal: "Run completed",
  run_failed: "Run failed",
};

function activityDotClass(type: string) {
  if (type === "run_started") return "bg-cyan-glow/80";
  if (type === "run_failed") return "bg-danger/80";
  if (type === "run_completed" || type === "run_terminal") return "bg-success/80";
  return "bg-text-4";
}

function ActivityFeed({ entries }: { entries: ActivityEntry[] }) {
  return (
    <div className="rounded-md border border-border/50 bg-card">
      <div className="flex items-center gap-2 px-4 py-2.5 border-b border-border/50">
        <span className="text-[10px] font-semibold uppercase tracking-[0.1em] text-text-3">
          Live activity
        </span>
        <span className="text-[10px] text-text-4">(last 20 events)</span>
      </div>
      <div className="divide-y divide-border/30 max-h-64 overflow-y-auto">
        {entries.map((entry) => (
          <div key={entry.id} className="flex items-center gap-3 px-4 py-2.5">
            <span
              className={cn("h-1.5 w-1.5 rounded-full shrink-0", activityDotClass(entry.type))}
            />
            <span className="text-[11px] text-text-3 shrink-0">
              {ACTIVITY_LABELS[entry.type] ?? entry.type}
            </span>
            <Link
              to="/jobs/$jobId"
              params={{ jobId: entry.jobId }}
              className="text-[11px] font-medium text-text-2 hover:text-text-1 truncate"
            >
              {entry.jobAlias}
            </Link>
            {entry.runId && (
              <Link
                to="/jobs/$jobId/runs/$runId"
                params={{ jobId: entry.jobId, runId: entry.runId }}
                className="text-[10px] font-mono text-text-4 hover:text-text-3 shrink-0"
              >
                {shortId(entry.runId)}
              </Link>
            )}
            <span className="ml-auto text-[10px] text-text-4 tabular-nums shrink-0 whitespace-nowrap">
              <RelativeTime date={entry.timestamp} />
            </span>
          </div>
        ))}
      </div>
    </div>
  );
}

// Re-exported type alias so the file compiles when JobRun is referenced indirectly.
export type { JobRun };
