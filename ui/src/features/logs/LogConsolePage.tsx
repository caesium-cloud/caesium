import { StatusGlyph } from "@/components/ui/status-badge";
import { statusMeta } from "@/lib/status";
import { useCallback, useMemo, useRef, useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";

import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { api } from "@/lib/api";
import { cn, formatUTCTime } from "@/lib/utils";
import {
  LogBadge,
  LogSearchInput,
  LogShell,
  LogToolbar,
  useAutoScroll,
} from "@/components/logs";
import { useLogStream, type LogEntry } from "./useLogStream";

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
          <h1 className="text-2xl font-bold lowercase text-text-1">Log Console</h1>
          <p className="text-sm text-muted-foreground mt-0.5">
            Real-time server log viewer for operators and admins.
          </p>
        </div>
        <section>
          <div>
            <h3 className="text-base flex items-center gap-2">

              Log Console Disabled
            </h3>
          </div>
          <div className="space-y-3">
            <p className="text-sm text-muted-foreground">
              The log console is not enabled on this Caesium instance. To enable it, set the following
              environment variable and restart:
            </p>
            <code className="block rounded bg-muted px-3 py-2 text-sm">
              CAESIUM_LOG_CONSOLE_ENABLED=true
            </code>
          </div>
        </section>
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
                ? "border-success/30 bg-success/10 text-success"
                : "border-danger/30 bg-danger/10 text-danger"
            }
          >
            {connected ? (
              <span className="inline-flex items-center gap-1.5">
                 Connected
              </span>
            ) : (
              <span className="inline-flex items-center gap-1.5">
                 Disconnected
              </span>
            )}
          </LogBadge>
          {levelData?.level && (
            <LogBadge>{levelData.level}</LogBadge>
          )}
        </>
      }
    >
      {/* Level filter */}
      <div className="flex items-center rounded-md border border-graphite/60 overflow-hidden">
        {LEVELS.map((lvl) => (
          <button
            key={lvl}
            onClick={() => setMinLevel(lvl)}
            className={cn(
              "px-2.5 py-1 text-xs font-normal capitalize transition-colors",
              minLevel === lvl
                ? "bg-primary text-primary-foreground"
                : "bg-midnight text-text-3 hover:bg-graphite/40 hover:text-text-1",
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
        className="h-7 rounded-md border border-graphite/60 bg-midnight px-2 text-xs text-text-2"
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
          className="h-7 px-2 text-xs text-text-2 hover:bg-graphite/40 hover:text-text-1"
          onClick={paused ? resume : pause}
        >
          <StatusGlyph meta={statusMeta(paused ? "paused" : "running")} />
          {paused ? "Resume" : "Pause"}
        </Button>
        <Button
          variant="ghost"
          size="sm"
          className="h-7 px-2 text-xs text-text-2 hover:bg-graphite/40 hover:text-text-1"
          onClick={clear}
        >

          Clear
        </Button>
      </div>
    </LogToolbar>
  );

  return (
    <div className="flex flex-col h-[calc(100vh-7rem)] space-y-4">
      {/* Page header */}
      <div className="shrink-0">
        <h1 className="text-2xl font-bold lowercase text-text-1">Log Console</h1>
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
          className="h-full overflow-auto text-xs"
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
  const ts = formatUTCTime(entry.ts, { milliseconds: true });
  const meta = { ...statusMeta(entry.level === "debug" ? "queued" : entry.level === "info" ? "succeeded" : entry.level === "warn" ? "paused" : "failed"), dotClass: "" };
  const hasFields = entry.fields && Object.keys(entry.fields).length > 0;

  return (
    <>
      <tr
        className="hover:bg-graphite/30 cursor-pointer border-b border-graphite/30"
        onClick={hasFields ? onToggle : undefined}
      >
        <td className="py-0.5 px-2 text-text-3 whitespace-nowrap align-top w-[85px]">
          {ts}
        </td>
        <td className="py-0.5 px-1 align-top w-[52px]">
          <span className="inline-flex items-center gap-2 text-xs font-bold lowercase" style={{ color: meta.fg }}>
            <StatusGlyph meta={meta} />{entry.level}
          </span>
        </td>
        <td className="py-0.5 px-2 text-text-2 break-all align-top">
          {entry.msg}
          {entry.caller && (
            <span className="ml-2 text-text-3">{entry.caller}</span>
          )}
        </td>
      </tr>
      {expanded && hasFields && (
        <tr className="bg-graphite/15">
          <td colSpan={3} className="px-4 py-2">
            <pre className="text-[11px] text-text-3 whitespace-pre-wrap break-all">
              {JSON.stringify(entry.fields, null, 2)}
            </pre>
          </td>
        </tr>
      )}
    </>
  );
}
