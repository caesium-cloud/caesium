import { useEffect, useRef } from "react";
import { Terminal } from "xterm";
import { FitAddon } from "xterm-addon-fit";
import "xterm/css/xterm.css";
import { Button } from "@/components/ui/button";
import { X, AlertTriangle, SkipForward } from "lucide-react";

interface LogViewerProps {
  jobId: string;
  runId: string;
  taskId: string;
  error?: string | null;
  status?: string;
  onClose: () => void;
}

export function LogViewer({ jobId, runId, taskId, error, status, onClose }: LogViewerProps) {
  const terminalRef = useRef<HTMLDivElement>(null);
  const xtermRef = useRef<Terminal | null>(null);

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

    xtermRef.current = term;

    const abortController = new AbortController();

    async function streamLogs() {
      try {
        const response = await fetch(
          `/v1/jobs/${jobId}/runs/${runId}/logs?task_id=${taskId}`,
          { signal: abortController.signal }
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
    <div className="flex flex-col h-full rounded-md overflow-hidden border border-border bg-card">
      <div className="flex items-center justify-between px-4 py-2 border-b border-border bg-muted">
        <span className="text-xs font-mono text-muted-foreground truncate">Task: {taskId}</span>
        <Button variant="ghost" size="icon" className="h-6 w-6 text-muted-foreground hover:text-foreground" onClick={onClose}>
            <X className="h-4 w-4" />
        </Button>
      </div>

      {error && status === 'skipped' ? (
        <div className="px-4 py-3 bg-slate-500/10 border-b border-slate-500/20 flex gap-3 items-start overflow-y-auto max-h-32">
          <SkipForward className="w-4 h-4 text-slate-400 shrink-0 mt-0.5" />
          <div className="flex flex-col gap-1">
            <span className="text-[10px] font-bold text-slate-400 uppercase tracking-wider">Skipped</span>
            <span className="text-xs text-slate-400/80 font-mono leading-relaxed">
              {error}
            </span>
          </div>
        </div>
      ) : error ? (
        <div className="px-4 py-3 bg-red-500/10 border-b border-red-500/20 flex gap-3 items-start overflow-y-auto max-h-32">
          <AlertTriangle className="w-4 h-4 text-red-500 shrink-0 mt-0.5" />
          <div className="flex flex-col gap-1">
            <span className="text-[10px] font-bold text-red-500 uppercase tracking-wider">Full Error Detail</span>
            <span className="text-xs text-red-400 font-mono leading-relaxed">
              {error}
            </span>
          </div>
        </div>
      ) : null}

      {/* Terminal pane stays dark — standard UX for log/terminal output */}
      <div ref={terminalRef} className="flex-1 overflow-hidden bg-[#0f172a] p-2" />
    </div>
  );
}
