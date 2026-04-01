import { useEffect, useState } from "react";
import { Link, useNavigate } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Pause, Play } from "lucide-react";
import { toast } from "sonner";
import { Duration } from "@/components/duration";
import { RelativeTime } from "@/components/relative-time";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { api, type Job, type JobRun } from "@/lib/api";
import { events, type CaesiumEvent } from "@/lib/events";
import { cn, shortId } from "@/lib/utils";
import { formatCacheShare, getRunCacheStats } from "./cache-utils";

export function JobsPage() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [streamHealthy, setStreamHealthy] = useState(events.isHealthy());
  const { data: jobs, isLoading, error } = useQuery({
    queryKey: ["jobs"],
    queryFn: api.getJobs,
    refetchInterval: streamHealthy ? false : 15000,
  });

  useEffect(() => {
    const onConnection = (healthy: boolean) => setStreamHealthy(healthy);
    const onRunEvent = (e: CaesiumEvent) => {
      const payload = e.payload as JobRun | undefined;
      const run = payload && payload.id ? payload : undefined;
      if (!run?.job_id && !e.job_id) {
        queryClient.invalidateQueries({ queryKey: ["jobs"] });
        return;
      }

      const jobID = run?.job_id || e.job_id!;
      queryClient.setQueryData(["jobs"], (old: Job[] | undefined) =>
        old?.map((job) => (job.id === jobID ? { ...job, latest_run: run ? { ...job.latest_run, ...run } : job.latest_run } : job)),
      );
      if (run) {
        queryClient.setQueryData(["job", jobID], (old: Job | undefined) =>
          old ? { ...old, latest_run: { ...old.latest_run, ...run } } : old,
        );
      }
    };

    const onPauseEvent = (e: CaesiumEvent) => {
      const payload = e.payload as Job | undefined;
      if (!payload?.id) {
        queryClient.invalidateQueries({ queryKey: ["jobs"] });
        return;
      }

      queryClient.setQueryData(["jobs"], (old: Job[] | undefined) =>
        old?.map((job) => (job.id === payload.id ? { ...job, paused: payload.paused } : job)),
      );
      queryClient.setQueryData(["job", payload.id], (old: Job | undefined) =>
        old ? { ...old, paused: payload.paused } : old,
      );
    };

    const onTaskCached = (e: CaesiumEvent) => {
      if (!e.job_id || !e.run_id) {
        queryClient.invalidateQueries({ queryKey: ["jobs"] });
        return;
      }

      queryClient.setQueryData(["jobs"], (old: Job[] | undefined) =>
        old?.map((job) => {
          const latestRun = job.latest_run;
          if (job.id !== e.job_id || !latestRun || latestRun.id !== e.run_id) {
            return job;
          }
          return {
            ...job,
            latest_run: {
              ...latestRun,
              cache_hits: (latestRun.cache_hits ?? 0) + 1,
            },
          };
        }),
      );
    };

    events.subscribeConnection(onConnection);
    ["run_started", "run_completed", "run_failed", "run_terminal"].forEach((type) => events.subscribe(type, onRunEvent));
    ["job_paused", "job_unpaused"].forEach((type) => events.subscribe(type, onPauseEvent));
    events.subscribe("task_cached", onTaskCached);

    return () => {
      events.unsubscribeConnection(onConnection);
      ["run_started", "run_completed", "run_failed", "run_terminal"].forEach((type) => events.unsubscribe(type, onRunEvent));
      ["job_paused", "job_unpaused"].forEach((type) => events.unsubscribe(type, onPauseEvent));
      events.unsubscribe("task_cached", onTaskCached);
    };
  }, [queryClient]);

  const triggerMutation = useMutation({
    mutationFn: ({ jobId }: { jobId: string }) => api.triggerJob(jobId),
    onSuccess: (run) => {
      queryClient.invalidateQueries({ queryKey: ["jobs"] });
      toast.success("Job triggered successfully");
      if (run?.job_id && run?.id) {
        navigate({ to: "/jobs/$jobId/runs/$runId", params: { jobId: run.job_id, runId: run.id } });
      }
    },
    onError: (err: Error) => {
      toast.error(`Failed to trigger job: ${err.message}`);
    },
  });

  const pauseMutation = useMutation({
    mutationFn: ({ jobId, paused }: { jobId: string; paused: boolean; hasActiveRun: boolean }) =>
      paused ? api.pauseJob(jobId) : api.unpauseJob(jobId),
    onSuccess: (job, variables) => {
      queryClient.setQueryData(["jobs"], (old: Job[] | undefined) =>
        old?.map((entry) => (entry.id === job.id ? { ...entry, paused: job.paused } : entry)),
      );
      queryClient.setQueryData(["job", job.id], (old: Job | undefined) => (old ? { ...old, paused: job.paused } : job));
      if (job.paused) {
        toast.success(variables.hasActiveRun ? "Job paused. The active run will finish, but new runs are blocked." : "Job paused. New runs are blocked.");
        return;
      }
      toast.success("Job unpaused");
    },
    onError: (err: Error) => {
      toast.error(`Failed to update job state: ${err.message}`);
    },
  });

  if (isLoading) return <div className="p-8 text-center text-muted-foreground">Loading jobs...</div>;
  if (error) return <div className="p-8 text-center text-destructive">Error loading jobs: {error.message}</div>;

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">Jobs</h1>
          <p className="text-sm text-muted-foreground">Manage pause state, trigger runs, and inspect recent execution metadata.</p>
        </div>
      </div>
      <div className="rounded-md border bg-card">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Alias</TableHead>
              <TableHead>State</TableHead>
              <TableHead>Latest Run</TableHead>
              <TableHead>Duration</TableHead>
              <TableHead className="text-right">Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {jobs?.length === 0 && (
              <TableRow>
                <TableCell colSpan={5} className="h-24 text-center">
                  No jobs found.
                </TableCell>
              </TableRow>
            )}
            {jobs?.map((job) => {
              const latestRun = job.latest_run;
              const isRunning = latestRun?.status === "running";
              const isPaused = job.paused;
              return (
                <TableRow
                  key={job.id}
                  className={cn(
                    isRunning && "bg-blue-500/5 animate-in fade-in duration-1000",
                    isPaused && "bg-amber-500/5",
                  )}
                >
                  <TableCell className="font-medium">
                    <div className="flex items-center gap-2">
                      <Link to="/jobs/$jobId" params={{ jobId: job.id }} className="hover:underline text-primary">
                        {job.alias}
                      </Link>
                      {isPaused ? (
                        <Badge variant="outline" className="border-amber-500/40 text-amber-300">
                          paused
                        </Badge>
                      ) : null}
                    </div>
                    <div className="text-[10px] font-mono text-muted-foreground">{shortId(job.id)}</div>
                  </TableCell>
                  <TableCell>
                    <div className="flex flex-col items-start gap-1.5">
                      <div className="flex items-center gap-2 whitespace-nowrap">
                        {isRunning ? renderRunStatus(latestRun) : !isPaused ? renderRunStatus(latestRun) : null}
                        {latestRun ? <RunStateSummary run={latestRun} /> : null}
                      </div>
                      {isPaused ? (
                        <span className="text-xs text-muted-foreground">
                          {isRunning ? "New runs are blocked while the current run drains." : "New runs are blocked until this job is unpaused."}
                        </span>
                      ) : null}
                    </div>
                  </TableCell>
                  <TableCell className="text-muted-foreground text-sm whitespace-nowrap">
                    {latestRun ? <RelativeTime date={latestRun.started_at} /> : "-"}
                  </TableCell>
                  <TableCell className="text-muted-foreground text-sm font-mono">
                    {latestRun ? <Duration start={latestRun.started_at} end={latestRun.completed_at} /> : "-"}
                  </TableCell>
                  <TableCell className="text-right">
                    <div className="flex justify-end gap-2">
                      <Button
                        variant="ghost"
                        size="icon"
                        onClick={() => triggerMutation.mutate({ jobId: job.id })}
                        disabled={triggerMutation.isPending || isPaused}
                        title={isPaused ? "Unpause the job before triggering" : "Trigger run"}
                      >
                        <Play className="h-4 w-4" />
                      </Button>
                      <Button
                        variant="ghost"
                        size="icon"
                        onClick={() => pauseMutation.mutate({ jobId: job.id, paused: !isPaused, hasActiveRun: isRunning })}
                        disabled={pauseMutation.isPending}
                        title={isPaused ? "Unpause job" : "Pause future runs"}
                      >
                        <Pause className="h-4 w-4" />
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              );
            })}
          </TableBody>
        </Table>
      </div>
    </div>
  );
}

function renderRunStatus(run?: JobRun) {
  if (!run) {
    return <span className="text-xs text-muted-foreground">No runs yet</span>;
  }

  const variant =
    run.status === "succeeded" || run.status === "completed"
      ? "success"
      : run.status === "failed"
        ? "destructive"
        : run.status === "running"
          ? "running"
          : "secondary";

  return <Badge variant={variant}>{run.status}</Badge>;
}

function RunStateSummary({ run }: { run: JobRun }) {
  const stats = getRunCacheStats(run);
  if (stats.cacheHits <= 0) {
    return null;
  }

  return (
    <span className="text-[11px] text-muted-foreground">
      <span className="font-medium text-teal-700 dark:text-teal-300">{stats.cacheHits} cached</span>
      {" · "}
      <span>{stats.executedTasks} executed</span>
      {" · "}
      <span>{formatCacheShare(stats)} reused</span>
    </span>
  );
}
