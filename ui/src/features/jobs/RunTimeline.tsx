import { useState } from "react";
import type { TaskRun } from "@/lib/api";
import { shortId } from "@/lib/utils";

interface Props {
  tasks: TaskRun[];
  runStartedAt: string;
}

const STATUS_COLORS: Record<string, string> = {
  succeeded: "#22c55e",
  cached: "#14b8a6",
  failed: "#ef4444",
  running: "#00b4d8",
  skipped: "#a3a3a3",
  pending: "#6b7280",
};

function formatMs(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60000) return `${(ms / 1000).toFixed(1)}s`;
  return `${Math.floor(ms / 60000)}m ${Math.floor((ms % 60000) / 1000)}s`;
}

export function RunTimeline({ tasks, runStartedAt }: Props) {
  const runStart = new Date(runStartedAt).getTime();
  // Pass Date.now as an initializer reference (not called during render) so
  // React captures wall-clock time once at mount without violating purity rules.
  const [now] = useState<number>(Date.now);

  // Only show tasks that have started
  const startedTasks = tasks.filter(t => t.started_at || t.status === "cached");
  if (startedTasks.length === 0) {
    return (
      <div className="flex items-center justify-center h-32 text-muted-foreground text-sm">
        No task execution data available yet.
      </div>
    );
  }

  // Compute relative start/end offsets in ms
  const taskTimes = startedTasks.map(t => {
    const startAt = t.started_at ?? t.created_at;
    const start = new Date(startAt).getTime() - runStart;
    const end = t.completed_at
      ? new Date(t.completed_at).getTime() - runStart
      : now - runStart;
    return { task: t, start: Math.max(0, start), end: Math.max(0, end) };
  });

  const maxEnd = Math.max(...taskTimes.map(t => t.end), 1);

  const ROW_H = 36;
  const LABEL_W = 180;
  const BAR_AREA = 600;
  const BAR_H = 18;
  const BAR_Y_OFFSET = (ROW_H - BAR_H) / 2;
  const svgHeight = taskTimes.length * ROW_H + 30; // +30 for time axis

  // Time axis ticks (5 ticks)
  const ticks = Array.from({ length: 6 }, (_, i) => Math.round((maxEnd / 5) * i));

  return (
    <div className="overflow-x-auto">
      <svg
        width={LABEL_W + BAR_AREA + 20}
        height={svgHeight}
        className="font-mono"
        style={{ fontFamily: "ui-monospace, monospace" }}
      >
        {/* Background rows */}
        {taskTimes.map((_, i) => (
          <rect
            key={i}
            x={0}
            y={i * ROW_H}
            width={LABEL_W + BAR_AREA + 20}
            height={ROW_H}
            fill={i % 2 === 0 ? "transparent" : "rgba(128,128,128,0.04)"}
          />
        ))}

        {/* Grid lines */}
        {ticks.map((tick, i) => {
          const x = LABEL_W + (tick / maxEnd) * BAR_AREA;
          return (
            <line
              key={i}
              x1={x}
              y1={0}
              x2={x}
              y2={svgHeight - 28}
              stroke="rgba(128,128,128,0.15)"
              strokeDasharray="3,3"
            />
          );
        })}

        {/* Task rows */}
        {taskTimes.map(({ task, start, end }, i) => {
          const barX = LABEL_W + (start / maxEnd) * BAR_AREA;
          const barW = Math.max(2, ((end - start) / maxEnd) * BAR_AREA);
          const color = STATUS_COLORS[task.status] ?? STATUS_COLORS.pending;
          const label = task.image
            ? task.image.split("/").pop()?.split(":")[0] ?? shortId(task.atom_id)
            : shortId(task.atom_id);
          const duration = end - start;

          return (
            <g key={task.id}>
              {/* Task label */}
              <text
                x={LABEL_W - 8}
                y={i * ROW_H + ROW_H / 2 + 4}
                textAnchor="end"
                fontSize={11}
                fill="currentColor"
                className="fill-muted-foreground"
              >
                {label.length > 18 ? label.substring(0, 16) + "…" : label}
              </text>

              {/* Bar background */}
              <rect
                x={LABEL_W}
                y={i * ROW_H + BAR_Y_OFFSET}
                width={BAR_AREA}
                height={BAR_H}
                fill="rgba(128,128,128,0.08)"
                rx={3}
              />

              {/* Bar */}
              <rect
                x={barX}
                y={i * ROW_H + BAR_Y_OFFSET}
                width={barW}
                height={BAR_H}
                fill={color}
                fillOpacity={task.status === "running" ? 0.9 : 0.75}
                rx={3}
              >
                {task.status === "running" && (
                  <animate
                    attributeName="fillOpacity"
                    values="0.6;0.95;0.6"
                    dur="1.5s"
                    repeatCount="indefinite"
                  />
                )}
              </rect>

              {/* Duration label inside bar if wide enough */}
              {barW > 40 && (
                <text
                  x={barX + barW / 2}
                  y={i * ROW_H + BAR_Y_OFFSET + BAR_H / 2 + 4}
                  textAnchor="middle"
                  fontSize={9}
                  fill="white"
                  fontWeight="600"
                >
                  {formatMs(duration)}
                </text>
              )}

              {/* Status dot */}
              <circle
                cx={barX + barW + 6}
                cy={i * ROW_H + ROW_H / 2}
                r={3}
                fill={color}
              />
            </g>
          );
        })}

        {/* Time axis */}
        <line
          x1={LABEL_W}
          y1={svgHeight - 28}
          x2={LABEL_W + BAR_AREA}
          y2={svgHeight - 28}
          stroke="rgba(128,128,128,0.3)"
        />
        {ticks.map((tick, i) => {
          const x = LABEL_W + (tick / maxEnd) * BAR_AREA;
          return (
            <g key={i}>
              <line x1={x} y1={svgHeight - 28} x2={x} y2={svgHeight - 22} stroke="rgba(128,128,128,0.4)" />
              <text
                x={x}
                y={svgHeight - 10}
                textAnchor="middle"
                fontSize={9}
                fill="currentColor"
                className="fill-muted-foreground"
              >
                {formatMs(tick)}
              </text>
            </g>
          );
        })}
      </svg>

      {/* Legend */}
      <div className="flex flex-wrap gap-4 mt-3 px-1">
        {Object.entries(STATUS_COLORS).map(([status, color]) => (
          <div key={status} className="flex items-center gap-1.5 text-xs text-muted-foreground">
            <span className="inline-block w-3 h-3 rounded-sm" style={{ backgroundColor: color, opacity: 0.75 }} />
            {status}
          </div>
        ))}
      </div>
    </div>
  );
}
