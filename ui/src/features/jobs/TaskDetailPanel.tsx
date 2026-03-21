import { useState, useEffect, useRef, useCallback, useLayoutEffect } from "react";
import { Terminal } from "xterm";
import { FitAddon } from "xterm-addon-fit";
import "xterm/css/xterm.css";
import {
  X,
  Info,
  ScrollText,
  AlertTriangle,
  SkipForward,
} from "lucide-react";
import { cn, formatDurationNs, formatKeyValueMap } from "@/lib/utils";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { ScrollArea } from "@/components/ui/scroll-area";
import type { JobTask, TaskRun } from "@/lib/api";

interface TaskDetailPanelProps {
  taskId: string;
  task?: JobTask;
  runTask?: TaskRun;
  taskType?: string;
  jobId: string;
  runId: string;
  onClose: () => void;
}

type TabId = "details" | "logs";

export function TaskDetailPanel({
  taskId,
  task,
  runTask,
  taskType,
  jobId,
  runId,
  onClose,
}: TaskDetailPanelProps) {
  const [activeTab, setActiveTab] = useState<TabId>("logs");
  const [isVisible, setIsVisible] = useState(false);
  // Tracks the pending close animation timer so stale timers can be cancelled.
  // Without this, quickly switching from task A → B could have A's timer fire
  // and call onClose after B's panel has already opened.
  const closeTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  // Animate in on mount
  useLayoutEffect(() => {
    requestAnimationFrame(() => setIsVisible(true));
  }, []);

  // Clear any pending close timer on unmount.
  useEffect(() => {
    return () => {
      if (closeTimerRef.current !== null) clearTimeout(closeTimerRef.current);
    };
  }, []);

  const handleClose = useCallback(() => {
    // Cancel any in-flight close before scheduling a new one.
    if (closeTimerRef.current !== null) clearTimeout(closeTimerRef.current);
    setIsVisible(false);
    // 200 ms matches the `duration-200` slide-out transition on the panel.
    closeTimerRef.current = setTimeout(() => {
      closeTimerRef.current = null;
      onClose();
    }, 200);
  }, [onClose]);

  // Close on Escape key
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") handleClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [handleClose]);

  const resolvedType = taskType || "task";
  const status = runTask?.status ?? "pending";

  return (
    <div
      className={cn(
        "absolute inset-y-0 right-0 z-20 flex w-[520px] max-w-[85%] flex-col",
        "border-l border-border/60 bg-card/95 backdrop-blur-xl",
        "shadow-[-8px_0_32px_rgba(0,0,0,0.25)]",
        "transition-transform duration-200 ease-out",
        isVisible ? "translate-x-0" : "translate-x-full",
      )}
    >
      {/* Header */}
      <div className="flex items-center justify-between border-b border-border/60 px-4 py-3">
        <div className="flex items-center gap-3 min-w-0">
          <StatusDot status={status} />
          <div className="min-w-0">
            <h3 className="truncate text-sm font-semibold text-foreground">
              {taskId}
            </h3>
            <div className="flex items-center gap-2 mt-0.5">
              <Badge variant="outline" className="text-[10px] px-1.5 py-0">
                {status}
              </Badge>
              {resolvedType !== "task" && (
                <Badge variant="outline" className="text-[10px] px-1.5 py-0">
                  {resolvedType}
                </Badge>
              )}
            </div>
          </div>
        </div>
        <Button
          variant="ghost"
          size="icon"
          className="h-7 w-7 shrink-0 text-muted-foreground hover:text-foreground"
          onClick={handleClose}
        >
          <X className="h-4 w-4" />
        </Button>
      </div>

      {/* Tab bar */}
      <div className="flex border-b border-border/60 px-2">
        <TabButton
          active={activeTab === "logs"}
          onClick={() => setActiveTab("logs")}
          icon={<ScrollText className="h-3.5 w-3.5" />}
          label="Logs"
        />
        <TabButton
          active={activeTab === "details"}
          onClick={() => setActiveTab("details")}
          icon={<Info className="h-3.5 w-3.5" />}
          label="Details"
        />
      </div>

      {/* Content */}
      <div className="flex-1 min-h-0 overflow-hidden">
        {activeTab === "details" ? (
          <ScrollArea className="h-full">
            <div className="p-4 space-y-4">
              {/* Error banner */}
              {runTask?.error && status === "skipped" ? (
                <div className="rounded-lg border border-slate-500/20 bg-slate-500/10 px-3 py-2.5 flex gap-3 items-start">
                  <SkipForward className="w-4 h-4 text-slate-400 shrink-0 mt-0.5" />
                  <div className="flex flex-col gap-1 min-w-0">
                    <span className="text-[10px] font-bold text-slate-400 uppercase tracking-wider">
                      Skipped
                    </span>
                    <span className="text-xs text-slate-400/80 font-mono leading-relaxed break-all">
                      {runTask.error}
                    </span>
                  </div>
                </div>
              ) : runTask?.error ? (
                <div className="rounded-lg border border-red-500/20 bg-red-500/10 px-3 py-2.5 flex gap-3 items-start">
                  <AlertTriangle className="w-4 h-4 text-red-500 shrink-0 mt-0.5" />
                  <div className="flex flex-col gap-1 min-w-0">
                    <span className="text-[10px] font-bold text-red-500 uppercase tracking-wider">
                      Error
                    </span>
                    <span className="text-xs text-red-400 font-mono leading-relaxed break-all">
                      {runTask.error}
                    </span>
                  </div>
                </div>
              ) : null}

              {/* Metadata grid */}
              <div className="grid grid-cols-2 gap-3">
                <MetadataCell label="Task ID" value={task?.id ?? runTask?.task_id ?? taskId} mono />
                <MetadataCell label="Trigger Rule" value={task?.trigger_rule ?? "all_success"} mono />
                <MetadataCell label="Attempts" value={formatAttempts(runTask)} mono />
                <MetadataCell label="Retries" value={String(task?.retries ?? 0)} mono />
                <MetadataCell label="Retry Delay" value={formatDurationNs(task?.retry_delay)} mono />
                <MetadataCell label="Backoff" value={task?.retry_backoff ? "Enabled" : "Disabled"} />
                <MetadataCell label="Claimed By" value={runTask?.claimed_by || "Unclaimed"} mono />
                <MetadataCell
                  label="Node Selector"
                  value={formatKeyValueMap(
                    (task?.node_selector || runTask?.node_selector) as Record<string, unknown>,
                  )}
                />
                <MetadataCell
                  label="Outstanding Predecessors"
                  value={String(runTask?.outstanding_predecessors ?? 0)}
                  mono
                />
              </div>

              {/* Outputs */}
              {runTask?.output && Object.keys(runTask.output).length > 0 && (
                <div>
                  <div className="mb-1.5 text-xs font-medium uppercase tracking-wide text-muted-foreground">
                    Output
                  </div>
                  <div className="rounded-lg border bg-muted/50 p-3 space-y-1">
                    {Object.entries(runTask.output).map(([key, value]) => (
                      <div key={key} className="flex gap-2 font-mono text-xs">
                        <span className="font-semibold text-muted-foreground">{key}:</span>
                        <span className="text-foreground">{value}</span>
                      </div>
                    ))}
                  </div>
                </div>
              )}
            </div>
          </ScrollArea>
        ) : (
          <EmbeddedLogViewer
            jobId={jobId}
            runId={runId}
            taskId={taskId}
            error={runTask?.error}
            status={status}
          />
        )}
      </div>
    </div>
  );
}

/* ── Embedded log viewer (no chrome, fills parent) ── */

function EmbeddedLogViewer({
  jobId,
  runId,
  taskId,
  error,
  status,
}: {
  jobId: string;
  runId: string;
  taskId: string;
  error?: string | null;
  status?: string;
}) {
  const terminalRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!terminalRef.current) return;

    const term = new Terminal({
      cursorBlink: false,
      cursorStyle: "bar",
      disableStdin: true,
      convertEol: true,
      fontSize: 12,
      fontFamily: 'JetBrains Mono, Menlo, Monaco, Consolas, "Courier New", monospace',
      theme: {
        background: "#0f172a",
      },
    });

    const fitAddon = new FitAddon();
    term.loadAddon(fitAddon);
    term.open(terminalRef.current);
    fitAddon.fit();

    const abortController = new AbortController();

    async function streamLogs() {
      try {
        const response = await fetch(
          `/v1/jobs/${jobId}/runs/${runId}/logs?${new URLSearchParams({ task_id: taskId })}`,
          { signal: abortController.signal },
        );
        if (!response.ok) {
          term.writeln(`\x1b[31mError: ${await response.text()}\x1b[0m`);
          return;
        }
        const reader = response.body?.getReader();
        if (!reader) return;
        const decoder = new TextDecoder();
        while (true) {
          const { done, value } = await reader.read();
          if (done) break;
          term.write(decoder.decode(value));
        }
      } catch (err: unknown) {
        if (err instanceof Error && err.name !== "AbortError") {
          term.writeln(`\x1b[31mConnection error: ${err.message}\x1b[0m`);
        }
      }
    }

    streamLogs();

    const handleResize = () => fitAddon.fit();
    window.addEventListener("resize", handleResize);

    return () => {
      abortController.abort();
      term.dispose();
      window.removeEventListener("resize", handleResize);
    };
  }, [jobId, runId, taskId]);

  return (
    <div className="flex h-full flex-col">
      {error && status === "skipped" ? (
        <div className="px-4 py-2.5 bg-slate-500/10 border-b border-slate-500/20 flex gap-3 items-start">
          <SkipForward className="w-3.5 h-3.5 text-slate-400 shrink-0 mt-0.5" />
          <div className="flex flex-col gap-0.5">
            <span className="text-[10px] font-bold text-slate-400 uppercase tracking-wider">
              Skipped
            </span>
            <span className="text-[10px] text-slate-400/80 font-mono leading-relaxed">
              {error}
            </span>
          </div>
        </div>
      ) : error ? (
        <div className="px-4 py-2.5 bg-red-500/10 border-b border-red-500/20 flex gap-3 items-start">
          <AlertTriangle className="w-3.5 h-3.5 text-red-500 shrink-0 mt-0.5" />
          <div className="flex flex-col gap-0.5">
            <span className="text-[10px] font-bold text-red-500 uppercase tracking-wider">
              Error
            </span>
            <span className="text-[10px] text-red-400 font-mono leading-relaxed">
              {error}
            </span>
          </div>
        </div>
      ) : null}
      <div ref={terminalRef} className="flex-1 overflow-hidden bg-[#0f172a] p-2" />
    </div>
  );
}

/* ── Small helpers ── */

function StatusDot({ status }: { status: string }) {
  const color =
    status === "succeeded" || status === "completed"
      ? "bg-emerald-400 shadow-emerald-400/50"
      : status === "failed"
        ? "bg-red-400 shadow-red-400/50"
        : status === "running"
          ? "bg-blue-400 shadow-blue-400/50 animate-pulse"
          : status === "skipped"
            ? "bg-slate-400"
            : "bg-slate-500";

  return (
    <span
      className={cn(
        "inline-block h-2.5 w-2.5 shrink-0 rounded-full shadow-[0_0_6px]",
        color,
      )}
    />
  );
}

function TabButton({
  active,
  onClick,
  icon,
  label,
}: {
  active: boolean;
  onClick: () => void;
  icon: React.ReactNode;
  label: string;
}) {
  return (
    <button
      onClick={onClick}
      className={cn(
        "flex items-center gap-1.5 px-3 py-2 text-xs font-medium transition-colors",
        "border-b-2 -mb-px",
        active
          ? "border-primary text-foreground"
          : "border-transparent text-muted-foreground hover:text-foreground/80",
      )}
    >
      {icon}
      {label}
    </button>
  );
}

function MetadataCell({
  label,
  value,
  mono = false,
}: {
  label: string;
  value: string;
  mono?: boolean;
}) {
  return (
    <div>
      <div className="mb-0.5 text-[10px] font-medium uppercase tracking-wide text-muted-foreground">
        {label}
      </div>
      <div
        className={cn(
          "text-xs text-foreground",
          mono && "font-mono",
        )}
      >
        {value}
      </div>
    </div>
  );
}

function formatAttempts(runTask?: TaskRun): string {
  if (!runTask?.attempt && !runTask?.max_attempts) return "1 / 1";
  return `${runTask.attempt ?? 1} / ${runTask.max_attempts ?? 1}`;
}
