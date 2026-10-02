import { StatusBadge } from "@/components/ui/status-badge";
import type { TaskRun } from "@/lib/api";

export function DagCounters({ tasks }: { tasks?: TaskRun[] }) {
  if (!tasks || tasks.length === 0) return null;

  const succeeded = tasks.filter((task) => task.status === "succeeded" || task.status === "completed").length;
  const running = tasks.filter((task) => task.status === "running").length;
  const cached = tasks.filter((task) => task.status === "cached").length;
  const failed = tasks.filter((task) => task.status === "failed").length;
  const blocked = tasks.filter((task) => task.status === "blocked").length;
  const skipped = tasks.filter((task) => task.status === "skipped").length;
  const cancelled = tasks.filter((task) => task.status === "cancelled").length;
  const waiting = tasks.filter((task) => task.status === "pending" || task.status === "queued").length;

  const parts = [
    { status: "succeeded", label: `${succeeded} succeeded`, count: succeeded },
    { status: "failed", label: `${failed} failed`, count: failed },
    { status: "skipped", label: `${skipped} skipped`, count: skipped },
  ];

  if (blocked > 0) parts.push({ status: "blocked", label: `${blocked} blocked`, count: blocked });
  if (cancelled > 0) parts.push({ status: "cancelled", label: `${cancelled} cancelled`, count: cancelled });

  if (running > 0) {
    parts.push({ status: "running", label: `${running} running`, count: running });
  }
  if (cached > 0) {
    parts.push({ status: "cached", label: `${cached} cached`, count: cached });
  }
  if (waiting > 0) {
    parts.push({ status: "queued", label: `${waiting} waiting`, count: waiting });
  }

  return (
    <div data-testid="dag-counters" className="flex min-w-0 max-w-full flex-wrap items-center gap-x-4 gap-y-2 text-xs tabular-nums">
      {parts.map((part) => (
        <span key={part.label} className="inline-flex items-center gap-1.5">
          <StatusBadge status={part.status} label={part.label} size="sm" muted={part.count === 0} />
        </span>
      ))}
    </div>
  );
}
