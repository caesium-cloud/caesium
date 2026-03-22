import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import {
  ArrowDown,
  Circle,
  Pause,
  Play,
  ScrollText,
  Search,
  Trash2,
  Wifi,
  WifiOff,
} from "lucide-react";
import { toast } from "sonner";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { api } from "@/lib/api";
import { cn } from "@/lib/utils";
import { useLogStream, type LogEntry } from "./useLogStream";

const LEVEL_COLORS: Record<string, string> = {
  debug: "text-slate-400 bg-slate-400/10",
  info: "text-blue-400 bg-blue-400/10",
  warn: "text-yellow-400 bg-yellow-400/10",
  error: "text-red-400 bg-red-400/10",
  dpanic: "text-red-500 bg-red-500/10",
  panic: "text-red-600 bg-red-600/10",
  fatal: "text-red-700 bg-red-700/10",
};

const LEVEL_DOT_COLORS: Record<string, string> = {
  debug: "fill-slate-400",
  info: "fill-blue-400",
  warn: "fill-yellow-400",
  error: "fill-red-400",
  dpanic: "fill-red-500",
  panic: "fill-red-600",
  fatal: "fill-red-700",
};

const LEVELS = ["debug", "info", "warn", "error"] as const;

export function LogConsolePage() {
  const [minLevel, setMinLevel] = useState<string>("debug");
  const [search, setSearch] = useState("");
  const [expandedSeqs, setExpandedSeqs] = useState<Set<number>>(new Set());
  const [autoScroll, setAutoScroll] = useState(true);
  const scrollRef = useRef<HTMLDivElement>(null);
  const bottomRef = useRef<HTMLDivElement>(null);

  const { entries, connected, paused, error: streamError, pause, resume, clear } = useLogStream();

  const {
    data: levelData,
    refetch: refetchLevel,
  } = useQuery({
    queryKey: ["log-level"],
    queryFn: api.getLogLevel,
    staleTime: 30_000,
  });

  const setLevelMutation = useMutation({
    mutationFn: api.setLogLevel,
    onSuccess: (data) => {
      toast.success(`Log level set to ${data.level}`);
      refetchLevel();
    },
    onError: () => toast.error("Failed to change log level"),
  });

  // Filter entries client-side.
  const filtered = useMemo(() => {
    const levelIdx = LEVELS.indexOf(minLevel as typeof LEVELS[number]);
    const lowerSearch = search.toLowerCase();
    return entries.filter((e) => {
      const eLevelIdx = LEVELS.indexOf(e.level as typeof LEVELS[number]);
      if (eLevelIdx !== -1 && eLevelIdx < levelIdx) return false;
      if (lowerSearch && !e.msg.toLowerCase().includes(lowerSearch)) return false;
      return true;
    });
  }, [entries, minLevel, search]);

  // Auto-scroll to bottom.
  useEffect(() => {
    if (autoScroll && bottomRef.current) {
      bottomRef.current.scrollIntoView({ behavior: "smooth" });
    }
  }, [filtered.length, autoScroll]);

  // Detect manual scroll to disable auto-scroll.
  const handleScroll = useCallback(() => {
    const el = scrollRef.current;
    if (!el) return;
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 60;
    setAutoScroll(atBottom);
  }, []);

  const jumpToBottom = useCallback(() => {
    setAutoScroll(true);
    bottomRef.current?.scrollIntoView({ behavior: "smooth" });
  }, []);

  const toggleExpand = useCallback((seq: number) => {
    setExpandedSeqs((prev) => {
      const next = new Set(prev);
      if (next.has(seq)) next.delete(seq);
      else next.add(seq);
      return next;
    });
  }, []);

  // Detect if log console is not enabled (stream error from 404).
  if (streamError?.includes("disconnected") && entries.length === 0) {
    return (
      <div className="space-y-6">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">Log Console</h1>
          <p className="text-sm text-muted-foreground mt-0.5">
            Real-time server log viewer for operators and admins.
          </p>
        </div>
        <Card>
          <CardHeader>
            <CardTitle className="text-base flex items-center gap-2">
              <ScrollText className="h-4 w-4" />
              Log Console Disabled
            </CardTitle>
          </CardHeader>
          <CardContent className="space-y-3">
            <p className="text-sm text-muted-foreground">
              The log console is not enabled on this Caesium instance. To enable it, set the following
              environment variable and restart:
            </p>
            <code className="block rounded bg-muted px-3 py-2 text-sm">
              CAESIUM_LOG_CONSOLE_ENABLED=true
            </code>
          </CardContent>
        </Card>
      </div>
    );
  }

  return (
    <div className="flex flex-col h-[calc(100vh-7rem)] space-y-4">
      {/* Header */}
      <div className="flex items-center justify-between shrink-0">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">Log Console</h1>
          <p className="text-sm text-muted-foreground mt-0.5">
            Real-time server log stream
          </p>
        </div>
        <div className="flex items-center gap-2">
          <Badge variant="outline" className="gap-1.5">
            {connected ? (
              <Wifi className="h-3 w-3 text-green-500" />
            ) : (
              <WifiOff className="h-3 w-3 text-destructive" />
            )}
            {connected ? "Connected" : "Disconnected"}
          </Badge>
          {levelData?.level && (
            <Badge variant="secondary" className="font-mono text-xs">
              {levelData.level}
            </Badge>
          )}
        </div>
      </div>

      {/* Controls toolbar */}
      <div className="flex flex-wrap items-center gap-2 shrink-0">
        {/* Level filter */}
        <div className="flex items-center rounded-md border border-border overflow-hidden">
          {LEVELS.map((lvl) => (
            <button
              key={lvl}
              onClick={() => setMinLevel(lvl)}
              className={cn(
                "px-2.5 py-1 text-xs font-medium capitalize transition-colors",
                minLevel === lvl
                  ? "bg-primary text-primary-foreground"
                  : "bg-background text-muted-foreground hover:bg-muted",
              )}
            >
              {lvl}
            </button>
          ))}
        </div>

        {/* Server level changer */}
        <select
          value={levelData?.level ?? "info"}
          onChange={(e) => setLevelMutation.mutate({ level: e.target.value })}
          className="h-7 rounded-md border border-border bg-background px-2 text-xs"
          title="Server log level"
        >
          {LEVELS.map((lvl) => (
            <option key={lvl} value={lvl}>
              Server: {lvl}
            </option>
          ))}
        </select>

        {/* Search */}
        <div className="relative flex-1 min-w-[180px] max-w-xs">
          <Search className="absolute left-2 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground" />
          <input
            type="text"
            placeholder="Filter messages..."
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            className="h-7 w-full rounded-md border border-border bg-background pl-7 pr-2 text-xs placeholder:text-muted-foreground focus:outline-none focus:ring-1 focus:ring-ring"
          />
        </div>

        <div className="flex items-center gap-1 ml-auto">
          <Button
            variant="outline"
            size="sm"
            className="h-7 px-2 text-xs"
            onClick={paused ? resume : pause}
          >
            {paused ? <Play className="h-3 w-3 mr-1" /> : <Pause className="h-3 w-3 mr-1" />}
            {paused ? "Resume" : "Pause"}
          </Button>
          <Button
            variant="outline"
            size="sm"
            className="h-7 px-2 text-xs"
            onClick={clear}
          >
            <Trash2 className="h-3 w-3 mr-1" />
            Clear
          </Button>
        </div>
      </div>

      {/* Log entries */}
      <div
        ref={scrollRef}
        onScroll={handleScroll}
        className="flex-1 overflow-auto rounded-md border border-border bg-[#0f172a] font-mono text-xs"
      >
        {filtered.length === 0 ? (
          <div className="flex items-center justify-center h-full text-slate-500">
            {entries.length === 0 ? "Waiting for log entries..." : "No entries match filters"}
          </div>
        ) : (
          <table className="w-full">
            <tbody>
              {filtered.map((entry) => (
                <LogRow
                  key={entry.sequence}
                  entry={entry}
                  expanded={expandedSeqs.has(entry.sequence)}
                  onToggle={() => toggleExpand(entry.sequence)}
                />
              ))}
            </tbody>
          </table>
        )}
        <div ref={bottomRef} />
      </div>

      {/* Jump to bottom */}
      {!autoScroll && (
        <div className="absolute bottom-20 right-10">
          <Button
            variant="secondary"
            size="sm"
            className="shadow-lg"
            onClick={jumpToBottom}
          >
            <ArrowDown className="h-3 w-3 mr-1" />
            Jump to latest
          </Button>
        </div>
      )}
    </div>
  );
}

function LogRow({
  entry,
  expanded,
  onToggle,
}: {
  entry: LogEntry;
  expanded: boolean;
  onToggle: () => void;
}) {
  const ts = new Date(entry.ts).toISOString().slice(11, 23); // HH:mm:ss.SSS
  const dotColor = LEVEL_DOT_COLORS[entry.level] ?? "fill-slate-400";
  const badgeColor = LEVEL_COLORS[entry.level] ?? "text-slate-400 bg-slate-400/10";
  const hasFields = entry.fields && Object.keys(entry.fields).length > 0;

  return (
    <>
      <tr
        className="hover:bg-white/5 cursor-pointer border-b border-white/5"
        onClick={hasFields ? onToggle : undefined}
      >
        <td className="py-0.5 px-2 text-slate-500 whitespace-nowrap align-top w-[85px]">
          {ts}
        </td>
        <td className="py-0.5 px-1 align-top w-[52px]">
          <span className={cn("inline-flex items-center gap-1 rounded px-1.5 py-0 text-[10px] font-semibold uppercase", badgeColor)}>
            <Circle className={cn("h-1.5 w-1.5", dotColor)} />
            {entry.level}
          </span>
        </td>
        <td className="py-0.5 px-2 text-slate-200 break-all align-top">
          {entry.msg}
          {entry.caller && (
            <span className="ml-2 text-slate-600">{entry.caller}</span>
          )}
        </td>
      </tr>
      {expanded && hasFields && (
        <tr className="bg-white/[0.02]">
          <td colSpan={3} className="px-4 py-2">
            <pre className="text-[11px] text-slate-400 whitespace-pre-wrap break-all">
              {JSON.stringify(entry.fields, null, 2)}
            </pre>
          </td>
        </tr>
      )}
    </>
  );
}
