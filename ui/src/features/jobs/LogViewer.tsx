import { useEffect, useRef } from "react";
import { Terminal } from "xterm";
import { FitAddon } from "xterm-addon-fit";
import "xterm/css/xterm.css";
import { Button } from "@/components/ui/button";
import { X } from "lucide-react";

interface LogViewerProps {
  jobId: string;
  runId: string;
  taskId: string;
  onClose: () => void;
}

export function LogViewer({ jobId, runId, taskId, onClose }: LogViewerProps) {
  const terminalRef = useRef<HTMLDivElement>(null);
  const xtermRef = useRef<Terminal | null>(null);

  useEffect(() => {
    if (!terminalRef.current) return;

    const term = new Terminal({
      cursorBlink: true,
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
    <div className="flex flex-col h-full bg-[#0f172a] rounded-md overflow-hidden border border-slate-800">
      <div className="flex items-center justify-between px-4 py-2 bg-slate-900 border-b border-slate-800">
        <span className="text-xs font-mono text-slate-400">Task: {taskId}</span>
        <Button variant="ghost" size="icon" className="h-6 w-6 text-slate-400 hover:text-white" onClick={onClose}>
            <X className="h-4 w-4" />
        </Button>
      </div>
      <div ref={terminalRef} className="flex-1 overflow-hidden p-2" />
    </div>
  );
}
