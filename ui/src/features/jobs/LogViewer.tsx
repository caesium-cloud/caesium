import { type ReactNode, useEffect, useRef, useState } from "react";
import { Terminal } from "xterm";
import { FitAddon } from "xterm-addon-fit";
import { SearchAddon } from "xterm-addon-search";
import "xterm/css/xterm.css";
import {
  AlertTriangle,
  ChevronDown,
  ChevronUp,
  Copy,
  Search,
  SkipForward,
} from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

const logHeaderState = "X-Caesium-Log-State";
const logHeaderSource = "X-Caesium-Log-Source";
const logHeaderTruncated = "X-Caesium-Log-Truncated";

type LogSource = "live" | "persisted" | null;
type LogState = "loading" | "streaming" | "complete" | "pending" | "empty" | "unavailable" | "error";

interface LogViewerProps {
  jobId: string;
  runId: string;
  taskId: string;
  error?: string | null;
  status?: string;
}

export function LogViewer({ jobId, runId, taskId, error, status }: LogViewerProps) {
  const terminalRef = useRef<HTMLDivElement>(null);
  const xtermRef = useRef<Terminal | null>(null);
  const fitAddonRef = useRef<FitAddon | null>(null);
  const searchAddonRef = useRef<SearchAddon | null>(null);
  const logBufferRef = useRef("");
  const [logState, setLogState] = useState<LogState>("loading");
  const [logSource, setLogSource] = useState<LogSource>(null);
  const [logTruncated, setLogTruncated] = useState(false);
  const [transportError, setTransportError] = useState<string | null>(null);
  const [hasLogOutput, setHasLogOutput] = useState(false);
  const [searchTerm, setSearchTerm] = useState("");
  const [caseSensitive, setCaseSensitive] = useState(false);

  useEffect(() => {
    if (!terminalRef.current) {
      return;
    }

    const terminal = new Terminal({
      cursorBlink: false,
      cursorStyle: "bar",
      disableStdin: true,
      convertEol: true,
      fontSize: 12,
      fontFamily: 'JetBrains Mono, Menlo, Monaco, Consolas, "Courier New", monospace',
      scrollback: 10000,
      theme: {
        background: "#020617",
        foreground: "#e2e8f0",
        cursor: "#38bdf8",
        cursorAccent: "#020617",
        selectionBackground: "rgba(56, 189, 248, 0.22)",
        black: "#0f172a",
        red: "#f87171",
        green: "#34d399",
        yellow: "#fbbf24",
        blue: "#60a5fa",
        magenta: "#f472b6",
        cyan: "#22d3ee",
        white: "#e2e8f0",
        brightBlack: "#475569",
        brightRed: "#fca5a5",
        brightGreen: "#6ee7b7",
        brightYellow: "#fcd34d",
        brightBlue: "#93c5fd",
        brightMagenta: "#f9a8d4",
        brightCyan: "#67e8f9",
        brightWhite: "#f8fafc",
      },
    });

    const fitAddon = new FitAddon();
    const searchAddon = new SearchAddon({ highlightLimit: 2000 });
    terminal.loadAddon(fitAddon);
    terminal.loadAddon(searchAddon);
    terminal.open(terminalRef.current);
    fitAddon.fit();

    xtermRef.current = terminal;
    fitAddonRef.current = fitAddon;
    searchAddonRef.current = searchAddon;

    const handleResize = () => fitAddon.fit();
    window.addEventListener("resize", handleResize);

    return () => {
      window.removeEventListener("resize", handleResize);
      searchAddonRef.current = null;
      fitAddonRef.current = null;
      xtermRef.current = null;
      terminal.dispose();
    };
  }, []);

  useEffect(() => {
    const terminal = xtermRef.current;
    if (terminal === null) {
      return;
    }
    const term = terminal;

    term.reset();
    logBufferRef.current = "";
    setHasLogOutput(false);
    setTransportError(null);
    setLogSource(null);
    setLogTruncated(false);
    setLogState("loading");

    const abortController = new AbortController();

    async function streamLogs() {
      try {
        const response = await fetch(
          `/v1/jobs/${jobId}/runs/${runId}/logs?${new URLSearchParams({ task_id: taskId })}`,
          { signal: abortController.signal },
        );

        const sourceHeader = response.headers.get(logHeaderSource);
        setLogSource(sourceHeader === "live" || sourceHeader === "persisted" ? sourceHeader : null);
        setLogTruncated(response.headers.get(logHeaderTruncated) === "true");

        if (response.status === 204) {
          const stateHeader = response.headers.get(logHeaderState);
          setLogState(parseNoContentState(stateHeader));
          return;
        }

        if (!response.ok) {
          setLogState("error");
          setTransportError(await readErrorMessage(response));
          return;
        }

        const reader = response.body?.getReader();
        if (!reader) {
          setLogState("empty");
          return;
        }

        const decoder = new TextDecoder();
        let sawOutput = false;
        setLogState(sourceHeader === "live" ? "streaming" : "complete");

        while (true) {
          const { done, value } = await reader.read();
          if (done) {
            break;
          }

          const chunk = decoder.decode(value, { stream: true });
          if (!chunk) {
            continue;
          }

          if (!sawOutput) {
            sawOutput = true;
            setHasLogOutput(true);
          }
          logBufferRef.current += chunk;
          term.write(chunk);
        }

        const tail = decoder.decode();
        if (tail) {
          if (!sawOutput) {
            sawOutput = true;
            setHasLogOutput(true);
          }
          logBufferRef.current += tail;
          term.write(tail);
        }

        setHasLogOutput(sawOutput);
        setLogState(sawOutput ? "complete" : "empty");
      } catch (err: unknown) {
        if (err instanceof Error && err.name === "AbortError") {
          return;
        }
        setLogState("error");
        setTransportError(err instanceof Error ? err.message : "Failed to load task logs");
      }
    }

    streamLogs();

    return () => {
      abortController.abort();
    };
  }, [jobId, runId, taskId]);

  useEffect(() => {
    const searchAddon = searchAddonRef.current;
    if (!searchAddon) {
      return;
    }

    if (!searchTerm) {
      searchAddon.clearDecorations();
      return;
    }

    searchAddon.findNext(searchTerm, searchOptions(caseSensitive, true));
  }, [caseSensitive, searchTerm]);

  const handleFindNext = () => {
    const searchAddon = searchAddonRef.current;
    if (!searchAddon || !searchTerm) {
      return;
    }

    if (!searchAddon.findNext(searchTerm, searchOptions(caseSensitive, false))) {
      toast.info(`No matches for "${searchTerm}"`);
    }
  };

  const handleFindPrevious = () => {
    const searchAddon = searchAddonRef.current;
    if (!searchAddon || !searchTerm) {
      return;
    }

    if (!searchAddon.findPrevious(searchTerm, searchOptions(caseSensitive, false))) {
      toast.info(`No matches for "${searchTerm}"`);
    }
  };

  const handleCopy = async () => {
    if (!logBufferRef.current) {
      return;
    }

    try {
      await navigator.clipboard.writeText(logBufferRef.current);
      toast.success("Copied task logs");
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Failed to copy task logs");
    }
  };

  const emptyState = getEmptyState(logState, status, logSource);

  return (
    <div className="flex h-full min-h-0 flex-col bg-slate-950">
      {error && status === "skipped" ? (
        <div className="border-b border-slate-500/20 bg-slate-500/10 px-4 py-2.5">
          <div className="flex items-start gap-3">
            <SkipForward className="mt-0.5 h-3.5 w-3.5 shrink-0 text-slate-400" />
            <div className="min-w-0">
              <div className="text-[10px] font-bold uppercase tracking-wider text-slate-400">Skipped</div>
              <div className="font-mono text-[10px] leading-relaxed text-slate-400/80">{error}</div>
            </div>
          </div>
        </div>
      ) : error ? (
        <div className="border-b border-red-500/20 bg-red-500/10 px-4 py-2.5">
          <div className="flex items-start gap-3">
            <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0 text-red-400" />
            <div className="min-w-0">
              <div className="text-[10px] font-bold uppercase tracking-wider text-red-400">Task Error</div>
              <div className="font-mono text-[10px] leading-relaxed text-red-300">{error}</div>
            </div>
          </div>
        </div>
      ) : null}

      <div className="flex flex-wrap items-center gap-2 border-b border-slate-800 bg-slate-950/80 px-3 py-2">
        <div className="flex min-w-[220px] flex-1 items-center gap-2 rounded-md border border-slate-800 bg-slate-900/80 px-2.5 py-1.5">
          <Search className="h-3.5 w-3.5 shrink-0 text-slate-500" />
          <input
            value={searchTerm}
            onChange={(event) => setSearchTerm(event.target.value)}
            placeholder="Search logs"
            className="w-full bg-transparent text-xs text-slate-100 outline-none placeholder:text-slate-500"
          />
        </div>

        <div className="flex items-center gap-1">
          <Button
            type="button"
            variant="ghost"
            size="icon"
            className="h-7 w-7 text-slate-300 hover:bg-slate-800 hover:text-slate-50"
            onClick={handleFindPrevious}
            disabled={!searchTerm || !hasLogOutput}
          >
            <ChevronUp className="h-3.5 w-3.5" />
          </Button>
          <Button
            type="button"
            variant="ghost"
            size="icon"
            className="h-7 w-7 text-slate-300 hover:bg-slate-800 hover:text-slate-50"
            onClick={handleFindNext}
            disabled={!searchTerm || !hasLogOutput}
          >
            <ChevronDown className="h-3.5 w-3.5" />
          </Button>
        </div>

        <button
          type="button"
          onClick={() => setCaseSensitive((current) => !current)}
          className={cn(
            "rounded-md border px-2 py-1 text-[10px] font-semibold uppercase tracking-wide transition-colors",
            caseSensitive
              ? "border-cyan-500/40 bg-cyan-500/10 text-cyan-200"
              : "border-slate-700 bg-slate-900 text-slate-400 hover:border-slate-600 hover:text-slate-200",
          )}
        >
          Aa
        </button>

        <Button
          type="button"
          variant="ghost"
          className="h-7 gap-1.5 px-2 text-[10px] font-semibold uppercase tracking-wide text-slate-300 hover:bg-slate-800 hover:text-slate-50"
          onClick={handleCopy}
          disabled={!hasLogOutput}
        >
          <Copy className="h-3.5 w-3.5" />
          Copy
        </Button>

        <div className="ml-auto flex flex-wrap items-center gap-2 text-[10px] font-semibold uppercase tracking-wide">
          <LogBadge>{renderStateLabel(logState)}</LogBadge>
          {logSource === "live" ? <LogBadge className="border-emerald-500/30 bg-emerald-500/10 text-emerald-200">Live</LogBadge> : null}
          {logSource === "persisted" ? <LogBadge className="border-blue-500/30 bg-blue-500/10 text-blue-200">Retained</LogBadge> : null}
          {logTruncated ? <LogBadge className="border-amber-500/30 bg-amber-500/10 text-amber-200">Truncated</LogBadge> : null}
        </div>
      </div>

      {transportError ? (
        <div className="border-b border-red-500/20 bg-red-500/10 px-4 py-2.5 text-[11px] text-red-200">
          {transportError}
        </div>
      ) : null}

      <div className="relative flex-1 overflow-hidden">
        <div ref={terminalRef} className="h-full w-full overflow-hidden bg-slate-950 px-3 py-2" />

        {!hasLogOutput && emptyState ? (
          <div className="absolute inset-0 flex items-center justify-center bg-slate-950/96 px-6 text-center">
            <div className="max-w-sm space-y-2">
              <div className="text-sm font-semibold text-slate-100">{emptyState.title}</div>
              <div className="text-xs leading-relaxed text-slate-400">{emptyState.body}</div>
            </div>
          </div>
        ) : null}
      </div>
    </div>
  );
}

function searchOptions(caseSensitive: boolean, incremental: boolean) {
  return {
    caseSensitive,
    incremental,
    decorations: {
      matchBackground: "#1e293b",
      matchBorder: "#475569",
      matchOverviewRuler: "#334155",
      activeMatchBackground: "#0f766e",
      activeMatchBorder: "#14b8a6",
      activeMatchColorOverviewRuler: "#14b8a6",
    },
  };
}

function parseNoContentState(state: string | null): LogState {
  switch (state) {
    case "pending":
      return "pending";
    case "unavailable":
      return "unavailable";
    default:
      return "empty";
  }
}

async function readErrorMessage(response: Response): Promise<string> {
  const fallback = `Failed to load logs (${response.status})`;
  const body = await response.text();
  if (!body) {
    return fallback;
  }

  try {
    const parsed = JSON.parse(body) as { message?: string; error?: string };
    if (parsed.message && parsed.error) {
      return `${parsed.message}: ${parsed.error}`;
    }
    return parsed.message || parsed.error || fallback;
  } catch {
    return body;
  }
}

function renderStateLabel(state: LogState): string {
  switch (state) {
    case "loading":
      return "Loading";
    case "streaming":
      return "Streaming";
    case "pending":
      return "Pending";
    case "empty":
      return "Empty";
    case "unavailable":
      return "Missing";
    case "error":
      return "Error";
    default:
      return "Ready";
  }
}

function getEmptyState(state: LogState, status?: string, source?: LogSource) {
  switch (state) {
    case "loading":
      return {
        title: "Loading task logs",
        body: "Establishing the log stream and checking for retained output.",
      };
    case "pending":
      return {
        title: "Logs will appear when the task starts",
        body: "This task has not begun emitting output yet.",
      };
    case "empty":
      return {
        title: "No log output captured",
        body:
          status === "skipped"
            ? "Skipped tasks typically do not emit stdout or stderr."
            : "This task finished without writing anything to stdout or stderr.",
      };
    case "unavailable":
      return {
        title: "No retained logs are available",
        body:
          source === "persisted"
            ? "Only a retained snapshot is available for this task."
            : "The runtime has already been cleaned up and this task did not retain a log snapshot.",
      };
    case "error":
      return {
        title: "Log stream failed",
        body: "Caesium could not load logs for this task. See the message above for details.",
      };
    default:
      return null;
  }
}

function LogBadge({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <span className={cn("rounded-md border border-slate-700 bg-slate-900 px-2 py-1 text-slate-300", className)}>
      {children}
    </span>
  );
}
