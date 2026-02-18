import { useParams, useNavigate, Link } from "@tanstack/react-router";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { api, type Atom, type JobRun, type TaskRun } from "@/lib/api";
import { events, type CaesiumEvent } from "@/lib/events";
import { Button } from "@/components/ui/button";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { JobDAG } from "./JobDAG";
import { Play, Clock, ChevronRight } from "lucide-react";
import { toast } from "sonner";
import { Badge } from "@/components/ui/badge";
import { Duration } from "@/components/duration";
import { RelativeTime } from "@/components/relative-time";
import { useEffect } from "react";

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

  useEffect(() => {
    const onEvent = (e: CaesiumEvent) => {
      if (e.job_id !== jobId) return;

      if (e.type === "run_started" || e.type === "run_completed" || e.type === "run_failed") {
        const runPayload = e.payload as JobRun;
        if (!runPayload) return;

        queryClient.setQueryData(["job", jobId, "runs"], (old: JobRun[] | undefined) => {
          if (!old) return [runPayload];
          const exists = old.find(r => r.id === runPayload.id);
          if (exists) {
            return old.map(r => r.id === runPayload.id ? { ...r, ...runPayload } : r);
          }
          return [runPayload, ...old];
        });
      }

      if (e.type.startsWith("task_")) {
        const taskPayload = e.payload as TaskRun;
        if (!taskPayload) return;

        queryClient.setQueryData(["job", jobId, "runs"], (old: JobRun[] | undefined) => {
          if (!old) return old;
          return old.map(run => {
            if (run.id === taskPayload.job_run_id) {
              const tasks = [...(run.tasks || [])];
              const taskIndex = tasks.findIndex(t => t.task_id === taskPayload.task_id);
              if (taskIndex > -1) {
                tasks[taskIndex] = { ...tasks[taskIndex], ...taskPayload };
              } else {
                // Background refetch if we see an unknown task to get full atom/metadata
                queryClient.invalidateQueries({ queryKey: ["job", jobId, "runs"] });
                tasks.push(taskPayload);
              }
              return { ...run, tasks };
            }
            return run;
          });
        });
      }
    };

    const eventTypes = ["run_started", "run_completed", "run_failed", "task_started", "task_succeeded", "task_failed", "task_skipped"];
    eventTypes.forEach(t => events.subscribe(t, onEvent));
    return () => eventTypes.forEach(t => events.unsubscribe(t, onEvent));
  }, [jobId, queryClient]);

  const { data: dag, isLoading: isLoadingDAG } = useQuery({
    queryKey: ["job", jobId, "dag"],
    queryFn: () => api.getJobDAG(jobId),
    refetchInterval: 30000,
  });

  const { data: atoms, isLoading: isLoadingAtoms } = useQuery({
    queryKey: ["atoms"],
    queryFn: api.getAtoms,
    select: (data) => {
        const map: Record<string, Atom> = {};
        data.forEach(a => map[a.id] = a);
        return map;
    }
  });

  const { data: trigger, isLoading: isLoadingTrigger } = useQuery({
    queryKey: ["trigger", job?.trigger_id],
    queryFn: () => job?.trigger_id ? api.getTrigger(job.trigger_id) : Promise.resolve(null),
    enabled: !!job?.trigger_id,
  });

  const formatCommand = (command: string) => {
    if (!command) return "N/A";
    try {
        const parsed = JSON.parse(command);
        if (Array.isArray(parsed)) return parsed.join(" ");
        return String(parsed);
    } catch {
        return command;
    }
  };

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

  if (isLoadingJob || isLoadingDAG || isLoadingAtoms || isLoadingRuns || isLoadingTrigger) return <div className="p-8">Loading...</div>;

  if (!job) return <div className="p-8">Job not found</div>;

  const sortedRuns = runs ? [...runs].sort((a, b) => new Date(b.created_at).getTime() - new Date(a.created_at).getTime()) : [];
  const latestRun = sortedRuns[0];
  const taskMetadata: Record<string, { status: string; started_at?: string; completed_at?: string; error?: string }> = {};
  latestRun?.tasks?.forEach(t => {
    taskMetadata[t.task_id] = {
        status: t.status,
        started_at: t.started_at,
        completed_at: t.completed_at,
        error: t.error
    };
  });

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
            <h1 className="text-2xl font-bold tracking-tight">{job.alias}</h1>
            <div className="flex items-center gap-2">
                <p className="text-muted-foreground font-mono text-xs">{job.id}</p>
                {latestRun && (
                    <>
                        <span className="text-muted-foreground">•</span>
                        <Badge variant={
                          latestRun.status === "succeeded" || latestRun.status === "completed" 
                            ? "success" 
                            : latestRun.status === "failed" 
                              ? "destructive" 
                              : latestRun.status === "running"
                                ? "running"
                                : "secondary"
                        } className="text-[10px] h-4">
                            {latestRun.status}
                        </Badge>
                        <span className="text-muted-foreground font-mono text-[10px]">
                            <Duration start={latestRun.started_at} end={latestRun.completed_at} />
                        </span>
                    </>
                )}
            </div>
        </div>
        <div className="flex gap-2">
             <Button onClick={() => triggerMutation.mutate(job.id)} disabled={triggerMutation.isPending}>
                <Play className="mr-2 h-4 w-4" /> Trigger
             </Button>
        </div>
      </div>

      <Tabs defaultValue="dag">
        <TabsList>
          <TabsTrigger value="dag">DAG</TabsTrigger>
          <TabsTrigger value="runs">Runs</TabsTrigger>
          <TabsTrigger value="atoms">Atoms</TabsTrigger>
          <TabsTrigger value="configuration">Configuration</TabsTrigger>
          <TabsTrigger value="definition">Definition</TabsTrigger>
        </TabsList>
        <TabsContent value="dag" className="h-[600px] mt-4 border rounded-md overflow-hidden">
            {dag && atoms && <JobDAG dag={dag} atoms={atoms} taskMetadata={taskMetadata} />}
        </TabsContent>
        <TabsContent value="runs" className="mt-4">
            <div className="rounded-md border bg-card divide-y">
                {runs?.length === 0 && (
                    <div className="p-8 text-center text-muted-foreground">No runs found for this job.</div>
                )}
                {runs?.map(run => (
                    <Link 
                        key={run.id}
                        to="/jobs/$jobId/runs/$runId"
                        params={{ jobId: job.id, runId: run.id }}
                        className="flex items-center justify-between p-4 hover:bg-muted/50 transition-colors group"
                    >
                        <div className="flex items-center gap-4">
                            <div className="p-2 rounded-full bg-slate-800/50 border border-slate-700/50">
                                <Clock className="h-4 w-4 text-slate-400" />
                            </div>
                            <div>
                                <div className="flex items-center gap-2">
                                    <span className="text-sm font-bold text-slate-200">
                                        <RelativeTime date={run.created_at} />
                                    </span>
                                    <Badge variant="outline" className="text-[10px] h-4 border-slate-700 text-slate-400 font-mono uppercase">
                                        {run.trigger_type || 'manual'}: {run.trigger_alias || 'user'}
                                    </Badge>
                                </div>
                                <div className="text-[10px] font-mono text-muted-foreground flex items-center gap-2">
                                    {run.id.substring(0, 8)}
                                    <span>•</span>
                                    <Duration start={run.started_at} end={run.completed_at} />
                                </div>
                            </div>
                        </div>
                        <div className="flex items-center gap-3">
                            <Badge variant={
                              run.status === "succeeded" || run.status === "completed" 
                                ? "success" 
                                : run.status === "failed" 
                                  ? "destructive" 
                                  : run.status === "running"
                                    ? "running"
                                    : "secondary"
                            }>
                                {run.status}
                            </Badge>
                            <ChevronRight className="h-4 w-4 text-muted-foreground group-hover:text-primary transition-colors" />
                        </div>
                    </Link>
                ))}
            </div>
        </TabsContent>
        <TabsContent value="atoms" className="mt-4">
            <div className="rounded-md border bg-card">
                <div className="p-4 border-b bg-muted/40">
                    <h3 className="font-semibold text-sm">Task & Atom Details</h3>
                </div>
                <div className="divide-y">
                    {dag?.nodes.map((node: any) => {
                        const atom = atoms?.[node.atom_id];
                        return (
                            <div key={node.id} className="p-4 grid grid-cols-1 md:grid-cols-2 gap-4">
                                <div>
                                    <div className="text-sm font-medium mb-1">Task ID</div>
                                    <div className="font-mono text-xs text-muted-foreground bg-muted p-1 rounded inline-block">
                                        {node.id}
                                    </div>
                                </div>
                                {atom && (
                                    <div className="space-y-2">
                                        <div className="grid grid-cols-[100px_1fr] gap-2 text-xs">
                                            <span className="text-muted-foreground font-medium">Atom ID:</span>
                                            <span className="font-mono text-muted-foreground">{atom.id}</span>
                                            
                                            <span className="text-muted-foreground font-medium">Engine:</span>
                                            <span className="font-mono uppercase">{atom.engine}</span>
                                            
                                            <span className="text-muted-foreground font-medium">Image:</span>
                                            <span className="font-mono text-blue-400">{atom.image}</span>
                                            
                                            <span className="text-muted-foreground font-medium">Command:</span>
                                            <code className="font-mono bg-slate-950 px-1 rounded border border-slate-800 break-all">
                                                {formatCommand(atom.command)}
                                            </code>
                                        </div>
                                    </div>
                                )}
                            </div>
                        );
                    })}
                </div>
            </div>
        </TabsContent>
        <TabsContent value="configuration" className="mt-4">
             <div className="grid gap-4 md:grid-cols-2">
                <div className="rounded-md border bg-card p-4 space-y-4">
                    <div className="flex items-center justify-between border-b pb-2">
                        <h3 className="font-semibold text-sm">Trigger Configuration</h3>
                        {trigger ? (
                             <Badge variant="outline">{trigger.type}</Badge>
                        ) : (
                             <Badge variant="secondary">No Trigger</Badge>
                        )}
                    </div>
                    {trigger ? (
                        <div className="space-y-3 text-sm">
                            <div className="grid grid-cols-[100px_1fr] gap-2">
                                <span className="text-muted-foreground">ID:</span>
                                <span className="font-mono text-xs">{trigger.id}</span>
                            </div>
                            <div className="grid grid-cols-[100px_1fr] gap-2">
                                <span className="text-muted-foreground">Alias:</span>
                                <span className="font-medium">{trigger.alias}</span>
                            </div>
                            <div className="grid grid-cols-[100px_1fr] gap-2">
                                <span className="text-muted-foreground">Config:</span>
                                <pre className="font-mono text-xs bg-muted p-2 rounded overflow-x-auto">
                                    {trigger.configuration}
                                </pre>
                            </div>
                        </div>
                    ) : (
                        <div className="text-muted-foreground text-sm py-4">
                            This job does not have an associated trigger.
                        </div>
                    )}
                </div>
             </div>
        </TabsContent>
        <TabsContent value="definition" className="mt-4">
            <pre className="p-4 bg-muted rounded-md overflow-auto text-xs border">
                {JSON.stringify(job, null, 2)}
            </pre>
        </TabsContent>
      </Tabs>
    </div>
  );
}
