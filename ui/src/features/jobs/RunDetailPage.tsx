import { useEffect, useMemo, useState } from "react";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronDown, ChevronRight } from "lucide-react";
import { toast } from "sonner";
import { NotFoundState } from "@/components/not-found-state";
import { Duration } from "@/components/duration";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Skeleton } from "@/components/ui/skeleton";
import { IdChip } from "@/components/ui/id-chip";
import { StatusBadge } from "@/components/ui/status-badge";
import { DataAssertionsPanel } from "@/features/datasets/DataAssertionsPanel";
import { HoldSkipReason } from "@/features/datasets/HoldSkipReason";
import { IncidentRibbon } from "@/features/incidents/IncidentRibbon";
import { INCIDENT_EVENT_TYPES } from "@/features/incidents/incident-utils";
import { useDagHeight } from "@/hooks/useDagHeight";
import { api, type Atom, type Incident, type JobRun, type JobTask, type TaskRun } from "@/lib/api";
import { usePrincipal } from "@/lib/auth";
import { events, type CaesiumEvent } from "@/lib/events";
import { formatUTCTime, formatUTCTimestamp } from "@/lib/utils";
import { getRunCacheStats, isTerminalRunStatus, mergeTerminalRunUpdate } from "./cache-utils";
import { rerunParams } from "./rerun-params";
import { CallbackRunsSection } from "./CallbackRunsSection";
import { JobDAG } from "./JobDAG";
import { ReceiptPanel } from "./ReceiptPanel";
import { ReplayDialog } from "./ReplayDialog";
import { RunCacheSummary } from "./RunCacheSummary";
import { RunProposalSummary } from "./RunProposalSummary";
import { RunTimeline } from "./RunTimeline";
import { TaskDetailPanel } from "./TaskDetailPanel";

const COMPARE_RUN_PICKER_LIMIT = 50;

export function RunDetailPage() {
  const { jobId, runId } = useParams({ strict: false }) as { jobId: string; runId: string };
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [selectedTaskId, setSelectedTaskId] = useState<string | null>(null);
  const [streamHealthy, setStreamHealthy] = useState(events.isHealthy());
  const [timelineOpen, setTimelineOpen] = useState(true);
  const [reproducibilityOpen, setReproducibilityOpen] = useState(false);
  const [replayDialogOpen, setReplayDialogOpen] = useState(false);
  const principal = usePrincipal();

  const { data: job } = useQuery({ queryKey: ["job", jobId], queryFn: () => api.getJob(jobId) });

  const { data: run, isLoading: isLoadingRun } = useQuery({
    queryKey: ["job", jobId, "runs", runId],
    queryFn: () => api.getJobRun(jobId, runId),
    refetchInterval: (query) =>
      !streamHealthy || isTerminalRunStatus(query.state.data?.status) ? 5000 : false,
  });

  const receiptQuery = useQuery({
    queryKey: ["job", jobId, "runs", runId, "receipt"],
    queryFn: () => api.getReceipt(jobId, runId),
    enabled: isTerminalRunStatus(run?.status),
    staleTime: 15_000,
    retry: false,
  });

  const { data: dag, isLoading: isLoadingDAG } = useQuery({
    queryKey: ["job", jobId, "dag"],
    queryFn: () => api.getJobDAG(jobId),
  });

  const { data: tasks, isLoading: isLoadingTasks } = useQuery({
    queryKey: ["job", jobId, "tasks"],
    queryFn: () => api.getJobTasks(jobId),
  });

  const { data: jobRuns, isLoading: isLoadingJobRuns } = useQuery({
    queryKey: ["job", jobId, "runs", { limit: COMPARE_RUN_PICKER_LIMIT }],
    queryFn: () => api.getJobRuns(jobId, { limit: COMPARE_RUN_PICKER_LIMIT }),
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

  const { data: features } = useQuery({
    queryKey: ["system-features"],
    queryFn: api.getSystemFeatures,
    staleTime: 60_000,
  });

  const { data: runIncidentList } = useQuery({
    queryKey: ["incidents", "job", jobId],
    queryFn: () => api.getIncidents({ job_id: jobId, limit: 200 }),
    enabled: features?.agent_remediation_enabled === true,
    refetchInterval: 30_000,
  });

  const isLoading = isLoadingRun || isLoadingDAG || isLoadingAtoms || isLoadingTasks;
  const [dagContainerRef, dagHeight] = useDagHeight(isLoading);

  const handleTaskSelect = (taskId: string) => setSelectedTaskId((prev) => (prev === taskId ? null : taskId));

  useEffect(() => {
    if (!runId || !jobId) return;

    const onConnection = (healthy: boolean) => setStreamHealthy(healthy);
    const onEvent = (e: CaesiumEvent) => {
      if (e.run_id && e.run_id !== runId) return;

      queryClient.setQueryData(["job", jobId, "runs", runId], (old: JobRun | undefined) => {
        if (!old) return old;

        if (e.type === "run_completed" || e.type === "run_succeeded" || e.type === "run_terminal") {
          const finalRun = e.payload as JobRun;
          if (finalRun?.tasks) return mergeTerminalRunUpdate(old, finalRun);
          toast.success("Run completed");
          return { ...old, status: "succeeded" };
        }

        if (e.type === "run_failed") {
          toast.error("Run failed");
          return { ...old, status: "failed" };
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
            queryClient.invalidateQueries({ queryKey: ["job", jobId, "runs", runId] });
            updatedTasks.push({
              id: taskID,
              job_run_id: runId,
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
    };

    events.subscribeConnection(onConnection);
    ["run_started", "run_completed", "run_failed", "run_terminal", "task_started", "task_succeeded", "task_failed", "task_skipped", "task_retrying", "task_cached"].forEach(
      (type) => events.subscribe(type, onEvent),
    );

    return () => {
      events.unsubscribeConnection(onConnection);
      ["run_started", "run_completed", "run_failed", "run_terminal", "task_started", "task_succeeded", "task_failed", "task_skipped", "task_retrying", "task_cached"].forEach(
        (type) => events.unsubscribe(type, onEvent),
      );
    };
  }, [jobId, runId, queryClient]);

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

  const taskMetadata = useMemo(() => {
    const metadata: Record<string, { status: string; started_at?: string; completed_at?: string; error?: string; rate_limit_retry_after?: string }> = {};
    run?.tasks?.forEach((task) => {
      metadata[task.task_id] = {
        status: task.status,
        started_at: task.started_at,
        completed_at: task.completed_at,
        error: task.error,
        rate_limit_retry_after: task.rate_limit_retry_after,
      };
    });
    return metadata;
  }, [run]);

  const taskDefinitions = useMemo(() => {
    const map: Record<string, JobTask> = {};
    tasks?.forEach((task) => {
      map[task.id] = task;
    });
    return map;
  }, [tasks]);

  const runTasks = useMemo(() => {
    const map: Record<string, TaskRun> = {};
    run?.tasks?.forEach((task) => {
      map[task.task_id] = task;
    });
    return map;
  }, [run?.tasks]);

  const compareRuns = useMemo(() => {
    return (jobRuns?.runs ?? [])
      .filter((candidate) => candidate.id !== runId)
      .sort((a, b) => {
        const aTime = runSortTimestamp(a);
        const bTime = runSortTimestamp(b);
        if (aTime !== bTime) return bTime - aTime;
        return b.id.localeCompare(a.id);
      })
      .slice(0, COMPARE_RUN_PICKER_LIMIT);
  }, [jobRuns, runId]);

  const runIncidents = useMemo(
    () =>
      (runIncidentList?.incidents ?? []).filter(
        (incident) => incident.run_id === runId || incident.remediation_target_run_id === runId,
      ),
    [runIncidentList?.incidents, runId],
  );

  const triggerMutation = useMutation({
    mutationFn: ({ jobId: triggeredJobId, params }: { jobId: string; params?: Record<string, string> }) =>
      api.triggerJob(triggeredJobId, params ? { params } : undefined),
    onSuccess: (newRun, { jobId: triggeredJobId }) => {
      toast.success("Job triggered");
      queryClient.invalidateQueries({ queryKey: ["job", triggeredJobId, "runs"] });
      if (newRun?.id) {
        navigate({ to: "/jobs/$jobId/runs/$runId", params: { jobId: triggeredJobId, runId: newRun.id } });
      }
    },
    onError: (err: Error) => toast.error(`Failed to trigger: ${err.message}`),
  });

  useEffect(() => {
    const shortcuts: Record<string, string> = { c: '[data-testid="run-compare-trigger"]', a: '[data-testid="all-runs-link"]', p: '[data-testid="run-replay-trigger"]', r: '[data-testid="run-rerun-trigger"]' };
    const onKey = (event: KeyboardEvent) => {
      const target = event.target as HTMLElement;
      const rerun = event.altKey && event.code === "KeyR";
      if (event.defaultPrevented || event.repeat || event.ctrlKey || event.metaKey || (event.altKey && !rerun) || target.closest('input, textarea, select, [contenteditable="true"], [role="dialog"], [role="menu"]')) return;
      const selector = rerun ? shortcuts.r : event.key === "r" ? undefined : shortcuts[event.key];
      if (selector && !document.querySelector('[role="dialog"]')) {
        const control = document.querySelector<HTMLElement>(selector);
        if (control && !control.hasAttribute('disabled')) { event.preventDefault(); control.click(); }
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);

  if (isLoading) {
    return (
      <div className="space-y-4 p-8">
        <Skeleton className="h-8 w-[220px]" />
        <Skeleton className="h-[400px] w-full" />
      </div>
    );
  }

  if (!run) {
    return (
      <NotFoundState
        title="Run not found"
        subtitle="The requested run could not be found or is no longer available."
      />
    );
  }

  const selectedTask = selectedTaskId ? taskDefinitions[selectedTaskId] : undefined;
  const selectedRunTask = selectedTaskId ? runTasks[selectedTaskId] : undefined;
  const selectedTaskIncidents = selectedTaskId
    ? runIncidents.filter((incident) => incidentMatchesTask(incident, selectedTaskId, selectedTask?.name))
    : [];
  const isLive = run.status === "running";
  const canLaunchReplay = principal.role === null || principal.canRunner;
  const replayGateReason = principal.role !== null && !principal.canRunner ? "Requires runner role" : undefined;
  const compareDisabledReason = isLoadingJobRuns
    ? "Loading runs to compare"
    : compareRuns.length === 0
      ? "No other runs to compare"
      : undefined;
  const callbackRuns = run.callbacks ?? [];

  return (
    <div className="space-y-5">
      {/* Header */}
      <div className="flex flex-col gap-3 md:flex-row md:items-start md:justify-between">
        <div>
          <div className="flex flex-wrap items-center gap-x-7 gap-y-2">
            <h1 data-testid="run-heading" className="text-2xl font-bold text-text-1">{job?.alias || run.job_alias || "pipeline"}</h1>
            <span className="text-xs text-text-3">run started {formatUTCTime(run.started_at)}</span>
            <StatusBadge status={run.status} />
            <IdChip value={runId} label="run id" />
          </div>
          <div className="mt-3 flex flex-wrap items-center gap-x-7 gap-y-2 text-xs text-text-3">
            <Link to="/jobs/$jobId/runs" params={{ jobId }} className="hover:text-text-1">← all runs</Link>
            <span>elapsed <Duration start={run.started_at} end={run.completed_at} /></span>
            <span>trigger {run.trigger_type || "manual"}</span>
            <span>tasks {(run.tasks ?? []).filter(task => ["succeeded", "cached", "skipped"].includes(task.status)).length}/{run.tasks?.length ?? 0} done</span>
            <span>receipt {isLive || receiptQuery.isPending ? "pending" : receiptQuery.data ? "available" : "unavailable"}</span>
          </div>
        </div>

        {/* Action cluster */}
        <div className="flex flex-wrap items-center gap-2">
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button
                variant="outline"
                size="sm"
                className="h-8 text-xs"
                disabled={isLoadingJobRuns || compareRuns.length === 0}
                data-testid="run-compare-trigger"
                title={compareDisabledReason}
              >
                Compare to run… <kbd aria-hidden="true" className="text-[11px] text-text-3">c</kbd>
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end" className="w-72">
              {compareRuns.map((candidate) => (
                <DropdownMenuItem
                  key={candidate.id}
                  data-testid="run-compare-option"
                  className="flex items-center justify-between gap-3"
                  onSelect={() =>
                    navigate({
                      to: "/jobs/$jobId/runs/$runId/diff",
                      params: { jobId, runId },
                      search: { to: candidate.id },
                    })
                  }
                >
                  <div className="min-w-0">
                    <div className="truncate text-xs">run started {formatRunTimestamp(candidate)}</div>
                    <div className="truncate text-[11px] text-text-3">
                      <IdChip value={candidate.id} label="run id" />
                    </div>
                  </div>
                  <StatusBadge status={candidate.status} size="sm" />
                </DropdownMenuItem>
              ))}
            </DropdownMenuContent>
          </DropdownMenu>
          <Button variant="outline" size="sm" className="h-8 text-xs" asChild>
            <Link
              to="/jobs/$jobId/runs"
              params={{ jobId }}
              data-testid="all-runs-link"
            >
              All runs <kbd aria-hidden="true" className="text-[11px] text-text-3">a</kbd>
            </Link>
          </Button>
          {canLaunchReplay ? (
            <Button
              variant="outline"
              size="sm"
              className="h-8 text-xs"
              onClick={() => setReplayDialogOpen(true)}
              data-testid="run-replay-trigger"
            >
              Replay… <kbd aria-hidden="true" className="text-[11px] text-text-3">p</kbd>
            </Button>
          ) : (
            <span className="inline-flex" title={replayGateReason}>
              <Button
                variant="outline"
                size="sm"
                className="h-8 text-xs"
                disabled
                title={replayGateReason}
                aria-describedby="run-replay-gate-reason"
                data-testid="run-replay-trigger"
              >
                  Replay… <kbd aria-hidden="true" className="text-[11px] text-text-3">p</kbd>
              </Button>
              <span
                id="run-replay-gate-reason"
                className="sr-only"
                data-testid="run-replay-gate-reason"
              >
                {replayGateReason}
              </span>
            </span>
          )}
          <Button
            variant="outline"
            size="sm"
            className="h-8 text-xs"
            onClick={() => {
              const params = rerunParams(run.params);
              triggerMutation.mutate({ jobId, params });
            }}
            data-testid="run-rerun-trigger"
            disabled={triggerMutation.isPending}
          >
            {triggerMutation.isPending ? "Re-running…" : "Re-run"} <kbd aria-hidden="true" className="text-[11px] text-text-3">Alt R</kbd>
          </Button>
          {isLive && (
            <Button
              variant="outline"
              size="sm"
              className="h-8 text-xs border-danger/30 text-danger hover:bg-danger/10"
              disabled
              title="Cancel not yet implemented"
            >

              Cancel
            </Button>
          )}
        </div>
      </div>

      {replayDialogOpen ? (
        <ReplayDialog
          jobId={jobId}
          baselineRunId={runId}
          open={replayDialogOpen}
          onOpenChange={setReplayDialogOpen}
        />
      ) : null}

      <HoldSkipReason reason={run.tasks?.find((task) => task.error?.startsWith("dataset_hold:"))?.error ?? run.skip_reason} />
      {run.tasks?.filter((task) => task.data_violations?.length || task.schema_violations?.length).map((task) => <DataAssertionsPanel key={task.id} task={task} />)}
      {/* Cache summary */}
      <div className="flex items-center gap-4">
        <RunCacheSummary run={run} />
      </div>

      <IncidentRibbon
        incidents={runIncidents}
        label="Run incident"
        testId="run-incident-ribbon"
      />

      {/* Gantt timeline */}
      <div data-testid="run-execution-timeline-section" className="rounded-lg border border-border bg-midnight overflow-hidden">
        <button
          type="button"
          className="flex w-full items-center gap-2 px-4 py-2.5 text-left border-b border-border/50 hover:bg-obsidian/30 transition-colors"
          onClick={() => setTimelineOpen((o) => !o)}
          aria-expanded={timelineOpen}
          aria-controls="run-execution-timeline-body"
        >
          {timelineOpen ? (
            <ChevronDown className="h-3.5 w-3.5 text-text-3" />
          ) : (
            <ChevronRight className="h-3.5 w-3.5 text-text-3" />
          )}
          <span className="text-[11px] font-bold lowercase text-text-3">
            Execution timeline
          </span>
          {isLive && (
            <StatusBadge status="running" label="live" size="sm" />
          )}
          <span className="ml-auto text-[11px] text-text-3">
            {run.tasks?.length ?? 0} tasks
          </span>
        </button>
        {timelineOpen && (
          <div id="run-execution-timeline-body" className="p-4">
            {run.tasks && run.tasks.length > 0 ? (
              <RunTimeline tasks={run.tasks} taskDefinitions={taskDefinitions} runStartedAt={run.started_at} runStatus={run.status} />
            ) : (
              <div className="text-[12px] text-text-3 py-4 text-center">
                No task execution data yet.
              </div>
            )}
          </div>
        )}
      </div>

      {/* Interactive DAG with task selection */}
      <div data-testid="run-interactive-dag-section" className="overflow-hidden rounded-md border border-border/50 bg-card">
        <div className="flex items-center gap-2 border-b border-border/50 px-4 py-2.5">
          <span className="text-[11px] font-bold lowercase text-text-3">
            graph
          </span>
          <span className="ml-auto text-[11px] text-text-3">
            {dag?.nodes?.length ?? 0} nodes
          </span>
        </div>
        <div
          ref={dagContainerRef}
          data-testid="run-dag-canvas-viewport"
          className="relative overflow-hidden bg-card"
          style={{ height: dagHeight ? `${dagHeight}px` : "600px" }}
        >
          {dag && atoms ? (
            <JobDAG
              dag={dag}
              atoms={atoms}
              taskDefinitions={taskDefinitions}
              runStartedAt={run.started_at}
              taskMetadata={taskMetadata}
              taskRunData={runTasks}
              onNodeClick={handleTaskSelect}
              selectedTaskId={selectedTaskId}
            />
          ) : null}

          {selectedTaskId ? (
            <TaskDetailPanel
              key={selectedTaskId}
              taskId={selectedTaskId}
              task={selectedTask}
              runTask={selectedRunTask}
              taskType={dag?.nodes?.find((n) => n.id === selectedTaskId)?.type}
              jobId={jobId}
              runId={runId}
              incidents={selectedTaskIncidents}
              onClose={() => setSelectedTaskId(null)}
            />
          ) : null}
        </div>
      </div>

      <RunProposalSummary tasks={run.tasks} taskDefinitions={taskDefinitions} onSelectTask={handleTaskSelect} />

      {callbackRuns.length > 0 ? <CallbackRunsSection callbacks={callbackRuns} /> : null}

      {/* Run parameters */}
      {run.params && Object.keys(run.params).length > 0 ? (
        <Card>
          <CardHeader className="pb-3">
            <CardTitle className="text-sm">Run Parameters</CardTitle>
          </CardHeader>
          <CardContent className="grid gap-2 md:grid-cols-2">
            {Object.entries(run.params).map(([key, value]) => (
              <div key={key}>
                <div className="text-[11px] lowercase text-text-3">{key}</div>
                <div className="text-sm text-text-1">{value}</div>
              </div>
            ))}
          </CardContent>
        </Card>
      ) : null}

      <div data-testid="run-reproducibility-section" className="overflow-hidden rounded-md border border-border/50 bg-card">
        <button
          type="button"
          className="flex w-full items-center gap-2 px-4 py-2.5 text-left hover:bg-obsidian/30 transition-colors"
          onClick={() => setReproducibilityOpen((open) => !open)}
          aria-expanded={reproducibilityOpen}
          aria-controls="run-reproducibility-body"
          data-testid="run-reproducibility-toggle"
        >
          {reproducibilityOpen ? (
            <ChevronDown className="h-3.5 w-3.5 text-text-3" />
          ) : (
            <ChevronRight className="h-3.5 w-3.5 text-text-3" />
          )}

          <span className="text-[11px] font-bold lowercase text-text-3">
            Reproducibility
          </span>
          <span className="ml-auto text-[11px] text-text-3">Receipt</span>
        </button>
        {reproducibilityOpen ? (
          <div id="run-reproducibility-body" className="border-t border-border/50 p-4">
            <ReceiptPanel jobId={jobId} runId={runId} />
          </div>
        ) : null}
      </div>
    </div>
  );
}

function incidentMatchesTask(incident: Incident, taskId: string, taskName?: string): boolean {
  return incident.task_id === taskId || incident.task_name === taskId || Boolean(taskName && incident.task_name === taskName);
}

function runSortTimestamp(run: JobRun): number {
  const parsed = parseTimestamp(run.started_at) ?? parseTimestamp(run.created_at);
  return parsed ?? Number.NEGATIVE_INFINITY;
}

function formatRunTimestamp(run: JobRun): string {
  const parsed = parseTimestamp(run.started_at) ?? parseTimestamp(run.created_at);
  return parsed !== undefined ? formatUTCTimestamp(parsed, "Unknown start time") : "Unknown start time";
}

function parseTimestamp(value: string | undefined): number | undefined {
  if (!value) return undefined;
  const timestamp = new Date(value).getTime();
  return Number.isFinite(timestamp) ? timestamp : undefined;
}
