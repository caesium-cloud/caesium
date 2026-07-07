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
    { label: `${done} done`, className: "text-success/80" },
    { label: `${failed} failed`, className: failed > 0 ? "text-danger/80" : "text-text-4" },
    { label: `${blocked} blocked`, className: blocked > 0 ? "text-warning/80" : "text-text-4" },
  ];

  if (running > 0) {
    parts.push({ label: `${running} running`, className: "text-cyan-glow/80" });
  }
  if (cached > 0) {
    parts.push({ label: `${cached} cached`, className: "text-cached/80" });
  }
  if (waiting > 0) {
    parts.push({ label: `${waiting} waiting`, className: "text-text-4" });
  }

  return (
    <div data-testid="dag-counters" className="flex flex-wrap items-center gap-1.5 text-[10px] font-mono tabular-nums">
      {parts.map((part, index) => (
        <span key={part.label} className="inline-flex items-center gap-1.5">
          {index > 0 ? <span className="text-text-4">·</span> : null}
          <span className={part.className}>{part.label}</span>
        </span>
      ))}
    </div>
  );
}
