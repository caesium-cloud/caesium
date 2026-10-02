import { useMemo, useState } from "react";
import { Link } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { toast } from "sonner";
import { RelativeTime } from "@/components/relative-time";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";

import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { api, type CacheEntry, type Job, type JobRun, type JobTask } from "@/lib/api";
import { IdChip } from "@/components/ui/id-chip";
import { describeCachePolicy, describeEffectiveCachePolicy, normalizeCacheConfig } from "./cache-utils";
import { RunCacheSummary } from "./RunCacheSummary";

interface CacheViewProps {
  jobId: string;
  job: Job;
  featuredRun?: JobRun | null;
  tasks?: JobTask[];
  taskPoliciesAvailable: boolean;
}

export function CacheView({ jobId, job, featuredRun, tasks, taskPoliciesAvailable }: CacheViewProps) {
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
  const hasJobPolicy = job.cache_config !== undefined && job.cache_config !== null;
  const jobPolicy = describeCachePolicy(job.cache_config);
  const jobPolicyEnabled = hasJobPolicy && normalizeCacheConfig(job.cache_config).enabled;

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
        <section className="border-b border-border py-3">
          <div className="pb-2">
            <h3 className="text-sm">Job Cache Policy</h3>
          </div>
          <div className="space-y-2">
            <Badge variant={jobPolicyEnabled ? "cached" : "outline"}>
              {!hasJobPolicy ? "Server default" : jobPolicyEnabled ? "Enabled" : "Disabled"}
            </Badge>
            {hasJobPolicy ? <p className="text-sm text-muted-foreground">{jobPolicy}</p> : null}
          </div>
        </section>
        <section className="border-b border-border py-3">
          <div className="pb-2">
            <h3 className="text-sm">Active Entries</h3>
          </div>
          <div className="space-y-2">
            <div className="text-2xl font-bold">{entries.length}</div>
            <p className="text-sm text-muted-foreground">Unexpired cache records for this job.</p>
          </div>
        </section>
        <section className="border-b border-border py-3">
          <div className="pb-2">
            <h3 className="text-sm">Featured Run</h3>
          </div>
          <div className="space-y-2">
            {featuredRun ? (
              <>
                <RunCacheSummary run={featuredRun} />
                <p className="text-xs text-muted-foreground">
                  Run <IdChip value={featuredRun.id} label="run id" />
                </p>
              </>
            ) : (
              <p className="text-sm text-muted-foreground">Trigger a run to see cache hit ratios here.</p>
            )}
          </div>
        </section>
      </div>

      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h3 className="text-sm font-bold">Cache Inventory</h3>
          <p className="text-sm text-muted-foreground">Inspect active cache entries and invalidate them by task or job.</p>
        </div>
        <Button
          variant="destructive"
          size="sm"
          onClick={() => invalidateAllMutation.mutate()}
          disabled={invalidateAllMutation.isPending || entries.length === 0}
        >

          Invalidate All
        </Button>
      </div>

      {isLoading ? (
        <div className="border-y border-border p-6 text-sm text-muted-foreground">Loading cache entries...</div>
      ) : entries.length === 0 ? (
        <div className="rounded-md border border-dashed bg-card/60 p-8 text-center">

          <div className="text-sm font-normal">No active cache entries</div>
          <p className="mt-1 text-sm text-muted-foreground">Successful cached tasks will appear here after runs populate the cache store.</p>
        </div>
      ) : (
        <div className="border-y border-border">
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
                    taskPolicyAvailable={taskPoliciesAvailable && task !== undefined}
                    jobCacheConfig={job.cache_config}
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
  taskPolicyAvailable,
  jobCacheConfig,
  jobId,
  pending,
  onInvalidate,
}: {
  entry: CacheEntry;
  task?: JobTask;
  taskPolicyAvailable: boolean;
  jobCacheConfig?: Job["cache_config"];
  jobId: string;
  pending: boolean;
  onInvalidate: (taskName: string) => void;
}) {
  return (
    <TableRow>
      <TableCell>
        <div className="font-bold">{entry.task_name}</div>
        {task?.id ? <div className="text-[11px] text-muted-foreground"><IdChip value={task.id} label="task id" /></div> : null}
      </TableCell>
      <TableCell className="text-sm text-muted-foreground" data-testid="cache-entry-policy">
        {taskPolicyAvailable ? describeEffectiveCachePolicy(task?.cache_config, jobCacheConfig) : "Policy unavailable"}
      </TableCell>
      <TableCell className="text-sm text-muted-foreground whitespace-nowrap">
        <RelativeTime date={entry.created_at} />
      </TableCell>
      <TableCell className="text-sm text-muted-foreground whitespace-nowrap">
        {entry.expires_at ? <span data-testid="cache-expiry"><RelativeTime date={entry.expires_at} future /></span> : "Never"}
      </TableCell>
      <TableCell className="text-sm">
        <Link to="/jobs/$jobId/runs/$runId" params={{ jobId, runId: entry.run_id }} className="text-primary hover:underline">
          open run
        </Link>
      </TableCell>
      <TableCell className="text-xs text-muted-foreground"><IdChip value={entry.hash} label="fingerprint" /></TableCell>
      <TableCell className="text-right">
        <Button variant="destructive" size="sm" onClick={() => onInvalidate(entry.task_name)} disabled={pending}>
          Invalidate Task
        </Button>
      </TableCell>
    </TableRow>
  );
}
