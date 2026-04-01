import type { CacheConfigValue, JobRun, TaskRun } from "@/lib/api";

export interface CachePolicySummary {
  enabled: boolean;
  ttl?: string;
  version?: number;
}

export interface RunCacheStats {
  cacheHits: number;
  executedTasks: number;
  totalTasks: number;
}

export function isTaskCached(task?: TaskRun | null): boolean {
  return Boolean(task?.cache_hit || task?.status === "cached");
}

export function getRunCacheStats(run?: Pick<JobRun, "cache_hits" | "executed_tasks" | "total_tasks" | "tasks"> | null): RunCacheStats {
  if (!run) {
    return { cacheHits: 0, executedTasks: 0, totalTasks: 0 };
  }

  if (typeof run.cache_hits === "number" && typeof run.executed_tasks === "number" && typeof run.total_tasks === "number") {
    return {
      cacheHits: run.cache_hits,
      executedTasks: run.executed_tasks,
      totalTasks: run.total_tasks,
    };
  }

  const tasks = run.tasks ?? [];
  let cacheHits = 0;
  let executedTasks = 0;
  for (const task of tasks) {
    if (isTaskCached(task)) {
      cacheHits++;
      continue;
    }
    if (task.status === "running" || task.status === "succeeded" || task.status === "failed") {
      executedTasks++;
    }
  }
  return { cacheHits, executedTasks, totalTasks: tasks.length };
}

export function formatCacheShare(stats: RunCacheStats): string {
  if (stats.totalTasks === 0) {
    return "0%";
  }
  return `${Math.round((stats.cacheHits / stats.totalTasks) * 100)}%`;
}

export function normalizeCacheConfig(raw?: CacheConfigValue): CachePolicySummary {
  if (raw === true) {
    return { enabled: true };
  }
  if (!raw) {
    return { enabled: false };
  }

  return {
    enabled: raw.enabled ?? true,
    ttl: raw.ttl,
    version: raw.version,
  };
}

export function describeCachePolicy(raw?: CacheConfigValue): string {
  const normalized = normalizeCacheConfig(raw);
  if (!normalized.enabled) {
    return "Disabled";
  }

  const fragments = ["Enabled"];
  if (normalized.ttl) {
    fragments.push(`TTL ${normalized.ttl}`);
  }
  if (typeof normalized.version === "number") {
    fragments.push(`v${normalized.version}`);
  }
  return fragments.join(" · ");
}
