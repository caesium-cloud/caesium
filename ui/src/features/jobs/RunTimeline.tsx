import type { JobTask, TaskRun } from "@/lib/api";
import { statusMeta } from "@/lib/status";
import { StatusBadge } from "@/components/ui/status-badge";
import { useUTCTick } from "@/components/ui/utc-clock";
import { fanoutStatusSegments } from "@/lib/fanout";

interface Props { tasks: TaskRun[]; taskDefinitions: Record<string, JobTask>; runStartedAt: string }
const LEGEND_STATUSES = ["succeeded", "cached", "failed", "running", "skipped", "queued"] as const;

function formatMs(ms: number): string {
  if (ms < 1000) return `${Math.round(ms)}ms`;
  if (ms < 60000) return `${(ms / 1000).toFixed(1)}s`;
  return `${Math.floor(ms / 60000)}m ${Math.floor((ms % 60000) / 1000)}s`;
}

export function RunTimeline({ tasks, taskDefinitions, runStartedAt }: Props) {
  const now = useUTCTick().getTime();
  const runStart = Date.parse(runStartedAt);
  const ordered = orderTasksByExecution(tasks, taskDefinitions);
  if (!ordered.length || !Number.isFinite(runStart)) return <div className="flex h-32 items-center justify-center text-sm text-text-3">No task execution data available yet.</div>;
  const live = tasks.some(task => statusMeta(task.status).label === "running" || statusMeta(task.status).label === "queued");
  const elapsed = Math.max(0, now - runStart);
  const taskTimes = ordered.map(task => {
    const status = statusMeta(task.status).label;
    const ghost = !task.started_at && status !== "cached";
    const start = ghost ? elapsed : Math.max(0, Date.parse(task.started_at ?? task.created_at) - runStart);
    const end = task.completed_at ? Math.max(start, Date.parse(task.completed_at) - runStart) : status === "running" ? elapsed : start;
    return { task, status, start: Number.isFinite(start) ? start : 0, end: Number.isFinite(end) ? end : 0, ghost };
  });
  const span = Math.max(1000, ...taskTimes.filter(row => !row.ghost).map(row => row.end), live ? elapsed : 0);
  const step = [1000, 2000, 5000, 10000, 30000, 60000].find(value => span / value <= 8) ?? Math.ceil(span / 480000) * 60000;
  const maxEnd = Math.ceil(span / step) * step + step;
  const nowPosition = Math.min(100, elapsed / maxEnd * 100);
  const ticks = Array.from({ length: Math.floor(maxEnd / step) + 1 }, (_, i) => i * step);
  return <div>
    <div className="mb-2 flex flex-wrap justify-end gap-4">{LEGEND_STATUSES.map(status => <StatusBadge key={status} status={status} size="sm" />)}</div>
    <div className="overflow-x-auto bg-midnight" tabIndex={0} aria-label="Execution timeline">
      <div className="min-w-[800px]">
        {taskTimes.map(({ task, status, start, end, ghost }, index) => {
          const label = taskLabel(task, taskDefinitions);
          const fanned = typeof task.partition_count === "number" && (task.partition_count > 1 || (task.partition_count > 0 && !!task.partition_value));
          const segments = fanned ? fanoutStatusSegments(task.partition_status_counts) : [];
          const meta = statusMeta(status);
          const left = ghost ? Math.min(nowPosition, 96) : start / maxEnd * 100;
          const width = ghost ? 4 : Math.max(.5, (end - start) / maxEnd * 100);
          const body = <div className="grid h-9 grid-cols-[180px_1fr] items-center border-b border-obsidian">
            <div className="flex items-center gap-2 pr-3 text-xs text-text-2"><span className="w-5 text-[11px] text-text-3">{String(index + 1).padStart(2, "0")}</span><StatusBadge status={status} variant="glyph" size="sm" /><span className="truncate" title={label}>{label}{fanned ? ` ×${task.partition_count}` : ""}</span></div>
            <div className="relative h-full">
              {ticks.map(tick => <span key={tick} aria-hidden="true" className="absolute inset-y-0 border-l border-border" style={{ left: `${tick / maxEnd * 100}%` }} />)}
              {live ? <span aria-hidden="true" className="absolute inset-y-0 border-l border-running" style={{ left: `${nowPosition}%` }} /> : null}
              <span title={`${label}: ${meta.label}, ${formatMs(end - start)}`} className={`absolute top-[10px] h-[10px] rounded-sm ${status === "running" ? "shadow-[0_0_10px_hsl(var(--running)/.5)]" : ""}`}
                style={{ left: `${left}%`, width: `${width}%`, background: status === "skipped" || ghost ? "transparent" : status === "running" ? "linear-gradient(90deg,hsl(var(--running)/.2),hsl(var(--running)))" : meta.fg, border: status === "skipped" ? "1px dashed hsl(var(--text-4))" : ghost ? "1px solid hsl(var(--gold))" : undefined }} />
              <span className="absolute top-[21px] max-w-full truncate text-[11px] italic text-text-3" style={{ left: `${Math.min(left, 70)}%` }}>{status === "skipped" ? task.error || "branch chose another path" : ghost ? "waits on upstream work" : formatMs(end - start)}</span>
              {segments.length ? <div data-testid="run-timeline-density-strip" className="absolute top-[29px] flex h-1" style={{ left: `${left}%`, width: `${width}%` }}>{segments.map(segment => <span key={segment.status} data-testid="run-timeline-density-segment" data-status={segment.status} style={{ width: `${segment.fraction * 100}%`, backgroundColor: statusMeta(segment.status).fg }} />)}</div> : null}
            </div>
          </div>;
          return <div key={task.id} data-testid="run-timeline-task-row" data-task-id={task.task_id} data-task-name={label} data-started-at={task.started_at ?? ""} data-partition-count={task.partition_count ?? 0}>
            {fanned ? <div data-testid="run-timeline-group-row">{body}</div> : body}
          </div>;
        })}
        <div className="ml-[180px] h-7 relative text-[11px] text-text-3">{ticks.map(tick => <span key={tick} className="absolute top-2 -translate-x-1/2" style={{ left: `${tick / maxEnd * 100}%` }}>{formatMs(tick)}</span>)}
          {live ? <span aria-hidden="true" className="cs-now-head absolute -top-1 h-[7px] w-[7px] rounded-full bg-running" style={{ left: `${nowPosition}%` }} /> : null}
        </div>
      </div>
    </div>
  </div>;
}

function orderTasksByExecution(tasks: TaskRun[], taskDefinitions: Record<string, JobTask>): TaskRun[] {
  if (tasks.length <= 1) return tasks;

  const topoRank = taskTopologicalRanks(tasks, taskDefinitions);
  const inputOrder = new Map(tasks.map((task, index) => [task.task_id, index]));

  return [...tasks].sort((a, b) => {
    const aStartedAt = executionTimestamp(a);
    const bStartedAt = executionTimestamp(b);
    if (aStartedAt !== bStartedAt) return aStartedAt - bStartedAt;

    const aTopoRank = topoRank.get(a.task_id) ?? Number.POSITIVE_INFINITY;
    const bTopoRank = topoRank.get(b.task_id) ?? Number.POSITIVE_INFINITY;
    if (aTopoRank !== bTopoRank) return aTopoRank - bTopoRank;

    return (inputOrder.get(a.task_id) ?? 0) - (inputOrder.get(b.task_id) ?? 0);
  });
}

function taskTopologicalRanks(tasks: TaskRun[], taskDefinitions: Record<string, JobTask>): Map<string, number> {
  const taskByID = new Map<string, TaskRun>();
  const adjacency = new Map<string, Set<string>>();
  const indegree = new Map<string, number>();
  const inputOrder = new Map<string, number>();

  tasks.forEach((task, index) => {
    taskByID.set(task.task_id, task);
    adjacency.set(task.task_id, new Set());
    indegree.set(task.task_id, 0);
    inputOrder.set(task.task_id, index);
  });

  tasks.forEach((task) => {
    const nextID = taskDefinitions[task.task_id]?.next_id;
    if (!nextID || nextID === task.task_id || !taskByID.has(nextID)) return;

    const edges = adjacency.get(task.task_id);
    if (!edges || edges.has(nextID)) return;

    edges.add(nextID);
    indegree.set(nextID, (indegree.get(nextID) ?? 0) + 1);
  });

  const compareByInputOrder = (a: TaskRun, b: TaskRun) => {
    return (inputOrder.get(a.task_id) ?? 0) - (inputOrder.get(b.task_id) ?? 0);
  };

  const ready = tasks.filter((task) => (indegree.get(task.task_id) ?? 0) === 0).sort(compareByInputOrder);
  const ordered: TaskRun[] = [];

  while (ready.length > 0) {
    const task = ready.shift();
    if (!task) break;
    ordered.push(task);

    for (const nextID of adjacency.get(task.task_id) ?? []) {
      const nextDegree = (indegree.get(nextID) ?? 0) - 1;
      indegree.set(nextID, nextDegree);

      const nextTask = taskByID.get(nextID);
      if (nextTask && nextDegree === 0) {
        ready.push(nextTask);
        ready.sort(compareByInputOrder);
      }
    }
  }

  const orderedIDs = new Set(ordered.map((task) => task.task_id));
  const unresolved = tasks
    .filter((task) => !orderedIDs.has(task.task_id))
    .sort(compareByInputOrder);

  return new Map([...ordered, ...unresolved].map((task, index) => [task.task_id, index]));
}

function executionTimestamp(task: TaskRun): number {
  const parsed = new Date(task.started_at ?? task.created_at).getTime();
  return Number.isFinite(parsed) ? parsed : Number.POSITIVE_INFINITY;
}

function taskLabel(task: TaskRun, taskDefinitions: Record<string, JobTask>): string {
  const name = taskDefinitions[task.task_id]?.name?.trim();
  if (name) return name;

  if (task.image) {
    return task.image.split("/").pop()?.split(":")[0] ?? "task";
  }

  return "task";
}
