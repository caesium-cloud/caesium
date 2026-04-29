import { useEffect, useMemo, useState } from "react";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { CalendarRange, History, List, Pause, Play, Settings2, FileText, Zap } from "lucide-react";
import { stringify as yamlStringify } from "yaml";
import { toast } from "sonner";
import { Duration } from "@/components/duration";
import { RelativeTime } from "@/components/relative-time";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { StatusBadge } from "@/components/ui/status-badge";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { BackfillDialog } from "./BackfillDialog";
import { BackfillsView } from "./BackfillsView";
import { CacheView } from "./CacheView";
import { describeCachePolicy, getRunCacheStats } from "./cache-utils";
import { JobDAG } from "./JobDAG";
import { RunCacheSummary } from "./RunCacheSummary";
import { TaskDetailPanel } from "./TaskDetailPanel";
import { TaskMetadataPanel } from "./TaskMetadataPanel";
import { useDagHeight } from "@/hooks/useDagHeight";
import { api, type Atom, type Job, type JobRun, type JobTask, type TaskRun, type Trigger } from "@/lib/api";
import { events, type CaesiumEvent } from "@/lib/events";
import { formatDurationNs, formatKeyValueMap, parseJSONConfig, shortId } from "@/lib/utils";

type SecondaryView = "runs" | "tasks" | "configuration" | "definition" | "backfills" | "cache" | null;

export function JobDetailPage() {
  const { jobId } = useParams({ strict: false }) as { jobId: string };
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [streamHealthy, setStreamHealthy] = useState(events.isHealthy());
  const [backfillDialogOpen, setBackfillDialogOpen] = useState(false);
  const [secondaryView, setSecondaryView] = useState<SecondaryView>(null);

  // URL-hash driven node selection
  const [selectedTaskId, setSelectedTaskId] = useState<string | null>(() => {
    const hash = window.location.hash.slice(1);
    return hash || null;
  });

  const handleNodeSelect = (taskId: string | null) => {
    setSelectedTaskId(taskId);
    if (taskId) {
      window.history.replaceState(null, "", `#${taskId}`);
    } else {
      window.history.replaceState(null, "", window.location.pathname + window.location.search);
    }
  };

  const { data: job, isLoading: isLoadingJob } = useQuery({
    queryKey: ["job", jobId],
    queryFn: () => api.getJob(jobId),
    refetchInterval: streamHealthy ? false : 15000,
  });

  const { data: runs, isLoading: isLoadingRuns } = useQuery({
    queryKey: ["job", jobId, "runs"],
    queryFn: () => api.getJobRuns(jobId),
    refetchInterval: streamHealthy ? false : 15000,
  });

  const { data: dag, isLoading: isLoadingDAG } = useQuery({
    queryKey: ["job", jobId, "dag"],
    queryFn: () => api.getJobDAG(jobId),
  });

  const { data: tasks, isLoading: isLoadingTasks } = useQuery({
    queryKey: ["job", jobId, "tasks"],
    queryFn: () => api.getJobTasks(jobId),
  });

  const { data: atoms, isLoading: isLoadingAtoms } = useQuery({
    queryKey: ["atoms"],
    queryFn: api.getAtoms,
    select: (data) => {
      const map: Record<string, Atom> = {};
      data.forEach((atom) => {
        map[atom.id] = atom;
      });
      return map;
    },
  });

  const { data: trigger, isLoading: isLoadingTrigger } = useQuery({
    queryKey: ["trigger", job?.trigger_id],
    queryFn: () => (job?.trigger_id ? api.getTrigger(job.trigger_id) : Promise.resolve(null)),
    enabled: !!job?.trigger_id,
  });

  const isLoading = isLoadingJob || isLoadingRuns || isLoadingDAG || isLoadingAtoms || isLoadingTasks || isLoadingTrigger;

  const [dagContainerRef, dagHeight] = useDagHeight(isLoading);

  useEffect(() => {
    const onConnection = (healthy: boolean) => setStreamHealthy(healthy);
    const onPauseEvent = (e: CaesiumEvent) => {
      if (e.job_id !== jobId) return;
      const payload = e.payload as Job | undefined;
      if (!payload) {
        queryClient.invalidateQueries({ queryKey: ["job", jobId] });
        return;
      }
      queryClient.setQueryData(["job", jobId], (old: Job | undefined) => (old ? { ...old, paused: payload.paused } : payload));
      queryClient.setQueryData(["jobs"], (old: Job[] | undefined) =>
        old?.map((entry) => (entry.id === payload.id ? { ...entry, paused: payload.paused } : entry)),
      );
    };

    events.subscribeConnection(onConnection);
    ["job_paused", "job_unpaused"].forEach((type) => events.subscribe(type, onPauseEvent));

    return () => {
      events.unsubscribeConnection(onConnection);
      ["job_paused", "job_unpaused"].forEach((type) => events.unsubscribe(type, onPauseEvent));
    };
  }, [jobId, queryClient]);

  const triggerMutation = useMutation({
    mutationFn: ({ jobId: currentJobId }: { jobId: string }) => api.triggerJob(currentJobId),
    onSuccess: (run) => {
      toast.success("Job triggered successfully");
      queryClient.invalidateQueries({ queryKey: ["job", jobId, "runs"] });
      navigate({ to: "/jobs/$jobId/runs/$runId", params: { jobId: run.job_id, runId: run.id } });
    },
    onError: (err: Error) => {
      toast.error(`Failed to trigger job: ${err.message}`);
    },
  });

  const pauseMutation = useMutation({
    mutationFn: ({ jobId: currentJobId, paused }: { jobId: string; paused: boolean; hasActiveRun: boolean }) =>
      paused ? api.pauseJob(currentJobId) : api.unpauseJob(currentJobId),
    onSuccess: (updated, variables) => {
      queryClient.setQueryData(["job", jobId], updated);
      queryClient.setQueryData(["jobs"], (old: Job[] | undefined) =>
        old?.map((entry) => (entry.id === updated.id ? { ...entry, paused: updated.paused } : entry)),
      );
      if (updated.paused) {
        toast.success(
          variables.hasActiveRun
            ? "Job paused. The active run will finish, but new runs are blocked."
            : "Job paused. New runs are blocked.",
        );
        return;
      }
      toast.success("Job unpaused");
    },
    onError: (err: Error) => {
      toast.error(`Failed to update job state: ${err.message}`);
    },
  });

  const sortedRuns = useMemo(
    () => (runs ? [...runs].sort((a, b) => new Date(b.started_at).getTime() - new Date(a.started_at).getTime()) : []),
    [runs],
  );
  const activeRunSummary = useMemo(() => sortedRuns.find((run) => run.status === "running"), [sortedRuns]);
  const featuredRunSummary = activeRunSummary ?? sortedRuns[0] ?? job?.latest_run;
  const featuredRunId = featuredRunSummary?.id;
  const { data: featuredRun, isLoading: isLoadingFeaturedRun } = useQuery({
    queryKey: ["job", jobId, "runs", featuredRunId],
    queryFn: () => api.getJobRun(jobId, featuredRunId!),
    enabled: !!featuredRunId,
    refetchInterval: streamHealthy ? false : featuredRunSummary?.status === "running" ? 5000 : 15000,
  });

  useEffect(() => {
    if (!featuredRunId) {
      return;
    }

    const onEvent = (e: CaesiumEvent) => {
      if (e.job_id !== jobId) return;
      if (e.run_id && e.run_id !== featuredRunId) return;

      queryClient.setQueryData(["job", jobId, "runs", featuredRunId], (old: JobRun | undefined) => {
        if (!old) return old;

        if (e.type === "run_started") {
          const startedRun = e.payload as JobRun | undefined;
          return startedRun?.id === featuredRunId ? { ...old, ...startedRun } : old;
        }

        if (e.type === "run_completed" || e.type === "run_succeeded" || e.type === "run_terminal") {
          const completedRun = e.payload as JobRun | undefined;
          if (completedRun?.id === featuredRunId) {
            return completedRun?.tasks ? completedRun : { ...old, ...completedRun, status: "succeeded" };
          }
          return old;
        }

        if (e.type === "run_failed") {
          const failedRun = e.payload as JobRun | undefined;
          if (failedRun?.id === featuredRunId) {
            return failedRun?.tasks ? failedRun : { ...old, ...failedRun, status: "failed" };
          }
          return old;
        }

        if (e.type.startsWith("task_")) {
          const taskUpdate = e.payload as TaskRun | undefined;
          const taskID = taskUpdate?.task_id || e.task_id;
          if (!taskID) return old;

          const updatedTasks = [...(old.tasks || [])];
          const existingIndex = updatedTasks.findIndex((task) => task.task_id === taskID);
          const nextStatus =
            e.type === "task_started"
              ? "running"
              : e.type === "task_succeeded"
                ? "succeeded"
                : e.type === "task_failed"
                  ? "failed"
                  : e.type === "task_skipped"
                    ? "skipped"
                    : e.type === "task_retrying"
                      ? "pending"
                      : e.type === "task_cached"
                        ? "cached"
                      : taskUpdate?.status || "pending";

          if (existingIndex >= 0) {
            updatedTasks[existingIndex] = {
              ...updatedTasks[existingIndex],
              ...taskUpdate,
              status: nextStatus,
            };
          } else {
            updatedTasks.push({
              id: taskID,
              job_run_id: featuredRunId,
              task_id: taskID,
              atom_id: taskUpdate?.atom_id || "",
              engine: taskUpdate?.engine || "",
              image: taskUpdate?.image || "",
              command: taskUpdate?.command || [],
              status: nextStatus,
              created_at: new Date().toISOString(),
              updated_at: new Date().toISOString(),
              ...taskUpdate,
            });
          }

          const summary = getRunCacheStats({ ...old, tasks: updatedTasks });
          return {
            ...old,
            tasks: updatedTasks,
            cache_hits: summary.cacheHits,
            executed_tasks: summary.executedTasks,
            total_tasks: summary.totalTasks,
          };
        }

        return old;
      });

      queryClient.invalidateQueries({ queryKey: ["job", jobId, "runs"] });
      queryClient.invalidateQueries({ queryKey: ["job", jobId] });
    };

    ["run_started", "run_completed", "run_failed", "run_terminal", "task_started", "task_succeeded", "task_failed", "task_skipped", "task_retrying", "task_cached"].forEach((type) =>
      events.subscribe(type, onEvent),
    );

    return () => {
      ["run_started", "run_completed", "run_failed", "run_terminal", "task_started", "task_succeeded", "task_failed", "task_skipped", "task_retrying", "task_cached"].forEach((type) =>
        events.unsubscribe(type, onEvent),
      );
    };
  }, [featuredRunId, jobId, queryClient]);

  const activeRun = featuredRun?.status === "running" ? featuredRun : activeRunSummary;
  const featuredRunTasks = useMemo(() => buildTaskRunMap(featuredRun?.tasks), [featuredRun?.tasks]);
  const taskMetadata = useMemo(() => buildTaskStatusMap(featuredRun?.tasks), [featuredRun?.tasks]);
  const taskStatus = useMemo(() => buildTaskStatusLookup(featuredRun?.tasks), [featuredRun?.tasks]);
  const taskDefinitions = useMemo(
    () => tasks?.reduce<Record<string, JobTask>>((acc, task) => { acc[task.id] = task; return acc; }, {}) ?? {},
    [tasks],
  );
  const triggerConfig = useMemo(() => parseJSONConfig(trigger?.configuration), [trigger?.configuration]);

  if (isLoading || (featuredRunId && isLoadingFeaturedRun)) {
    return <div className="p-8">Loading...</div>;
  }

  if (!job) {
    return <div className="p-8">Job not found</div>;
  }

  const selectedTask = selectedTaskId ? taskDefinitions[selectedTaskId] : undefined;
  const selectedRunTask = selectedTaskId ? featuredRunTasks[selectedTaskId] : undefined;

  return (
    <div className="space-y-4">
      {/* Header row */}
      <div className="flex flex-col gap-3 md:flex-row md:items-start md:justify-between">
        <div>
            <div className="text-[10px] font-semibold uppercase tracking-[0.18em] text-text-3 mb-1">Pipeline</div>
            <div className="flex items-center gap-2.5">
              <h1 className="text-xl font-semibold text-text-1 tracking-tight">{job.alias}</h1>
              <StatusBadge status={job.paused ? "paused" : (featuredRun?.status ?? "queued")} size="sm" />
            </div>
            <div className="mt-1 flex flex-wrap items-center gap-2 text-xs text-text-3">
              <span className="font-mono text-text-4">{shortId(job.id)}</span>
              {featuredRun ? (
                <>
                  <span className="text-text-4">·</span>
                  <span>
                    {activeRun ? "Started" : "Last run"}{" "}
                    <RelativeTime date={featuredRun.started_at} />
                  </span>
                  <span className="text-text-4">·</span>
                  <span className="font-mono tabular-nums">
                    <Duration start={featuredRun.started_at} end={featuredRun.completed_at} />
                  </span>
                </>
              ) : null}
            </div>
            {featuredRun ? <div className="mt-2"><RunCacheSummary run={featuredRun} /></div> : null}
        </div>
        <div className="flex items-center gap-2">
          {/* Secondary view buttons */}
          <div className="flex items-center gap-1 mr-2">
            <Button variant="ghost" size="sm" className="h-8 px-2.5 text-xs" onClick={() => setSecondaryView("runs")}>
              <History className="mr-1.5 h-3.5 w-3.5" />
              Runs
            </Button>
            <Button variant="ghost" size="sm" className="h-8 px-2.5 text-xs" onClick={() => setSecondaryView("tasks")}>
              <List className="mr-1.5 h-3.5 w-3.5" />
              Tasks
            </Button>
            <Button variant="ghost" size="sm" className="h-8 px-2.5 text-xs" onClick={() => setSecondaryView("configuration")}>
              <Settings2 className="mr-1.5 h-3.5 w-3.5" />
              Config
            </Button>
            <Button variant="ghost" size="sm" className="h-8 px-2.5 text-xs" onClick={() => setSecondaryView("definition")}>
              <FileText className="mr-1.5 h-3.5 w-3.5" />
              YAML
            </Button>
            <Button variant="ghost" size="sm" className="h-8 px-2.5 text-xs" onClick={() => setSecondaryView("backfills")}>
              <CalendarRange className="mr-1.5 h-3.5 w-3.5" />
              Backfills
            </Button>
            <Button variant="ghost" size="sm" className="h-8 px-2.5 text-xs" onClick={() => setSecondaryView("cache")}>
              Cache
            </Button>
          </div>
          <div className="h-6 w-px bg-border" />
          <Button size="sm" onClick={() => triggerMutation.mutate({ jobId: job.id })} disabled={triggerMutation.isPending || job.paused}>
            <Play className="mr-1.5 h-3.5 w-3.5" />
            Trigger
          </Button>
          <Button
            variant="outline"
            size="sm"
            onClick={() => setBackfillDialogOpen(true)}
            disabled={job.paused || trigger?.type !== "cron"}
            title={trigger?.type !== "cron" ? "Backfill requires a cron trigger" : undefined}
          >
            <CalendarRange className="mr-1.5 h-3.5 w-3.5" />
            Backfill
          </Button>
          <Button
            variant="outline"
            size="sm"
            onClick={() => pauseMutation.mutate({ jobId: job.id, paused: !job.paused, hasActiveRun: !!activeRun })}
            disabled={pauseMutation.isPending}
          >
            <Pause className="mr-1.5 h-3.5 w-3.5" />
            {job.paused ? "Unpause" : "Pause"}
          </Button>
        </div>
      </div>

      {/* DAG — fills to bottom of viewport */}
      <div
        ref={dagContainerRef}
        className="relative flex flex-col overflow-hidden rounded-md border bg-card"
        style={{ height: dagHeight ? `${dagHeight}px` : "600px" }}
      >
        {/* Compact overlay status bar */}
        <div className="flex items-center justify-between border-b border-border/50 px-4 py-1.5 gap-4">
          <div className="flex items-center gap-2 text-xs text-text-3 min-w-0">
            {featuredRun ? (
              <>
                {activeRun && (
                  <span className="flex items-center gap-1.5">
                    <Zap className="h-3 w-3 text-cyan-glow animate-pulse" />
                    <span className="text-cyan-glow/80 font-medium">Live</span>
                  </span>
                )}
                <span className="truncate">
                  {activeRun ? "Overlay from" : "Latest overlay — run"}{" "}
                  <Link
                    to="/jobs/$jobId/runs/$runId"
                    params={{ jobId, runId: featuredRun.id }}
                    className="font-mono text-cyan-glow/70 hover:text-cyan-glow"
                  >
                    {shortId(featuredRun.id)}
                  </Link>
                </span>
              </>
            ) : (
              <span className="text-text-4">DAG topology — trigger a run to see live state</span>
            )}
          </div>

          <div className="flex items-center gap-3 shrink-0">
            {/* Live task counters */}
            {featuredRun && (
              <DagCounters tasks={featuredRun.tasks} />
            )}
            {job.paused && (
              <StatusBadge status="paused" variant="soft" size="sm" />
            )}
          </div>
        </div>

        {/* DAG canvas */}
        <div className="flex-1 min-h-0">
          {dag && atoms ? (
            <JobDAG
              dag={dag}
              atoms={atoms}
              taskDefinitions={taskDefinitions}
              taskMetadata={taskMetadata}
              taskStatus={taskStatus}
              taskRunData={featuredRunTasks}
              onNodeClick={featuredRun ? handleNodeSelect : undefined}
              selectedTaskId={selectedTaskId}
            />
          ) : null}
        </div>

        {/* Slide-over task detail panel */}
        {selectedTaskId && featuredRun ? (
          <TaskDetailPanel
            key={selectedTaskId}
            taskId={selectedTaskId}
            task={selectedTask}
            runTask={selectedRunTask}
            taskType={dag?.nodes?.find(n => n.id === selectedTaskId)?.type}
            jobId={jobId}
            runId={featuredRun.id}
            onClose={() => handleNodeSelect(null)}
          />
        ) : null}
      </div>

      {/* Secondary views dialog */}
      <Dialog open={secondaryView !== null} onOpenChange={(open) => !open && setSecondaryView(null)}>
        <DialogContent className={`${secondaryView === "cache" ? "max-w-5xl" : "max-w-3xl"} max-h-[80vh] flex flex-col gap-0 overflow-hidden p-0`}>
          <DialogHeader className="shrink-0 px-6 pt-6 pb-4">
            <DialogTitle>
              {secondaryView === "runs" && "Run History"}
              {secondaryView === "tasks" && "Task Definitions"}
              {secondaryView === "configuration" && "Configuration"}
              {secondaryView === "definition" && "Job Definition (YAML)"}
              {secondaryView === "backfills" && "Backfills"}
              {secondaryView === "cache" && "Cache"}
            </DialogTitle>
          </DialogHeader>
          <div className="flex-1 min-h-0 overflow-auto px-6 pb-6">
            {secondaryView === "runs" && (
              <RunsView runs={sortedRuns} job={job} />
            )}
            {secondaryView === "tasks" && (
              <TasksView tasks={tasks} atoms={atoms} dag={dag} featuredRunTasks={featuredRunTasks} />
            )}
            {secondaryView === "configuration" && (
              <ConfigurationView job={job} trigger={trigger} triggerConfig={triggerConfig} />
            )}
            {secondaryView === "definition" && (
              <pre className="overflow-auto rounded-md border bg-muted p-4 text-xs">{yamlStringify(job)}</pre>
            )}
            {secondaryView === "backfills" && (
              <BackfillsView jobId={jobId} />
            )}
            {secondaryView === "cache" && (
              <CacheView jobId={jobId} job={job} featuredRun={featuredRun} tasks={tasks} />
            )}
          </div>
        </DialogContent>
      </Dialog>

      <BackfillDialog
        jobId={jobId}
        open={backfillDialogOpen}
        onOpenChange={setBackfillDialogOpen}
        disabled={job.paused}
      />
    </div>
  );
}

/* ── DAG live counters ── */

function DagCounters({ tasks }: { tasks?: TaskRun[] }) {
  if (!tasks || tasks.length === 0) return null;
  const done = tasks.filter((t) => t.status === "succeeded" || t.status === "completed").length;
  const running = tasks.filter((t) => t.status === "running").length;
  const cached = tasks.filter((t) => t.status === "cached").length;
  const queued = tasks.filter((t) => t.status === "pending" || t.status === "queued").length;
  const total = tasks.length;

  return (
    <div className="flex items-center gap-3 text-[10px] font-mono tabular-nums">
      <span className="text-success/80">{done}/{total} done</span>
      {running > 0 && <span className="text-cyan-glow/80">{running} active</span>}
      {cached > 0 && <span className="text-cached/80">{cached} cached</span>}
      {queued > 0 && <span className="text-text-4">{queued} queued</span>}
    </div>
  );
}

/* ── Secondary view components ── */

function RunsView({ runs, job }: { runs: JobRun[]; job: Job }) {
  return (
    <div className="rounded-md border bg-card divide-y">
      {runs.length === 0 ? (
        <div className="p-8 text-center text-muted-foreground">No runs found for this job.</div>
      ) : null}
      {runs.map((run) => (
        <Link
          key={run.id}
          to="/jobs/$jobId/runs/$runId"
          params={{ jobId: job.id, runId: run.id }}
          className="flex items-center justify-between gap-3 p-4 transition-colors hover:bg-muted/50"
        >
          <div className="space-y-1">
            <div className="flex items-center gap-2">
              <span className="font-medium">{new Date(run.started_at).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" })}</span>
              {run.params && Object.keys(run.params).length > 0 ? (
                <Badge variant="outline">{Object.keys(run.params).length} params</Badge>
              ) : null}
            </div>
            <div className="text-xs text-muted-foreground">
              <RelativeTime date={run.started_at} /> · <span className="font-mono">{shortId(run.id)}</span> ·{" "}
              <span className="font-mono">
                <Duration start={run.started_at} end={run.completed_at} />
              </span>
            </div>
          </div>
          {renderRunStatus(run.status)}
        </Link>
      ))}
    </div>
  );
}

function TasksView({
  tasks,
  atoms,
  dag,
  featuredRunTasks,
}: {
  tasks: Parameters<typeof api.getJobTasks> extends [string] ? Awaited<ReturnType<typeof api.getJobTasks>> | undefined : never;
  atoms: Record<string, Atom> | undefined;
  dag: Awaited<ReturnType<typeof api.getJobDAG>> | undefined;
  featuredRunTasks: Record<string, TaskRun>;
}) {
  return (
    <div className="space-y-4">
      {tasks?.map((task) => (
        <Card key={task.id}>
          <CardHeader className="pb-3">
            <CardTitle className="text-sm">{atoms?.[task.atom_id]?.image || `Task ${shortId(task.id)}`}</CardTitle>
          </CardHeader>
          <CardContent className="grid gap-3 text-sm md:grid-cols-2">
            <TaskMetadataPanel task={task} runTask={featuredRunTasks[task.id]} taskType={dag?.nodes?.find(n => n.id === task.id)?.type} framed={false} />
            <div className="space-y-3">
              <div>
                <div className="mb-1 text-xs uppercase tracking-wide text-muted-foreground">Atom ID</div>
                <div className="font-mono text-xs">{task.atom_id}</div>
              </div>
              <div>
                <div className="mb-1 text-xs uppercase tracking-wide text-muted-foreground">Command</div>
                <div className="font-mono text-xs break-all">{formatCommand(atoms?.[task.atom_id]?.command)}</div>
              </div>
            </div>
          </CardContent>
        </Card>
      ))}
    </div>
  );
}

function ConfigurationView({
  job,
  trigger,
  triggerConfig,
}: {
  job: Job;
  trigger: Trigger | null | undefined;
  triggerConfig: Record<string, unknown> | null;
}) {
  return (
    <div className="grid gap-4 md:grid-cols-2">
      <Card>
        <CardHeader className="pb-3">
          <CardTitle className="text-sm">Trigger Configuration</CardTitle>
        </CardHeader>
        <CardContent className="space-y-3 text-sm">
          {renderTriggerSummary(trigger)}
          {triggerConfig?.defaultParams && typeof triggerConfig.defaultParams === "object" ? (
            <div>
              <div className="mb-1 text-xs uppercase tracking-wide text-muted-foreground">Default Params</div>
              <div className="font-mono text-xs">{formatKeyValueMap(triggerConfig.defaultParams as Record<string, unknown>)}</div>
            </div>
          ) : null}
          <div>
            <div className="mb-1 text-xs uppercase tracking-wide text-muted-foreground">Raw Config</div>
            <pre className="overflow-x-auto rounded-md bg-muted p-3 text-xs">{trigger?.configuration || "{}"}</pre>
          </div>
        </CardContent>
      </Card>
      <Card>
        <CardHeader className="pb-3">
          <CardTitle className="text-sm">Job Metadata</CardTitle>
        </CardHeader>
        <CardContent className="space-y-3 text-sm">
          <div>
            <div className="mb-1 text-xs uppercase tracking-wide text-muted-foreground">Pause State</div>
            <div>{job.paused ? "Paused (blocks new runs)" : "Active"}</div>
          </div>
          {job.run_timeout ? (
            <div>
              <div className="mb-1 text-xs uppercase tracking-wide text-muted-foreground">Run Timeout</div>
              <div className="font-mono text-xs">{formatDurationNs(job.run_timeout)}</div>
            </div>
          ) : null}
          {job.task_timeout ? (
            <div>
              <div className="mb-1 text-xs uppercase tracking-wide text-muted-foreground">Task Timeout</div>
              <div className="font-mono text-xs">{formatDurationNs(job.task_timeout)}</div>
            </div>
          ) : null}
          {job.max_parallel_tasks ? (
            <div>
              <div className="mb-1 text-xs uppercase tracking-wide text-muted-foreground">Max Parallel Tasks</div>
              <div className="font-mono text-xs">{job.max_parallel_tasks}</div>
            </div>
          ) : null}
          <div>
            <div className="mb-1 text-xs uppercase tracking-wide text-muted-foreground">Cache Policy</div>
            <div className="font-mono text-xs">{describeCachePolicy(job.cache_config)}</div>
          </div>
          <div>
            <div className="mb-1 text-xs uppercase tracking-wide text-muted-foreground">Labels</div>
            <div className="font-mono text-xs">{formatKeyValueMap(job.labels)}</div>
          </div>
          <div>
            <div className="mb-1 text-xs uppercase tracking-wide text-muted-foreground">Annotations</div>
            <div className="font-mono text-xs">{formatKeyValueMap(job.annotations)}</div>
          </div>
        </CardContent>
      </Card>
    </div>
  );
}

/* ── Helpers ── */

function buildTaskStatusMap(tasks?: TaskRun[]) {
  const metadata: Record<string, { status: string; started_at?: string; completed_at?: string; error?: string }> = {};
  tasks?.forEach((task) => {
    metadata[task.task_id] = {
      status: task.status,
      started_at: task.started_at,
      completed_at: task.completed_at,
      error: task.error,
    };
  });
  return metadata;
}

function buildTaskStatusLookup(tasks?: TaskRun[]) {
  const map: Record<string, string> = {};
  tasks?.forEach((task) => {
    map[task.task_id] = task.status;
  });
  return map;
}

function buildTaskRunMap(tasks?: TaskRun[]) {
  const map: Record<string, TaskRun> = {};
  tasks?.forEach((task) => {
    map[task.task_id] = task;
  });
  return map;
}

function renderRunStatus(status: string) {
  const variant =
    status === "succeeded" || status === "completed"
      ? "success"
      : status === "failed"
        ? "destructive"
        : status === "running"
          ? "running"
          : "secondary";

  return <Badge variant={variant}>{status}</Badge>;
}

function renderTriggerSummary(trigger: Trigger | null | undefined) {
  if (!trigger) {
    return <div className="text-muted-foreground">This job does not have an associated trigger.</div>;
  }

  return (
    <>
      <div>
        <div className="mb-1 text-xs uppercase tracking-wide text-muted-foreground">Type</div>
        <div>{trigger.type}</div>
      </div>
      <div>
        <div className="mb-1 text-xs uppercase tracking-wide text-muted-foreground">Alias</div>
        <div>{trigger.alias}</div>
      </div>
      <div>
        <div className="mb-1 text-xs uppercase tracking-wide text-muted-foreground">Trigger ID</div>
        <div className="font-mono text-xs">{trigger.id}</div>
      </div>
    </>
  );
}

function formatCommand(command?: string | string[]) {
  if (!command) {
    return "N/A";
  }
  if (Array.isArray(command)) {
    return command.join(" ");
  }
  return command;
}
