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
import { Play, Activity, Trash2, Search, X, ChevronLeft, ChevronRight } from "lucide-react";
import { toast } from "sonner";
import { Badge } from "@/components/ui/badge";
import { Duration } from "@/components/duration";
import { RelativeTime } from "@/components/relative-time";
import { cn } from "@/lib/utils";
import { useEffect, useMemo, useState } from "react";

const PAGE_SIZE = 20;

export function JobsPage() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [search, setSearch] = useState("");
  const [labelFilter, setLabelFilter] = useState<string | null>(null);
  const [page, setPage] = useState(0);
  const [deleteConfirm, setDeleteConfirm] = useState<string | null>(null);

  const { data: jobs, isLoading, error } = useQuery({
    queryKey: ["jobs"],
    queryFn: api.getJobs,
    refetchInterval: 30000,
  });

  useEffect(() => {
    const onEvent = (e: CaesiumEvent) => {
      if (e.type === "run_started" || e.type === "run_completed" || e.type === "run_failed") {
        queryClient.setQueryData(["jobs"], (old: Job[] | undefined) => {
          if (!old) return old;
          const run = e.payload as JobRun;
          if (!run) return old;
          return old.map(job =>
            job.id === run.job_id ? { ...job, latest_run: run } : job
          );
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
    onError: (err) => toast.error(`Failed to trigger job: ${err.message}`),
  });

  const deleteMutation = useMutation({
    mutationFn: api.deleteJob,
    onSuccess: (_, id) => {
      queryClient.setQueryData(["jobs"], (old: Job[] | undefined) =>
        old?.filter(j => j.id !== id)
      );
      toast.success("Job deleted");
      setDeleteConfirm(null);
    },
    onError: () => toast.error("Failed to delete job"),
  });

  // Collect all unique label keys for filter chips
  const allLabelKeys = useMemo(() => {
    const keys = new Set<string>();
    jobs?.forEach(j => Object.keys(j.labels || {}).forEach(k => keys.add(k)));
    return Array.from(keys);
  }, [jobs]);

  const filtered = useMemo(() => {
    let result = jobs || [];
    if (search) {
      const q = search.toLowerCase();
      result = result.filter(j => j.alias.toLowerCase().includes(q) || j.id.includes(q));
    }
    if (labelFilter) {
      result = result.filter(j => j.labels && labelFilter in j.labels);
    }
    return result;
  }, [jobs, search, labelFilter]);

  const totalPages = Math.ceil(filtered.length / PAGE_SIZE);
  const pageJobs = filtered.slice(page * PAGE_SIZE, (page + 1) * PAGE_SIZE);


  if (isLoading) return <div className="p-8 text-center text-muted-foreground">Loading jobs...</div>;
  if (error) return <div className="p-8 text-center text-destructive">Error loading jobs: {error.message}</div>;

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <h1 className="text-2xl font-bold tracking-tight">Jobs</h1>
        <span className="text-sm text-muted-foreground">{filtered.length} job{filtered.length !== 1 ? "s" : ""}</span>
      </div>

      {/* Search & label filters */}
      <div className="flex flex-wrap gap-2 items-center">
        <div className="relative flex-1 min-w-[200px] max-w-sm">
          <Search className="absolute left-2.5 top-2.5 h-4 w-4 text-muted-foreground pointer-events-none" />
          <input
            value={search}
            onChange={e => { setSearch(e.target.value); setPage(0); }}
            placeholder="Search jobs..."
            className="w-full rounded-md border bg-background px-3 py-2 pl-8 text-sm ring-offset-background placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
          />
          {search && (
            <button onClick={() => { setSearch(""); setPage(0); }} className="absolute right-2.5 top-2.5 text-muted-foreground hover:text-foreground">
              <X className="h-4 w-4" />
            </button>
          )}
        </div>
        {allLabelKeys.map(key => (
          <button
            key={key}
            onClick={() => { setLabelFilter(labelFilter === key ? null : key); setPage(0); }}
            className={cn(
              "rounded-full px-3 py-1 text-xs border transition-colors",
              labelFilter === key
                ? "bg-primary text-primary-foreground border-primary"
                : "bg-background text-muted-foreground border-border hover:border-primary hover:text-primary"
            )}
          >
            {key}
          </button>
        ))}
        {labelFilter && (
          <button onClick={() => { setLabelFilter(null); setPage(0); }} className="text-xs text-muted-foreground hover:text-foreground flex items-center gap-1">
            <X className="h-3 w-3" /> Clear filter
          </button>
        )}
      </div>

      <div className="rounded-md border bg-card">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Alias</TableHead>
              <TableHead>Labels</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Last Run</TableHead>
              <TableHead>Duration</TableHead>
              <TableHead className="text-right">Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {pageJobs.length === 0 && (
              <TableRow>
                <TableCell colSpan={6} className="h-24 text-center text-muted-foreground">
                  {filtered.length === 0 && (search || labelFilter) ? "No jobs match your filter." : "No jobs found."}
                </TableCell>
              </TableRow>
            )}
            {pageJobs.map((job) => {
              const isRunning = job.latest_run?.status === "running";
              const labelEntries = Object.entries(job.labels || {});
              return (
                <TableRow key={job.id} className={cn(isRunning && "bg-primary/5 animate-in fade-in duration-1000")}>
                  <TableCell className="font-medium relative overflow-hidden">
                    {isRunning && <div className="absolute left-0 top-0 bottom-0 w-1 bg-primary animate-pulse" />}
                    <Link to="/jobs/$jobId" params={{ jobId: job.id }} className="hover:underline text-primary">
                      {job.alias}
                    </Link>
                    <div className="text-[10px] font-mono text-muted-foreground">{job.id.substring(0, 8)}</div>
                  </TableCell>
                  <TableCell>
                    <div className="flex flex-wrap gap-1">
                      {labelEntries.map(([k, v]) => (
                        <button
                          key={k}
                          onClick={() => { setLabelFilter(labelFilter === k ? null : k); setPage(0); }}
                          title={`Filter by label: ${k}`}
                          className="text-[10px] font-mono bg-muted rounded px-1.5 py-0.5 text-muted-foreground hover:text-primary hover:bg-primary/10 transition-colors"
                        >
                          {k}={String(v)}
                        </button>
                      ))}
                    </div>
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
                    <div className="flex items-center justify-end gap-1">
                      <Button
                        variant="ghost"
                        size="icon"
                        onClick={() => triggerMutation.mutate(job.id)}
                        disabled={triggerMutation.isPending}
                        title="Trigger Run"
                      >
                        <Play className="h-4 w-4" />
                      </Button>
                      {deleteConfirm === job.id ? (
                        <div className="flex items-center gap-1">
                          <Button
                            variant="destructive"
                            size="sm"
                            onClick={() => deleteMutation.mutate(job.id)}
                            disabled={deleteMutation.isPending}
                          >
                            Confirm
                          </Button>
                          <Button variant="ghost" size="sm" onClick={() => setDeleteConfirm(null)}>
                            Cancel
                          </Button>
                        </div>
                      ) : (
                        <Button
                          variant="ghost"
                          size="icon"
                          onClick={() => setDeleteConfirm(job.id)}
                          title="Delete Job"
                          className="text-muted-foreground hover:text-destructive"
                        >
                          <Trash2 className="h-4 w-4" />
                        </Button>
                      )}
                    </div>
                  </TableCell>
                </TableRow>
              );
            })}
          </TableBody>
        </Table>
      </div>

      {/* Pagination */}
      {totalPages > 1 && (
        <div className="flex items-center justify-between text-sm text-muted-foreground">
          <span>
            Showing {page * PAGE_SIZE + 1}–{Math.min((page + 1) * PAGE_SIZE, filtered.length)} of {filtered.length}
          </span>
          <div className="flex items-center gap-2">
            <Button variant="outline" size="icon" onClick={() => setPage(p => p - 1)} disabled={page === 0}>
              <ChevronLeft className="h-4 w-4" />
            </Button>
            <span className="px-2">Page {page + 1} of {totalPages}</span>
            <Button variant="outline" size="icon" onClick={() => setPage(p => p + 1)} disabled={page >= totalPages - 1}>
              <ChevronRight className="h-4 w-4" />
            </Button>
          </div>
        </div>
      )}
    </div>
  );
}
