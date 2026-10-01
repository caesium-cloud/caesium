import { Link, useNavigate } from "@tanstack/react-router";
import { useMutation, useQueryClient } from "@tanstack/react-query";

import { toast } from "sonner";
import { Duration } from "@/components/duration";
import { RelativeTime } from "@/components/relative-time";
import { Button } from "@/components/ui/button";
import { EmptyState } from "@/components/ui/empty-state";
import { RunStrip, RunStripAxis } from "@/components/ui/run-strip";
import { IdChip } from "@/components/ui/id-chip";
import { StatusBadge } from "@/components/ui/status-badge";
import { api, type Job, type JobRun } from "@/lib/api";
import { cn } from "@/lib/utils";
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
            ? "Job paused: active run will finish, new runs blocked."
            : "Job paused: new runs blocked.",
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
        <div className="h-10 rounded-md bg-muted/30" />
        <div className="rounded-md border border-border/50 bg-card divide-y divide-border/50">
          {Array.from({ length: 5 }).map((_, i) => (
            <div key={i} className="h-14 bg-muted/20" />
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
      <div data-testid="jobs-table-scroll" className="overflow-x-auto border-y border-border bg-transparent">
        {rows.length === 0 ? (
          <EmptyState
            title={search || statusFilter !== "all" ? "No pipelines match" : "No pipelines yet"}
            subtitle={
              search || statusFilter !== "all"
                ? "Try clearing your filter or search term."
                : "Apply a job definition to get started."
            }
            action={<Button variant="outline" onClick={() => { if (search || statusFilter !== "all") { setSearch(""); setStatusFilter("all"); } else { navigate({ to: "/jobdefs" }); } }}>{search || statusFilter !== "all" ? "clear filters" : "apply a jobdef"}</Button>}
            className="py-20"
          />
        ) : (
          <div className="min-w-[720px] overflow-hidden">
            {/* Column headers */}
            <div
              className="grid items-center px-4 border-b border-border"
              style={{ gridTemplateColumns: "230px 130px 96px 86px minmax(0,1fr) 120px" }}
            >
              <span className="text-[11px] font-bold lowercase text-text-3">Pipeline</span>
              <span className="text-[11px] font-bold lowercase text-text-3">Status</span>
              <span className="text-[11px] font-bold lowercase text-text-3">Last run</span>
              <span className="text-[11px] font-bold lowercase text-text-3">Duration</span>
              <RunStripAxis />
              <span className="sr-only">Actions</span>
            </div>

            {rows.map((job) => {
              const lr = job.latest_run;
              const isRunning = lr?.status === "running";
              const isPaused = job.paused;

              return (
                <div
                  key={job.id}
                  data-testid="job-row"
                  className={cn(
                    "group grid items-center px-4 py-0 border-b border-border/40 last:border-0 transition-colors",
                    "hover:bg-obsidian/60",
                    isRunning && [
                      "shadow-[inset_2px_0_0_hsl(var(--running))]",
                      "bg-running/[.06]",
                    ],
                    isPaused && !isRunning && "bg-gold/[.06] shadow-[inset_2px_0_0_hsl(var(--gold))]",
                  )}
                  style={{ gridTemplateColumns: "230px 130px 96px 86px minmax(0,1fr) 120px", height: "56px" }}
                >
                  {/* Alias column */}
                  <div className="min-w-0 py-2 pr-3">
                    <div className="flex items-center gap-2">
                      <Link
                        to="/jobs/$jobId"
                        params={{ jobId: job.id }}
                        className="text-sm font-bold text-text-1 hover:text-cyan-glow truncate transition-colors"
                      >
                        {job.alias}
                      </Link>
                      {isPaused && (
                        <StatusBadge status="paused" variant="word" size="sm" />
                      )}
                    </div>
                  </div>

                  {/* Status column */}
                  <div className="py-3">
                    {lr ? (
                      <StatusBadge status={lr.status} size="sm" />
                    ) : (
                      <span className="text-[11px] text-text-3">—</span>
                    )}
                  </div>

                  {/* Last run column */}
                  <div className="py-3 text-sm text-text-2 tabular-nums">
                    {lr ? <RelativeTime date={lr.started_at} /> : <span className="text-text-3">—</span>}
                  </div>

                  {/* Duration column */}
                  <div className="py-3 pr-4 text-right text-[13px] text-text-2 tabular-nums">
                    {lr ? (
                      <Duration start={lr.started_at} end={lr.completed_at} />
                    ) : (
                      <span className="text-text-3">—</span>
                    )}
                  </div>

                  {/* Sparkline column */}
                  <div className="min-w-0">
                    <RunStrip runs={job.lastRuns} />
                  </div>

                  {/* Actions column */}
                  <div className="flex items-center justify-end gap-0.5 py-2 opacity-100 transition-opacity [@media(hover:hover)]:opacity-0 [@media(hover:hover)]:group-hover:opacity-100 group-focus-within:opacity-100">
                    <Button
                      variant="default"
                      size="sm"
                      className="h-7 px-1.5 text-[11px]"
                      onClick={() => triggerMutation.mutate({ jobId: job.id })}
                      disabled={triggerMutation.isPending || isPaused}
                      title={isPaused ? "Unpause before triggering" : "Trigger run"}
                      aria-label={isPaused ? "Unpause before triggering" : "Trigger run"}
                    >
                      trigger
                    </Button>
                    <Button
                      variant="outline"
                      size="sm"
                      className="h-7 px-1.5 text-[11px]"
                      onClick={() =>
                        pauseMutation.mutate({ jobId: job.id, paused: !isPaused, hasActiveRun: isRunning })
                      }
                      disabled={pauseMutation.isPending}
                      title={isPaused ? "Unpause" : "Pause future runs"}
                      aria-label={isPaused ? "Unpause job" : "Pause future runs"}
                    >
                      {isPaused ? "resume" : "pause"}
                    </Button>
                  </div>
                </div>
              );
            })}
          </div>
        )}
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
      <h1 className="text-2xl font-bold lowercase text-text-1">Jobs</h1>
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
      <div className="flex max-w-full items-center gap-1 overflow-x-auto border-y border-border bg-transparent p-1">
        {FILTER_CHIPS.map(({ key, label }) => {
          const count = counts[key];
          const isActive = statusFilter === key;
          return (
            <button
              key={key}
              type="button"
              onClick={() => onStatusFilter(key)}
              className={cn(
                "shrink-0 flex items-center gap-1.5 rounded px-2.5 py-1 text-[11px] font-normal transition-colors",
                isActive
                  ? "text-text-1 border-b-2 border-cyan"
                  : "text-text-3 hover:text-text-2 hover:bg-obsidian/50",
              )}
            >
              {key === "all" ? label : <StatusBadge status={key} size="sm" />}
              {count > 0 && (
                <span className="text-xs text-text-3">{count}</span>
              )}
            </button>
          );
        })}
      </div>

      {/* Search */}
      <div className="relative flex-1 min-w-[160px] max-w-xs">

        <input
          type="search"
          value={search}
          onChange={(e) => onSearch(e.target.value)}
          placeholder="Filter pipelines…"
          className={cn(
            "w-full pl-8 pr-3 py-1.5 text-[12px] rounded-md border border-border/50",
            "bg-card text-text-1 placeholder:text-text-3",
            "focus:outline-none focus:ring-1 focus:ring-cyan/40 focus:border-cyan/40",
          )}
        />
      </div>

      {/* Sort */}
      <div className="flex items-center gap-1.5 text-[11px] text-text-3 sm:ml-auto">
        <label htmlFor="jobs-sort">Sort</label>
        <select
          id="jobs-sort"
          aria-label="Sort jobs"
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
  run_failed: "Run failed",
  run_cancelled: "Run cancelled",
};

function ActivityFeed({ entries }: { entries: ActivityEntry[] }) {
  return (
    <div className="border-y border-border" data-testid="activity-feed">
      <div className="flex items-center gap-2 px-4 py-2.5 border-b border-border/50">
        <span className="text-[11px] font-bold lowercase text-text-3">
          Live activity
        </span>
        <span className="text-[11px] text-text-3">(last 20 events)</span>
      </div>
      <div className="divide-y divide-border/30 max-h-64 overflow-y-auto">
        {entries.map((entry) => (
          <div key={entry.id} data-testid="activity-entry" className="flex items-center gap-3 px-4 py-2.5">
            <StatusBadge status={entry.type === "run_started" ? "running" : entry.type === "run_completed" ? "succeeded" : entry.type === "run_failed" ? "failed" : "paused"} variant="glyph" size="sm" label={ACTIVITY_LABELS[entry.type] ?? entry.type} />
            <span className="text-[11px] text-text-3 shrink-0">
              {ACTIVITY_LABELS[entry.type] ?? entry.type}
            </span>
            <Link
              to="/jobs/$jobId"
              params={{ jobId: entry.jobId }}
              className="text-[11px] font-normal text-text-2 hover:text-text-1 truncate"
            >
              {entry.jobAlias}
            </Link>
            {entry.runId && (
              <Link
                to="/jobs/$jobId/runs/$runId"
                params={{ jobId: entry.jobId, runId: entry.runId }}
                className="text-[11px] text-text-3 hover:text-text-3 shrink-0"
              >
                open run
              </Link>
            )}
            {entry.runId ? <IdChip value={entry.runId} label="run id" /> : null}
            <span className="ml-auto text-[11px] text-text-3 tabular-nums shrink-0 whitespace-nowrap">
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
