import { useEffect, useMemo, useState } from "react";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowLeft, ChevronDown, ChevronRight, RotateCcw, Square, History } from "lucide-react";
import { toast } from "sonner";
import { RelativeTime } from "@/components/relative-time";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { StatusBadge } from "@/components/ui/status-badge";
import { useDagHeight } from "@/hooks/useDagHeight";
import { api, type Atom, type JobRun, type JobTask, type TaskRun } from "@/lib/api";
import { events, type CaesiumEvent } from "@/lib/events";
import { shortId } from "@/lib/utils";
import { getRunCacheStats } from "./cache-utils";
import { JobDAG } from "./JobDAG";
import { RunCacheSummary } from "./RunCacheSummary";
import { RunLogViewer } from "./RunLogViewer";
import { RunTimeline } from "./RunTimeline";
import { TaskDetailPanel } from "./TaskDetailPanel";

export function RunDetailPage() {
  const { jobId, runId } = useParams({ strict: false }) as { jobId: string; runId: string };
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [selectedTaskId, setSelectedTaskId] = useState<string | null>(null);
  const [streamHealthy, setStreamHealthy] = useState(events.isHealthy());
  const [timelineOpen, setTimelineOpen] = useState(true);

  const { data: run, isLoading: isLoadingRun } = useQuery({
    queryKey: ["job", jobId, "runs", runId],
    queryFn: () => api.getJobRun(jobId, runId),
    refetchInterval: streamHealthy ? false : 5000,
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

  const isLoading = isLoadingRun || isLoadingDAG || isLoadingAtoms || isLoadingTasks;
  const [dagContainerRef, dagHeight] = useDagHeight(isLoading);

  useEffect(() => {
    if (!runId || !jobId) return;

    const onConnection = (healthy: boolean) => setStreamHealthy(healthy);
    const onEvent = (e: CaesiumEvent) => {
      if (e.run_id && e.run_id !== runId) return;

      queryClient.setQueryData(["job", jobId, "runs", runId], (old: JobRun | undefined) => {
        if (!old) return old;

        if (e.type === "run_completed" || e.type === "run_succeeded" || e.type === "run_terminal") {
          const finalRun = e.payload as JobRun;
          if (finalRun?.tasks) return finalRun;
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

  const taskMetadata = useMemo(() => {
    const metadata: Record<string, { status: string; started_at?: string; completed_at?: string; error?: string }> = {};
    run?.tasks?.forEach((task) => {
      metadata[task.task_id] = {
        status: task.status,
        started_at: task.started_at,
        completed_at: task.completed_at,
        error: task.error,
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

  const triggerMutation = useMutation({
    mutationFn: () => api.triggerJob(jobId),
    onSuccess: (newRun) => {
      toast.success("Job triggered");
      queryClient.invalidateQueries({ queryKey: ["job", jobId, "runs"] });
      if (newRun?.id) {
        navigate({ to: "/jobs/$jobId/runs/$runId", params: { jobId, runId: newRun.id } });
      }
    },
    onError: (err: Error) => toast.error(`Failed to trigger: ${err.message}`),
  });

  if (isLoading) {
    return (
      <div className="space-y-4 p-8">
        <Skeleton className="h-8 w-[220px]" />
        <Skeleton className="h-[400px] w-full" />
      </div>
    );
  }

  if (!run) return <div className="p-8 text-text-3">Run not found</div>;

  const selectedTask = selectedTaskId ? taskDefinitions[selectedTaskId] : undefined;
  const selectedRunTask = selectedTaskId ? runTasks[selectedTaskId] : undefined;
  const isLive = run.status === "running";

  return (
    <div className="space-y-5">
      {/* Header */}
      <div className="flex flex-col gap-3 md:flex-row md:items-start md:justify-between">
        <div>
          <div className="flex items-center gap-2 mb-1">
            <Link
              to="/jobs/$jobId"
              params={{ jobId }}
              className="flex items-center gap-1 text-[11px] text-text-3 hover:text-text-2 transition-colors"
            >
              <ArrowLeft className="h-3 w-3" />
              Job
            </Link>
            <span className="text-text-4">/</span>
            <span className="text-[11px] text-text-3">Run</span>
          </div>
          <div className="text-[10px] font-semibold uppercase tracking-[0.18em] text-text-3 mb-1">
            Run detail
          </div>
          <div className="flex items-center gap-2.5">
            <h1 className="text-xl font-semibold text-text-1 font-mono tracking-tight">
              Run {shortId(runId)}
            </h1>
            <StatusBadge status={run.status} size="sm" />
          </div>
          <div className="mt-1 flex items-center gap-2 text-xs text-text-3">
            <RelativeTime date={run.started_at} />
            <span className="text-text-4">·</span>
            <span className="font-mono text-text-4 text-[10px]">{runId}</span>
          </div>
        </div>

        {/* Action cluster */}
        <div className="flex items-center gap-2">
          <Button
            variant="ghost"
            size="sm"
            className="h-8 text-xs"
            onClick={() => window.history.back()}
          >
            <History className="mr-1.5 h-3.5 w-3.5" />
            All runs
          </Button>
          <Button
            variant="outline"
            size="sm"
            className="h-8 text-xs"
            onClick={() => triggerMutation.mutate()}
            disabled={triggerMutation.isPending}
          >
            <RotateCcw className="mr-1.5 h-3.5 w-3.5" />
            Re-run
          </Button>
          {isLive && (
            <Button
              variant="outline"
              size="sm"
              className="h-8 text-xs border-danger/30 text-danger hover:bg-danger/10"
              disabled
              title="Cancel not yet implemented"
            >
              <Square className="mr-1.5 h-3.5 w-3.5" />
              Cancel
            </Button>
          )}
        </div>
      </div>

      {/* Cache summary */}
      <div className="flex items-center gap-4">
        <RunCacheSummary run={run} />
      </div>

      {/* Gantt timeline */}
      <div className="rounded-md border border-border/50 bg-card overflow-hidden">
        <button
          type="button"
          className="flex w-full items-center gap-2 px-4 py-2.5 text-left border-b border-border/50 hover:bg-obsidian/30 transition-colors"
          onClick={() => setTimelineOpen((o) => !o)}
        >
          {timelineOpen ? (
            <ChevronDown className="h-3.5 w-3.5 text-text-3" />
          ) : (
            <ChevronRight className="h-3.5 w-3.5 text-text-3" />
          )}
          <span className="text-[10px] font-semibold uppercase tracking-[0.1em] text-text-3">
            Execution timeline
          </span>
          {isLive && (
            <span className="ml-2 text-[10px] text-cyan-glow/70 font-medium animate-pulse">Live</span>
          )}
          <span className="ml-auto text-[10px] text-text-4">
            {run.tasks?.length ?? 0} tasks
          </span>
        </button>
        {timelineOpen && (
          <div className="p-4">
            {run.tasks && run.tasks.length > 0 ? (
              <RunTimeline tasks={run.tasks} runStartedAt={run.started_at} />
            ) : (
              <div className="text-[12px] text-text-4 py-4 text-center">
                No task execution data yet.
              </div>
            )}
          </div>
        )}
      </div>

      {/* Log viewer for selected task */}
      {selectedTaskId && (
        <div className="rounded-md border border-border/50 bg-card overflow-hidden">
          <div className="flex items-center justify-between px-4 py-2.5 border-b border-border/50">
            <span className="text-[10px] font-semibold uppercase tracking-[0.1em] text-text-3">
              Logs — {atoms?.[runTasks[selectedTaskId]?.atom_id]?.image?.split("/").pop()?.split(":")[0] ?? shortId(selectedTaskId)}
            </span>
            <button
              type="button"
              className="text-[10px] text-text-4 hover:text-text-2 transition-colors"
              onClick={() => setSelectedTaskId(null)}
            >
              Close
            </button>
          </div>
          <div className="h-80">
            <RunLogViewer
              jobId={jobId}
              runId={runId}
              taskId={selectedTaskId}
              isRunning={isLive}
              taskStatus={runTasks[selectedTaskId]?.status}
              taskError={runTasks[selectedTaskId]?.error}
            />
          </div>
        </div>
      )}

      {/* Run parameters */}
      {run.params && Object.keys(run.params).length > 0 ? (
        <Card>
          <CardHeader className="pb-3">
            <CardTitle className="text-sm">Run Parameters</CardTitle>
          </CardHeader>
          <CardContent className="grid gap-2 md:grid-cols-2">
            {Object.entries(run.params).map(([key, value]) => (
              <div key={key}>
                <div className="text-[10px] uppercase tracking-wide text-text-3">{key}</div>
                <div className="font-mono text-sm text-text-1">{value}</div>
              </div>
            ))}
          </CardContent>
        </Card>
      ) : null}

      {/* DAG with task selection */}
      <div
        ref={dagContainerRef}
        className="relative overflow-hidden rounded-md border border-border/50 bg-card"
        style={{ height: dagHeight ? `${dagHeight}px` : "600px" }}
      >
        {dag && atoms ? (
          <JobDAG
            dag={dag}
            atoms={atoms}
            taskDefinitions={taskDefinitions}
            taskMetadata={taskMetadata}
            taskRunData={runTasks}
            onNodeClick={(id) => setSelectedTaskId((prev) => (prev === id ? null : id))}
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
            onClose={() => setSelectedTaskId(null)}
          />
        ) : null}
      </div>
    </div>
  );
}
