import { useCallback, useMemo, useRef, useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import {
  ArrowDown,
  Circle,
  Pause,
  Play,
  ScrollText,
  Trash2,
  Wifi,
  WifiOff,
} from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { api } from "@/lib/api";
import { cn } from "@/lib/utils";
import {
  LogBadge,
  LogSearchInput,
  LogShell,
  LogToolbar,
  useAutoScroll,
} from "@/components/logs";
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

  const { autoScroll, handleScroll, jumpToBottom } = useAutoScroll({
    scrollRef,
    bottomRef,
    dependency: filtered.length,
  });

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

  const emptyState = entries.length === 0
    ? { title: "Waiting for log entries...", body: "Entries will appear as the server emits them." }
    : filtered.length === 0
      ? { title: "No entries match filters", body: "Try broadening the level or search filter." }
      : null;

  const toolbar = (
    <LogToolbar
      status={
        <>
          <LogBadge
            className={
              connected
                ? "border-emerald-500/30 bg-emerald-500/10 text-emerald-200"
                : "border-red-500/30 bg-red-500/10 text-red-300"
            }
          >
            {connected ? (
              <span className="inline-flex items-center gap-1.5">
                <Wifi className="h-3 w-3" /> Connected
              </span>
            ) : (
              <span className="inline-flex items-center gap-1.5">
                <WifiOff className="h-3 w-3" /> Disconnected
              </span>
            )}
          </LogBadge>
          {levelData?.level && (
            <LogBadge className="font-mono">{levelData.level}</LogBadge>
          )}
        </>
      }
    >
      {/* Level filter */}
      <div className="flex items-center rounded-md border border-slate-700 overflow-hidden">
        {LEVELS.map((lvl) => (
          <button
            key={lvl}
            onClick={() => setMinLevel(lvl)}
            className={cn(
              "px-2.5 py-1 text-xs font-medium capitalize transition-colors",
              minLevel === lvl
                ? "bg-primary text-primary-foreground"
                : "bg-slate-900 text-slate-400 hover:bg-slate-800 hover:text-slate-200",
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
        className="h-7 rounded-md border border-slate-700 bg-slate-900 px-2 text-xs text-slate-300"
        title="Server log level"
      >
        {LEVELS.map((lvl) => (
          <option key={lvl} value={lvl}>
            Server: {lvl}
          </option>
        ))}
      </select>

      {/* Search */}
      <LogSearchInput
        value={search}
        onChange={setSearch}
        placeholder="Filter messages..."
        className="max-w-xs"
      />

      <div className="flex items-center gap-1">
        <Button
          variant="ghost"
          size="sm"
          className="h-7 px-2 text-xs text-slate-300 hover:bg-slate-800 hover:text-slate-50"
          onClick={paused ? resume : pause}
        >
          {paused ? <Play className="h-3 w-3 mr-1" /> : <Pause className="h-3 w-3 mr-1" />}
          {paused ? "Resume" : "Pause"}
        </Button>
        <Button
          variant="ghost"
          size="sm"
          className="h-7 px-2 text-xs text-slate-300 hover:bg-slate-800 hover:text-slate-50"
          onClick={clear}
        >
          <Trash2 className="h-3 w-3 mr-1" />
          Clear
        </Button>
      </div>
    </LogToolbar>
  );

  return (
    <div className="flex flex-col h-[calc(100vh-7rem)] space-y-4">
      {/* Page header */}
      <div className="shrink-0">
        <h1 className="text-2xl font-bold tracking-tight">Log Console</h1>
        <p className="text-sm text-muted-foreground mt-0.5">
          Real-time server log stream
        </p>
      </div>

      {/* Log viewer */}
      <LogShell
        toolbar={toolbar}
        emptyState={emptyState}
        hasVisibleOutput={filtered.length > 0}
        className="flex-1 rounded-md border border-border"
      >
        <div
          ref={scrollRef}
          onScroll={handleScroll}
          className="h-full overflow-auto font-mono text-xs"
        >
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
          <div ref={bottomRef} />
        </div>

        {/* Jump to bottom */}
        {!autoScroll && (
          <div className="absolute bottom-4 right-4 z-10">
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
      </LogShell>
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
