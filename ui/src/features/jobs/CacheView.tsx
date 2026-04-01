import { useMemo, useState } from "react";
import { Link } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { DatabaseZap, Trash2 } from "lucide-react";
import { toast } from "sonner";
import { RelativeTime } from "@/components/relative-time";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { api, type CacheEntry, type Job, type JobRun, type JobTask } from "@/lib/api";
import { shortId } from "@/lib/utils";
import { describeCachePolicy } from "./cache-utils";
import { RunCacheSummary } from "./RunCacheSummary";

interface CacheViewProps {
  jobId: string;
  job: Job;
  featuredRun?: JobRun | null;
  tasks?: JobTask[];
}

export function CacheView({ jobId, job, featuredRun, tasks }: CacheViewProps) {
  const queryClient = useQueryClient();
  const [pendingTaskName, setPendingTaskName] = useState<string | null>(null);

  const { data, isLoading } = useQuery({
    queryKey: ["job", jobId, "cache"],
    queryFn: () => api.getJobCache(jobId),
  });

  const tasksByName = useMemo(() => {
    const map = new Map<string, JobTask>();
    tasks?.forEach((task) => map.set(task.name, task));
    return map;
  }, [tasks]);

  const entries = useMemo(
    () => [...(data?.entries ?? [])].sort((a, b) => new Date(b.created_at).getTime() - new Date(a.created_at).getTime()),
    [data?.entries],
  );

  const invalidateAllMutation = useMutation({
    mutationFn: () => api.deleteJobCache(jobId),
    onSuccess: () => {
      toast.success("Cleared all cache entries for this job");
      queryClient.invalidateQueries({ queryKey: ["job", jobId, "cache"] });
    },
    onError: (err: Error) => toast.error(`Failed to clear job cache: ${err.message}`),
  });

  const invalidateTaskMutation = useMutation({
    mutationFn: (taskName: string) => api.deleteTaskCache(jobId, taskName),
    onSuccess: (_, taskName) => {
      toast.success(`Cleared cache entries for ${taskName}`);
      setPendingTaskName(null);
      queryClient.invalidateQueries({ queryKey: ["job", jobId, "cache"] });
    },
    onError: (err: Error) => {
      setPendingTaskName(null);
      toast.error(`Failed to clear task cache: ${err.message}`);
    },
  });

  return (
    <div className="space-y-4">
      <div className="grid gap-4 md:grid-cols-3">
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="text-sm">Job Cache Policy</CardTitle>
          </CardHeader>
          <CardContent className="space-y-2">
            <Badge variant={job.cache_config ? "cached" : "outline"}>{job.cache_config ? "Enabled" : "Disabled"}</Badge>
            <p className="text-sm text-muted-foreground">{describeCachePolicy(job.cache_config)}</p>
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="text-sm">Active Entries</CardTitle>
          </CardHeader>
          <CardContent className="space-y-2">
            <div className="text-2xl font-semibold">{entries.length}</div>
            <p className="text-sm text-muted-foreground">Unexpired cache records for this job.</p>
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="text-sm">Featured Run</CardTitle>
          </CardHeader>
          <CardContent className="space-y-2">
            {featuredRun ? (
              <>
                <RunCacheSummary run={featuredRun} />
                <p className="text-xs text-muted-foreground">
                  Run <span className="font-mono">{shortId(featuredRun.id)}</span>
                </p>
              </>
            ) : (
              <p className="text-sm text-muted-foreground">Trigger a run to see cache hit ratios here.</p>
            )}
          </CardContent>
        </Card>
      </div>

      <div className="flex items-center justify-between">
        <div>
          <h3 className="text-sm font-semibold">Cache Inventory</h3>
          <p className="text-sm text-muted-foreground">Inspect active cache entries and invalidate them by task or job.</p>
        </div>
        <Button
          variant="outline"
          size="sm"
          onClick={() => invalidateAllMutation.mutate()}
          disabled={invalidateAllMutation.isPending || entries.length === 0}
        >
          <Trash2 className="mr-1.5 h-3.5 w-3.5" />
          Invalidate All
        </Button>
      </div>

      {isLoading ? (
        <div className="rounded-md border bg-card p-6 text-sm text-muted-foreground">Loading cache entries...</div>
      ) : entries.length === 0 ? (
        <div className="rounded-md border border-dashed bg-card/60 p-8 text-center">
          <DatabaseZap className="mx-auto mb-3 h-8 w-8 text-muted-foreground" />
          <div className="text-sm font-medium">No active cache entries</div>
          <p className="mt-1 text-sm text-muted-foreground">Successful cached tasks will appear here after runs populate the cache store.</p>
        </div>
      ) : (
        <div className="rounded-md border bg-card">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Task</TableHead>
                <TableHead>Policy</TableHead>
                <TableHead>Created</TableHead>
                <TableHead>Expires</TableHead>
                <TableHead>Source Run</TableHead>
                <TableHead>Hash</TableHead>
                <TableHead className="text-right">Action</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {entries.map((entry) => {
                const task = tasksByName.get(entry.task_name);
                return (
                  <CacheEntryRow
                    key={`${entry.task_name}:${entry.hash}`}
                    entry={entry}
                    task={task}
                    jobId={jobId}
                    pending={pendingTaskName === entry.task_name && invalidateTaskMutation.isPending}
                    onInvalidate={(taskName) => {
                      setPendingTaskName(taskName);
                      invalidateTaskMutation.mutate(taskName);
                    }}
                  />
                );
              })}
            </TableBody>
          </Table>
        </div>
      )}
    </div>
  );
}

function CacheEntryRow({
  entry,
  task,
  jobId,
  pending,
  onInvalidate,
}: {
  entry: CacheEntry;
  task?: JobTask;
  jobId: string;
  pending: boolean;
  onInvalidate: (taskName: string) => void;
}) {
  return (
    <TableRow>
      <TableCell>
        <div className="font-medium">{entry.task_name}</div>
        {task?.id ? <div className="text-[10px] font-mono text-muted-foreground">{shortId(task.id)}</div> : null}
      </TableCell>
      <TableCell className="text-sm text-muted-foreground">{describeCachePolicy(task?.cache_config)}</TableCell>
      <TableCell className="text-sm text-muted-foreground whitespace-nowrap">
        <RelativeTime date={entry.created_at} />
      </TableCell>
      <TableCell className="text-sm text-muted-foreground whitespace-nowrap">
        {entry.expires_at ? <RelativeTime date={entry.expires_at} /> : "Never"}
      </TableCell>
      <TableCell className="text-sm">
        <Link to="/jobs/$jobId/runs/$runId" params={{ jobId, runId: entry.run_id }} className="font-mono text-primary hover:underline">
          {shortId(entry.run_id)}
        </Link>
      </TableCell>
      <TableCell className="font-mono text-xs text-muted-foreground">{shortId(entry.hash, 12)}</TableCell>
      <TableCell className="text-right">
        <Button variant="ghost" size="sm" onClick={() => onInvalidate(entry.task_name)} disabled={pending}>
          Invalidate Task
        </Button>
      </TableCell>
    </TableRow>
  );
}
