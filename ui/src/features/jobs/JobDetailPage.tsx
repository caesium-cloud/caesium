import { useEffect, useMemo } from "react";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Pause, Play } from "lucide-react";
import { toast } from "sonner";
import { Duration } from "@/components/duration";
import { RelativeTime } from "@/components/relative-time";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { JobDAG } from "./JobDAG";
import { TaskMetadataPanel } from "./TaskMetadataPanel";
import { api, type Atom, type Job, type TaskRun, type Trigger } from "@/lib/api";
import { events, type CaesiumEvent } from "@/lib/events";
import { formatKeyValueMap, parseJSONConfig } from "@/lib/utils";

export function JobDetailPage() {
  const { jobId } = useParams({ strict: false }) as { jobId: string };
  const navigate = useNavigate();
  const queryClient = useQueryClient();

  const { data: job, isLoading: isLoadingJob } = useQuery({
    queryKey: ["job", jobId],
    queryFn: () => api.getJob(jobId),
    refetchInterval: 30000,
  });

  const { data: runs, isLoading: isLoadingRuns } = useQuery({
    queryKey: ["job", jobId, "runs"],
    queryFn: () => api.getJobRuns(jobId),
    refetchInterval: 30000,
  });

  const { data: dag, isLoading: isLoadingDAG } = useQuery({
    queryKey: ["job", jobId, "dag"],
    queryFn: () => api.getJobDAG(jobId),
    refetchInterval: 30000,
  });

  const { data: tasks, isLoading: isLoadingTasks } = useQuery({
    queryKey: ["job", jobId, "tasks"],
    queryFn: () => api.getJobTasks(jobId),
    refetchInterval: 30000,
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

  useEffect(() => {
    const onRunEvent = (e: CaesiumEvent) => {
      if (e.job_id !== jobId) return;
      queryClient.invalidateQueries({ queryKey: ["job", jobId, "runs"] });
      queryClient.invalidateQueries({ queryKey: ["job", jobId] });
    };

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

    ["run_started", "run_completed", "run_failed", "task_started", "task_succeeded", "task_failed", "task_skipped"].forEach((type) =>
      events.subscribe(type, onRunEvent),
    );
    ["job_paused", "job_unpaused"].forEach((type) => events.subscribe(type, onPauseEvent));

    return () => {
      ["run_started", "run_completed", "run_failed", "task_started", "task_succeeded", "task_failed", "task_skipped"].forEach((type) =>
        events.unsubscribe(type, onRunEvent),
      );
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
    mutationFn: ({ jobId: currentJobId, paused }: { jobId: string; paused: boolean }) =>
      paused ? api.pauseJob(currentJobId) : api.unpauseJob(currentJobId),
    onSuccess: (updated) => {
      queryClient.setQueryData(["job", jobId], updated);
      queryClient.setQueryData(["jobs"], (old: Job[] | undefined) =>
        old?.map((entry) => (entry.id === updated.id ? { ...entry, paused: updated.paused } : entry)),
      );
      toast.success(updated.paused ? "Job paused" : "Job unpaused");
    },
    onError: (err: Error) => {
      toast.error(`Failed to update job state: ${err.message}`);
    },
  });

  const sortedRuns = useMemo(
    () => (runs ? [...runs].sort((a, b) => new Date(b.started_at).getTime() - new Date(a.started_at).getTime()) : []),
    [runs],
  );
  const latestRun = sortedRuns[0];
  const latestRunTasks = useMemo(() => buildTaskRunMap(latestRun?.tasks), [latestRun?.tasks]);
  const taskMetadata = useMemo(() => buildTaskStatusMap(latestRun?.tasks), [latestRun?.tasks]);
  const triggerConfig = useMemo(() => parseJSONConfig(trigger?.configuration), [trigger?.configuration]);

  if (isLoadingJob || isLoadingRuns || isLoadingDAG || isLoadingAtoms || isLoadingTasks || isLoadingTrigger) {
    return <div className="p-8">Loading...</div>;
  }

  if (!job) {
    return <div className="p-8">Job not found</div>;
  }

  return (
    <div className="space-y-6">
      <div className="flex flex-col gap-4 md:flex-row md:items-start md:justify-between">
        <div>
          <div className="flex items-center gap-2">
            <h1 className="text-2xl font-bold tracking-tight">{job.alias}</h1>
            <Badge variant={job.paused ? "outline" : "secondary"} className={job.paused ? "border-amber-500/40 text-amber-300" : ""}>
              {job.paused ? "Paused" : "Active"}
            </Badge>
            {latestRun ? renderRunStatus(latestRun.status) : null}
          </div>
          <div className="mt-2 flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
            <span className="font-mono">{job.id}</span>
            {latestRun ? (
              <>
                <span>•</span>
                <span>Last run <RelativeTime date={latestRun.started_at} /></span>
                <span>•</span>
                <span className="font-mono">
                  <Duration start={latestRun.started_at} end={latestRun.completed_at} />
                </span>
              </>
            ) : null}
          </div>
        </div>
        <div className="flex gap-2">
          <Button onClick={() => triggerMutation.mutate({ jobId: job.id })} disabled={triggerMutation.isPending || job.paused}>
            <Play className="mr-2 h-4 w-4" />
            Trigger
          </Button>
          <Button
            variant="outline"
            onClick={() => pauseMutation.mutate({ jobId: job.id, paused: !job.paused })}
            disabled={pauseMutation.isPending}
          >
            <Pause className="mr-2 h-4 w-4" />
            {job.paused ? "Unpause" : "Pause"}
          </Button>
        </div>
      </div>

      {latestRun?.params && Object.keys(latestRun.params).length > 0 ? (
        <Card>
          <CardHeader className="pb-3">
            <CardTitle className="text-sm">Latest Run Parameters</CardTitle>
          </CardHeader>
          <CardContent className="grid gap-2 md:grid-cols-2">
            {Object.entries(latestRun.params).map(([key, value]) => (
              <div key={key}>
                <div className="text-xs uppercase tracking-wide text-muted-foreground">{key}</div>
                <div className="font-mono text-sm">{value}</div>
              </div>
            ))}
          </CardContent>
        </Card>
      ) : null}

      <Tabs defaultValue="dag">
        <TabsList>
          <TabsTrigger value="dag">DAG</TabsTrigger>
          <TabsTrigger value="runs">Runs</TabsTrigger>
          <TabsTrigger value="tasks">Tasks</TabsTrigger>
          <TabsTrigger value="configuration">Configuration</TabsTrigger>
          <TabsTrigger value="definition">Definition</TabsTrigger>
        </TabsList>

        <TabsContent value="dag" className="mt-4 h-[600px] overflow-hidden rounded-md border">
          {dag && atoms ? <JobDAG dag={dag} atoms={atoms} taskMetadata={taskMetadata} /> : null}
        </TabsContent>

        <TabsContent value="runs" className="mt-4">
          <div className="rounded-md border bg-card divide-y">
            {sortedRuns.length === 0 ? (
              <div className="p-8 text-center text-muted-foreground">No runs found for this job.</div>
            ) : null}
            {sortedRuns.map((run) => (
              <Link
                key={run.id}
                to="/jobs/$jobId/runs/$runId"
                params={{ jobId: job.id, runId: run.id }}
                className="flex items-center justify-between gap-3 p-4 transition-colors hover:bg-muted/50"
              >
                <div className="space-y-1">
                  <div className="flex items-center gap-2">
                    <span className="font-medium">{new Date(run.started_at).toLocaleString()}</span>
                    {run.params && Object.keys(run.params).length > 0 ? (
                      <Badge variant="outline">{Object.keys(run.params).length} params</Badge>
                    ) : null}
                  </div>
                  <div className="text-xs text-muted-foreground">
                    <RelativeTime date={run.started_at} /> • <span className="font-mono">{run.id.substring(0, 8)}</span> •{" "}
                    <span className="font-mono">
                      <Duration start={run.started_at} end={run.completed_at} />
                    </span>
                  </div>
                </div>
                {renderRunStatus(run.status)}
              </Link>
            ))}
          </div>
        </TabsContent>

        <TabsContent value="tasks" className="mt-4 space-y-4">
          {tasks?.map((task) => (
            <div key={task.id} className="grid gap-4 lg:grid-cols-[minmax(0,2fr)_minmax(320px,1fr)]">
              <Card>
                <CardHeader className="pb-3">
                  <CardTitle className="text-sm">{atoms?.[task.atom_id]?.image || `Task ${task.id.substring(0, 8)}`}</CardTitle>
                </CardHeader>
                <CardContent className="grid gap-3 text-sm md:grid-cols-2">
                  <TaskMetadataPanel task={task} runTask={latestRunTasks[task.id]} framed={false} />
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
            </div>
          ))}
        </TabsContent>

        <TabsContent value="configuration" className="mt-4 grid gap-4 md:grid-cols-2">
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
                <div>{job.paused ? "Paused" : "Active"}</div>
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
        </TabsContent>

        <TabsContent value="definition" className="mt-4">
          <pre className="overflow-auto rounded-md border bg-muted p-4 text-xs">{JSON.stringify(job, null, 2)}</pre>
        </TabsContent>
      </Tabs>
    </div>
  );
}

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
