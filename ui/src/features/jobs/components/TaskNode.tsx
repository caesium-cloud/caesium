import { taskPresentation } from "../task-presentation";
import { memo } from "react";
import { Handle, Position, type NodeProps } from "reactflow";
import { isRecord } from "@/lib/typeGuards";
import { cn, formatCommandForDisplay, formatUTCTimestamp, normalizeCommand } from "@/lib/utils";
import { statusMeta, statusKeyForDomain } from "@/lib/status";
import { StatusGlyph } from "@/components/ui/status-badge";
import { fanoutStatusSegments } from "@/lib/fanout";
import { getHandleVisibility } from "./node-edges";
import { ImageReference } from "@/components/ui/image-reference";
import { Duration } from "@/components/duration";

export const TaskNode = memo(({ data }: NodeProps) => {
  const { label, atom, status, isSelected, startedAt, completedAt, engine, command, error, rateLimitRetryAfter, partitionCount, partitionStatusCounts, partitionValue } = data;
  const taskLabel = typeof label === "string" ? label : "";
  const branch = data.taskType === "branch";
  const runtime = getRuntimeHints(atom?.spec);
  const handles = getHandleVisibility(data.edgeDegree);
  const state = statusKeyForDomain(status === "completed" ? "succeeded" : status) ?? "unknown";
  const presentation = taskPresentation({ status: data.recordedStatus ?? status, started_at: startedAt, completed_at: completedAt, updated_at: data.updatedAt ?? "" }, data.runStatus, data.runCompletedAt);
  const meta = { ...statusMeta(state), label: presentation.label };
  const isFanned = typeof partitionCount === "number" && (partitionCount > 1 || (partitionCount > 0 && !!partitionValue));
  const segments = fanoutStatusSegments(partitionStatusCounts as Record<string, number> | undefined);
  const rawCommand = command || atom?.command;
  const args = normalizeCommand(rawCommand);
  const shell = args.length >= 2 && ["sh", "bash", "/bin/sh", "/bin/bash"].includes(args[0]) && args[1] === "-c";
  const visibleCommand = formatCommandForDisplay(shell ? args.slice(2) : rawCommand, "no command");
  const runtimeEngine = String(engine || atom?.engine || "unknown").toLowerCase();
  const image = String(atom?.image || "unknown").split("/").pop() || "unknown";
  const border = state === "running" ? "border-running shadow-[0_0_24px_hsl(var(--running)/.18)]" : state === "succeeded" ? "border-success/45" : state === "failed" ? "border-danger" : state === "cached" ? "border-cached border-dashed" : state === "skipped" ? "border-border" : state === "unknown" ? "border-border border-dashed" : "border-gold/50";
  const note = presentation.note ?? (error ? String(error).split("\n")[0] : rateLimitRetryAfter ? `Rate-limited until ${formatRetryAfter(rateLimitRetryAfter)}` : state === "cached" ? "Successful output restored from cache. No container started." : startedAt ? data.runStartedAt ? `started at +${Math.max(0, (Date.parse(startedAt) - Date.parse(data.runStartedAt)) / 1000).toFixed(2)}s` : `started at ${formatUTCTimestamp(startedAt)}` : state === "skipped" ? "branch chose another path" : "waits on upstream work");
  const prefix = branch ? "branch" : "task";
  return <div className="relative h-[112px] w-[260px]">
    {isFanned ? <>
      <div aria-hidden="true" data-testid="fanout-stack-card" className="absolute inset-0 -z-20 translate-x-2 translate-y-2 rounded-lg border border-border bg-node-surface" />
      <div aria-hidden="true" data-testid="fanout-stack-card" className="absolute inset-0 -z-10 translate-x-1 translate-y-1 rounded-lg border border-border bg-node-surface" />
    </> : null}
    <div className={cn("relative h-full rounded-lg border bg-midnight px-3", border, isSelected && "ring-2 ring-ring ring-offset-2 ring-offset-background")}>
      {["running", "succeeded", "cached", "queued"].includes(state) ? <span aria-hidden="true" data-testid="task-node-electron" className={`cs-electron cs-electron-${state}`} /> : null}
      {handles.showTargetHandle ? <Handle data-testid={`${prefix}-node-target-handle`} type="target" position={Position.Left} className="h-2 w-2 border border-dag-bg bg-cyan" /> : null}
      <div className="flex h-8 items-center gap-2">
        <StatusGlyph meta={meta} testId={`status-icon-${status === "pending" ? "pending" : state}`} />
        <span className="sr-only">{meta.label}</span>
        <span data-testid="task-node-label" title={taskLabel} className="min-w-0 flex-1 truncate text-sm font-bold text-text-1">{taskLabel}</span>
        {isFanned ? <span data-testid="fanout-badge" className="text-[11px] text-text-3">×{partitionCount}</span> : null}
        <span className={cn("text-[11px] tabular-nums", state === "running" ? "text-running" : "text-text-3")}>
          {startedAt ? <>{presentation.uncertain ? "≥" : ""}<Duration start={startedAt} end={presentation.end} /></> : ""}
        </span>
      </div>
      {segments.length > 0 ? <div data-testid="fanout-status-strip" title="Partition status breakdown" className="absolute left-0 right-0 top-8 flex h-1 overflow-hidden">
        {segments.map(segment => <span key={segment.status} data-testid="fanout-status-segment" data-status={segment.status} title={`${segment.status}: ${segment.count}`} style={{ width: `${segment.fraction * 100}%`, backgroundColor: statusMeta(segment.status).fg }} />)}
      </div> : null}
      <div className="flex h-5 min-w-0 items-center gap-2 text-[11px] text-text-3">
        <span data-testid="task-node-image" title={atom?.image} className="min-w-0 flex-1 truncate"><ImageReference image={image} /></span>
        {shell ? <span>shell</span> : null}
        {runtime.volumeCount > 0 ? <span data-testid="runtime-volume-badge" title={`${runtime.volumeCount} resolved volume ${runtime.volumeCount === 1 ? "mount" : "mounts"}`}>volume {runtime.volumeCount}</span> : null}
        {runtime.hasKubernetesIdentity ? <span data-testid="runtime-identity-badge" title={runtime.serviceAccountName ? `ServiceAccount ${runtime.serviceAccountName}` : "Kubernetes pod identity settings"}>SA</span> : null}
        <span data-testid={`engine-icon-${runtimeEngine.includes("k8s") ? "kubernetes" : runtimeEngine}`}>{branch ? <span className="text-gold">branch</span> : runtimeEngine}</span>
      </div>
      <div className="mt-1 flex h-6 min-w-0 items-center gap-2 rounded-sm bg-void px-2 text-[11px]" title={visibleCommand}>
        <span className="text-cyan">$</span>
        <span className="min-w-0 truncate text-text-2">{visibleCommand}</span>
      </div>
      <div data-testid={rateLimitRetryAfter ? "task-rate-limit-indicator" : "task-node-note"} className={cn("mt-1 truncate text-[11px] italic", error && state !== "skipped" ? "text-danger" : rateLimitRetryAfter ? "text-gold" : "text-text-3")} title={note}>{presentation.incomplete ? `${presentation.label} · last reported ${data.recordedStatus ?? status}` : note}</div>
      {handles.showSourceHandle ? <Handle data-testid={`${prefix}-node-source-handle`} type="source" position={Position.Right} className="h-2 w-2 border border-dag-bg bg-cyan" /> : null}
    </div>
  </div>;
});
TaskNode.displayName = "TaskNode";

function formatRetryAfter(value: unknown) {
  if (typeof value !== 'string' || value.trim() === '') {
    return 'the current window resets';
  }
  return formatUTCTimestamp(value, value);
}

function getRuntimeHints(spec: unknown) {
  if (!isRecord(spec)) {
    return {
      volumeCount: 0,
      hasKubernetesIdentity: false,
      serviceAccountName: '',
    };
  }

  const resolvedVolumeMounts = Array.isArray(spec.resolvedVolumeMounts)
    ? spec.resolvedVolumeMounts
    : [];
  const kubernetes = isRecord(spec.kubernetes) ? spec.kubernetes : null;
  const rawServiceAccountName = kubernetes?.serviceAccountName;
  const serviceAccountName = typeof rawServiceAccountName === 'string'
    ? rawServiceAccountName.trim()
    : '';
  const rawPodAnnotations = kubernetes?.podAnnotations;
  const hasPodAnnotations = isRecord(rawPodAnnotations) && Object.keys(rawPodAnnotations).length > 0;
  const rawAutomountServiceAccountToken = kubernetes?.automountServiceAccountToken;
  const hasAutomountSetting = typeof rawAutomountServiceAccountToken === 'boolean';

  return {
    volumeCount: resolvedVolumeMounts.length,
    hasKubernetesIdentity: serviceAccountName !== '' || hasPodAnnotations || hasAutomountSetting,
    serviceAccountName,
  };
}
