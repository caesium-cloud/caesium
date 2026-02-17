import { useParams } from "@tanstack/react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api, type Atom, type JobRun, type TaskRun } from "@/lib/api";
import { events, type CaesiumEvent } from "@/lib/events";
import { JobDAG } from "./JobDAG";
import { LogViewer } from "./LogViewer";
import { useEffect, useMemo, useState } from "react";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { toast } from "sonner";

export function RunDetailPage() {
    const { jobId, runId } = useParams({ strict: false }) as { jobId: string; runId: string };
    const queryClient = useQueryClient();
    const [selectedTaskId, setSelectedTaskId] = useState<string | null>(null);

    const { data: run, isLoading: isLoadingRun } = useQuery({
        queryKey: ["job", jobId, "runs", runId],
        queryFn: () => api.getJobRun(jobId, runId),
        refetchInterval: 5000, // Fallback to polling every 5s
    });

    const { data: dag, isLoading: isLoadingDAG } = useQuery({
        queryKey: ["job", jobId, "dag"],
        queryFn: () => api.getJobDAG(jobId),
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

    useEffect(() => {
        if (!runId || !jobId) return;
        
        const onEvent = (e: CaesiumEvent) => {
             queryClient.setQueryData(["job", jobId, "runs", runId], (old: JobRun | undefined) => {
                if (!old) return old;
                
                if (e.type === "run_completed" || e.type === "run_succeeded") {
                    const finalRun = e.payload as JobRun;
                    if (finalRun && finalRun.tasks) return finalRun;
                    
                    toast.success("Run completed");
                    return { ...old, status: "succeeded" };
                } 
                
                if (e.type === "run_failed") {
                    toast.error("Run failed");
                    return { ...old, status: "failed" };
                } 
                
                if (e.type.startsWith("task_")) {
                    const taskUpdate = e.payload as TaskRun;
                    const updated = { ...old };
                    const tasks = [...(updated.tasks || [])];
                    const taskId = taskUpdate?.task_id || e.task_id;
                    
                    if (!taskId) return old;

                    const taskIndex = tasks.findIndex(t => t.task_id === taskId);
                    
                    let status = taskUpdate?.status;
                    if (e.type === "task_started") status = "running";
                    else if (e.type === "task_succeeded") status = "succeeded";
                    else if (e.type === "task_failed") status = "failed";
                    else if (e.type === "task_skipped") status = "skipped";

                    if (taskIndex > -1) {
                        tasks[taskIndex] = { 
                            ...tasks[taskIndex], 
                            ...(taskUpdate || {}),
                            status: status || tasks[taskIndex].status 
                        };
                    } else {
                        // If task not found in current snapshot, trigger a background refetch
                        // to ensure we get the full task details (atom_id, engine, image, etc.)
                        queryClient.invalidateQueries({ queryKey: ["job", jobId, "runs", runId] });
                        
                        const baseTask: TaskRun = {
                            id: taskId,
                            job_run_id: runId,
                            task_id: taskId,
                            atom_id: "",
                            engine: "",
                            image: "",
                            command: [],
                            status: status || "pending",
                            created_at: new Date().toISOString(),
                            updated_at: new Date().toISOString(),
                        };
                        const newTask: TaskRun = taskUpdate ? { ...baseTask, ...taskUpdate } : baseTask;
                        tasks.push(newTask);
                    }
                    
                    updated.tasks = tasks;
                    return updated;
                }
                
                return old;
             });
        };
        
        const eventTypes = ["run_started", "run_completed", "run_failed", "task_started", "task_succeeded", "task_failed", "task_skipped"];
        eventTypes.forEach(t => events.subscribe(t, onEvent));

        return () => {
            eventTypes.forEach(t => events.unsubscribe(t, onEvent));
        }
    }, [jobId, runId, queryClient]);

    const taskMetadata = useMemo(() => {
        const meta: Record<string, { status: string; started_at?: string; completed_at?: string; error?: string }> = {};
        run?.tasks?.forEach(t => {
            meta[t.task_id] = {
                status: t.status,
                started_at: t.started_at,
                completed_at: t.completed_at,
                error: t.error
            };
        });
        return meta;
    }, [run]);

    if (isLoadingRun || isLoadingDAG || isLoadingAtoms) return <div className="p-8 space-y-4">
        <Skeleton className="h-8 w-[200px]" />
        <Skeleton className="h-[400px] w-full" />
    </div>;

    if (!run) return <div className="p-8">Run not found</div>;

    return (
        <div className="space-y-6">
            <div className="flex items-center justify-between">
                <div>
                    <h1 className="text-2xl font-bold tracking-tight">Run {runId.substring(0, 8)}</h1>
                    <p className="text-muted-foreground font-mono text-sm">Job: {jobId}</p>
                </div>
                <div className="flex gap-2 items-center">
                    <span className="text-sm text-muted-foreground">Status:</span>
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
                </div>
            </div>

            <div className="border rounded-md bg-card h-[600px] overflow-hidden">
                 {dag && atoms && (
                    <JobDAG 
                        dag={dag} 
                        atoms={atoms} 
                        taskMetadata={taskMetadata} 
                        onNodeClick={setSelectedTaskId}
                        selectedTaskId={selectedTaskId}
                    />
                 )}
            </div>

            {selectedTaskId && (
                <div className="h-[400px]">
                    <LogViewer
                        jobId={jobId}
                        runId={runId}
                        taskId={selectedTaskId}
                        onClose={() => setSelectedTaskId(null)}
                    />
                </div>
            )}
        </div>
    );
}
