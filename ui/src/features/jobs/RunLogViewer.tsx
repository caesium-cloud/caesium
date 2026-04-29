import { useCallback, useEffect, useRef, useState } from "react";
import { useVirtualizer } from "@tanstack/react-virtual";
import { ArrowDown } from "lucide-react";
import { withAuthHeaders } from "@/lib/auth";
import { cn } from "@/lib/utils";

interface RunLogViewerProps {
  jobId: string;
  runId: string;
  taskId: string;
  isRunning: boolean;
  taskStatus?: string;
  taskError?: string | null;
}

type LogLine = { id: number; text: string };

// Mirrors the server header values from the existing LogViewer component.
type LogState = "loading" | "streaming" | "complete" | "pending" | "unavailable" | "error" | "empty";

const SCROLL_KEY_PREFIX = "caesium-run-log-scroll:";

function parseNoContentState(header: string | null): LogState {
  switch (header) {
    case "pending": return "pending";
    case "unavailable": return "unavailable";
    default: return "empty";
  }
}

export function RunLogViewer({
  jobId,
  runId,
  taskId,
  isRunning,
  taskStatus,
  taskError,
}: RunLogViewerProps) {
  const [lines, setLines] = useState<LogLine[]>([]);
  const [status, setStatus] = useState<LogState>("loading");
  const [errorMsg, setErrorMsg] = useState<string | null>(null);
  const [atBottom, setAtBottom] = useState(true);

  const parentRef = useRef<HTMLDivElement>(null);
  const lineIdRef = useRef(0);
  const abortRef = useRef<AbortController | null>(null);
  // Tracks whether scroll was saved before the last fetch reset so we can
  // restore it unconditionally on mount/remount without checking atBottom state
  // (which is always reset to true at fetch start).
  const savedScrollRef = useRef<number | null>(null);

  const rowVirtualizer = useVirtualizer({
    count: lines.length,
    getScrollElement: () => parentRef.current,
    estimateSize: () => 18,
    overscan: 40,
  });

  const scrollKey = `${SCROLL_KEY_PREFIX}${runId}:${taskId}`;

  const saveScroll = useCallback(() => {
    if (parentRef.current) {
      const pos = parentRef.current.scrollTop;
      sessionStorage.setItem(scrollKey, String(pos));
      savedScrollRef.current = pos;
    }
  }, [scrollKey]);

  // Scroll position is restored unconditionally — atBottom is reset to true at
  // fetch start, so guarding on it would always prevent restoration (Codex P2).
  const restoreScroll = useCallback(() => {
    const saved = sessionStorage.getItem(scrollKey);
    if (saved && parentRef.current) {
      parentRef.current.scrollTop = Number(saved);
      setAtBottom(false);
    }
  }, [scrollKey]);

  const handleScroll = useCallback(() => {
    const el = parentRef.current;
    if (!el) return;
    const isNearBottom = el.scrollTop + el.clientHeight >= el.scrollHeight - 40;
    setAtBottom(isNearBottom);
    if (!isNearBottom) saveScroll();
    else sessionStorage.removeItem(scrollKey);
  }, [saveScroll, scrollKey]);

  const scrollToBottom = useCallback(() => {
    const el = parentRef.current;
    if (!el) return;
    el.scrollTo({ top: el.scrollHeight, behavior: "smooth" });
    setAtBottom(true);
    sessionStorage.removeItem(scrollKey);
    savedScrollRef.current = null;
  }, [scrollKey]);

  // Auto-scroll when tailing
  useEffect(() => {
    if (atBottom && parentRef.current) {
      parentRef.current.scrollTop = parentRef.current.scrollHeight;
    }
  }, [lines.length, atBottom]);

  // Fetch + stream logs. Re-runs when taskStatus changes so a pending task
  // automatically starts streaming once it transitions to running (Gemini HIGH,
  // Codex P1). Previously the dep array only had [jobId, runId, taskId, isRunning].
  useEffect(() => {
    setLines([]);
    setStatus("loading");
    setErrorMsg(null);
    setAtBottom(true);
    lineIdRef.current = 0;

    const abort = new AbortController();
    abortRef.current = abort;

    async function fetchLogs() {
      try {
        const headers = withAuthHeaders({});
        const response = await fetch(
          `/v1/jobs/${jobId}/runs/${runId}/logs?${new URLSearchParams({ task_id: taskId })}`,
          { signal: abort.signal, headers },
        );

        // Parse the state header on 204 — the server uses this to communicate
        // pending/unavailable rather than truly empty (Codex P1).
        if (response.status === 204) {
          const stateHeader = response.headers.get("X-Caesium-Log-State");
          setStatus(parseNoContentState(stateHeader));
          return;
        }

        if (!response.ok) {
          const body = await response.text().catch(() => "");
          setErrorMsg(`HTTP ${response.status}${body ? `: ${body}` : ""}`);
          setStatus("error");
          return;
        }

        const sourceHeader = response.headers.get("X-Caesium-Log-Source");
        setStatus(sourceHeader === "live" ? "streaming" : "complete");

        const reader = response.body?.getReader();
        if (!reader) { setStatus("empty"); return; }

        const decoder = new TextDecoder();
        let buffer = "";
        let sawOutput = false;

        while (true) {
          const { done, value } = await reader.read();
          if (done) break;
          const chunk = decoder.decode(value, { stream: true });
          buffer += chunk;
          sawOutput = true;

          const newlineIdx = buffer.lastIndexOf("\n");
          if (newlineIdx === -1) continue;

          const toFlush = buffer.slice(0, newlineIdx + 1);
          buffer = buffer.slice(newlineIdx + 1);

          // Use .slice(0, -1) instead of .filter(Boolean) so intentionally empty
          // lines are preserved (Gemini MED, Codex P2).
          const newLines: LogLine[] = toFlush.split("\n").slice(0, -1).map((text) => ({
            id: ++lineIdRef.current,
            text,
          }));
          if (newLines.length > 0) {
            setLines((prev) => [...prev, ...newLines]);
          }
        }

        // Flush any trailing content that didn't end with a newline
        if (buffer) {
          setLines((prev) => [...prev, { id: ++lineIdRef.current, text: buffer }]);
        }

        setStatus(sawOutput ? "complete" : "empty");
      } catch (err) {
        if (err instanceof Error && err.name === "AbortError") return;
        setStatus("error");
        setErrorMsg(err instanceof Error ? err.message : "Failed to load logs");
      }
    }

    fetchLogs();

    return () => { abort.abort(); };
  }, [jobId, runId, taskId, isRunning, taskStatus]);

  // Restore saved scroll after lines have rendered (Codex P2 fix: no atBottom guard)
  useEffect(() => {
    if (lines.length > 0) restoreScroll();
  }, [lines.length, restoreScroll]);

  const virtualItems = rowVirtualizer.getVirtualItems();

  return (
    <div className="relative flex flex-col h-full bg-[#020617] text-[#e2e8f0] font-mono text-[11px]">
      {/* Status bar */}
      <div className="flex items-center gap-2 px-3 py-1.5 border-b border-white/5 bg-black/20 shrink-0">
        <StatusPill status={status} />
        {taskError && (
          <span className="text-red-400/80 truncate">{taskError}</span>
        )}
      </div>

      {/* Log lines (virtualized) */}
      <div
        ref={parentRef}
        className="flex-1 overflow-auto overscroll-contain"
        onScroll={handleScroll}
      >
        {lines.length === 0 && (
          <div className="flex items-center justify-center h-full text-[#64748b] text-[12px]">
            {status === "loading" && "Loading logs…"}
            {status === "pending" && "Logs will appear when the task starts."}
            {status === "unavailable" && "No retained logs available for this task."}
            {status === "empty" && "No output captured for this task."}
            {status === "error" && (errorMsg ?? "Failed to load logs.")}
            {status === "streaming" && "Waiting for output…"}
          </div>
        )}

        {lines.length > 0 && (
          <div style={{ height: `${rowVirtualizer.getTotalSize()}px`, position: "relative" }}>
            {virtualItems.map((vItem) => {
              const line = lines[vItem.index];
              return (
                <div
                  key={vItem.key}
                  data-index={vItem.index}
                  ref={rowVirtualizer.measureElement}
                  style={{
                    position: "absolute",
                    top: 0,
                    left: 0,
                    width: "100%",
                    transform: `translateY(${vItem.start}px)`,
                  }}
                  className="flex items-start gap-3 px-3 hover:bg-white/5"
                >
                  <span className="select-none text-[#334155] tabular-nums w-10 shrink-0 text-right leading-[18px]">
                    {vItem.index + 1}
                  </span>
                  <pre className="whitespace-pre-wrap break-all leading-[18px] text-[11px]">
                    {line.text}
                  </pre>
                </div>
              );
            })}
          </div>
        )}
      </div>

      {/* "Jump to live" pill */}
      {!atBottom && isRunning && (
        <button
          type="button"
          onClick={scrollToBottom}
          className={cn(
            "absolute bottom-3 right-3 flex items-center gap-1.5 rounded-full px-3 py-1.5",
            "bg-cyan-glow/20 border border-cyan-glow/30 text-cyan-glow text-[10px] font-semibold",
            "hover:bg-cyan-glow/30 transition-colors shadow-lg",
          )}
        >
          <ArrowDown className="h-3 w-3" />
          Jump to live
        </button>
      )}
    </div>
  );
}

type LogStatus = LogState;

function StatusPill({ status }: { status: LogStatus }) {
  const map: Record<LogStatus, { label: string; cls: string }> = {
    loading:     { label: "Loading",     cls: "bg-white/10 text-[#94a3b8]" },
    streaming:   { label: "Live",        cls: "bg-emerald-500/10 border-emerald-500/30 text-emerald-300" },
    complete:    { label: "Complete",    cls: "bg-blue-500/10 border-blue-500/30 text-blue-300" },
    pending:     { label: "Pending",     cls: "bg-amber-500/10 border-amber-500/30 text-amber-300" },
    unavailable: { label: "Unavailable", cls: "bg-white/5 border-white/10 text-[#64748b]" },
    error:       { label: "Error",       cls: "bg-red-500/10 border-red-500/30 text-red-300" },
    empty:       { label: "Empty",       cls: "bg-white/5 text-[#64748b]" },
  };
  const { label, cls } = map[status];
  return (
    <span className={cn("rounded-full border px-2 py-px text-[9px] font-semibold uppercase tracking-wider", cls)}>
      {label}
    </span>
  );
}
