import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api, type Job, type JobRun } from "@/lib/api";
import { events, type CaesiumEvent } from "@/lib/events";
import type { RunSummary } from "@/components/ui/sparkline";

export type StatusFilter = "all" | "running" | "succeeded" | "failed" | "paused";
export type SortKey = "alias" | "status" | "last_run";

export interface JobRow extends Job {
  lastRuns: RunSummary[];
}

export interface JobCounts {
  all: number;
  running: number;
  succeeded: number;
  failed: number;
  paused: number;
}

export interface ActivityEntry {
  id: string;
  type: string;
  jobId: string;
  jobAlias: string;
  runId?: string;
  timestamp: string;
}

const STATUS_FILTERS: StatusFilter[] = ["all", "running", "succeeded", "failed", "paused"];
const SORT_KEYS: SortKey[] = ["alias", "status", "last_run"];

function readUrlParams(): { status: StatusFilter; q: string; sort: SortKey } {
  const params = new URLSearchParams(window.location.search);
  const status = params.get("status") as StatusFilter | null;
  const sort = params.get("sort") as SortKey | null;
  return {
    status: status && STATUS_FILTERS.includes(status) ? status : "all",
    q: params.get("q") ?? "",
    sort: sort && SORT_KEYS.includes(sort) ? sort : "alias",
  };
}

function writeUrlParams(status: StatusFilter, q: string, sort: SortKey) {
  const params = new URLSearchParams();
  if (status !== "all") params.set("status", status);
  if (q) params.set("q", q);
  if (sort !== "alias") params.set("sort", sort);
  const search = params.toString();
  const url = search ? `${window.location.pathname}?${search}` : window.location.pathname;
  window.history.replaceState(null, "", url);
}

type BackendJobRun = { status: string; duration?: number };
type JobWithLastRuns = Job & { last_runs?: BackendJobRun[] };

export function useJobsView() {
  const queryClient = useQueryClient();
  const [streamHealthy, setStreamHealthy] = useState(events.isHealthy());

  const initial = useMemo(() => readUrlParams(), []);
  const [statusFilter, setStatusFilterState] = useState<StatusFilter>(initial.status);
  const [search, setSearchState] = useState(initial.q);
  const [sort, setSortState] = useState<SortKey>(initial.sort);
  const [activity, setActivity] = useState<ActivityEntry[]>([]);
  const activityIdRef = useRef(0);

  const setStatusFilter = useCallback((f: StatusFilter) => setStatusFilterState(f), []);
  const setSearch = useCallback((s: string) => setSearchState(s), []);
  const setSort = useCallback((s: SortKey) => setSortState(s), []);

  useEffect(() => {
    writeUrlParams(statusFilter, search, sort);
  }, [statusFilter, search, sort]);

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
      const jobID = run?.job_id ?? e.job_id;

      if (!jobID) {
        queryClient.invalidateQueries({ queryKey: ["jobs"] });
        return;
      }

      queryClient.setQueryData(["jobs"], (old: Job[] | undefined) =>
        old?.map((job) =>
          job.id === jobID
            ? { ...job, latest_run: run ? { ...job.latest_run, ...run } : job.latest_run }
            : job,
        ),
      );
      if (run) {
        queryClient.setQueryData(["job", jobID], (old: Job | undefined) =>
          old ? { ...old, latest_run: { ...old.latest_run, ...run } } : old,
        );
      }

      const alias =
        run?.job_alias ??
        queryClient.getQueryData<Job[]>(["jobs"])?.find((j) => j.id === jobID)?.alias ??
        jobID;

      setActivity((prev) => {
        const entry: ActivityEntry = {
          id: String(++activityIdRef.current),
          type: e.type,
          jobId: jobID,
          jobAlias: alias,
          runId: e.run_id ?? run?.id,
          timestamp: e.timestamp ?? new Date().toISOString(),
        };
        return [entry, ...prev].slice(0, 20);
      });
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
          const lr = job.latest_run;
          if (job.id !== e.job_id || !lr || lr.id !== e.run_id) return job;
          return { ...job, latest_run: { ...lr, cache_hits: (lr.cache_hits ?? 0) + 1 } };
        }),
      );
    };

    events.subscribeConnection(onConnection);
    ["run_started", "run_completed", "run_failed", "run_terminal"].forEach((t) =>
      events.subscribe(t, onRunEvent),
    );
    ["job_paused", "job_unpaused"].forEach((t) => events.subscribe(t, onPauseEvent));
    events.subscribe("task_cached", onTaskCached);

    return () => {
      events.unsubscribeConnection(onConnection);
      ["run_started", "run_completed", "run_failed", "run_terminal"].forEach((t) =>
        events.unsubscribe(t, onRunEvent),
      );
      ["job_paused", "job_unpaused"].forEach((t) => events.unsubscribe(t, onPauseEvent));
      events.unsubscribe("task_cached", onTaskCached);
    };
  }, [queryClient]);

  const counts = useMemo<JobCounts>(() => {
    if (!jobs) return { all: 0, running: 0, succeeded: 0, failed: 0, paused: 0 };
    return jobs.reduce<JobCounts>(
      (acc, job) => {
        acc.all++;
        if (job.paused) acc.paused++;
        const s = job.latest_run?.status;
        if (s === "running") acc.running++;
        else if (s === "succeeded" || s === "completed") acc.succeeded++;
        else if (s === "failed") acc.failed++;
        return acc;
      },
      { all: 0, running: 0, succeeded: 0, failed: 0, paused: 0 },
    );
  }, [jobs]);

  const rows = useMemo<JobRow[]>(() => {
    if (!jobs) return [];

    let filtered: Job[] = jobs;

    if (statusFilter !== "all") {
      filtered = filtered.filter((job) => {
        if (statusFilter === "paused") return job.paused;
        const s = job.latest_run?.status;
        if (statusFilter === "running") return s === "running";
        if (statusFilter === "succeeded") return s === "succeeded" || s === "completed";
        if (statusFilter === "failed") return s === "failed";
        return true;
      });
    }

    if (search.trim()) {
      const q = search.trim().toLowerCase();
      filtered = filtered.filter((job) => job.alias.toLowerCase().includes(q));
    }

    const sorted = [...filtered].sort((a, b) => {
      if (sort === "status") {
        return (a.latest_run?.status ?? "z").localeCompare(b.latest_run?.status ?? "z");
      }
      if (sort === "last_run") {
        return (b.latest_run?.started_at ?? "").localeCompare(a.latest_run?.started_at ?? "");
      }
      return a.alias.localeCompare(b.alias);
    });

    return sorted.map((job) => ({
      ...job,
      lastRuns: ((job as JobWithLastRuns).last_runs ?? [])
        .slice(-14)
        .map((r) => ({ status: r.status, duration: r.duration ?? null })),
    }));
  }, [jobs, statusFilter, search, sort]);

  return {
    rows,
    counts,
    search,
    setSearch,
    statusFilter,
    setStatusFilter,
    sort,
    setSort,
    isLoading,
    error: error as Error | null,
    activity,
  };
}
