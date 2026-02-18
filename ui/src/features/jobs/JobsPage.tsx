import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { api, type Job, type JobRun } from "@/lib/api";
import { events, type CaesiumEvent } from "@/lib/events";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Button } from "@/components/ui/button";
import { Play, Activity } from "lucide-react";
import { toast } from "sonner";
import { Badge } from "@/components/ui/badge";
import { Duration } from "@/components/duration";
import { RelativeTime } from "@/components/relative-time";
import { cn } from "@/lib/utils";
import { useEffect } from "react";

export function JobsPage() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { data: jobs, isLoading, error } = useQuery({
    queryKey: ["jobs"],
    queryFn: api.getJobs,
    refetchInterval: 30000, // Minimal polling as fallback
  });

  useEffect(() => {
    const onEvent = (e: CaesiumEvent) => {
      if (e.type === "run_started" || e.type === "run_completed" || e.type === "run_failed") {
        queryClient.setQueryData(["jobs"], (old: Job[] | undefined) => {
          if (!old) return old;
          const run = e.payload as JobRun;
          if (!run) return old;

          return old.map(job => {
            if (job.id === run.job_id) {
              return { ...job, latest_run: run };
            }
            return job;
          });
        });
      }
    };

    const eventTypes = ["run_started", "run_completed", "run_failed"];
    eventTypes.forEach(t => events.subscribe(t, onEvent));
    return () => eventTypes.forEach(t => events.unsubscribe(t, onEvent));
  }, [queryClient]);

  const triggerMutation = useMutation({
    mutationFn: api.triggerJob,
    onSuccess: (run) => {
      toast.success("Job triggered successfully");
      navigate({ to: "/jobs/$jobId/runs/$runId", params: { jobId: run.job_id, runId: run.id } });
    },
    onError: (err) => {
      toast.error(`Failed to trigger job: ${err.message}`);
    },
  });

  if (isLoading) return <div className="p-8 text-center text-muted-foreground">Loading jobs...</div>;
  if (error) return <div className="p-8 text-center text-destructive">Error loading jobs: {error.message}</div>;

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <h1 className="text-2xl font-bold tracking-tight">Jobs</h1>
        {/* <Button>Create Job</Button> */}
      </div>
      <div className="rounded-md border bg-card">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Alias</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Last Run</TableHead>
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
              const isRunning = job.latest_run?.status === "running";
              return (
                <TableRow key={job.id} className={cn(isRunning && "bg-blue-500/5 animate-in fade-in duration-1000")}>
                  <TableCell className="font-medium relative overflow-hidden">
                    {isRunning && <div className="absolute left-0 top-0 bottom-0 w-1 bg-blue-500 animate-pulse" />}
                    <Link to="/jobs/$jobId" params={{ jobId: job.id }} className="hover:underline text-primary">
                      {job.alias}
                    </Link>
                    <div className="text-[10px] font-mono text-muted-foreground">{job.id.substring(0, 8)}</div>
                  </TableCell>
                  <TableCell>
                    {job.latest_run ? (
                      <Badge variant={
                        job.latest_run.status === "succeeded" || job.latest_run.status === "completed" 
                          ? "success" 
                          : job.latest_run.status === "failed" 
                            ? "destructive" 
                            : isRunning
                              ? "running"
                              : "secondary"
                      } className={cn(isRunning && "pl-1.5")}>
                        {isRunning && <Activity className="mr-1 h-3 w-3 animate-spin" />}
                        {job.latest_run.status}
                      </Badge>
                    ) : (
                      <span className="text-muted-foreground text-xs">-</span>
                    )}
                  </TableCell>
                  <TableCell className="text-muted-foreground text-sm whitespace-nowrap">
                    {job.latest_run ? <RelativeTime date={job.latest_run.started_at} /> : "-"}
                  </TableCell>
                  <TableCell className="text-muted-foreground text-sm font-mono">
                    {job.latest_run ? (
                      <Duration start={job.latest_run.started_at} end={job.latest_run.completed_at} />
                    ) : "-"}
                  </TableCell>
                  <TableCell className="text-right">
                    <Button
                      variant="ghost"
                      size="icon"
                      onClick={() => triggerMutation.mutate(job.id)}
                      disabled={triggerMutation.isPending}
                      title="Trigger Run"
                    >
                      <Play className="h-4 w-4" />
                    </Button>
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
