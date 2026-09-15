import type { CacheConfigValue, CallbackRun, JobRun, TaskRun } from "@/lib/api";

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

const terminalRunStatuses = new Set(["succeeded", "failed", "cancelled", "skipped"]);
const terminalCallbackStatuses = new Set(["succeeded", "failed"]);

/** Whether a run no longer receives task lifecycle events. */
export function isTerminalRunStatus(status: string | undefined): boolean {
  return status !== undefined && terminalRunStatuses.has(status);
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

/**
 * Terminal SSE snapshots are written before completion callbacks are
 * dispatched. Their `callbacks` collection is therefore often explicitly
 * empty even when a later REST response has persisted callback deliveries.
 * Preserve those deliveries while applying the event's task/status updates.
 * A mismatched payload is ignored as an additional cache fence.
 */
export function mergeTerminalRunUpdate(current: JobRun, terminal: JobRun): JobRun {
  if (terminal.id !== current.id) {
    return current;
  }

  return {
    ...current,
    ...terminal,
    callbacks: mergeCallbackRuns(current.callbacks, terminal.callbacks),
  };
}

function mergeCallbackRuns(
  current: CallbackRun[] | undefined,
  terminal: CallbackRun[] | undefined,
): CallbackRun[] | undefined {
  if (!current?.length) return terminal;
  if (!terminal?.length) return current;

  const currentByID = new Map(current.map((callback) => [callback.id, callback]));
  const unseen: CallbackRun[] = [];
  for (const callback of terminal) {
    const existing = currentByID.get(callback.id);
    if (existing) {
      currentByID.set(callback.id, newerCallbackRun(existing, callback));
    } else {
      unseen.push(callback);
    }
  }

  return [...current.map((callback) => currentByID.get(callback.id) ?? callback), ...unseen];
}

function newerCallbackRun(current: CallbackRun, incoming: CallbackRun): CallbackRun {
  const currentTerminal = terminalCallbackStatuses.has(current.status);
  const incomingTerminal = terminalCallbackStatuses.has(incoming.status);
  if (incomingTerminal !== currentTerminal) {
    return incomingTerminal ? incoming : current;
  }

  const currentTimestamp = current.completed_at ?? current.started_at;
  const incomingTimestamp = incoming.completed_at ?? incoming.started_at;
  return Date.parse(incomingTimestamp) > Date.parse(currentTimestamp) ? incoming : current;
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
  return describeNormalizedCachePolicy(normalizeCacheConfig(raw));
}

/** Applies job defaults before a task's explicit boolean or partial-map override. */
export function resolveCachePolicy(task?: CacheConfigValue, job?: CacheConfigValue): CachePolicySummary {
  const inherited = normalizeCacheConfig(job);
  if (task === undefined || task === null) {
    return inherited;
  }
  if (typeof task === "boolean") {
    return { ...inherited, enabled: task };
  }

  return {
    enabled: task.enabled ?? true,
    ttl: task.ttl ?? inherited.ttl,
    version: task.version ?? inherited.version,
  };
}

/** Includes whether the row inherits its job policy or overrides it itself. */
export function describeEffectiveCachePolicy(task?: CacheConfigValue, job?: CacheConfigValue): string {
  const policy = describeNormalizedCachePolicy(resolveCachePolicy(task, job));
  if (task !== undefined && task !== null) {
    return `Override: ${policy}`;
  }
  if (job !== undefined && job !== null) {
    return `Inherited: ${policy}`;
  }
  return "Server default";
}

function describeNormalizedCachePolicy(normalized: CachePolicySummary): string {
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
