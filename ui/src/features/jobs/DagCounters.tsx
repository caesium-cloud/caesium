import { StatusBadge } from "@/components/ui/status-badge";
import type { TaskRun } from "@/lib/api";

export function DagCounters({ tasks }: { tasks?: TaskRun[] }) {
  if (!tasks || tasks.length === 0) return null;

  const done = tasks.filter((task) => task.status === "succeeded" || task.status === "completed").length;
  const running = tasks.filter((task) => task.status === "running").length;
  const cached = tasks.filter((task) => task.status === "cached").length;
  const failed = tasks.filter((task) => task.status === "failed").length;
  const blocked = tasks.filter((task) => task.status === "blocked" || task.status === "skipped").length;
  const waiting = tasks.filter((task) => task.status === "pending" || task.status === "queued").length;

  const parts = [
    { status: "succeeded", label: `${done} done`, className: "text-success" },
    { status: "failed", label: `${failed} failed`, className: failed > 0 ? "text-danger" : "text-text-3" },
    { status: "skipped", label: `${blocked} blocked`, className: blocked > 0 ? "text-warning" : "text-text-3" },
  ];

  if (running > 0) {
    parts.push({ status: "running", label: `${running} running`, className: "text-cyan-glow" });
  }
  if (cached > 0) {
    parts.push({ status: "cached", label: `${cached} cached`, className: "text-cached" });
  }
  if (waiting > 0) {
    parts.push({ status: "queued", label: `${waiting} waiting`, className: "text-text-3" });
  }

  return (
    <div data-testid="dag-counters" className="flex flex-wrap items-center gap-1.5 text-[11px] tabular-nums">
      {parts.map((part, index) => (
        <span key={part.label} className="inline-flex items-center gap-1.5">
          {index > 0 ? <span className="inline-block w-3" aria-hidden="true" /> : null}
          <StatusBadge status={part.status} label={part.label} size="sm" />
        </span>
      ))}
    </div>
  );
}
