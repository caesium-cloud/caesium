import { useEffect, useMemo, useState } from "react";
import { Link, useParams } from "@tanstack/react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Clock } from "lucide-react";
import { toast } from "sonner";
import { RelativeTime } from "@/components/relative-time";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { api, type Atom, type JobTask, type JobRun, type TaskRun } from "@/lib/api";
import { events, type CaesiumEvent } from "@/lib/events";
import { shortId } from "@/lib/utils";
import { JobDAG } from "./JobDAG";
import { LogViewer } from "./LogViewer";
import { TaskMetadataPanel } from "./TaskMetadataPanel";

export function RunDetailPage() {
  const { jobId, runId } = useParams({ strict: false }) as { jobId: string; runId: string };
  const queryClient = useQueryClient();
  const [selectedTaskId, setSelectedTaskId] = useState<string | null>(null);

  const { data: run, isLoading: isLoadingRun } = useQuery({
    queryKey: ["job", jobId, "runs", runId],
    queryFn: () => api.getJobRun(jobId, runId),
    refetchInterval: 5000,
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

  useEffect(() => {
    if (!runId || !jobId) return;

    const onEvent = (e: CaesiumEvent) => {
      if (e.run_id && e.run_id !== runId) return;

      queryClient.setQueryData(["job", jobId, "runs", runId], (old: JobRun | undefined) => {
        if (!old) return old;

        if (e.type === "run_completed" || e.type === "run_succeeded") {
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

          return { ...old, tasks: updatedTasks };
        }

        return old;
      });
    };

    ["run_started", "run_completed", "run_failed", "task_started", "task_succeeded", "task_failed", "task_skipped"].forEach((type) =>
      events.subscribe(type, onEvent),
    );

    return () => {
      ["run_started", "run_completed", "run_failed", "task_started", "task_succeeded", "task_failed", "task_skipped"].forEach((type) =>
        events.unsubscribe(type, onEvent),
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

  if (isLoadingRun || isLoadingDAG || isLoadingAtoms || isLoadingTasks) {
    return (
      <div className="space-y-4 p-8">
        <Skeleton className="h-8 w-[220px]" />
        <Skeleton className="h-[400px] w-full" />
      </div>
    );
  }

  if (!run) return <div className="p-8">Run not found</div>;

  const selectedTask = selectedTaskId ? taskDefinitions[selectedTaskId] : undefined;
  const selectedRunTask = selectedTaskId ? runTasks[selectedTaskId] : undefined;

  return (
    <div className="space-y-6">
      <div className="flex flex-col gap-4 md:flex-row md:items-start md:justify-between">
        <div>
          <div className="mb-1 flex items-center gap-2">
            <Link to="/jobs/$jobId" params={{ jobId }} className="text-sm font-medium text-blue-400 hover:underline">
              Job Details
            </Link>
            <span className="text-muted-foreground">/</span>
            <h1 className="text-2xl font-bold tracking-tight">Run {shortId(runId)}</h1>
          </div>
          <div className="flex flex-wrap items-center gap-3 text-xs text-muted-foreground">
            <div className="flex items-center gap-1.5">
              <Clock className="h-3.5 w-3.5" />
              <RelativeTime date={run.started_at} />
            </div>
            <span>•</span>
            <span className="font-mono">{runId}</span>
          </div>
        </div>
        {renderRunStatus(run.status)}
      </div>

      {run.params && Object.keys(run.params).length > 0 ? (
        <Card>
          <CardHeader className="pb-3">
            <CardTitle className="text-sm">Run Parameters</CardTitle>
          </CardHeader>
          <CardContent className="grid gap-2 md:grid-cols-2">
            {Object.entries(run.params).map(([key, value]) => (
              <div key={key}>
                <div className="text-xs uppercase tracking-wide text-muted-foreground">{key}</div>
                <div className="font-mono text-sm">{value}</div>
              </div>
            ))}
          </CardContent>
        </Card>
      ) : null}

      <div className="h-[600px] overflow-hidden rounded-md border bg-card">
        {dag && atoms ? (
          <JobDAG
            dag={dag}
            atoms={atoms}
            taskMetadata={taskMetadata}
            onNodeClick={setSelectedTaskId}
            selectedTaskId={selectedTaskId}
          />
        ) : null}
      </div>

      {selectedTaskId ? (
        <div className="space-y-4">
          <TaskMetadataPanel task={selectedTask} runTask={selectedRunTask} />
          <div className="h-[400px]">
            <LogViewer
              jobId={jobId}
              runId={runId}
              taskId={selectedTaskId}
              error={selectedRunTask?.error}
              onClose={() => setSelectedTaskId(null)}
            />
          </div>
        </div>
      ) : null}
    </div>
  );
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
