import { useEffect } from "react";
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
import { cn } from "@/lib/utils";

export function JobsPage() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { data: jobs, isLoading, error } = useQuery({
    queryKey: ["jobs"],
    queryFn: api.getJobs,
    refetchInterval: 30000,
  });

  useEffect(() => {
    const onRunEvent = (e: CaesiumEvent) => {
      queryClient.invalidateQueries({ queryKey: ["jobs"] });
      queryClient.invalidateQueries({ queryKey: ["job", e.job_id] });
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

    ["run_started", "run_completed", "run_failed"].forEach((type) => events.subscribe(type, onRunEvent));
    ["job_paused", "job_unpaused"].forEach((type) => events.subscribe(type, onPauseEvent));

    return () => {
      ["run_started", "run_completed", "run_failed"].forEach((type) => events.unsubscribe(type, onRunEvent));
      ["job_paused", "job_unpaused"].forEach((type) => events.unsubscribe(type, onPauseEvent));
    };
  }, [queryClient]);

  const triggerMutation = useMutation({
    mutationFn: ({ jobId }: { jobId: string }) => api.triggerJob(jobId),
    onSuccess: (run) => {
      queryClient.invalidateQueries({ queryKey: ["jobs"] });
      toast.success("Job triggered successfully");
      navigate({ to: "/jobs/$jobId/runs/$runId", params: { jobId: run.job_id, runId: run.id } });
    },
    onError: (err: Error) => {
      toast.error(`Failed to trigger job: ${err.message}`);
    },
  });

  const pauseMutation = useMutation({
    mutationFn: ({ jobId, paused }: { jobId: string; paused: boolean }) =>
      paused ? api.pauseJob(jobId) : api.unpauseJob(jobId),
    onSuccess: (job) => {
      queryClient.setQueryData(["jobs"], (old: Job[] | undefined) =>
        old?.map((entry) => (entry.id === job.id ? { ...entry, paused: job.paused } : entry)),
      );
      queryClient.setQueryData(["job", job.id], (old: Job | undefined) => (old ? { ...old, paused: job.paused } : job));
      toast.success(job.paused ? "Job paused" : "Job unpaused");
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
                    <div className="text-[10px] font-mono text-muted-foreground">{job.id.substring(0, 8)}</div>
                  </TableCell>
                  <TableCell>
                    {isPaused ? (
                      <Badge variant="outline" className="border-amber-500/40 text-amber-300">
                        Paused
                      </Badge>
                    ) : (
                      renderRunStatus(latestRun)
                    )}
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
                        onClick={() => pauseMutation.mutate({ jobId: job.id, paused: !isPaused })}
                        disabled={pauseMutation.isPending}
                        title={isPaused ? "Unpause job" : "Pause job"}
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
