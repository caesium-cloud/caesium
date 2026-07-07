import { useEffect, useMemo, useState, type ReactNode } from "react";
import { Link, useNavigate, useParams, useRouterState } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { CalendarRange, FileText, FileWarning, History, List, ListOrdered, MoreHorizontal, Pause, Play, Settings2, ShieldCheck, XCircle, Zap } from "lucide-react";
import { stringify as yamlStringify } from "yaml";
import { toast } from "sonner";
import { Duration } from "@/components/duration";
import { NotFoundState } from "@/components/not-found-state";
import { RelativeTime } from "@/components/relative-time";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { StatusBadge } from "@/components/ui/status-badge";
import { IncidentRibbon } from "@/features/incidents/IncidentRibbon";
import { INCIDENT_EVENT_TYPES, formatIncidentClass, incidentAge, incidentSummary, isResolvedIncident } from "@/features/incidents/incident-utils";
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
import { DagCounters } from "./DagCounters";
import { JobDAG } from "./JobDAG";
import { buildJobAuthoringManifest, formatCommandForDisplay } from "./job-detail-manifest";
import { RunCacheSummary } from "./RunCacheSummary";
import { TaskDetailPanel } from "./TaskDetailPanel";
import { TaskMetadataPanel } from "./TaskMetadataPanel";
import { TriggerDialog } from "./TriggerDialog";
import { useDagHeight } from "@/hooks/useDagHeight";
import { ApiError, api, type Atom, type Incident, type Job, type JobRun, type JobTask, type RunQueueItem, type TaskRun, type Trigger } from "@/lib/api";
import { events, type CaesiumEvent } from "@/lib/events";
import { formatDurationNs, formatKeyValueMap, formatUTCTimestamp, parseJSONConfig, shortId } from "@/lib/utils";

type SecondaryView = "runs" | "tasks" | "configuration" | "definition" | "backfills" | "cache";

const secondaryViewSegments: Record<SecondaryView, string> = {
  runs: "runs",
  tasks: "tasks",
  configuration: "config",
  definition: "yaml",
  backfills: "backfills",
  cache: "cache",
};

export function JobDetailPage() {
  const { jobId } = useParams({ strict: false }) as { jobId: string };
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const pathname = useRouterState({ select: (state) => state.location.pathname });
  const secondaryView = secondaryViewFromPath(pathname);
  const [streamHealthy, setStreamHealthy] = useState(events.isHealthy());
  const [backfillDialogOpen, setBackfillDialogOpen] = useState(false);
  const [triggerDialogOpen, setTriggerDialogOpen] = useState(false);

  // URL-hash driven node selection
  const [selectedTaskId, setSelectedTaskId] = useState<string | null>(() => {
    const hash = window.location.hash.slice(1);
    if (hash.startsWith("queue-")) {
      return null;
    }
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

  // Short-id deep links: GET /v1/jobs/:id resolves an unambiguous UUID prefix,
  // but the sub-resource routes (runs/queue/dag/tasks) still require a full
  // UUID. Once the job resolves, canonicalize the URL to its full id so every
  // sub-fetch, SSE filter, and mutation below operates on the full id.
  useEffect(() => {
    if (job && job.id !== jobId) {
      navigate({ to: jobViewPath(job.id, secondaryView), replace: true });
    }
  }, [job, jobId, navigate, secondaryView]);

  const { data: runs, isLoading: isLoadingRuns } = useQuery({
    queryKey: ["job", jobId, "runs"],
    queryFn: () => api.getJobRuns(jobId),
    refetchInterval: streamHealthy ? false : 15000,
  });

  const { data: queueRows, isLoading: isLoadingQueue } = useQuery({
    queryKey: ["job", jobId, "queue"],
    queryFn: () => api.getJobQueue(jobId),
    refetchInterval: streamHealthy ? 3000 : 15000,
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

  const { data: features } = useQuery({
    queryKey: ["system-features"],
    queryFn: api.getSystemFeatures,
    staleTime: 60_000,
  });

  const { data: jobIncidentList } = useQuery({
    queryKey: ["incidents", "job", jobId],
    queryFn: () => api.getIncidents({ job_id: jobId, limit: 200 }),
    enabled: features?.agent_remediation_enabled === true,
    refetchInterval: 30_000,
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
    mutationFn: ({ jobId: currentJobId, params }: { jobId: string; params: Record<string, string> }) =>
      api.triggerJob(currentJobId, Object.keys(params).length > 0 ? { params } : undefined),
    onSuccess: (run) => {
      queryClient.invalidateQueries({ queryKey: ["job", jobId, "queue"] });
      queryClient.invalidateQueries({ queryKey: ["job", jobId, "runs"] });
      setTriggerDialogOpen(false);
      if (!run?.id) {
        toast.success("Job queued");
        return;
      }
      toast.success("Job triggered successfully");
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

  const cancelQueueMutation = useMutation({
    mutationFn: ({ jobId: currentJobId, queueId }: { jobId: string; queueId: string }) =>
      api.cancelQueuedRun(currentJobId, queueId),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["job", jobId, "queue"] });
      queryClient.invalidateQueries({ queryKey: ["job", jobId, "runs"] });
      toast.success("Queued run cancelled");
    },
    onError: (err: Error) => {
      if (err instanceof ApiError && err.status === 409) {
        toast.error("Queued run already started — can't cancel");
        queryClient.invalidateQueries({ queryKey: ["job", jobId, "queue"] });
        queryClient.invalidateQueries({ queryKey: ["job", jobId, "runs"] });
        return;
      }
      toast.error(`Failed to cancel queued run: ${err.message}`);
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
      queryClient.invalidateQueries({ queryKey: ["job", jobId, "queue"] });
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

  useEffect(() => {
    if (features?.agent_remediation_enabled !== true) return;
    const onIncidentEvent = (e: CaesiumEvent) => {
      if (e?.job_id && e.job_id !== jobId) return;
      queryClient.invalidateQueries({ queryKey: ["incidents", "job", jobId] });
    };
    INCIDENT_EVENT_TYPES.forEach((type) => events.subscribe(type, onIncidentEvent));
    return () => {
      INCIDENT_EVENT_TYPES.forEach((type) => events.unsubscribe(type, onIncidentEvent));
    };
  }, [features?.agent_remediation_enabled, jobId, queryClient]);

  const activeRun = featuredRun?.status === "running" ? featuredRun : activeRunSummary;
  const featuredRunTasks = useMemo(() => buildTaskRunMap(featuredRun?.tasks), [featuredRun?.tasks]);
  const taskMetadata = useMemo(() => buildTaskStatusMap(featuredRun?.tasks), [featuredRun?.tasks]);
  const taskStatus = useMemo(() => buildTaskStatusLookup(featuredRun?.tasks), [featuredRun?.tasks]);
  const taskDefinitions = useMemo(
    () => tasks?.reduce<Record<string, JobTask>>((acc, task) => { acc[task.id] = task; return acc; }, {}) ?? {},
    [tasks],
  );
  const triggerConfig = useMemo(() => parseJSONConfig(trigger?.configuration), [trigger?.configuration]);
  const jobManifest = useMemo(
    () => job ? buildJobAuthoringManifest({ job, tasks, trigger, atoms, dag }) : null,
    [job, tasks, trigger, atoms, dag],
  );

  if (isLoading || (featuredRunId && isLoadingFeaturedRun)) {
    return <div className="p-8">Loading...</div>;
  }

  if (!job) {
    return (
      <NotFoundState
        title="Job not found"
        subtitle="The requested job could not be found or is no longer available."
      />
    );
  }

  const selectedTask = selectedTaskId ? taskDefinitions[selectedTaskId] : undefined;
  const selectedRunTask = selectedTaskId ? featuredRunTasks[selectedTaskId] : undefined;
  const jobIncidents = jobIncidentList?.incidents ?? [];
  const activeIncidents = jobIncidents.filter((incident) => !isResolvedIncident(incident));
  const selectedTaskIncidents = selectedTaskId
    ? jobIncidents.filter((incident) => incidentMatchesTask(incident, selectedTaskId, selectedTask?.name))
    : [];

  return (
    <div className="space-y-4">
      <div className="space-y-3">
        <div className="flex flex-col gap-3 lg:flex-row lg:items-start lg:justify-between">
          <div className="min-w-0">
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
          <div className="flex shrink-0 items-center gap-2">
            <Button
              size="sm"
              aria-label="Trigger job"
              onClick={() => setTriggerDialogOpen(true)}
              disabled={triggerMutation.isPending || job.paused}
            >
              <Play className="mr-1.5 h-3.5 w-3.5" />
              Trigger
            </Button>
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button
                  variant="outline"
                  size="icon"
                  aria-label="More job actions"
                  data-testid="job-actions-overflow"
                >
                  <MoreHorizontal className="h-4 w-4" />
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end" className="w-48">
                <DropdownMenuItem
                  aria-label="Backfill job"
                  disabled={job.paused || trigger?.type !== "cron"}
                  onSelect={() => setBackfillDialogOpen(true)}
                >
                  <CalendarRange className="h-4 w-4" />
                  Backfill
                </DropdownMenuItem>
                <DropdownMenuItem
                  aria-label={job.paused ? "Unpause job" : "Pause job"}
                  disabled={pauseMutation.isPending}
                  onSelect={() => pauseMutation.mutate({ jobId: job.id, paused: !job.paused, hasActiveRun: !!activeRun })}
                >
                  <Pause className="h-4 w-4" />
                  {job.paused ? "Unpause" : "Pause"}
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          </div>
        </div>
        <div
          className="flex max-w-full items-center gap-1 overflow-x-auto rounded-md border bg-card/80 p-1"
          data-testid="job-detail-view-tabs"
        >
          <ViewTab jobId={job.id} view="runs" activeView={secondaryView} icon={<History className="h-3.5 w-3.5" />}>
            Runs
          </ViewTab>
          <ViewTab jobId={job.id} view="tasks" activeView={secondaryView} icon={<List className="h-3.5 w-3.5" />}>
            Tasks
          </ViewTab>
          <ViewTab jobId={job.id} view="configuration" activeView={secondaryView} icon={<Settings2 className="h-3.5 w-3.5" />}>
            Config
          </ViewTab>
          <ViewTab jobId={job.id} view="definition" activeView={secondaryView} icon={<FileText className="h-3.5 w-3.5" />}>
            YAML
          </ViewTab>
          <ViewTab jobId={job.id} view="backfills" activeView={secondaryView} icon={<CalendarRange className="h-3.5 w-3.5" />}>
            Backfills
          </ViewTab>
          <ViewTab jobId={job.id} view="cache" activeView={secondaryView}>
            Cache
          </ViewTab>
        </div>
      </div>

      <RunQueuePanel
        rows={queueRows}
        isLoading={isLoadingQueue}
        jobId={job.id}
        cancelingQueueId={cancelQueueMutation.isPending ? cancelQueueMutation.variables?.queueId : undefined}
        onCancel={(queueId) => cancelQueueMutation.mutate({ jobId: job.id, queueId })}
      />

      {features?.agent_remediation_enabled === true ? (
        <RemediationOverview job={job} incidents={jobIncidents} activeIncidents={activeIncidents} />
      ) : null}

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
            {featuredRun && <DagCounters tasks={featuredRun.tasks} />}
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
            incidents={selectedTaskIncidents}
            onClose={() => handleNodeSelect(null)}
          />
        ) : null}
      </div>

      <Dialog
        open={secondaryView !== null}
        onOpenChange={(open) => {
          if (!open) {
            navigate({ to: "/jobs/$jobId", params: { jobId: job.id }, replace: true });
          }
        }}
      >
        {secondaryView ? (
          <DialogContent className={`${secondaryView === "cache" ? "max-w-5xl" : "max-w-3xl"} max-h-[80vh] flex flex-col gap-0 overflow-hidden p-0`}>
            <DialogHeader className="shrink-0 px-6 pt-6 pb-4">
              <DialogTitle>{secondaryViewTitle(secondaryView)}</DialogTitle>
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
                <div className="space-y-3">
                  <div className="flex gap-2 rounded-md border border-warning/30 bg-warning/10 p-3 text-xs text-text-2">
                    <FileWarning className="mt-0.5 h-4 w-4 shrink-0 text-warning" />
                    <div>
                      <div className="font-medium text-warning">Reconstructed from loaded job-detail data</div>
                      <div className="mt-1 text-text-3">
                        Root callbacks are not exposed by this page&apos;s loaded endpoints. Original multi-engine
                        volume declarations are represented from resolved mounts when possible.
                      </div>
                    </div>
                  </div>
                  <pre className="overflow-auto rounded-md border bg-muted p-4 text-xs">
                    {yamlStringify(jobManifest)}
                  </pre>
                </div>
              )}
              {secondaryView === "backfills" && (
                <BackfillsView jobId={job.id} />
              )}
              {secondaryView === "cache" && (
                <CacheView jobId={job.id} job={job} featuredRun={featuredRun} tasks={tasks} />
              )}
            </div>
          </DialogContent>
        ) : null}
      </Dialog>

      <TriggerDialog
        open={triggerDialogOpen}
        onOpenChange={setTriggerDialogOpen}
        disabled={job.paused}
        isPending={triggerMutation.isPending}
        onConfirm={(params) => triggerMutation.mutate({ jobId: job.id, params })}
      />

      <BackfillDialog
        jobId={job.id}
        open={backfillDialogOpen}
        onOpenChange={setBackfillDialogOpen}
        disabled={job.paused}
      />
    </div>
  );
}

function RunQueuePanel({
  rows,
  isLoading,
  jobId,
  cancelingQueueId,
  onCancel,
}: {
  rows?: RunQueueItem[];
  isLoading: boolean;
  jobId: string;
  cancelingQueueId?: string;
  onCancel: (queueId: string) => void;
}) {
  const hasRows = (rows?.length ?? 0) > 0;
  if (!hasRows && !isLoading) {
    return null;
  }

  return (
    <div data-testid="run-queue-panel" className="rounded-md border bg-card px-4 py-3">
      <div className="mb-3 flex items-center justify-between gap-3">
        <div className="flex items-center gap-2">
          <ListOrdered className="h-4 w-4 text-cyan-glow" />
          <h2 className="text-sm font-semibold text-text-1">Run queue</h2>
        </div>
        <Badge variant="outline" className="font-mono text-[10px]">
          {rows?.length ?? 0} pending
        </Badge>
      </div>
      {hasRows ? (
        <div className="divide-y rounded-md border bg-background/40">
          {rows?.map((row) => (
            <div
              key={row.id}
              id={`queue-${row.id}`}
              data-testid="run-queue-row"
              className="grid gap-3 px-3 py-2 text-xs md:grid-cols-[minmax(120px,0.8fr)_minmax(220px,1.4fr)_minmax(160px,1fr)_auto] md:items-center"
            >
              <div className="min-w-0">
                <div className="flex flex-wrap items-center gap-2">
                  <span className="font-mono text-text-3">#{row.position}</span>
                  <Badge variant="outline" className="w-fit font-mono text-[10px]">
                    {formatPriority(row.priority)}
                  </Badge>
                </div>
                <div className="mt-1 font-mono text-[10px] text-text-4">{shortId(row.id)}</div>
              </div>
              <div className="min-w-0">
                <div className="truncate text-text-2" title={queuePendingReason(row)}>
                  {queuePendingReason(row)}
                </div>
                <div className="mt-1 text-text-3">
                  enqueued <RelativeTime date={row.enqueued_at} />
                </div>
              </div>
              <span className="min-w-0 truncate font-mono text-text-2" title={formatQueueParams(row.params)}>
                {formatQueueParams(row.params)}
              </span>
              <div className="flex items-center justify-end gap-1">
                <a
                  href={`/jobs/${encodeURIComponent(jobId)}#queue-${row.id}`}
                  className="rounded-md px-2 py-1 text-xs text-cyan-glow hover:bg-cyan-glow/10"
                  aria-label={`Inspect queued run ${shortId(row.id)}`}
                  data-testid="run-queue-inspect-link"
                >
                  Inspect
                </a>
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  className="h-7 px-2 text-xs text-danger hover:text-danger"
                  aria-label={`Cancel queued run ${shortId(row.id)}`}
                  disabled={cancelingQueueId === row.id}
                  onClick={() => onCancel(row.id)}
                >
                  <XCircle className="mr-1 h-3.5 w-3.5" />
                  {cancelingQueueId === row.id ? "Canceling" : "Cancel"}
                </Button>
              </div>
            </div>
          ))}
        </div>
      ) : (
        <div className="rounded-md border bg-background/40 px-3 py-2 text-xs text-text-3">
          Loading queued runs...
        </div>
      )}
    </div>
  );
}

function ViewTab({
  jobId,
  view,
  activeView,
  icon,
  children,
}: {
  jobId: string;
  view: SecondaryView;
  activeView: SecondaryView | null;
  icon?: ReactNode;
  children: ReactNode;
}) {
  const isActive = activeView === view;
  return (
    <Button
      asChild
      variant={isActive ? "secondary" : "ghost"}
      size="sm"
      className="h-8 shrink-0 px-2.5 text-xs"
    >
      <Link
        to={jobViewPath(jobId, view)}
        aria-current={isActive ? "page" : undefined}
      >
        {icon}
        {children}
      </Link>
    </Button>
  );
}

function RemediationOverview({
  job,
  incidents,
  activeIncidents,
}: {
  job: Job;
  incidents: Incident[];
  activeIncidents: Incident[];
}) {
  const policyFields = remediationPolicyFields(job, incidents, activeIncidents);
  const recentIncidents = [...incidents]
    .sort((a, b) => new Date(b.opened_at).getTime() - new Date(a.opened_at).getTime())
    .slice(0, 5);

  return (
    <div className="grid gap-4 lg:grid-cols-[minmax(0,1.2fr)_minmax(0,0.8fr)]">
      <div className="space-y-3" data-testid="job-incident-history">
        <IncidentRibbon
          incidents={activeIncidents}
          label="Active job incident"
          testId="job-incident-ribbon"
        />
        <Card className="border-graphite/40 bg-midnight/30">
          <CardHeader className="border-b border-graphite/40 pb-3">
            <CardTitle className="flex items-center gap-2 text-sm">
              <FileWarning className="h-4 w-4 text-warning" />
              Incident history
            </CardTitle>
          </CardHeader>
          <CardContent className="p-0">
            {recentIncidents.length > 0 ? (
              <div className="divide-y divide-border/40">
                {recentIncidents.map((incident) => (
                  <Link
                    key={incident.id}
                    to="/incidents/$incidentId"
                    params={{ incidentId: incident.id }}
                    className="grid gap-3 px-4 py-3 text-sm transition-colors hover:bg-graphite/10 md:grid-cols-[140px_120px_minmax(0,1fr)] md:items-center"
                  >
                    <div className="flex flex-wrap items-center gap-2">
                      <StatusBadge status={incident.status} size="sm" />
                      <span className="font-mono text-[10px] text-text-4">{incidentAge(incident)}</span>
                    </div>
                    <Badge variant="outline" className="w-fit border-warning/30 bg-warning/10 text-[10px] text-warning">
                      {formatIncidentClass(incident.class)}
                    </Badge>
                    <div className="min-w-0">
                      <div className="truncate text-xs text-text-2">{incidentSummary(incident)}</div>
                      <div className="mt-0.5 font-mono text-[10px] text-text-4">#{shortId(incident.id)}</div>
                    </div>
                  </Link>
                ))}
              </div>
            ) : (
              <div className="p-4 text-sm text-text-3">No remediation incidents recorded for this job.</div>
            )}
          </CardContent>
        </Card>
      </div>

      <Card className="h-fit border-cyan-glow/25 bg-cyan-glow/5" data-testid="job-remediation-policy">
        <CardHeader className="border-b border-cyan-glow/15 pb-3">
          <CardTitle className="flex items-center gap-2 text-sm">
            <ShieldCheck className="h-4 w-4 text-cyan-glow" />
            Remediation policy
          </CardTitle>
        </CardHeader>
        <CardContent className="space-y-3 p-4">
          {policyFields.map(([key, value]) => (
            <div key={key}>
              <div className="text-[10px] font-semibold uppercase tracking-wide text-text-3">{key}</div>
              <div className="mt-1 break-words font-mono text-xs text-text-1">{value}</div>
            </div>
          ))}
        </CardContent>
      </Card>
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
              <span className="font-medium">{formatUTCTimestamp(run.started_at, run.started_at)}</span>
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
                <div className="font-mono text-xs break-all">{formatCommandForDisplay(atoms?.[task.atom_id]?.command)}</div>
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
  const metadata: Record<string, { status: string; started_at?: string; completed_at?: string; error?: string; rate_limit_retry_after?: string }> = {};
  tasks?.forEach((task) => {
    metadata[task.task_id] = {
      status: task.status,
      started_at: task.started_at,
      completed_at: task.completed_at,
      error: task.error,
      rate_limit_retry_after: task.rate_limit_retry_after,
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

function remediationPolicyFields(
  job: Job,
  incidents: Incident[],
  activeIncidents: Incident[],
): Array<[string, string]> {
  const fields: Array<[string, string]> = [
    ["feature", "enabled"],
    ["incidents", `${activeIncidents.length} active / ${incidents.length} total`],
    ["observed classes", formatObservedClasses(incidents)],
  ];
  Object.entries({ ...(job.labels ?? {}), ...(job.annotations ?? {}) }).forEach(([key, value]) => {
    if (key.toLowerCase().includes("remediation")) {
      fields.push([key, String(value)]);
    }
  });
  return fields;
}

function formatObservedClasses(incidents: Incident[]): string {
  const classes = [...new Set(incidents.map((incident) => formatIncidentClass(incident.class)))];
  return classes.length > 0 ? classes.join(", ") : "none observed";
}

function incidentMatchesTask(incident: Incident, taskId: string, taskName?: string): boolean {
  return incident.task_id === taskId || incident.task_name === taskId || Boolean(taskName && incident.task_name === taskName);
}

function secondaryViewFromPath(pathname: string): SecondaryView | null {
  const lastSegment = pathname.split("/").filter(Boolean).at(-1);
  switch (lastSegment) {
    case "runs":
      return "runs";
    case "tasks":
      return "tasks";
    case "config":
      return "configuration";
    case "yaml":
      return "definition";
    case "backfills":
      return "backfills";
    case "cache":
      return "cache";
    default:
      return null;
  }
}

function jobViewPath(jobId: string, view: SecondaryView | null): string {
  const base = `/jobs/${encodeURIComponent(jobId)}`;
  return view ? `${base}/${secondaryViewSegments[view]}` : base;
}

function secondaryViewTitle(view: SecondaryView): string {
  switch (view) {
    case "runs":
      return "Run History";
    case "tasks":
      return "Task Definitions";
    case "configuration":
      return "Configuration";
    case "definition":
      return "Job Definition (YAML)";
    case "backfills":
      return "Backfills";
    case "cache":
      return "Cache";
  }
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

function formatPriority(priority: number) {
  switch (priority) {
    case 1:
      return "low";
    case 3:
      return "high";
    default:
      return "normal";
  }
}

function formatQueueParams(params?: Record<string, string>) {
  if (!params || Object.keys(params).length === 0) {
    return "no params";
  }
  return Object.keys(params)
    .sort()
    .map((key) => `${key}=${params[key]}`)
    .join(", ");
}

function queuePendingReason(row: RunQueueItem) {
  const explicitReason = row.pending_reason ?? row.wait_reason ?? row.blocked_reason ?? row.reason;
  if (row.blocked === true) {
    return explicitReason ? `Blocked: ${explicitReason}` : "Blocked by scheduler policy";
  }
  if (explicitReason) {
    return explicitReason;
  }
  return `Waiting for a run slot; ${formatPriority(row.priority)} priority at position #${row.position}`;
}
