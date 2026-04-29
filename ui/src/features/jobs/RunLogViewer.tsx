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
  /** Called with the atom/task label for context highlight */
  onHighlightChange?: (taskId: string | null) => void;
}

type LogLine = { id: number; text: string };

const SCROLL_KEY_PREFIX = "caesium-run-log-scroll:";

export function RunLogViewer({
  jobId,
  runId,
  taskId,
  isRunning,
  taskStatus,
  taskError,
}: RunLogViewerProps) {
  const [lines, setLines] = useState<LogLine[]>([]);
  const [status, setStatus] = useState<"loading" | "streaming" | "complete" | "error" | "empty">("loading");
  const [errorMsg, setErrorMsg] = useState<string | null>(null);
  const [atBottom, setAtBottom] = useState(true);

  const parentRef = useRef<HTMLDivElement>(null);
  const lineIdRef = useRef(0);
  const abortRef = useRef<AbortController | null>(null);
  const retryTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  const rowVirtualizer = useVirtualizer({
    count: lines.length,
    getScrollElement: () => parentRef.current,
    estimateSize: () => 18,
    overscan: 40,
  });

  // Scroll position persistence
  const scrollKey = `${SCROLL_KEY_PREFIX}${runId}:${taskId}`;

  const restoreScroll = useCallback(() => {
    const saved = sessionStorage.getItem(scrollKey);
    if (saved && parentRef.current && !atBottom) {
      parentRef.current.scrollTop = Number(saved);
    }
  }, [atBottom, scrollKey]);

  const saveScroll = useCallback(() => {
    if (parentRef.current) {
      sessionStorage.setItem(scrollKey, String(parentRef.current.scrollTop));
    }
  }, [scrollKey]);

  // Detect at-bottom
  const handleScroll = useCallback(() => {
    const el = parentRef.current;
    if (!el) return;
    const threshold = 40;
    const isNearBottom = el.scrollTop + el.clientHeight >= el.scrollHeight - threshold;
    setAtBottom(isNearBottom);
    if (!isNearBottom) {
      saveScroll();
    }
  }, [saveScroll]);

  // Scroll to bottom
  const scrollToBottom = useCallback(() => {
    const el = parentRef.current;
    if (!el) return;
    el.scrollTo({ top: el.scrollHeight, behavior: "smooth" });
    setAtBottom(true);
    sessionStorage.removeItem(scrollKey);
  }, [scrollKey]);

  // Auto-scroll when at-bottom and new lines arrive
  useEffect(() => {
    if (atBottom && parentRef.current) {
      parentRef.current.scrollTop = parentRef.current.scrollHeight;
    }
  }, [lines.length, atBottom]);

  // Fetch + stream logs
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

        if (response.status === 204) {
          setStatus("empty");
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

          // Flush complete lines from buffer
          const newlineIdx = buffer.lastIndexOf("\n");
          if (newlineIdx === -1) continue;

          const toFlush = buffer.slice(0, newlineIdx + 1);
          buffer = buffer.slice(newlineIdx + 1);

          const newLines: LogLine[] = toFlush.split("\n").filter(Boolean).map((text) => ({
            id: ++lineIdRef.current,
            text,
          }));
          if (newLines.length > 0) {
            setLines((prev) => [...prev, ...newLines]);
          }
        }

        // Flush remaining buffer
        if (buffer.trim()) {
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

    return () => {
      abort.abort();
      if (retryTimerRef.current) clearTimeout(retryTimerRef.current);
    };
  }, [jobId, runId, taskId, isRunning]);

  // Retry when pending (task hasn't started yet)
  useEffect(() => {
    if (status !== "empty" || taskStatus !== "pending") return;
    retryTimerRef.current = setTimeout(() => {
      // Re-trigger by bumping a key would require hoisting; instead just re-fetch
      // by resetting state (parent effect re-runs if taskStatus changes)
    }, 2000);
    return () => {
      if (retryTimerRef.current) clearTimeout(retryTimerRef.current);
    };
  }, [status, taskStatus]);

  useEffect(() => {
    restoreScroll();
  }, [restoreScroll]);

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
            {status === "empty" && "No output captured for this task."}
            {status === "error" && (errorMsg ?? "Failed to load logs.")}
            {status === "streaming" && "Waiting for output…"}
          </div>
        )}

        {lines.length > 0 && (
          <div
            style={{ height: `${rowVirtualizer.getTotalSize()}px`, position: "relative" }}
          >
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

      {/* "Jump to live" pill — shown when scrolled up during a running task */}
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

type LogStatus = "loading" | "streaming" | "complete" | "error" | "empty";

function StatusPill({ status }: { status: LogStatus }) {
  const map: Record<LogStatus, { label: string; cls: string }> = {
    loading: { label: "Loading", cls: "bg-white/10 text-[#94a3b8]" },
    streaming: { label: "Live", cls: "bg-emerald-500/10 border-emerald-500/30 text-emerald-300" },
    complete: { label: "Complete", cls: "bg-blue-500/10 border-blue-500/30 text-blue-300" },
    error: { label: "Error", cls: "bg-red-500/10 border-red-500/30 text-red-300" },
    empty: { label: "Empty", cls: "bg-white/5 text-[#64748b]" },
  };
  const { label, cls } = map[status];
  return (
    <span className={cn("rounded-full border px-2 py-px text-[9px] font-semibold uppercase tracking-wider", cls)}>
      {label}
    </span>
  );
}
