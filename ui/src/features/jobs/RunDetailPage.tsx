import { useParams } from "@tanstack/react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api, type Atom } from "@/lib/api";
import { events, type CaesiumEvent } from "@/lib/events";
import { JobDAG } from "./JobDAG";
import { LogViewer } from "./LogViewer";
import { useEffect, useMemo, useState } from "react";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { Button } from "@/components/ui/button";
import { Terminal } from "lucide-react";
import { toast } from "sonner";

export function RunDetailPage() {
    const { jobId, runId } = useParams({ strict: false }) as { jobId: string; runId: string };
    const queryClient = useQueryClient();
    const [selectedTaskId, setSelectedTaskId] = useState<string | null>(null);

    const { data: run, isLoading: isLoadingRun } = useQuery({
        queryKey: ["job", jobId, "runs", runId],
        queryFn: () => api.getJobRun(jobId, runId),
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
        
        events.connect({ job_id: jobId, run_id: runId });

        const onEvent = (e: CaesiumEvent) => {
             // Invalidate run query to fetch latest state
             queryClient.invalidateQueries({ queryKey: ["job", jobId, "runs", runId] });
             if (e.type === "run_completed") {
                toast.success("Run completed");
             }
             if (e.type === "run_failed") {
                toast.error("Run failed");
             }
        };
        
        const eventTypes = ["run_started", "run_completed", "run_failed", "task_started", "task_succeeded", "task_failed", "task_skipped"];
        eventTypes.forEach(t => events.subscribe(t, onEvent));

        return () => {
            eventTypes.forEach(t => events.unsubscribe(t, onEvent));
            events.disconnect();
        }
    }, [jobId, runId, queryClient]);

    const taskStatus = useMemo(() => {
        const statusMap: Record<string, string> = {};
        run?.tasks?.forEach(t => {
            statusMap[t.task_id] = t.status;
        });
        return statusMap;
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
                    <Badge variant={run.status === "succeeded" || run.status === "completed" ? "default" : run.status === "failed" ? "destructive" : "secondary"}>
                        {run.status}
                    </Badge>
                </div>
            </div>

            <div className="border rounded-md bg-card">
                 {dag && atoms && <JobDAG dag={dag} atoms={atoms} taskStatus={taskStatus} />}
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
            
            <div className="space-y-2">
                <h3 className="text-lg font-medium">Tasks</h3>
                <div className="space-y-1">
                    {run.tasks?.map(task => (
                        <div key={task.id} className="flex justify-between items-center p-2 border rounded text-sm">
                            <span className="font-mono">{task.task_id}</span>
                            <div className="flex gap-2 items-center">
                                <span className="text-muted-foreground">{task.atom_id}</span>
                                <Badge variant="outline">{task.status}</Badge>
                                {task.status !== 'pending' && (
                                    <Button variant="ghost" size="icon" className="h-8 w-8" onClick={() => setSelectedTaskId(task.task_id)}>
                                        <Terminal className="h-4 w-4" />
                                    </Button>
                                )}
                            </div>
                        </div>
                    ))}
                </div>
            </div>
        </div>
    );
}
