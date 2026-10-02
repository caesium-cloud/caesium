import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api, type Job, type JobRun } from "@/lib/api";
import { events, type CaesiumEvent } from "@/lib/events";
import { mergeLatestRun } from "./run-updates";
import type { RunSummary } from "@/components/ui/run-strip";

export type StatusFilter = "all" | "running" | "succeeded" | "failed" | "paused";
export type SortKey = "alias" | "status" | "last_run";
export type HistoryWindow = 900 | 3600 | 86400;

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

interface PendingActivityEntry {
  type: string;
  jobId: string;
  runId?: string;
  timestamp: string;
}

const STATUS_FILTERS: StatusFilter[] = ["all", "running", "succeeded", "failed", "paused"];
const SORT_KEYS: SortKey[] = ["alias", "status", "last_run"];
const ACTIVITY_LIMIT = 20;
const ACTIVITY_KEY_LIMIT = 100;
const RUN_ACTIVITY_EVENTS = ["run_started", "run_retried", "run_completed", "run_failed", "run_cancelled"] as const;

function readUrlParams(): { status: StatusFilter; q: string; sort: SortKey; historyWindow: HistoryWindow } {
  const params = new URLSearchParams(window.location.search);
  const status = params.get("status") as StatusFilter | null;
  const sort = params.get("sort") as SortKey | null;
  const history = Number(params.get("history"));
  return {
    status: status && STATUS_FILTERS.includes(status) ? status : "all",
    q: params.get("q") ?? "",
    sort: sort && SORT_KEYS.includes(sort) ? sort : "alias",
    historyWindow: history === 3600 || history === 86400 ? history : 900,
  };
}

function writeUrlParams(status: StatusFilter, q: string, sort: SortKey, historyWindow: HistoryWindow) {
  const params = new URLSearchParams();
  if (status !== "all") params.set("status", status);
  if (q) params.set("q", q);
  if (sort !== "alias") params.set("sort", sort);
  if (historyWindow !== 900) params.set("history", String(historyWindow));
  const search = params.toString();
  const url = search ? `${window.location.pathname}?${search}` : window.location.pathname;
  window.history.replaceState(window.history.state, "", url);
}

function activityKey(e: CaesiumEvent, jobId: string, runId?: string) {
  if (runId) return `${e.type}:${runId}`;
  if (e.sequence !== undefined) return `${e.type}:sequence:${e.sequence}`;
  return `${e.type}:${jobId}:${e.timestamp}`;
}

function rememberActivityKey(keys: Set<string>, key: string) {
  if (keys.has(key)) return false;
  keys.add(key);
  while (keys.size > ACTIVITY_KEY_LIMIT) {
    const oldest = keys.values().next().value;
    if (oldest === undefined) break;
    keys.delete(oldest);
  }
  return true;
}

export function useJobsView() {
  const queryClient = useQueryClient();
  const [streamHealthy, setStreamHealthy] = useState(events.isHealthy());

  const initial = useMemo(() => readUrlParams(), []);
  const [statusFilter, setStatusFilterState] = useState<StatusFilter>(initial.status);
  const [search, setSearchState] = useState(initial.q);
  const [sort, setSortState] = useState<SortKey>(initial.sort);
  const [historyWindow, setHistoryWindow] = useState(initial.historyWindow);
  const [activity, setActivity] = useState<ActivityEntry[]>([]);
  const activityIdRef = useRef(0);
  const activityKeysRef = useRef(new Set<string>());
  const jobAliasesRef = useRef(new Map<string, string>());
  const pendingActivityRef = useRef<PendingActivityEntry[]>([]);

  const setStatusFilter = useCallback((f: StatusFilter) => setStatusFilterState(f), []);
  const setSearch = useCallback((s: string) => setSearchState(s), []);
  const setSort = useCallback((s: SortKey) => setSortState(s), []);

  useEffect(() => {
    writeUrlParams(statusFilter, search, sort, historyWindow);
  }, [statusFilter, search, sort, historyWindow]);

  const { data: jobs, isLoading, error } = useQuery({
    queryKey: ["jobs"],
    queryFn: api.getJobs,
    refetchInterval: streamHealthy ? false : 15000,
  });

  const appendActivity = useCallback((draft: PendingActivityEntry, alias: string) => {
    setActivity((prev) => {
      const entry: ActivityEntry = {
        id: String(++activityIdRef.current),
        type: draft.type,
        jobId: draft.jobId,
        jobAlias: alias,
        runId: draft.runId,
        timestamp: draft.timestamp,
      };
      return [entry, ...prev].slice(0, ACTIVITY_LIMIT);
    });
  }, []);

  useEffect(() => {
    const aliases = new Map<string, string>();
    jobs?.forEach((job) => aliases.set(job.id, job.alias));
    jobAliasesRef.current = aliases;

    if (pendingActivityRef.current.length === 0) return;

    const ready: Array<{ draft: PendingActivityEntry; alias: string }> = [];
    const pending: PendingActivityEntry[] = [];
    pendingActivityRef.current.forEach((draft) => {
      const alias = aliases.get(draft.jobId);
      if (alias) {
        ready.push({ draft, alias });
      } else {
        pending.push(draft);
      }
    });
    pendingActivityRef.current = pending;

    if (ready.length === 0) return;

    setActivity((prev) => {
      const entries = ready
        .slice()
        .reverse()
        .map(({ draft, alias }) => ({
          id: String(++activityIdRef.current),
          type: draft.type,
          jobId: draft.jobId,
          jobAlias: alias,
          runId: draft.runId,
          timestamp: draft.timestamp,
        }));
      return [...entries, ...prev].slice(0, ACTIVITY_LIMIT);
    });
  }, [jobs]);

  useEffect(() => {
    const onConnection = (healthy: boolean) => setStreamHealthy(healthy);
    let refreshTimer: ReturnType<typeof setTimeout> | undefined;
    const refreshHistory = () => {
      // Fixed coalescing window: sustained events cannot postpone a refresh.
      if (refreshTimer !== undefined) return;
      refreshTimer = setTimeout(() => {
        refreshTimer = undefined;
        queryClient.invalidateQueries({ queryKey: ["jobs"] });
      }, 250);
    };

    const onRunEvent = (e: CaesiumEvent) => {
      const payload = e.payload as JobRun | undefined;
      const run = payload && payload.id ? payload : undefined;
      const jobID = run?.job_id ?? e.job_id;

      if (!jobID) {
        refreshHistory();
        return;
      }

      queryClient.setQueryData(["jobs"], (old: Job[] | undefined) =>
        old?.map((job) =>
          job.id === jobID
            ? { ...job, latest_run: run ? mergeLatestRun(job.latest_run, run) : job.latest_run }
            : job,
        ),
      );
      if (run) {
        queryClient.setQueryData(["job", jobID], (old: Job | undefined) =>
          old ? { ...old, latest_run: mergeLatestRun(old.latest_run, run) } : old,
        );
      }

      const payloadAlias = run?.job_alias?.trim();
      const cachedAlias =
        jobAliasesRef.current.get(jobID) ??
        queryClient.getQueryData<Job[]>(["jobs"])?.find((j) => j.id === jobID)?.alias;
      const alias = payloadAlias || cachedAlias;
      const runId = e.run_id ?? run?.id;
      const key = activityKey(e, jobID, runId);

      if (!rememberActivityKey(activityKeysRef.current, key)) return;

      // The list projection owns the bounded history as well as its durations.
      // Updating only latest_run leaves the strip frozen after an SSE event.
      refreshHistory();

      const draft: PendingActivityEntry = {
        type: e.type,
        jobId: jobID,
        runId,
        timestamp: e.timestamp ?? new Date().toISOString(),
      };

      if (alias) {
        appendActivity(draft, alias);
        return;
      }

      pendingActivityRef.current.push(draft);
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
    RUN_ACTIVITY_EVENTS.forEach((t) => events.subscribe(t, onRunEvent));
    ["job_paused", "job_unpaused"].forEach((t) => events.subscribe(t, onPauseEvent));
    events.subscribe("task_cached", onTaskCached);

    return () => {
      clearTimeout(refreshTimer);
      events.unsubscribeConnection(onConnection);
      RUN_ACTIVITY_EVENTS.forEach((t) => events.unsubscribe(t, onRunEvent));
      ["job_paused", "job_unpaused"].forEach((t) => events.unsubscribe(t, onPauseEvent));
      events.unsubscribe("task_cached", onTaskCached);
    };
  }, [appendActivity, queryClient]);

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

    return sorted.map((job) => {
      const history = [...(job.last_runs ?? [])];
      const latest = job.latest_run;
      // SSE updates latest_run immediately; the list's bounded history can lag.
      // Preserve the server timestamp (including sub-ms precision) to reconcile it.
      if (latest && Number.isFinite(Date.parse(latest.started_at))) {
        const duration = latest.completed_at
          ? (Date.parse(latest.completed_at) - Date.parse(latest.started_at)) / 1000
          : null;
        const entry = { started_at: latest.started_at, status: latest.status,
          duration: duration !== null && Number.isFinite(duration) && duration >= 0 ? duration : null };
        const index = history.findIndex(run => run.started_at === latest.started_at);
        if (index >= 0) history[index] = entry;
        else history.push(entry);
      }
      return { ...job, lastRuns: history
        .sort((a, b) => a.started_at.localeCompare(b.started_at))
        .slice(-10)
        .map(r => ({ status: r.status, duration: r.duration ?? null, startedAt: r.started_at })) };
    });
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
    historyWindow,
    setHistoryWindow,
    isLoading,
    error: error as Error | null,
    activity,
  };
}
