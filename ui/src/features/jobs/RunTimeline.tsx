import type { JobTask, TaskRun } from "@/lib/api";
import { statusMeta } from "@/lib/status";
import { StatusBadge } from "@/components/ui/status-badge";
import { useUTCTick } from "@/components/ui/utc-clock";
import { fanoutStatusSegments } from "@/lib/fanout";

interface Props { tasks: TaskRun[]; taskDefinitions: Record<string, JobTask>; runStartedAt: string; runStatus?: string }
const LEGEND_STATUSES = ["succeeded", "cached", "failed", "running", "skipped", "queued"] as const;

function formatMs(ms: number): string {
  if (ms < 10) return `${Number(ms.toFixed(2))}ms`;
  if (ms < 1000) return `${Math.round(ms)}ms`;
  if (ms < 60000) return `${(ms / 1000).toFixed(1)}s`;
  return `${Math.floor(ms / 60000)}m ${Math.floor((ms % 60000) / 1000)}s`;
}

export function RunTimeline({ tasks, taskDefinitions, runStartedAt, runStatus }: Props) {
  const live = runStatus ? ["running", "queued", "pending"].includes(runStatus) : tasks.some(task => ["running", "queued"].includes(statusMeta(task.status).label));
  const now = useUTCTick(live).getTime();
  const runStart = Date.parse(runStartedAt);
  const ordered = orderTasksByExecution(tasks, taskDefinitions);
  if (!ordered.length || !Number.isFinite(runStart)) return <div className="flex h-32 items-center justify-center text-sm text-text-3">No task execution data available yet.</div>;
  const elapsed = Math.max(0, now - runStart);
  const taskTimes = ordered.map(task => {
    const status = statusMeta(task.status).label;
    const unstarted = !task.started_at && status !== "cached";
    const ghost = live && unstarted;
    const start = ghost ? elapsed : Math.max(0, Date.parse(task.started_at ?? task.completed_at ?? task.created_at) - runStart);
    const end = task.completed_at ? Math.max(start, Date.parse(task.completed_at) - runStart) : status === "running" ? elapsed : start;
    return { task, status, start: Number.isFinite(start) ? start : 0, end: Number.isFinite(end) ? end : 0, ghost, unstarted };
  });
  const span = Math.max(1, ...taskTimes.filter(row => !row.ghost).map(row => row.end), live ? elapsed : 0);
  // Roughly five intervals, including sub-second runs. No extra empty interval.
  const magnitude = 10 ** Math.floor(Math.log10(span / 5));
  const step = [1, 2, 5, 10].map(value => value * magnitude).find(value => span / value <= 5)!;
  const maxEnd = Math.ceil(span / step) * step;
  const nowPosition = Math.min(100, elapsed / maxEnd * 100);
  const ticks = Array.from({ length: Math.round(maxEnd / step) + 1 }, (_, i) => i * step);
  return <div>
    <div className="mb-3 flex flex-wrap justify-end gap-x-4 gap-y-2">{LEGEND_STATUSES.map(status => <StatusBadge key={status} status={status} size="sm" />)}</div>
    <p className="mb-2 text-xs text-text-3 md:hidden">Scroll timeline horizontally → · task names stay visible</p>
    <div className="overflow-x-auto bg-midnight" tabIndex={0} role="region" aria-label="Execution timeline">
      <div className="relative min-w-[620px] [--timeline-gutter:160px] md:[--timeline-gutter:220px]">
        <div className="relative ml-[var(--timeline-gutter)] h-8 text-[11px] text-text-3">{ticks.map((tick, index) => <span data-testid="timeline-tick" key={tick} className="absolute top-1 whitespace-nowrap" style={{ left: `${tick / maxEnd * 100}%`, transform: `translateX(${index === 0 ? 0 : index === ticks.length - 1 ? -100 : -50}%)` }}>{formatMs(tick)}</span>)}</div>
        <div aria-hidden="true" className="pointer-events-none absolute bottom-0 left-[var(--timeline-gutter)] right-0 top-8" style={{ containerType: "inline-size" }}>
          {ticks.map(tick => <span key={tick} className="absolute inset-y-0 border-l border-border" style={{ left: `${tick / maxEnd * 100}%` }} />)}
          {live ? <span data-testid="run-timeline-now" className="absolute inset-y-0 left-0 z-[1] border-l border-running/60" style={{ transform: `translateX(${nowPosition}cqw)` }}>
            <span className="cs-now-head absolute -top-1 h-2 w-2 rounded-full bg-running" style={{ left: nowPosition > 99 ? -8 : -4 }} />
          </span> : null}
        </div>
        {taskTimes.map(({ task, status, start, end, ghost, unstarted }, index) => {
          const label = taskLabel(task, taskDefinitions);
          const fanned = typeof task.partition_count === "number" && (task.partition_count > 1 || (task.partition_count > 0 && !!task.partition_value));
          const segments = fanned ? fanoutStatusSegments(task.partition_status_counts) : [];
          const meta = statusMeta(status);
          const left = ghost ? Math.min(nowPosition, 96) : start / maxEnd * 100;
          const width = ghost ? 4 : (end - start) / maxEnd * 100;
          const annotation = status === "skipped" ? "Skipped" : ghost ? "Waiting on upstream" : unstarted ? "Did not start" : formatMs(end - start);
          const reason = task.error || (status === "skipped" ? "Branch chose another path" : undefined);
          const body = <div className="grid min-h-12 grid-cols-[var(--timeline-gutter)_1fr] border-b border-border">
            <div className="sticky left-0 z-10 border-r border-border bg-midnight py-1.5 pr-3 text-xs text-text-2">
              <div className="flex items-center gap-2"><span className="text-text-3">{String(index + 1).padStart(2, "0")}</span><StatusBadge status={status} variant="glyph" size="sm" /><span className="truncate" title={label}>{label}{fanned ? ` ×${task.partition_count}` : ""}</span></div>
              {reason ? <details className="ml-6 mt-1 text-text-3"><summary className="cursor-pointer">{annotation} · reason</summary><p className="mt-2 break-words [overflow-wrap:anywhere]">{reason}</p></details> : <div className="ml-6 mt-1 text-text-3">{annotation}</div>}
            </div>
            <div className="relative min-w-0">
              <span data-testid="run-timeline-bar" data-ghost={ghost} title={`${label}: ${meta.label}, ${formatMs(end - start)}`} className={`absolute top-5 h-2.5 rounded-sm ${status === "running" && live ? "cs-live-bar cs-duration-bar" : ""}`}
                style={{ left: `${left}%`, width: width > 0 ? `${width}%` : 2, transform: left === 100 ? "translateX(-100%)" : undefined, background: status === "skipped" || ghost ? "transparent" : status === "running" ? "linear-gradient(90deg,hsl(var(--running)/.2),hsl(var(--running)))" : meta.fg, border: status === "skipped" ? "1px dashed hsl(var(--text-3))" : ghost ? "1px solid hsl(var(--gold))" : undefined }} />
              {segments.length ? <div data-testid="run-timeline-density-strip" className="absolute top-9 flex h-1" style={{ left: `${left}%`, width: `${width}%` }}>{segments.map(segment => <span key={segment.status} data-testid="run-timeline-density-segment" data-status={segment.status} style={{ width: `${segment.fraction * 100}%`, backgroundColor: statusMeta(segment.status).fg }} />)}</div> : null}
            </div>
          </div>;
          return <div key={task.id} data-testid="run-timeline-task-row" data-task-id={task.task_id} data-task-name={label} data-started-at={task.started_at ?? ""} data-partition-count={task.partition_count ?? 0}>
            {fanned ? <div data-testid="run-timeline-group-row">{body}</div> : body}
          </div>;
        })}
      </div>
    </div>
    <p className="mt-2 text-xs text-text-3">Elapsed from run start · zero-duration events shown as a marker</p>
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
